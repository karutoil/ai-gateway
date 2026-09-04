package proxy

// Self-contained tests for /v1/responses streaming (native pass-through and
// translated chat/anthropic → Responses SSE). Deliberately shares NO helpers
// with other test files: harness, SSE parser and assertions live here.

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/resilience"

	"github.com/go-chi/chi/v5"
)

// ---------------------------------------------------------------- harness

type rsGateway struct {
	url      string
	key      string
	h        *Handler
	provID   string
	database *sql.DB
}

func rsNewGateway(t *testing.T, providerType models.ProviderType, baseURL string, tune func(*Handler)) *rsGateway {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	p, err := ps.Create("prov-under-test", providerType, baseURL, "sk-upstream-test")
	if err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("test-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0}
	h.Timeouts.StreamIdle = 2 * time.Second // small-but-comfortable watchdog
	if tune != nil {
		tune(h)
	}
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/responses", h.Responses)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &rsGateway{url: srv.URL, key: k.Key, h: h, provID: p.ID, database: database}
}

func rsPostStreaming(t *testing.T, gw *rsGateway, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", gw.url+"/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+gw.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// ---------------------------------------------------------------- sse parsing

type rsEvent struct {
	name string
	data string
}

// rsParseEvents splits raw SSE text into events on blank-line boundaries.
func rsParseEvents(raw string) []rsEvent {
	var out []rsEvent
	for _, blk := range strings.Split(raw, "\n\n") {
		var ev rsEvent
		var dataLines []string
		for _, ln := range strings.Split(blk, "\n") {
			ln = strings.TrimSuffix(ln, "\r")
			switch {
			case strings.HasPrefix(ln, "event:"):
				ev.name = strings.TrimSpace(strings.TrimPrefix(ln, "event:"))
			case strings.HasPrefix(ln, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(ln, "data:")))
			}
		}
		if ev.name == "" && len(dataLines) == 0 {
			continue
		}
		ev.data = strings.Join(dataLines, "\n")
		out = append(out, ev)
	}
	return out
}

func rsNames(events []rsEvent) []string {
	names := make([]string, len(events))
	for i, ev := range events {
		names[i] = ev.name
	}
	return names
}

func rsExpectNames(t *testing.T, events []rsEvent, want []string, raw string) {
	t.Helper()
	got := rsNames(events)
	if len(got) != len(want) {
		t.Fatalf("event count mismatch:\n got %v\nwant %v\nraw=%s", got, want, raw)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event order mismatch at %d:\n got %v\nwant %v\nraw=%s", i, got, want, raw)
		}
	}
}

type rsUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type rsEnvelope struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	Delta          string `json:"delta"`
	Text           string `json:"text"`
	Response       struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Usage  rsUsage
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
}

func rsDecode(t *testing.T, ev rsEvent) rsEnvelope {
	t.Helper()
	var env rsEnvelope
	if err := json.Unmarshal([]byte(ev.data), &env); err != nil {
		t.Fatalf("malformed %q data %q: %v", ev.name, ev.data, err)
	}
	return env
}

func rsConcatDeltas(t *testing.T, events []rsEvent) string {
	t.Helper()
	var sb strings.Builder
	for _, ev := range events {
		if ev.name != "response.output_text.delta" {
			continue
		}
		sb.WriteString(rsDecode(t, ev).Delta)
	}
	return sb.String()
}

func rsFindTerminal(events []rsEvent) *rsEvent {
	for i := range events {
		if events[i].name == "response.failed" {
			return &events[i]
		}
	}
	return nil
}

func rsIndexOfName(t *testing.T, events []rsEvent, name string) int {
	t.Helper()
	for i := range events {
		if events[i].name == name {
			return i
		}
	}
	t.Fatalf("missing event %s in %v", name, rsNames(events))
	return -1
}

// ---------------------------------------------------------------- upstreams

// rsUpstream counts hits per registered path.
type rsUpstream struct {
	srv  *httptest.Server
	hits map[string]*atomic.Int32
}

func rsNewUpstream(t *testing.T, handlers map[string]http.HandlerFunc) *rsUpstream {
	t.Helper()
	u := &rsUpstream{hits: map[string]*atomic.Int32{}}
	mux := http.NewServeMux()
	for path, fn := range handlers {
		counter := &atomic.Int32{}
		u.hits[path] = counter
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			fn(w, r)
		})
	}
	u.srv = httptest.NewServer(mux)
	t.Cleanup(u.srv.Close)
	return u
}

