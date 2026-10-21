package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// Generated media must not be written into request_logs verbatim.
//
// Before this guard, LOG_BODIES=true stored the first BodyLogMaxBytes (8 kB) of
// every response body, so each image request wrote ~8 kB of opaque base64 into
// the OLTP database forever — inflating the table, widening every export, and
// parking a chunk of a customer's generated asset in the logs. The summary must
// keep the diagnostic facts (count, encoding, status, revised prompt) and drop
// the payload.

func TestMediaBodySummaryDropsBase64Payload(t *testing.T) {
	payload := strings.Repeat("A", 40_000)
	body := []byte(`{"created":1730000000,"data":[{"b64_json":"` + payload + `","revised_prompt":"a red panda astronaut"}]}`)

	got := mediaBodySummary(body, 8192)

	if strings.Contains(got, "AAAA") {
		t.Fatalf("summary still contains base64 payload: %q", got)
	}
	if len(got) > 8192 {
		t.Fatalf("summary exceeds the body-log cap: %d bytes", len(got))
	}
	for _, want := range []string{`"images":1`, `"image_encoding":"b64_json"`, "red panda astronaut"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %s: %s", want, got)
		}
	}
	if !strings.Contains(got, `"bytes":400`) {
		t.Errorf("summary should record the body size: %s", got)
	}
}

func TestMediaBodySummaryKeepsURLEncoding(t *testing.T) {
	body := []byte(`{"data":[{"url":"https://cdn.example/x.png"},{"url":"https://cdn.example/y.png"}]}`)
	got := mediaBodySummary(body, 8192)
	for _, want := range []string{`"images":2`, `"image_encoding":"url"`} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %s: %s", want, got)
		}
	}
}

func TestMediaBodySummaryVideoJob(t *testing.T) {
	body := []byte(`{"id":"video_abc","object":"video","status":"in_progress","progress":42,"seconds":"8","size":"1280x720"}`)
	got := mediaBodySummary(body, 8192)
	for _, want := range []string{`"id":"video_abc"`, `"object":"video"`, `"status":"in_progress"`, `"progress":42`} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %s: %s", want, got)
		}
	}
}

// A provider error body is the most useful thing to log, so it must survive.
func TestMediaBodySummaryKeepsError(t *testing.T) {
	body := []byte(`{"error":{"code":"moderation_rejected","message":"prompt rejected by the safety system"}}`)
	got := mediaBodySummary(body, 8192)
	if !strings.Contains(got, "moderation_rejected") || !strings.Contains(got, "safety system") {
		t.Fatalf("error detail lost: %s", got)
	}
}

// Non-media endpoints must keep the existing truncation behaviour.
func TestIsGeneratedMediaEndpoint(t *testing.T) {
	media := []string{"images.generations", "images.edits", "videos", "videos.poll", "videos.list", "videos.content"}
	for _, e := range media {
		if !isGeneratedMediaEndpoint(e) {
			t.Errorf("%q should be treated as generated media", e)
		}
	}
	for _, e := range []string{"chat.completions", "messages", "embeddings", "systemone", "responses", "completions"} {
		if isGeneratedMediaEndpoint(e) {
			t.Errorf("%q must NOT be treated as generated media", e)
		}
	}
}

// The summary is still secret-scrubbed: a provider echoing a key in an error
// message must not persist it.
func TestMediaBodySummaryScrubsSecrets(t *testing.T) {
	body := []byte(`{"error":{"message":"bad key sk-abcdef0123456789"}}`)
	got := mediaBodySummary(body, 8192)
	if strings.Contains(got, "sk-abcdef0123456789") {
		t.Fatalf("secret leaked into summary: %s", got)
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(got), &probe); err != nil {
		t.Fatalf("summary is not valid JSON: %v (%s)", err, got)
	}
}
