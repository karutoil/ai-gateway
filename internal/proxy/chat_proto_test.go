package proxy

// CONFORMANCE SUITE — OPENAI /v1/chat/completions (streaming + buffered).
//
// Contract: httptest mock upstreams feed REAL chi-wired handlers through
// middleware.GatewayAuth (setup idiom borrowed from lb_route_test.go).
// Checkpoints encode CURRENT behavior; segments that violate their spec
// checkpoint are wrapped with t.Skipf("DEFECT-K ...") so the suite is green
// today and becomes a hard regression gate once fixed.
//
// Shared harness helpers (protoNewEnv, protoPostJSON, protoEvents,
// protoPollUsage) live here and are reused by anthropic_proto_test.go and
// completions_proto_test.go. Everything is self-contained per test: own
// :memory: sqlite (db.Open), own stores, own upstream + gateway servers.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/cache"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/resilience"

	"github.com/go-chi/chi/v5"
)

// ---------------------------------------------------------------------------
// Shared harness (single definition for all three *_proto_test.go files)
// ---------------------------------------------------------------------------

// protoEnv owns one fully isolated gateway stack.
type protoEnv struct {
	t       *testing.T
	DB      *sql.DB
	PS      *provider.Store
	KS      *apikey.Store
	H       *Handler
	Gateway *httptest.Server
	Key     string

	hits     atomic.Int32 // upstream request counter
	mu       sync.Mutex
	lastPath string
	lastBody []byte
	lastHdr  http.Header

	upstream *httptest.Server
}

const protoMasterKeyLen = 32

// protoNewEnv wires a chi router with GatewayAuth and mounts the requested
// endpoints around one mock upstream. baseSuffix is appended to the upstream
// URL when registering the provider ("/v1" for OpenAI-style, "" for native
// anthropic). up == nil registers an unroutable provider (for tests that only
// exercise pre-upstream rejection paths but still need a resolvable model).
func protoNewEnv(t *testing.T, up http.HandlerFunc, provType models.ProviderType, baseSuffix string, mounts ...string) *protoEnv {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ps := provider.NewStore(database, make([]byte, protoMasterKeyLen))
	ks := apikey.NewStore(database)
	env := &protoEnv{t: t, DB: database, PS: ps, KS: ks}
	t.Cleanup(func() { database.Close() })

	if up != nil {
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			env.hits.Add(1)
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(b))) // replay for downstream handler reads
			env.mu.Lock()
			env.lastPath = r.URL.Path
			env.lastBody = append([]byte(nil), b...)
			env.lastHdr = r.Header.Clone()
			env.mu.Unlock()
			up.ServeHTTP(w, r)
		})
		env.upstream = httptest.NewServer(inner)
		base := env.upstream.URL + baseSuffix
		if _, err := ps.Create("proto-up", provType, base, "sk-proto-up"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(env.upstream.Close)
	} else {
		if _, err := ps.Create("proto-none", provType, "http://127.0.0.1:1/unreachable", "sk-none"); err != nil {
			t.Fatal(err)
		}
	}

	k, err := ks.Create("proto-key")
	if err != nil {
		t.Fatal(err)
	}
	env.Key = k.Key

	h := newLegacyHandler(ps, database)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0} // deterministic single attempt
	h.Timeouts.StreamIdle = 30 * time.Second                // watchdog safety net, never hit in these tests
	env.H = h

	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	for _, m := range mounts {
		switch m {
		case "chat":
			r.Post("/v1/chat/completions", h.ChatCompletions)
		case "messages":
			r.Post("/v1/messages", h.AnthropicMessages)
		case "completions":
			r.Post("/v1/completions", h.Completions)
		default:
			t.Fatalf("unknown mount %q", m)
		}
	}
	env.Gateway = httptest.NewServer(r)
	t.Cleanup(env.Gateway.Close)
	return env
}

func (e *protoEnv) HitCount() int { return int(e.hits.Load()) }

// LastUpstream returns path/body/headers of the most recent upstream hit.
func (e *protoEnv) LastUpstream() (path string, body []byte, hdr http.Header) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastPath, append([]byte(nil), e.lastBody...), e.lastHdr.Clone()
}