func (u *rsUpstream) hitsFor(path string) int32 {
	if c, ok := u.hits[path]; ok {
		return c.Load()
	}
	return 0
}

var rsChatPieces = []string{"Hel", "lo wor", "ld"}

func rsChatStreamHandler(capturedBody *atomic.Value) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody.Store(string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		frames := []string{
			`{"id":"cmpl-x","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"cmpl-x","choices":[{"index":0,"delta":{"content":"` + rsChatPieces[0] + `"}}]}`,
			`{"id":"cmpl-x","choices":[{"index":0,"delta":{"content":"` + rsChatPieces[1] + `"}}]}`,
			`{"id":"cmpl-x","choices":[{"index":0,"delta":{"content":"` + rsChatPieces[2] + `"}}]}`,
			`{"id":"cmpl-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"cmpl-x","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`,
			`[DONE]`,
		}
		for _, f := range frames {
			io.WriteString(w, "data: "+f+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

// ------------------------------------------------------- 1. chat golden path

func TestResponsesStreamChatGoldenSequence(t *testing.T) {
	var capturedChatBody atomic.Value
	up := rsNewUpstream(t, map[string]http.HandlerFunc{
		"/v1/responses":        func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"/v1/chat/completions": rsChatStreamHandler(&capturedChatBody),
	})

	gw := rsNewGateway(t, models.ProviderOpenAI, up.srv.URL+"/v1", nil)

	resp := rsPostStreaming(t, gw, `{"model":"gpt-rs-test","input":"say hi","stream":true,"max_output_tokens":64}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(b))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}
	if v := resp.Header.Get("X-Cache"); v != "MISS" {
		t.Fatalf("expected X-Cache MISS header, got %q", v)
	}

	raw, rawErr := io.ReadAll(resp.Body)
	if rawErr != nil {
		t.Fatalf("stream ended with client error: %v (raw=%s)", rawErr, string(raw))
	}
	events := rsParseEvents(string(raw))

	rsExpectNames(t, events, []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}, string(raw))

	prevSeq := 0
	for _, ev := range events {
		env := rsDecode(t, ev)
		if env.SequenceNumber <= prevSeq {
			t.Fatalf("sequence_number not strictly increasing at %s: %d after %d", ev.name, env.SequenceNumber, prevSeq)
		}
		prevSeq = env.SequenceNumber
	}

	wantText := rsChatPieces[0] + rsChatPieces[1] + rsChatPieces[2]
	if full := rsConcatDeltas(t, events); full != wantText {
		t.Fatalf("concatenated deltas = %q, want %q", full, wantText)
	}
	if doneText := rsDecode(t, events[7]).Text; doneText != wantText {
		t.Fatalf("output_text.done text = %q, want %q", doneText, wantText)
	}

	completed := rsDecode(t, events[len(events)-1])
	if completed.Response.Status != "completed" {
		t.Fatalf("final response status = %q", completed.Response.Status)
	}
	if completed.Response.Usage.InputTokens != 7 || completed.Response.Usage.OutputTokens != 5 || completed.Response.Usage.TotalTokens != 12 {
		t.Fatalf("completed usage wrong: %+v", completed.Response.Usage)
	}

	body, _ := capturedChatBody.Load().(string)
	var reqMap map[string]interface{}
	if json.Unmarshal([]byte(body), &reqMap) != nil || reqMap["stream"] != true {
		t.Fatalf("outbound chat body missing forced stream=true: %s", body)
	}

	if n := up.hitsFor("/v1/chat/completions"); n != 1 {
		t.Fatalf("expected exactly 1 upstream chat hit, got %d", n)
	}

	var ptoks, ctoks, total int
	var isStreamFlag any
	err := gw.database.QueryRow(`SELECT prompt_tokens, completion_tokens, total_tokens, is_stream FROM request_logs ORDER BY rowid DESC LIMIT 1`).Scan(&ptoks, &ctoks, &total, &isStreamFlag)
	if err != nil {
		t.Fatalf("request_logs query failed: %v", err)
	}
	isStream := 0
	switch v := isStreamFlag.(type) {
	case bool:
		if v {
			isStream = 1
		}
	case int64:
		isStream = int(v)
	}
	if ptoks != 7 || ctoks != 5 || total != 12 || isStream != 1 {
		t.Fatalf("logged usage mismatch: prompt=%d completion=%d total=%d stream=%d", ptoks, ctoks, total, isStream)
	}
}

// --------------------------------------------------- 2. anthropic golden path

func TestResponsesStreamAnthropicGoldenSkipsThinking(t *testing.T) {
	up := rsNewUpstream(t, map[string]http.HandlerFunc{
		"/v1/messages": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			frames := [][2]string{
				{"message_start", `{"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":9}}}`},
				{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`},
				{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"thinking"}}`},
				{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"secret-plan"}}`},
				{"content_block_stop", `{"type":"content_block_stop","index":1}`},
				{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi "}}`},
				{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"there"}}`},
				{"content_block_stop", `{"type":"content_block_stop","index":0}`},
				{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`},
				{"message_stop", `{"type":"message_stop"}`},
			}
			for _, f := range frames {
				io.WriteString(w, "event: "+f[0]+"\ndata: "+f[1]+"\n\n")
				if fl != nil {
					fl.Flush()
				}
			}
		},
	})

	gw := rsNewGateway(t, models.ProviderAnthropic, up.srv.URL, nil)

	resp := rsPostStreaming(t, gw, `{"model":"claude-rs-test","input":"say hi","stream":true,"max_output_tokens":64}`)
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	events := rsParseEvents(string(raw))

	rsExpectNames(t, events, []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}, string(raw))

	if full := rsConcatDeltas(t, events); full != "Hi there" {
		t.Fatalf("concatenated deltas = %q, want %q", full, "Hi there")
	}
	if strings.Contains(string(raw), "secret-plan") {
		t.Fatal("thinking block content leaked into Responses stream")
	}

	completed := rsDecode(t, events[len(events)-1])
	if completed.Response.Status != "completed" ||
		completed.Response.Usage.InputTokens != 9 ||
		completed.Response.Usage.OutputTokens != 4 ||
		completed.Response.Usage.TotalTokens != 13 {
		t.Fatalf("anthropic mapped usage wrong: %+v status=%s", completed.Response.Usage, completed.Response.Status)
	}
}

