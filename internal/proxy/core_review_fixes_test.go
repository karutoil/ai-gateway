package proxy

// CORE REVIEW FIX TESTS.
//
// One focused test per defect fixed during the proxy-request-core review:
//
//   A1  anthropicToOpenAIChatResponse dropped tool_use blocks and reported
//       finish_reason "stop" for tool-invoking turns → chat / /v1/responses
//       agents could never see the model's tool invocation.
//   A2  The anthropic→openai translations (chat, completions, responses)
//       dropped Anthropic prompt-cache token fields, so the caller's
//       re-extract of usage under-billed every cached-prompt turn on the
//       translated path (extractUsage folds cache_* onto prompt; the
//       converters threw the fields away).
//   A3  chatToResponsesJSON dropped OpenAI prompt_tokens_details.cached_tokens
//       (cache billed at the fresh input rate on the translated responses
//       path).
//   B1  injectStreamUsage / forceStreamTrue re-encoded the whole body through
//       map[string]interface{} (float64), corrupting integer literals above
//       2^53 — seed is spec'd up to 2^63-1 (same class as replaceModelInBody).
//   C1  sanitizeAttribution / errSnippet / truncateDetail cut on byte
//       boundaries: a split multi-byte rune produced invalid UTF-8 — for
//       attribution values that fails the whole request_logs INSERT on
//       Postgres ("invalid byte sequence"), silently losing the row.
//   D1  The clientCaused relay wrote the 2 KiB errSnippet to the client;
//       larger upstream error bodies arrived truncated mid-JSON and
//       unparseable. attemptOutcome now carries the full bounded body.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"ai-gateway/internal/models"
)

// ---------------------------------------------------------------------------
// A1 — tool_use → tool_calls translation
// ---------------------------------------------------------------------------

func TestAnthChatConversionTranslatesToolUse(t *testing.T) {
	const anth = `{"id":"msg_tu","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[` +
		`{"type":"text","text":"I'll check that."},` +
		`{"type":"tool_use","id":"toolu_01A","name":"get_weather","input":{"city":"Berlin","unit":"c"}}` +
		`],"stop_reason":"tool_use","usage":{"input_tokens":50,"output_tokens":20}}`

	out := anthropicToOpenAIChatResponse([]byte(anth), "m")
	if out == nil {
		t.Fatal("converter returned nil")
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("converted body not JSON: %v", err)
	}
	choices, _ := m["choices"].([]interface{})
	if len(choices) != 1 {
		t.Fatalf("want 1 choice, got %d", len(choices))
	}
	ch, _ := choices[0].(map[string]interface{})

	// finish_reason: tool_use must map to "tool_calls", never "stop" —
	// "stop" makes agents treat the turn as complete and discard the calls.
	if fr, _ := ch["finish_reason"].(string); fr != "tool_calls" {
		t.Fatalf("A1: stop_reason tool_use must map to finish_reason tool_calls, got %q", fr)
	}

	msg, _ := ch["message"].(map[string]interface{})
	tcs, ok := msg["tool_calls"].([]interface{})
	if !ok || len(tcs) != 1 {
		t.Fatalf("A1: tool_use block must become message.tool_calls, got %v", msg["tool_calls"])
	}
	tc, _ := tcs[0].(map[string]interface{})
	if tc["id"] != "toolu_01A" || tc["type"] != "function" {
		t.Fatalf("A1: tool_call id/type wrong: %v", tc)
	}
	fn, _ := tc["function"].(map[string]interface{})
	if fn["name"] != "get_weather" {
		t.Fatalf("A1: tool_call function.name wrong: %v", fn)
	}
	var argsMap map[string]interface{}
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &argsMap); err != nil {
		t.Fatalf("A1: tool_call arguments must be valid JSON, got %q: %v", fn["arguments"], err)
	}
	if argsMap["city"] != "Berlin" || argsMap["unit"] != "c" {
		t.Fatalf("A1: tool_call arguments payload wrong: %v", argsMap)
	}
	if txt, _ := msg["content"].(string); txt != "I'll check that." {
		t.Fatalf("A1: text content lost: %q", txt)
	}
}