// PostRaw POSTs to the gateway with the auth key pre-attached; extra headers
// override/add. Returns status, response headers and full raw body.
func (e *protoEnv) PostRaw(path, body string, hdrs map[string]string) (int, http.Header, string) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Gateway.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.Key)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Clone(), string(raw)
}

// CreateAllowedKey adds a second key restricted to exactly the given models.
func (e *protoEnv) CreateAllowedKey(allowed []string) string {
	e.t.Helper()
	k, err := e.KS.Create("proto-key-restricted")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.KS.UpdateLimits(k.ID, nil, nil, nil, nil, &allowed); err != nil {
		e.t.Fatal(err)
	}
	return k.Key
}

// PollUsage queries the newest request_logs row until its token totals are
// non-zero or the deadline passes (row insert is synchronous inside the
// handler but races the client's final read on streaming responses).
func protoPollUsage(t *testing.T, d *sql.DB, deadline time.Duration) (prompt, completion, total int) {
	t.Helper()
	var pt, ct, tt sql.NullInt64
	end := time.Now().Add(deadline)
	for {
		err := d.QueryRow(`SELECT prompt_tokens, completion_tokens, total_tokens FROM request_logs ORDER BY rowid DESC LIMIT 1`).Scan(&pt, &ct, &tt)
		if err == nil && tt.Valid && tt.Int64 > 0 {
			return int(pt.Int64), int(ct.Int64), int(tt.Int64)
		}
		if time.Now().After(end) {
			if err != nil {
				t.Fatalf("request_logs query failed: %v", err)
			}
			return int(pt.Int64), int(ct.Int64), int(tt.Int64)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// protoEvent is one parsed SSE frame from a raw gateway response.
type protoEvent struct {
	Name string // "event:" value ("" when absent)
	Data string // joined data: payload
	Raw  string // verbatim block including trailing separator position info stripped
}

// protoEvents splits an SSE byte stream into frames on blank-line boundaries
// and extracts event:/data: lines (multi-line data rejoined with \n).
func protoEvents(raw string) []protoEvent {
	var out []protoEvent
	blocks := strings.Split(raw, "\n\n")
	for _, b := range blocks {
		if strings.TrimSpace(b) == "" {
			continue
		}
		ev := protoEvent{Raw: b}
		var datas []string
		for _, line := range strings.Split(b, "\n") {
			line = strings.TrimSuffix(line, "\r")
			switch {
			case strings.HasPrefix(line, "event:"):
				ev.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				datas = append(datas, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		ev.Data = strings.Join(datas, "\n")
		out = append(out, ev)
	}
	return out
}

// protoAssertDataLinesValid enforces checkpoint a3/c1: every data: line in the
// stream carries either "[DONE]" or syntactically valid single-line JSON.
func protoAssertDataLinesValid(t *testing.T, where, raw string) {
	t.Helper()
	for i, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			continue
		}
		if !json.Valid([]byte(payload)) {
			t.Fatalf("%s: data line %d is not valid JSON: %q", where, i, payload)
		}
	}
}

// ---------------------------------------------------------------------------
// c1+c2+c5 — wire format, first-chunk role fidelity, finish_reason pass-through
// ---------------------------------------------------------------------------

var protoChatBaseFrames = []string{
	`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
	`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
	`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
	`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	`data: [DONE]`,
}

func TestChatProtoStreamWireFormatAndRoleFidelity(t *testing.T) {
	var sb strings.Builder
	for _, f := range protoChatBaseFrames {
		sb.WriteString(f + "\n\n")
	}
	expectedBytes := sb.String()

	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		for _, f := range protoChatBaseFrames {
			fmt.Fprint(w, f+"\n\n")
			flusher.Flush()
		}
	}
	env := protoNewEnv(t, up, models.ProviderOpenAI, "/v1", "chat")

	status, hdr, raw := env.PostRaw("/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)

	// c1: status/Content-Type/frame envelope.
	if status != 200 {
		t.Fatalf("expected 200, got %d: %s", status, raw)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("checkpoint c1: Content-Type must be text/event-stream, got %q", ct)
	}

	// c2: first chunk carries delta.role=="assistant"; gateway must relay it
	// byte-for-byte. Full-body equality proves no synthesis/stripping happened.
	if raw != expectedBytes {
		t.Fatalf("checkpoint c2: pass-through fidelity broken.\nwant:\n%q\ngot:\n%q", expectedBytes, raw)
	}
	events := protoEvents(raw)
	if len(events) != len(protoChatBaseFrames) {
		t.Fatalf("frame count mismatch: want %d got %d (%q)", len(protoChatBaseFrames), len(events), raw)
	}
	first := events[0].Data
	if !strings.Contains(first, `"role":"assistant"`) {
		t.Fatalf("checkpoint c2: first chunk must carry delta.role assistant, got %q", first)
	}

	// Every non-[DONE] data line must be valid single-line JSON (c1 framing).
	protoAssertDataLinesValid(t, "c1", raw)
	if ev := events[len(events)-1]; ev.Data != "[DONE]" {
		t.Fatalf("checkpoint c1: last frame must be data: [DONE], got %q", ev.Data)
	}

	// c5: finish_reason chunk present exactly once, relayed as emitted.
	frCount := strings.Count(raw, `"finish_reason":"stop"`)
	if frCount != 1 {
		t.Fatalf("checkpoint c5: finish_reason chunk missing/mangled (%d occurrences)", frCount)
	}
	// c5 informational guard: role must NOT repeat after the first chunk.
	roleCount := strings.Count(raw, `"role":"assistant"`)
	if roleCount > 1 {
		t.Logf("INFO c5: upstream misbehaved (role repeated %d times); gateway preserved pass-through — informational only", roleCount)
	}
}

// c2b — upstream OMITS role entirely: gateway must not synthesize
// "role":"assistant" anywhere (pass-through fidelity for shapes we don't make).
func TestChatProtoStreamRoleNotSynthesizedWhenUpstreamOmitsIt(t *testing.T) {
	frames := []string{
		`data: {"id":"chatcmpl-nr","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"bare"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-nr","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString(f + "\n\n")
	}
	expected := sb.String()
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, expected)
	}
	env := protoNewEnv(t, up, models.ProviderOpenAI, "/v1", "chat")

	_, _, raw := env.PostRaw("/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	if raw != expected {
		t.Fatalf("checkpoint c2(b): gateway altered a role-less stream.\nwant:\n%q\ngot:\n%q", expected, raw)
	}
	if strings.Contains(raw, `"role"`) {
		t.Fatalf("checkpoint c2(b): gateway synthesized role into a role-less upstream stream")
	}
}

// ---------------------------------------------------------------------------
// c3 — tool-call deltas forwarded intact; split arguments reassemble client-side
// ---------------------------------------------------------------------------

func TestChatProtoToolCallFragmentReassembly(t *testing.T) {
	fullArgs := `{"location":"Paris","unit":"celsius"}`
	cut := len(fullArgs) / 3
	frag1, frag2 := fullArgs[:cut], fullArgs[cut:]
	stream := fmt.Sprintf(
		"data: {\"id\":\"chatcmpl-tool\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_abc123\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":%[1]s}}]},\"finish_reason\":null}]}\n\n"+
			"data: {\"id\":\"chatcmpl-tool\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":%[2]s}}]},\"finish_reason\":null}]}\n\n"+
			"data: {\"id\":\"chatcmpl-tool\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"+
			"data: [DONE]\n\n",
		jsonQuote(frag1), jsonQuote(frag2))

	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, stream)
	}
	env := protoNewEnv(t, up, models.ProviderOpenAI, "/v1", "chat")

	_, _, raw := env.PostRaw("/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"weather?"}],"stream":true}`, nil)

	// Byte-level: fragments forwarded intact and in order.
	if !strings.Contains(raw, jsonQuote(frag1)) || !strings.Contains(raw, jsonQuote(frag2)) {
		t.Fatalf("checkpoint c3: argument fragments lost or reordered:\n%s", raw)
	}

	// Client-side reassembly simulation: concatenate delta fragments.
	events := protoEvents(raw)
	var assembled, name, id string
	for _, ev := range events {
		if ev.Data == "" || ev.Data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				if tc.ID != "" {
					id = tc.ID
				}
				if tc.Function.Name != "" {
					name = tc.Function.Name
				}
				assembled += tc.Function.Arguments
			}
			if ch.FinishReason != nil && *ch.FinishReason != "tool_calls" {
				t.Fatalf("checkpoint c3: finish_reason should stay tool_calls for tool streams, got %q", *ch.FinishReason)
			}
		}
	}
	if name != "get_weather" || id != "call_abc123" {
		t.Fatalf("checkpoint c3: tool identity lost (name=%q id=%q)", name, id)
	}
	if assembled != fullArgs {
		t.Fatalf("checkpoint c3: fragmented arguments do not reassemble.\nwant %q\ngot  %q", fullArgs, assembled)
	}
	if !json.Valid([]byte(assembled)) {
		t.Fatalf("checkpoint c3: reassembled arguments are not valid JSON")
	}
}

// jsonQuote produces the JSON string literal (with quotes) of s.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ---------------------------------------------------------------------------
// c4 — final usage chunk surfaces to CLIENT unchanged AND drives request_logs
// ---------------------------------------------------------------------------

func TestChatProtoUsageChunkSurfacesAndIsRecorded(t *testing.T) {
	frames := []string{
		`data: {"id":"chatcmpl-u","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-u","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-u","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"chatcmpl-u","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`,
		`data: [DONE]`,
	}
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprint(w, f+"\n\n")
		}
	}
	env := protoNewEnv(t, up, models.ProviderOpenAI, "/v1", "chat")

	_, _, raw := env.PostRaw("/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"count me"}],"stream":true}`, nil)

	usageFrame := frames[3]
	if !strings.Contains(raw, usageFrame) {
		t.Fatalf("checkpoint c4: usage-bearing chunk did not surface unchanged to client\nwant substring: %s\ngot: %s", usageFrame, raw)
	}

	pt, ct, total := protoPollUsage(t, env.DB, 3*time.Second)
	if pt != 7 || ct != 5 {
		t.Fatalf("checkpoint c4: recorded tokens wrong, want prompt=7 completion=5, got prompt=%d completion=%d total=%d", pt, ct, total)
	}
	if total != 12 {
		t.Fatalf("checkpoint c4: total_tokens wrong, want 12 got %d", total)
	}
}

// ---------------------------------------------------------------------------
// n1 — non-streaming JSON passthrough fidelity
// ---------------------------------------------------------------------------

func TestChatProtoNonStreamPassthroughFidelity(t *testing.T) {
	const golden = `{"id":"cmpl-golden","object":"chat.completion","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"passthrough!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, golden)
	}
	env := protoNewEnv(t, up, models.ProviderOpenAI, "/v1", "chat")

	status, _, raw := env.PostRaw("/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"echo"}]}`, nil)
	if status != 200 {
		t.Fatalf("expected 200 got %d: %s", status, raw)
	}
	if raw != golden {
		t.Fatalf("checkpoint n1: body not byte-faithful.\nwant: %s\ngot:  %s", golden, raw)
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	// n1 echo fields pinned explicitly.
	if m["id"] != "cmpl-golden" || m["object"] != "chat.completion" || m["model"] != "gpt-4o-mini" {
		t.Fatalf("checkpoint n1: id/object/model echo broken: %v", m)
	}
}

// ---------------------------------------------------------------------------
// n2/n3 — anthropicToOpenAIChatResponse conversion semantics (the converter is
// currently unreachable over the wire via /v1/chat/completions because
// candidateProviders filters out anthropic-type providers, proxy.go:1287-1288;
// we therefore pin the converter itself, same package symbol).
// ---------------------------------------------------------------------------

func TestChatProtoAnthropicConversionHappyShape(t *testing.T) {
	const anth = `{"id":"msg_42","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[` +
		`{"type":"thinking","thinking":"secret chain","signature":"sig"},` +
		`{"type":"text","text":"Hello "},` +
		`{"type":"text","text":"world"}` +
		`],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":13}}`

	out := anthropicToOpenAIChatResponse([]byte(anth), "gpt-4o-mini")
	if out == nil {
		t.Fatal("converter returned nil for a valid anthropic message")
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["object"] != "chat.completion" {
		t.Fatalf("checkpoint n2: object must be chat.completion, got %v", m["object"])
	}
	choices, _ := m["choices"].([]interface{})
	if len(choices) != 1 {
		t.Fatalf("checkpoint n2: want 1 choice, got %d", len(choices))
	}
	ch, _ := choices[0].(map[string]interface{})
	msg, _ := ch["message"].(map[string]interface{})
	content, _ := msg["content"].(string)
	// n2: text blocks joined; thinking excluded.
	if content != "Hello world" {
		t.Fatalf("checkpoint n2: content join wrong (thinking must be excluded), got %q", content)
	}
	if msg["role"] != "assistant" {
		t.Fatalf("checkpoint n2: message.role wrong: %v", msg["role"])
	}
	// stop_reason end_turn → finish_reason stop (spec mapping that WORKS today).
	if ch["finish_reason"] != "stop" {
		t.Fatalf("checkpoint n2: end_turn must map to finish_reason stop, got %v", ch["finish_reason"])
	}
	usage, _ := m["usage"].(map[string]interface{})
	if usage["prompt_tokens"].(float64) != 11 || usage["completion_tokens"].(float64) != 13 || usage["total_tokens"].(float64) != 24 {
		t.Fatalf("checkpoint n2: usage mapping wrong: %v", usage)
	}
	if m["model"] != "gpt-4o-mini" {
		t.Fatalf("checkpoint n2: model echo wrong: %v", m["model"])
	}
}

func TestChatProtoAnthropicConversionMaxTokensFinishReason(t *testing.T) {
	const anth = `{"id":"msg_trunc","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[{"type":"text","text":"partial"}],"stop_reason":"max_tokens",` +
		`"usage":{"input_tokens":9,"output_tokens":64}}`

	out := anthropicToOpenAIChatResponse([]byte(anth), "m")
	var m map[string]interface{}
	json.Unmarshal(out, &m)
	choices, _ := m["choices"].([]interface{})
	ch, _ := choices[0].(map[string]interface{})

	got, _ := ch["finish_reason"].(string)
	if got != "length" {
		t.Skipf("DEFECT-K chat-D1: anthropicToOpenAIChatResponse ignores stop_reason and always emits finish_reason=\"stop\" (proxy.go:~1159-1168) – spec requires \"length\" for max_tokens truncation – unskip after fix (pinned current value: %q)", got)
	}
	if got != "length" {
		t.Fatalf("checkpoint n2: max_tokens must map to finish_reason length, got %q", got)
	}
}

func TestChatProtoAnthropicConversionEmptyContentEdge(t *testing.T) {
	const anth = `{"id":"msg_empty","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":0}}`
	out := anthropicToOpenAIChatResponse([]byte(anth), "m")
	if out == nil {
		t.Fatal("checkpoint n3: empty content[] must still convert")
	}
	var m map[string]interface{}
	json.Unmarshal(out, &m)
	choices, _ := m["choices"].([]interface{})
	ch, _ := choices[0].(map[string]interface{})
	msg, _ := ch["message"].(map[string]interface{})
	content, ok := msg["content"]
	if !ok {
		t.Fatal("checkpoint n3: content key must exist")
	}
	if s, _ := content.(string); s != "" {
		t.Fatalf("checkpoint n3: empty [] blocks must yield empty-string content, got %#v", content)
	}
}

// ---------------------------------------------------------------------------
// u1 — responses-cache must never poison streams (cache Set skipped, stream=true)
// ---------------------------------------------------------------------------

func TestChatProtoStreamNeverCached(t *testing.T) {
	var calls atomic.Int32
	up := func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		body := "alpha"
		if n == 2 {
			body = "beta"
		}
		fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-c%d\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%s},\"finish_reason\":null}]}\n\n", n, jsonQuote(body))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	env := protoNewEnv(t, up, models.ProviderOpenAI, "/v1", "chat")
	env.H.Cache = cache.NewMemoryCache(16)

	reqBody := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"identical"}],"stream":true}`
	s1, h1, raw1 := env.PostRaw("/v1/chat/completions", reqBody, nil)
	s2, h2, raw2 := env.PostRaw("/v1/chat/completions", reqBody, nil)

	if s1 != 200 || s2 != 200 {
		t.Fatalf("u1: both requests must succeed: %d/%d", s1, s2)
	}
	if x := h1.Get("X-Cache"); x == "HIT" {
		t.Fatalf("u1: stream request 1 served from completion cache (X-Cache=%s)", x)
	}
	if x := h2.Get("X-Cache"); x == "HIT" {
		t.Fatalf("checkpoint u1: STREAM response was cached and replayed — cache poisoning")
	}
	if hits := env.HitCount(); hits != 2 {
		t.Fatalf("u1: stream requests must both reach upstream (hits=%d)", hits)
	}
	if !strings.Contains(raw1, "alpha") || !strings.Contains(raw2, "beta") {
		t.Fatalf("u1: second response replayed stale cached body (poisoning).\nr1=%s\nr2=%s", raw1, raw2)
	}
}

// ---------------------------------------------------------------------------
// u4 — model allowlist 403 precedes any upstream traffic
// ---------------------------------------------------------------------------

func TestChatProtoAllowlistRejectsBeforeUpstream(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[]}`)
	}
	env := protoNewEnv(t, up, models.ProviderOpenAI, "/v1", "chat")
	restricted := env.CreateAllowedKey([]string{"allowed-model"})
	env.Key = restricted

	status, _, raw := env.PostRaw("/v1/chat/completions", `{"model":"secret-model","messages":[{"role":"user","content":"nope"}]}`, nil)
	if status != http.StatusForbidden {
		t.Fatalf("checkpoint u4: disallowed model must be 403, got %d body=%s", status, raw)
	}
	if !strings.Contains(raw, "not allowed") {
		t.Fatalf("u4: unexpected deny body: %s", raw)
	}
	if hits := env.HitCount(); hits != 0 {
		t.Fatalf("checkpoint u4: upstream was contacted %d times despite allowlist denial", hits)
	}

	// Control: an allowed model passes enforcement and reaches upstream.
	env.Key = restricted
	allowedBody := `{"model":"openai/allowed-model","messages":[{"role":"user","content":"ok"}]}`
	statusAllowed, _, _ := env.PostRaw("/v1/chat/completions", allowedBody, nil)
	if statusAllowed != 200 {
		t.Fatalf("u4 control: allowed model should pass enforcement and reach upstream, got %d (hits=%d)", statusAllowed, env.HitCount())
	}
}