// --------------------------------------- 3. mid-stream truncation → honest failure

func TestResponsesStreamMidStreamTruncationFailsHonestly(t *testing.T) {
	up := rsNewUpstream(t, map[string]http.HandlerFunc{
		"/v1/responses": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"/v1/chat/completions": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{"content":"AB"}}]}`+"\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(20 * time.Millisecond)
			io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{"content":"CD"}}]}`+"\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(20 * time.Millisecond)
			panic(http.ErrAbortHandler) // decapitate the connection mid-stream
		},
	})

	gw := rsNewGateway(t, models.ProviderOpenAI, up.srv.URL+"/v1", func(h *Handler) {})

	resp := rsPostStreaming(t, gw, `{"model":"gpt-rs-test","input":"hi","stream":true}`)
	raw, _ := io.ReadAll(resp.Body) // truncated body: read anomalies tolerated
	resp.Body.Close()

	events := rsParseEvents(string(raw))
	terminal := rsFindTerminal(events)
	if terminal == nil {
		t.Fatalf("expected terminal response.failed event after truncation, raw=%s", string(raw))
	}
	env := rsDecode(t, *terminal)
	if env.Response.Status != "failed" {
		t.Fatalf("terminal status = %q, want failed", env.Response.Status)
	}
	if env.Response.Error == nil || env.Response.Error.Code != "upstream_error" {
		t.Fatalf("terminal error wrong: %+v", env.Response.Error)
	}
	for _, ev := range events[rsIndexOfName(t, events, "response.failed")+1:] {
		if ev.name == "response.output_text.delta" {
			t.Fatal("deltas emitted after response.failed")
		}
	}

	// post-commit retry must never re-fire against the provider.
	if n := up.hitsFor("/v1/chat/completions"); n != 1 {
		t.Fatalf("post-commit retry detected: %d upstream hits, want 1", n)
	}
}

// ------------------------------------------- 4. native trickle pass-through