func TestAnthChatConversionToolUseNoInput(t *testing.T) {
	const anth = `{"id":"msg_tu2","type":"message","role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_02B","name":"noop"}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":2}}`
	out := anthropicToOpenAIChatResponse([]byte(anth), "m")
	var m map[string]interface{}
	json.Unmarshal(out, &m)
	msg := m["choices"].([]interface{})[0].(map[string]interface{})["message"].(map[string]interface{})
	tcs, ok := msg["tool_calls"].([]interface{})
	if !ok || len(tcs) != 1 {
		t.Fatalf("A1: id-less input tool_use must still convert, got %v", msg["tool_calls"])
	}
	fn := tcs[0].(map[string]interface{})["function"].(map[string]interface{})
	if fn["arguments"] != "{}" {
		t.Fatalf("A1: missing input must default arguments to {}, got %q", fn["arguments"])
	}
}

// ---------------------------------------------------------------------------
// A2 — cache-token billing passthrough through the translations
// ---------------------------------------------------------------------------

func TestAnthChatConversionCarriesCacheTokens(t *testing.T) {
	const anth = `{"id":"msg_cache","type":"message","role":"assistant",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":10,"output_tokens":4,` +
		`"cache_read_input_tokens":90,"cache_creation_input_tokens":40}}`

	out := anthropicToOpenAIChatResponse([]byte(anth), "m")
	if out == nil {
		t.Fatal("converter returned nil")
	}

	// Billing path: extractUsage must fold the cache fields onto prompt,
	// exactly as it does for the raw anthropic body (input + read + write).
	pt, ct := extractUsage(out)
	if pt != 140 || ct != 4 {
		t.Fatalf("A2: extractUsage on converted body must fold cache tokens (want 140/4), got %d/%d", pt, ct)
	}

	// Pricing split: anthropic family fields keep Fresh = input only.
	split := cacheSplitFromBody(out)
	if split.Fresh != 10 || split.CacheRead != 90 || split.CacheWrite != 40 || split.Completion != 4 {
		t.Fatalf("A2: cacheSplitFromBody on converted body wrong: %+v", split)
	}
}

func TestAnthCompletionConversionCarriesCacheTokens(t *testing.T) {
	const anth = `{"id":"msg_c2","type":"message","role":"assistant",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":10,"output_tokens":4,` +
		`"cache_read_input_tokens":90,"cache_creation_input_tokens":40}}`
	out := anthropicToOpenAICompletion([]byte(anth), "m")
	if out == nil {
		t.Fatal("converter returned nil")
	}
	pt, ct := extractUsage(out)
	if pt != 140 || ct != 4 {
		t.Fatalf("A2: completion conversion must fold cache tokens (want 140/4), got %d/%d", pt, ct)
	}
	split := cacheSplitFromBody(out)
	if split.Fresh != 10 || split.CacheRead != 90 || split.CacheWrite != 40 {
		t.Fatalf("A2: completion conversion split wrong: %+v", split)
	}
}

// ---------------------------------------------------------------------------
// A3 — chatToResponsesJSON cache detail passthrough
// ---------------------------------------------------------------------------

func TestChatToResponsesJSONCarriesAnthropicCache(t *testing.T) {
	chat := `{"id":"c1","object":"chat.completion","model":"m","choices":[` +
		`{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":4,` +
		`"cache_read_input_tokens":90,"cache_creation_input_tokens":40}}`
	out := chatToResponsesJSON([]byte(chat), "m")
	if out == nil {
		t.Fatal("converter returned nil")
	}
	pt, ct := extractUsage(out)
	if pt != 140 || ct != 4 {
		t.Fatalf("A3: responses translation must fold cache tokens (want 140/4), got %d/%d", pt, ct)
	}
	split := cacheSplitFromBody(out)
	if split.Fresh != 10 || split.CacheRead != 90 || split.CacheWrite != 40 {
		t.Fatalf("A3: responses translation split wrong: %+v", split)
	}
}