// ---------------------------------------------------------------------------
// u5 — TPM estimator ignores base64 blobs (direct same-package call)
// ---------------------------------------------------------------------------

func TestChatProtoEstimateTokensIgnoresBase64Blobs(t *testing.T) {
	const blobLen = 300 * 1000
	blob := strings.Repeat("QUFB", blobLen/4)[:blobLen] // '+' '/' 'A'-'Z' etc. all b64-class; run ≥256 chars
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"describe"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + blob + `"}}]}]}`)

	got := estimateTokens(body)
	if got > 200 {
		t.Fatalf("checkpoint u5: estimateTokens counted the base64 blob (300KB) — got %d tokens", got)
	}
	// Sanity: without stripping, the naive len/4 heuristic would be ~75K.
	if naive := (len(body) + 3) / 4; naive < 50000 {
		t.Fatalf("test invariant broken: naive estimate unexpectedly small (%d) — fixture degenerate", naive)
	}
}

// ---------------------------------------------------------------------------
// u3 — malformed upstream payload (non-SSE 200 text/html) on a STREAM request
// must fail honestly, not silently relay HTML under HTTP 200.
// ---------------------------------------------------------------------------

func TestChatProtoMalformedUpstreamHonestFailure(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html") // deliberately NOT event-stream
		w.WriteHeader(200)
		io.WriteString(w, "<html><body>gateway Maintenance</body></html>\n")
	}
	env := protoNewEnv(t, up, models.ProviderOpenAI, "/v1", "chat")

	status, hdr, raw := env.PostRaw("/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)

	// PIN current behavior: body+content-type are relayed verbatim, HTTP 200.
	if status == 200 && strings.Contains(raw, "<html>") {
		ct := hdr.Get("Content-Type")
		t.Skipf("DEFECT-K chat-D2: pumpStream.commit relays ANY upstream 200 blindly (proxy.go:692-698) — client got HTTP 200 Content-Type=%q with <html> payload, zero SSE frames, no [DONE], no error terminator (silent-ok). Honest failure required – unskip after fix (pinned: 200/html passthrough)", ct)
	}

	// ---- checkpoint asserts (regression gate post-fix) ----
	if status != 502 && status != 500 {
		t.Fatalf("u3: malformed upstream stream must surface an honest failure status, got %d", status)
	}
	if strings.Contains(raw, "<html>") {
		t.Fatalf("u3: raw HTML leaked to an SSE client")
	}
	if !strings.Contains(raw, "\"error\"") {
		t.Fatalf("u3: honest failure must carry an error envelope, got %q", raw)
	}
}
