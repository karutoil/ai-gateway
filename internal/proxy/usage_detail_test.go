package proxy

// Usage-logging detail tests: extractUsageDetail parsing, stream text
// assembly into chat-shaped log bodies, finish-reason normalization, and the
// LOG_BODIES=0 privacy contract. Uses the shared proto harness.
//
// Conventions follow chat_proto_test.go: real chi-wired handlers, :memory:
// sqlite, mock upstreams.

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/models"
)

func TestExtractUsageDetailOpenAIDetail(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":6},"completion_tokens_details":{"reasoning_tokens":3}}}`)
	pt, ct, d := extractUsageDetail(body)
	if pt != 10 || ct != 5 {
		t.Fatalf("tokens wrong: prompt=%d completion=%d", pt, ct)
	}
	if d.CacheRead != 6 || d.Reasoning != 3 {
		t.Fatalf("detail wrong: %+v", d)
	}
	if d.FinishReason != "stop" {
		t.Fatalf("finish reason wrong: %q", d.FinishReason)
	}
}

func TestExtractUsageDetailAnthropicCacheNoBillingFold(t *testing.T) {
	// Cache tokens must be reported SEPARATELY here — extractUsage (billing)
	// folds them into prompt, extractUsageDetail must not.
	body := []byte(`{"role":"assistant","content":[{"type":"text","text":"x"}],"stop_reason":"max_tokens","usage":{"input_tokens":10,"cache_read_input_tokens":7,"cache_creation_input_tokens":4,"output_tokens":5}}`)
	pt, ct, d := extractUsageDetail(body)
	if pt != 10 {
		t.Fatalf("detail prompt must NOT fold cache tokens, got %d", pt)
	}
	if ct != 5 || d.CacheRead != 7 || d.CacheWrite != 4 {
		t.Fatalf("detail wrong: pt=%d ct=%d %+v", pt, ct, d)
	}
	if d.FinishReason != "length" {
		t.Fatalf("max_tokens should normalize to length, got %q", d.FinishReason)
	}
	// Cross-check: the billing extractor still folds (existing behavior).
	bpt, _ := extractUsage(body)
	if bpt != 10+7+4 {
		t.Fatalf("billing fold broken: %d", bpt)
	}
}

func TestNormalizeFinishReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"tool_use":      "tool_calls",
		"max_tokens":    "length",
		"refusal":       "content_filter",
		"completed":     "stop",
		"in_progress":   "",
		"queued":        "",
		"":              "",
		"weird_vendor":  "weird_vendor",
	}
	for in, want := range cases {
		if got := normalizeFinishReason(in); got != want {
			t.Errorf("normalizeFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// Chat SSE with content deltas + reasoning + final usage chunk: the logged
// response_body must be the assembled assistant message, not the raw SSE wall.
func TestChatStreamAssemblesAssistantMessageIntoLog(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n")
		fl.Flush()
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo world\"}}]}\n\n")
		fl.Flush()
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":5,\"completion_tokens_details\":{\"reasoning_tokens\":2},\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\n")
		fl.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}
	env := protoNewEnv(t, up, models.ProviderOpenAICompatible, "/v1", "chat")
	env.H.LogBodies = true
	env.H.BodyLogMaxBytes = 8192

	status, _, _ := env.PostRaw("/v1/chat/completions", `{"model":"whatever","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}

	pt, ct, total := protoPollUsage(t, env.DB, 3*time.Second)
	if pt != 7 || ct != 5 || total != 12 {
		t.Fatalf("usage wrong: %d/%d/%d", pt, ct, total)
	}
	var respBody, finish sql.NullString
	var cacheRead, reasoning sql.NullInt64
	if err := env.DB.QueryRow(`SELECT response_body, finish_reason, cache_read_tokens, reasoning_tokens FROM request_logs ORDER BY rowid DESC LIMIT 1`).Scan(&respBody, &finish, &cacheRead, &reasoning); err != nil {
		t.Fatal(err)
	}
	if !respBody.Valid || !strings.Contains(respBody.String, "Hello world") {
		t.Fatalf("response_body not assembled: %v", respBody)
	}
	if strings.Contains(respBody.String, "data:") {
		t.Fatalf("response_body still raw SSE: %.120s", respBody.String)
	}
	if finish.String != "stop" {
		t.Fatalf("finish_reason = %q", finish.String)
	}
	if cacheRead.Int64 != 3 || reasoning.Int64 != 2 {
		t.Fatalf("token detail wrong: cache_read=%v reasoning=%v", cacheRead, reasoning)
	}
}

// Anthropic SSE: text deltas accumulate; stop_reason max_tokens normalizes to
// "length" on the log row.
func TestAnthropicStreamAssemblesTextAndStopReason(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9,\"cache_read_input_tokens\":4}}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial ans\"}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"wer\"}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":6}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		fl.Flush()
	}
	// Native anthropic upstream (no /v1 suffix, messages dialect both sides).
	env := protoNewEnv(t, up, models.ProviderAnthropic, "", "messages")
	env.H.LogBodies = true
	env.H.BodyLogMaxBytes = 8192

	status, _, _ := env.PostRaw("/v1/messages", `{"model":"whatever","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	protoPollUsage(t, env.DB, 3*time.Second)
	var respBody, finish sql.NullString
	var cacheRead sql.NullInt64
	if err := env.DB.QueryRow(`SELECT response_body, finish_reason, cache_read_tokens FROM request_logs ORDER BY rowid DESC LIMIT 1`).Scan(&respBody, &finish, &cacheRead); err != nil {
		t.Fatal(err)
	}
	if !respBody.Valid || !strings.Contains(respBody.String, "partial answer") {
		t.Fatalf("assembled text missing: %v", respBody)
	}
	if finish.String != "length" {
		t.Fatalf("stop_reason max_tokens should log as length, got %q", finish.String)
	}
	if cacheRead.Int64 != 4 {
		t.Fatalf("cache_read_tokens = %v", cacheRead)
	}
}

// Privacy contract: LOG_BODIES=0 must store NO bodies even when capture ran.
func TestLogBodiesOffStoresNoBodies(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"secret reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}
	env := protoNewEnv(t, up, models.ProviderOpenAICompatible, "/v1", "chat")
	env.H.LogBodies = false

	status, _, _ := env.PostRaw("/v1/chat/completions", `{"model":"whatever","messages":[{"role":"user","content":"private prompt"}]}`, nil)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	protoPollUsage(t, env.DB, 3*time.Second)
	var reqBody, respBody sql.NullString
	var finish sql.NullString
	if err := env.DB.QueryRow(`SELECT request_body, response_body, finish_reason FROM request_logs ORDER BY rowid DESC LIMIT 1`).Scan(&reqBody, &respBody, &finish); err != nil {
		t.Fatal(err)
	}
	if reqBody.Valid || respBody.Valid {
		t.Fatalf("LOG_BODIES=0 must not store bodies: req=%v resp=%v", reqBody, respBody)
	}
	if finish.String != "stop" {
		t.Fatalf("finish_reason should still be recorded, got %q", finish.String)
	}
}

// Responses-API dialect (native pass-through): text deltas + terminal
// response.completed with nested usage must assemble and record detail.
func TestResponsesDialectStreamCapture(t *testing.T) {
	sc := newStreamCapture(8192)
	for _, frame := range []string{
		`{"type":"response.output_text_delta","delta":{"text":"assembled res"}}`,
		`{"type":"response.output_text_delta","delta":{"text":"ponse"}}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":5}}}}`,
	} {
		var m map[string]interface{}
		_ = json.Unmarshal([]byte(frame), &m)
		sc.observe(sseEvent{data: []byte(frame)}, m, false)
	}
	out := sc.body()
	if out == nil || !strings.Contains(string(out), "assembled response") {
		t.Fatalf("responses dialect assembly failed: %s", out)
	}
	if sc.detail.CacheRead != 5 || sc.detail.FinishReason != "stop" {
		t.Fatalf("responses detail wrong: %+v", sc.detail)
	}
}

// Captured bodies are credential-scrubbed before persistence.
func TestStreamCaptureScrubbed(t *testing.T) {
	sc := newStreamCapture(8192)
	sc.observe(sseEvent{data: []byte(`{"choices":[{"delta":{"content":"key is sk-abc123def456ghi789 and Bearer xyzsecrettoken"}}]}`)}, mustMap(t, `{"choices":[{"delta":{"content":"key is sk-abc123def456ghi789 and Bearer xyzsecrettoken"}}]}`), false)
	out := sc.body()
	if out == nil {
		t.Fatal("no assembled body")
	}
	logged := ScrubSecrets(string(out))
	if strings.Contains(logged, "sk-abc123def456ghi789") || strings.Contains(logged, "xyzsecrettoken") {
		t.Fatalf("secrets survived scrubbing: %s", logged)
	}
}

func mustMap(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Non-stream path: finish_reason + detail land on the log row from the
// response JSON.
func TestNonStreamDetailRecorded(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
	}
	env := protoNewEnv(t, up, models.ProviderOpenAICompatible, "/v1", "chat")
	env.H.LogBodies = true

	status, _, raw := env.PostRaw("/v1/chat/completions", `{"model":"whatever","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status = %d body=%s", status, raw)
	}
	protoPollUsage(t, env.DB, 3*time.Second)
	var finish sql.NullString
	if err := env.DB.QueryRow(`SELECT finish_reason FROM request_logs ORDER BY rowid DESC LIMIT 1`).Scan(&finish); err != nil {
		t.Fatal(err)
	}
	if finish.String != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", finish.String)
	}
}