func TestChatToResponsesJSONCarriesOpenAICachedSubset(t *testing.T) {
	// OpenAI semantics: cached_tokens is a SUBSET of prompt_tokens (100) —
	// it must not be folded onto the total, only split out for pricing.
	chat := `{"id":"c2","object":"chat.completion","model":"m","choices":[` +
		`{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":100,"completion_tokens":6,` +
		`"prompt_tokens_details":{"cached_tokens":80}}}`
	out := chatToResponsesJSON([]byte(chat), "m")
	if out == nil {
		t.Fatal("converter returned nil")
	}
	pt, ct := extractUsage(out)
	if pt != 100 || ct != 6 {
		t.Fatalf("A3: OpenAI subset semantics must NOT fold (want 100/6), got %d/%d", pt, ct)
	}
	split := cacheSplitFromBody(out)
	if split.Fresh != 20 || split.CacheRead != 80 || split.Completion != 6 {
		t.Fatalf("A3: OpenAI cache split wrong: %+v", split)
	}
}

// ---------------------------------------------------------------------------
// B1 — integer-literal preservation in stream usage / stream:true injection
// ---------------------------------------------------------------------------

func TestInjectStreamUsagePreservesLargeIntegers(t *testing.T) {
	// seed 9007199254740993 (2^53+1) is the smallest literal a float64
	// round-trip corrupts (→ 9007199254740992).
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"stream":true,"seed":9007199254740993,"metadata":{"trace_id":9223372036854775807}}`)
	out, changed := injectStreamUsage(body)
	if !changed {
		t.Fatal("B1: include_usage must be injected for a streaming body")
	}
	if !strings.Contains(string(out), `"seed":9007199254740993`) {
		t.Fatalf("B1: seed corrupted by injectStreamUsage round-trip: %s", out)
	}
	if !strings.Contains(string(out), `"trace_id":9223372036854775807`) {
		t.Fatalf("B1: metadata integer corrupted: %s", out)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("B1: injected body not JSON: %v", err)
	}
	var so map[string]json.RawMessage
	if err := json.Unmarshal(m["stream_options"], &so); err != nil {
		t.Fatalf("B1: stream_options not an object: %v", err)
	}
	if string(so["include_usage"]) != "true" {
		t.Fatalf("B1: include_usage must be true, got %s", so["include_usage"])
	}
}

func TestInjectStreamUsageExistingOptionsAndNoStream(t *testing.T) {
	// Already-requested usage: unchanged, no rewrite.
	body := []byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true},"seed":9007199254740993}`)
	if out, changed := injectStreamUsage(body); changed || string(out) != string(body) {
		t.Fatalf("B1: include_usage already true must leave the body byte-identical, got %s", out)
	}
	// Pre-existing stream_options object with other keys: preserved.
	body = []byte(`{"stream":true,"stream_options":{"foo":1},"seed":9007199254740993}`)
	out, changed := injectStreamUsage(body)
	if !changed || !strings.Contains(string(out), `"foo":1`) || !strings.Contains(string(out), `"seed":9007199254740993`) {
		t.Fatalf("B1: existing stream_options keys / integers must survive: %s", out)
	}
	// Non-streaming body: no injection.
	body = []byte(`{"model":"m","stream":false,"seed":9007199254740993}`)
	if _, changed := injectStreamUsage(body); changed {
		t.Fatal("B1: non-streaming body must not be rewritten")
	}
}