func TestResponsesStreamNativeTricklesFirstChunkEarly(t *testing.T) {
	var (
		wroteFirst atomic.Int64
		wroteLast  atomic.Int64
		gotFirst   atomic.Int64
	)
	up := rsNewUpstream(t, map[string]http.HandlerFunc{
		"/v1/responses": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
			if fl != nil {
				fl.Flush()
			}
			wroteFirst.Store(time.Now().UnixNano())
			time.Sleep(100 * time.Millisecond)
			io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"middle\"}\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(100 * time.Millisecond)
			// Record the timestamp BEFORE the final write: once the client has
			// read the completed event off the wire, wroteLast is guaranteed
			// set (TCP causality), so the assertion below can't race the
			// handler goroutine on a loaded -race runner.
			wroteLast.Store(time.Now().UnixNano())
			io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":11,\"output_tokens\":3,\"total_tokens\":14}}}\n\n")
			if fl != nil {
				fl.Flush()
			}
		},
	})

	gw := rsNewGateway(t, models.ProviderOpenAI, up.srv.URL+"/v1", nil)

	resp := rsPostStreaming(t, gw, `{"model":"gpt-rs-test","input":"hi","stream":true}`)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("native passthrough content-type = %q", ct)
	}

	// readUntilEvent accumulates lines until one complete SSE block lands.
	readUntilEvent := func(reader *bufio.Reader) string {
		var sb strings.Builder
		for {
			line, err := reader.ReadString('\n')
			sb.WriteString(line)
			if err != nil {
				return sb.String()
			}
			if sb.Len() > 0 && strings.TrimSpace(line) == "" {
				return sb.String()
			}
		}
	}

	reader := bufio.NewReader(resp.Body)
	first := readUntilEvent(reader)
	gotFirst.Store(time.Now().UnixNano())
	second := readUntilEvent(reader)
	third := readUntilEvent(reader)

	if !strings.Contains(first, "response.created") {
		t.Fatalf("first relayed chunk should be response.created, got %q", first)
	}
	full := first + second + third
	if !strings.Contains(full, "\"middle\"") || !strings.Contains(full, "input_tokens") {
		t.Fatalf("native transcript incomplete: %q", full)
	}

	// Liveness proof: the FIRST chunk reached the client strictly before the
	// upstream finished writing its LAST chunk — no whole-body buffering.
	if gotFirst.Load() <= 0 || wroteFirst.Load() <= 0 || wroteLast.Load() <= 0 {
		t.Fatal("timing capture incomplete")
	}
	if gotFirst.Load() >= wroteLast.Load() {
		t.Fatalf("first chunk arrived at %d, not before last upstream write at %d (buffering suspected)",
			gotFirst.Load(), wroteLast.Load())
	}

	// The gateway's bookkeeping (usage log) lands just after the final frame
	// is relayed and the upstream EOF propagates — poll briefly for the row.
	var ptoks, ctoks, total int
	var isStreamFlag any
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := gw.database.QueryRow(`SELECT prompt_tokens, completion_tokens, total_tokens, is_stream FROM request_logs ORDER BY rowid DESC LIMIT 1`).Scan(&ptoks, &ctoks, &total, &isStreamFlag)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("native usage log never landed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	isStream := 0
	switch v := isStreamFlag.(type) {
	case bool:
		if v {
			isStream = 1
		}
	case int64:
		isStream = int(v)
	}
	if ptoks != 11 || ctoks != 3 || total != 14 || isStream != 1 {
		t.Fatalf("native harvested usage mismatch: prompt=%d completion=%d total=%d stream=%d", ptoks, ctoks, total, isStream)
	}
}

// ------------------------------- 5. pre-commit 500 exhausts retries cleanly

