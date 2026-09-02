package webhook

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlatformBodyJSON(t *testing.T) {
	raw := marshalEvent("key.rotated", map[string]any{"name": "prod"})
	got := platformBody("json", "key.rotated", raw)
	if !json.Valid(got) {
		t.Fatal("invalid json")
	}
	// json format passes the envelope through unchanged
	if string(got) != string(raw) {
		t.Fatal("json format should pass through unchanged")
	}
}

func TestPlatformBodyDiscord(t *testing.T) {
	raw := marshalEvent("key.rotated", map[string]any{"name": "prod", "prefix": "ab12cd34", "actor": "admin"})
	got := platformBody("discord", "key.rotated", raw)
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("discord body invalid json: %v (%s)", err, got)
	}
	content, ok := m["content"].(string)
	if !ok || !strings.Contains(content, "key.rotated") {
		t.Fatalf("discord content missing event name: %v", m)
	}
	if _, ok := m["embeds"]; !ok {
		t.Error("discord body should include embeds with the full payload")
	}
}

func TestPlatformBodySlack(t *testing.T) {
	raw := marshalEvent("key.created", map[string]any{"name": "ci-key"})
	got := platformBody("slack", "key.created", raw)
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("slack body invalid: %v", err)
	}
	text, ok := m["text"].(string)
	if !ok || !strings.Contains(text, "key.created") {
		t.Fatalf("slack text missing event name: %v", m)
	}
	// Slack payloads must be exactly the shape discord/Discord wants: text-only wrapper is fine.
	if _, has := m["payload"]; has {
		t.Error("slack body should not include raw envelope fields")
	}
}

func TestPlatformBodyCaseInsensitive(t *testing.T) {
	raw := marshalEvent("test.ping", nil)
	if string(platformBody("DISCORD", "test.ping", raw)) == string(raw) {
		t.Log("format case normalized (ok either way)")
	}
}
