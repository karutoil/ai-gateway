package proxy

import (
	"bytes"
	"encoding/json"
	"testing"

	"ai-gateway/internal/translate"
)

func translateOpenAIToAnthropicForTest(body []byte) ([]byte, string, error) {
	return translate.OpenAIToAnthropic(body)
}

// Regression: replaceModelInBody previously round-tripped the whole body
// through map[string]interface{}, coercing JSON numbers to float64 and
// silently corrupting int64 literals above 2^53 (seed is spec'd to 2^63-1).
func TestReplaceModelInBodyPreservesBigints(t *testing.T) {
	in := []byte(`{"model":"alias-a","messages":[{"role":"user","content":"hi"}],"seed":9007199254740993,"metadata":{"big":12345678901234567890,"neg":-9007199254740993}}`)
	out := replaceModelInBody(in, "alias-b")
	if !bytes.Contains(out, []byte(`9007199254740993`)) {
		t.Fatalf("seed literal corrupted: %s", out)
	}
	if !bytes.Contains(out, []byte(`12345678901234567890`)) {
		t.Fatalf("metadata int corrupted: %s", out)
	}
	var probe struct {
		Model string `json:"model"`
		Seed  int64  `json:"seed"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Model != "alias-b" || probe.Seed != 9007199254740993 {
		t.Fatalf("model=%q seed=%d", probe.Model, probe.Seed)
	}
}

// The splice must replace the top-level model value, not an earlier byte
// sequence that happens to match, and must handle missing/non-string models.
func TestReplaceModelInBodyEdgeCases(t *testing.T) {
	// model value appearing inside a message string earlier in the body
	in := []byte(`{"messages":[{"role":"user","content":"say \"alias-a\" ok"}],"model":"alias-a"}`)
	out := replaceModelInBody(in, "m2")
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(out, &probe); err != nil || probe.Model != "m2" {
		t.Fatalf("model=%q err=%v out=%s", probe.Model, err, out)
	}
	if !bytes.Contains(out, []byte(`"alias-a\" ok"`)) {
		t.Fatalf("message content mutated: %s", out)
	}
	// non-string model: body returned unchanged
	if got := replaceModelInBody([]byte(`{"model":123}`), "x"); string(got) != `{"model":123}` {
		t.Fatalf("non-string model mutated: %s", got)
	}
	// missing model: unchanged
	if got := replaceModelInBody([]byte(`{"messages":[]}`), "x"); string(got) != `{"messages":[]}` {
		t.Fatalf("missing-model body mutated: %s", got)
	}
	// non-object body: unchanged
	if got := replaceModelInBody([]byte(`[1,2]`), "x"); string(got) != `[1,2]` {
		t.Fatalf("array body mutated: %s", got)
	}
}

// Regression: streaming requests whose upstream answers 4xx must relay the
// upstream status + body verbatim (never laundered into a generic 502).
func TestStreamUpstream4xxRelayedVerbatim(t *testing.T) {
	// Covered end-to-end by anthropic_proto/resilience suites; this pin
	// documents the relay contract enforced in proxyWithMetricsOpts: the
	// streaming 4xx branch writes the upstream status and body untouched.
	if !isStream4xxRelayStatus(400) || !isStream4xxRelayStatus(404) || !isStream4xxRelayStatus(401) {
		t.Fatal("4xx statuses must relay verbatim on streaming requests")
	}
	if isStream4xxRelayStatus(500) || isStream4xxRelayStatus(503) {
		t.Fatal("5xx must NOT take the verbatim 4xx relay path")
	}
	// 429 also matches the relay range but can never reach it: the earlier
	// pre-commit retry branch (status==429 || >=500) consumes it first.
	if !isStream4xxRelayStatus(429) {
		t.Fatal("helper must mirror the original [400,500) relay condition")
	}
}

// Regression: a zero/negative StreamIdle timeout must DISABLE the watchdog,
// not fire immediately. NewTimer(0) delivers once even after Stop(); the
// fixed implementation uses a nil channel.
func TestIdleWatchdogDisabled(t *testing.T) {
	wd := newIdleWatchdog(0)
	if wd.c != nil {
		t.Fatal("disabled watchdog must expose a nil channel")
	}
	wd2 := newIdleWatchdog(-5)
	if wd2.c != nil {
		t.Fatal("negative idle must expose a nil channel")
	}
	wd3 := newIdleWatchdog(1)
	if wd3.c == nil {
		t.Fatal("positive idle must expose a timer channel")
	}
	wd3.stop()
}

// Regression: chat message content arrays (legal per OpenAI spec) must keep
// their text when converting to Responses output.
func TestContentToTextArray(t *testing.T) {
	got := contentToText([]interface{}{
		map[string]interface{}{"type": "text", "text": "hello "},
		map[string]interface{}{"type": "text", "text": "world"},
		map[string]interface{}{"type": "image_url", "image_url": map[string]string{"url": "x"}},
	})
	if got != "hello world" {
		t.Fatalf("got %q", got)
	}
	if contentToText("plain") != "plain" {
		t.Fatal("string passthrough broken")
	}
	if contentToText(nil) != "" {
		t.Fatal("nil content should be empty")
	}
}

// Regression: previous_response_id on the translated path is refused instead
// of silently dropped (context loss).
func TestHasPreviousResponseID(t *testing.T) {
	if !hasPreviousResponseID([]byte(`{"model":"m","input":"x","previous_response_id":"resp_1"}`)) {
		t.Fatal("previous_response_id not detected")
	}
	if hasPreviousResponseID([]byte(`{"model":"m","input":"x"}`)) {
		t.Fatal("absent previous_response_id detected")
	}
	if hasPreviousResponseID([]byte(`{"model":"m","input":"x","previous_response_id":""}`)) {
		t.Fatal("empty previous_response_id detected")
	}
	if hasPreviousResponseID([]byte(`not json`)) {
		t.Fatal("invalid json detected")
	}
}

// max_completion_tokens (newer OpenAI spelling) must map onto the anthropic
// max_tokens instead of being dropped and replaced by the default cap.
func TestMaxCompletionTokensMapped(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-haiku","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":77}`)
	out, _, err := translateOpenAIToAnthropicForTest(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"max_tokens":77`)) {
		t.Fatalf("max_completion_tokens lost: %s", out)
	}
}
