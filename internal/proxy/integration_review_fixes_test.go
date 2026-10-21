package proxy

// Regression tests for the integration review fixes:
//
//   F1 — client-caused upstream error relay is consistent across endpoints:
//        /v1/chat/completions, /v1/messages and /v1/embeddings (all routed
//        through proxyCandidatesWithOpts in fallback.go) must relay the FULL
//        upstream error body for client-caused 5xx, exactly like the
//        Completions/Responses handlers — not a 512-char snippet inside the
//        generic proxy_error envelope.
//
//   F3 — legacy /v1/completions finish_reason: "tool_calls" is not a valid
//        finish_reason in the legacy text-completion dialect (and that path
//        emits no tool_calls field), so a tool_use stop must render as
//        "stop" there while chat keeps "tool_calls".

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"ai-gateway/internal/models"
)

// ---------------------------------------------------------------------------
// F1 — full-body client-caused relay on the fallback.go paths
// ---------------------------------------------------------------------------

func TestClientCausedRelayFullBodyOnFallbackPaths(t *testing.T) {
	// An upstream 500 carrying invalid_request semantics (deterministic
	// client error) whose JSON verdict exceeds the 2 KiB snippet cap: the
	// relay must hand the client the COMPLETE body, parseable end to end.
	longMsg := strings.Repeat("field temperature: invalid type; expected a number but got string; ", 40) // ~4.7KB
	upstreamBody := fmt.Sprintf(`{"error":{"code":"invalid_request","type":"invalid_request_error","message":%q}}`, longMsg)

	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(upstreamBody))
	}

	// chat + embeddings share one OpenAI-dialect env; messages uses a native
	// anthropic upstream env.
	openAIEnv := protoNewEnv(t, up, models.ProviderOpenAI, "/v1", "chat", "embeddings")
	for _, tc := range []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`},
		{"chat-stream", "/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`},
		{"embeddings", "/v1/embeddings", `{"model":"text-embedding-3-small","input":"hello"}`},
	} {
		status, hdr, raw := openAIEnv.PostRaw(tc.path, tc.body, nil)
		if status != http.StatusInternalServerError {
			t.Fatalf("F1 %s: expected relayed 500, got %d: %s", tc.name, status, raw)
		}
		if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("F1 %s: relay must keep JSON content type, got %q", tc.name, ct)
		}
		if raw == upstreamBody {
			continue // exact relay
		}
		t.Fatalf("F1 %s: relayed body must be the complete upstream verdict verbatim (got %d bytes, want %d; head=%q)",
			tc.name, len(raw), len(upstreamBody), head(raw, 80))
	}

	anthUp := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, longMsg)))
	}
	anthEnv := protoNewEnv(t, anthUp, models.ProviderAnthropic, "", "messages")
	status, _, raw := anthEnv.PostRaw("/v1/messages", `{"model":"claude-3-5-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("F1 messages: expected relayed 500, got %d: %s", status, raw)
	}
	if !strings.Contains(raw, "expected a number but got string") || len(raw) < len(longMsg) {
		t.Fatalf("F1 messages: relayed body truncated: %d bytes, tail=%q", len(raw), tail(raw, 64))
	}
}

// ---------------------------------------------------------------------------
// F3 — legacy /v1/completions finish_reason
// ---------------------------------------------------------------------------

func TestLegacyCompletionsFinishReasonIsValidForTheDialect(t *testing.T) {
	// A native anthropic upstream answering a tool_use stop: /v1/completions
	// clients must receive a finish_reason from the legacy vocabulary
	// (stop/length), never "tool_calls" — that value does not exist in the
	// text-completion dialect and no tool_calls field is emitted there.
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"type":"message",
			"id":"msg_tooluse_1",
			"content":[
				{"type":"text","text":"I'll check that."},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}
			],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":10,"output_tokens":20}
		}`))
	}
	env := protoNewEnv(t, up, models.ProviderOpenAI, "", "completions") // claude model: heuristic anthropic translation path (l2 idiom)

	status, _, raw := env.PostRaw("/v1/completions", `{"model":"claude-3-5-haiku-20241022","prompt":"weather in SF?"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("F3: expected 200, got %d: %s", status, raw)
	}
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Text         string `json:"text"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("F3: response not parseable: %v (%s)", err, raw)
	}
	if resp.Object != "text_completion" {
		t.Fatalf("F3: object = %q, want text_completion", resp.Object)
	}
	if len(resp.Choices) == 0 {
		t.Fatalf("F3: no choices in %s", raw)
	}
	if got := resp.Choices[0].FinishReason; got != "stop" {
		t.Fatalf("F3: legacy finish_reason = %q, want \"stop\" (\"tool_calls\" is invalid for the /v1/completions dialect)", got)
	}
	if !strings.Contains(resp.Choices[0].Text, "check that") {
		t.Fatalf("F3: text content lost: %q", resp.Choices[0].Text)
	}
}