func TestForceStreamTruePreservesNumbers(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"seed":9007199254740993,"max_tokens":8}`)
	out, changed := forceStreamTrue(body)
	if !changed {
		t.Fatal("B1: stream:true must be injected")
	}
	if !strings.Contains(string(out), `"seed":9007199254740993`) {
		t.Fatalf("B1: seed corrupted by forceStreamTrue round-trip: %s", out)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	if string(m["stream"]) != "true" {
		t.Fatalf("B1: stream must be true, got %s", m["stream"])
	}
	if string(m["max_tokens"]) != "8" {
		t.Fatalf("B1: max_tokens must stay an integer literal, got %s", m["max_tokens"])
	}
	// Already true: no rewrite at all.
	already := []byte(`{"stream":true,"seed":9007199254740993}`)
	if out, changed := forceStreamTrue(already); changed || string(out) != string(already) {
		t.Fatalf("B1: already-streaming body must be untouched, got %s", out)
	}
}

// ---------------------------------------------------------------------------
// C1 — rune-safe truncation
// ---------------------------------------------------------------------------

func TestTruncateUTF8RuneBoundary(t *testing.T) {
	// 199 ASCII bytes + one 3-byte rune (total 202): a 200-byte cap must not
	// split the rune.
	s := strings.Repeat("a", 199) + "€"
	got := truncateUTF8(s, 200)
	if got != strings.Repeat("a", 199) {
		t.Fatalf("C1: cut must back off to the rune boundary, got %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("C1: truncated string must remain valid UTF-8")
	}
	if got := truncateUTF8("short", 200); got != "short" {
		t.Fatalf("C1: strings under the cap must pass through, got %q", got)
	}
	if got := truncateUTF8("€€€", 0); got != "" {
		t.Fatalf("C1: zero cap must yield empty, got %q", got)
	}
	// All-ASCII long string still caps exactly.
	if got := truncateUTF8(strings.Repeat("a", 300), 200); len(got) != 200 {
		t.Fatalf("C1: ASCII cap must be exact, got %d", len(got))
	}
}

func TestSanitizeAttributionRuneSafe(t *testing.T) {
	// 199 ASCII bytes + a 3-byte emoji: the old byte cut at 200 split the
	// emoji, producing invalid UTF-8 that fails the request_logs INSERT on
	// Postgres (invalid byte sequence for encoding "UTF8").
	s := strings.Repeat("u", 199) + "😀"
	got := sanitizeAttribution(s)
	if !utf8.ValidString(got) {
		t.Fatalf("C1: sanitized attribution must be valid UTF-8, got %q", got)
	}
	if len(got) > maxAttributionLen {
		t.Fatalf("C1: sanitized attribution must stay within the byte cap, got %d", len(got))
	}
	// Header-shaped value stays intact end to end.
	a := attributionFromRequest(&http.Request{Header: http.Header{
		"X-End-User": []string{"agent-42"},
	}}, nil)
	if a.EndUser != "agent-42" {
		t.Fatalf("C1: normal attribution value mangled: %+v", a)
	}
}

func TestErrSnippetRuneSafe(t *testing.T) {
	long := strings.Repeat("e", 199) + "日" + strings.Repeat("x", 400)
	got := errSnippet([]byte(long))
	if !utf8.ValidString(got) {
		t.Fatalf("C1: errSnippet must be valid UTF-8, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// D1 — clientCaused relay carries the complete upstream error body
// ---------------------------------------------------------------------------

func TestClientCausedRelayFullBody(t *testing.T) {
	// An upstream 500 carrying code "invalid_request" (deterministic client
	// error) whose JSON verdict exceeds the 2 KiB log-snippet cap: the relay
	// must hand the client the COMPLETE body, parseable end to end.
	longMsg := strings.Repeat("field temperature: invalid type; expected a number but got string; ", 40) // ~4.7KB
	upstreamBody := fmt.Sprintf(`{"error":{"code":"invalid_request","type":"invalid_request_error","message":%q}}`, longMsg)

	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(upstreamBody))
	}
	env := protoNewEnv(t, up, models.ProviderOpenAI, "/v1", "completions")

	status, hdr, raw := env.PostRaw("/v1/completions", `{"model":"gpt-3.5-turbo-instruct","prompt":"Say:"}`, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("D1: expected relayed 500, got %d: %s", status, raw)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("D1: relay must keep JSON content type, got %q", ct)
	}
	if !json.Valid([]byte(raw)) {
		t.Fatalf("D1: relayed body must be complete valid JSON (was truncated at the 2 KiB snippet cap): %d bytes, tail=%q", len(raw), raw[len(raw)-64:])
	}
	if len(raw) < len(upstreamBody) {
		t.Fatalf("D1: relayed body truncated: got %d bytes, upstream sent %d", len(raw), len(upstreamBody))
	}
}