func TestResponsesStreamPreCommitRetryThenFailedFrame(t *testing.T) {
	up := rsNewUpstream(t, map[string]http.HandlerFunc{
		"/v1/responses": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"/v1/chat/completions": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":{"message":"kaput"}}`)
		},
	})

	gw := rsNewGateway(t, models.ProviderOpenAI, up.srv.URL+"/v1", func(h *Handler) {
		h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond}
	})

	resp := rsPostStreaming(t, gw, `{"model":"gpt-rs-test","input":"hi","stream":true}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exhausted pre-commit retries should answer as committed SSE 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected SSE content-type on clean failure, got %q", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	events := rsParseEvents(string(raw))

	terminal := rsFindTerminal(events)
	if terminal == nil {
		t.Fatalf("expected single response.failed frame, raw=%s", string(raw))
	}
	env := rsDecode(t, *terminal)
	if env.Response.Status != "failed" ||
		env.Response.Error == nil ||
		env.Response.Error.Code != "upstream_error" ||
		!strings.Contains(env.Response.Error.Message, "kaput") {
		t.Fatalf("failure envelope wrong: status=%q err=%+v", env.Response.Status, env.Response.Error)
	}
	for _, ev := range events {
		if ev.name != "response.failed" {
			t.Fatalf("unexpected non-terminal event %q on exhausted-retry path", ev.name)
		}
	}

	// MaxRetries:1 ⇒ initial attempt + one retry against the SAME provider.
	if n := up.hitsFor("/v1/chat/completions"); n != 2 {
		t.Fatalf("expected 2 upstream attempts (initial + retry), got %d", n)
	}
}

// --------------------------------- 6. native ping/[DONE] filtered for strict clients
//
// Strict Responses clients (Grok Build CLI's Rust serde enum) fail hard with
// "unknown variant `ping`" instead of ignoring transport keepalives. The
// native pass-through must drop ping + [DONE] while relaying real
// response.* frames byte-exact.
func TestResponsesStreamNativeFiltersPingForStrictClients(t *testing.T) {
	up := rsNewUpstream(t, map[string]http.HandlerFunc{
		"/v1/responses": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			frames := []string{
				"event: ping\ndata: {\"type\":\"ping\"}\n\n",
				"event: response.created\ndata: {\"type\":\"response.created\"}\n\n",
				"data: {\"type\":\"ping\"}\n\n",
				": upstream keepalive comment\n\n",
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n",
				"data: [DONE]\n\n",
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n",
			}
			for _, f := range frames {
				io.WriteString(w, f)
				if fl != nil {
					fl.Flush()
				}
			}
		},
	})

	gw := rsNewGateway(t, models.ProviderOpenAI, up.srv.URL+"/v1", nil)

	resp := rsPostStreaming(t, gw, `{"model":"gpt-rs-test","input":"hi","stream":true}`)
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	rawStr := string(raw)

	// Strict-client simulation: no ping variant and no [DONE] sentinel may
	// reach the client; every data payload must be a response.* / error type
	// or a comment (which rsParseEvents already skips).
	if strings.Contains(rawStr, `"type":"ping"`) || strings.Contains(rawStr, "event: ping") {
		t.Fatalf("ping leaked to strict client, raw=%s", rawStr)
	}
	if strings.Contains(rawStr, "[DONE]") {
		t.Fatalf("[DONE] leaked to Responses client (terminator is response.completed), raw=%s", rawStr)
	}

	events := rsParseEvents(rawStr)
	for _, ev := range events {
		if ev.name == "ping" {
			t.Fatalf("ping event relayed, raw=%s", rawStr)
		}
		if ev.name != "" && !strings.HasPrefix(ev.name, "response.") {
			t.Fatalf("non-response event %q relayed to strict client, raw=%s", ev.name, rawStr)
		}
	}

	// Real protocol frames survive filtering.
	names := rsNames(events)
	hasCreated, hasDelta, hasCompleted := false, false, false
	for _, n := range names {
		switch n {
		case "response.created":
			hasCreated = true
		case "response.output_text.delta":
			hasDelta = true
		case "response.completed":
			hasCompleted = true
		}
	}
	if !hasCreated || !hasDelta || !hasCompleted {
		t.Fatalf("real frames lost during ping filtering, got %v raw=%s", names, rawStr)
	}
}

// Strict clients require output on every Response object.
func TestResponsesStreamResponseObjectsCarryOutput(t *testing.T) {
	up := rsNewUpstream(t, map[string]http.HandlerFunc{
		"/v1/responses": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"/v1/chat/completions": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":{"message":"kaput"}}`)
		},
	})
	gw := rsNewGateway(t, models.ProviderOpenAI, up.srv.URL+"/v1", func(h *Handler) {
		h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0, BaseDelay: time.Millisecond}
	})
	resp := rsPostStreaming(t, gw, `{"model":"gpt-rs-test","input":"hi","stream":true}`)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, ev := range rsParseEvents(string(raw)) {
		if !strings.HasPrefix(ev.name, "response.") {
			continue
		}
		var env map[string]interface{}
		if err := json.Unmarshal([]byte(ev.data), &env); err != nil {
			t.Fatalf("malformed %q data: %v", ev.name, err)
		}
		inner, _ := env["response"].(map[string]interface{})
		if inner == nil {
			continue
		}
		if _, ok := inner["output"]; !ok {
			t.Fatalf("event %s missing output: %s", ev.name, ev.data)
		}
	}
}