func TestLegacyCompletionsFinishReasonLengthPreserved(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"type":"message",
			"id":"msg_max_1",
			"content":[{"type":"text","text":"cut off"}],
			"stop_reason":"max_tokens",
			"usage":{"input_tokens":10,"output_tokens":20}
		}`))
	}
	env := protoNewEnv(t, up, models.ProviderOpenAI, "", "completions") // claude model: heuristic anthropic translation path (l2 idiom)

	_, _, raw := env.PostRaw("/v1/completions", `{"model":"claude-3-5-haiku-20241022","prompt":"hi"}`, nil)
	var resp struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil || len(resp.Choices) == 0 {
		t.Fatalf("F3: unparseable response: %v (%s)", err, raw)
	}
	if got := resp.Choices[0].FinishReason; got != "length" {
		t.Fatalf("F3: max_tokens must still map to \"length\", got %q", got)
	}
}

// The chat converter must KEEP "tool_calls": a tool-invoking turn reported
// as "stop" makes agents treat the turn as complete and discard the tool
// calls. (Unit level: the anthropicToOpenAIChatResponse converter is the
// chat-path counterpart of anthropicToOpenAICompletion above.)
func TestChatConverterKeepsToolCallsFinishReason(t *testing.T) {
	body := []byte(`{
		"type":"message",
		"id":"msg_tooluse_2",
		"content":[
			{"type":"text","text":"I'll check that."},
			{"type":"tool_use","id":"toolu_2","name":"get_weather","input":{"city":"SF"}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":10,"output_tokens":20}
	}`)
	converted := anthropicToOpenAIChatResponse(body, "claude-3-5-haiku-20241022")
	if converted == nil {
		t.Fatal("F3: chat conversion produced no output")
	}
	var resp struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				ToolCalls []any `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(converted, &resp); err != nil || len(resp.Choices) == 0 {
		t.Fatalf("F3: unparseable conversion: %v (%s)", err, converted)
	}
	if got := resp.Choices[0].FinishReason; got != "tool_calls" {
		t.Fatalf("F3: chat finish_reason = %q, want \"tool_calls\"", got)
	}
	if len(resp.Choices[0].Message.ToolCalls) == 0 {
		t.Fatalf("F3: chat tool_calls lost: %s", converted)
	}
}

// The streaming reframe (anthToOpenAICompletionStream) is the same legacy
// dialect: a tool_use stop must arrive as "stop", never "tool_calls".
func TestLegacyCompletionsStreamFinishReasonIsValidForTheDialect(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		protoWriteAnthStream(w, []string{
			`event: message_start
data: {"type":"message_start","message":{"id":"msg_ls_f3","type":"message","role":"assistant","usage":{"input_tokens":4,"output_tokens":1}}}`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"chunk"}}`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
			`event: message_stop
data: {"type":"message_stop"}`,
		})
	}
	env := protoNewEnv(t, up, models.ProviderOpenAI, "", "completions") // claude model: anthropic translation path

	streamBody := fmt.Sprintf(`{"model":"%s","prompt":"Go","stream":true}`, protoLegacyModelAnth)
	status, _, raw := env.PostRaw("/v1/completions", streamBody, nil)
	if status != http.StatusOK {
		t.Fatalf("F3-stream: expected 200, got %d: %s", status, raw)
	}
	if !strings.Contains(raw, "data: [DONE]") {
		t.Fatalf("F3-stream: stream must end with [DONE]: %s", raw)
	}
	if strings.Contains(raw, `"tool_calls"`) {
		t.Fatalf("F3-stream: legacy stream emitted finish_reason \"tool_calls\" (invalid for /v1/completions): %s", raw)
	}
	var lastFinish string
	for _, ev := range protoEvents(raw) {
		if !strings.Contains(ev.Data, `"finish_reason"`) {
			continue
		}
		var chunk struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(ev.Data), &chunk) == nil && len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil {
			lastFinish = *chunk.Choices[0].FinishReason
		}
	}
	if lastFinish != "stop" {
		t.Fatalf("F3-stream: terminal finish_reason = %q, want \"stop\"", lastFinish)
	}
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
