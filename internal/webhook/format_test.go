package webhook

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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

func decodeBody(t *testing.T, got []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("body invalid json: %v (%s)", err, got)
	}
	return m
}

func discordEmbed(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	if u, _ := m["username"].(string); u != "AI Gateway" {
		t.Errorf("discord username = %q, want %q", u, "AI Gateway")
	}
	embeds, _ := m["embeds"].([]any)
	if len(embeds) != 1 {
		t.Fatalf("expected 1 embed, got %d (%v)", len(embeds), m)
	}
	embed, ok := embeds[0].(map[string]any)
	if !ok {
		t.Fatalf("embed wrong shape: %v", embeds[0])
	}
	return embed
}

func TestPlatformBodyDiscord(t *testing.T) {
	raw := marshalEvent("key.rotated", map[string]any{"name": "prod", "prefix": "ab12cd34", "actor": "admin"})
	m := decodeBody(t, platformBody("discord", "key.rotated", raw))
	if content, ok := m["content"].(string); ok && strings.Contains(content, "{") {
		t.Errorf("discord content should be human text, not JSON: %q", content)
	}
	embed := discordEmbed(t, m)
	title, _ := embed["title"].(string)
	if !strings.Contains(title, "API Key Rotated") {
		t.Errorf("discord title missing human name: %q", title)
	}
	desc, _ := embed["description"].(string)
	if !strings.Contains(desc, "prod") || !strings.Contains(desc, "admin") {
		t.Errorf("discord description should name key + actor: %q", desc)
	}
	if strings.Contains(desc, `"prefix"`) || strings.Contains(desc, `{"name"`) {
		t.Errorf("discord description must not dump raw JSON: %q", desc)
	}
	if color, _ := embed["color"].(float64); int(color) != eventBlue {
		t.Errorf("rotated color = %v, want %d", color, eventBlue)
	}
	footer, _ := (embed["footer"].(map[string]any))["text"].(string)
	if !strings.Contains(footer, "key.rotated") {
		t.Errorf("footer should carry event name: %q", footer)
	}
	if ts, _ := embed["timestamp"].(string); ts != "" {
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			t.Errorf("timestamp not RFC3339: %q", ts)
		}
	} else {
		t.Error("embed missing timestamp")
	}
	fields, _ := embed["fields"].([]any)
	if len(fields) == 0 {
		t.Fatal("expected named fields, got none")
	}
	joined, _ := json.Marshal(fields)
	if !strings.Contains(string(joined), "prod") || !strings.Contains(string(joined), "ab12cd34") {
		t.Errorf("fields should include key + prefix: %s", joined)
	}
}

func TestPlatformBodyDiscordQuota(t *testing.T) {
	raw := marshalEvent("billing.over_quota", map[string]any{
		"prefix": "ab12", "org_id": "org1", "limit_kind": "daily_tokens", "limit": 1000, "used": 1250,
	})
	m := decodeBody(t, platformBody("discord", "billing.over_quota", raw))
	embed := discordEmbed(t, m)
	if title, _ := embed["title"].(string); !strings.Contains(title, "Quota Exceeded") {
		t.Errorf("quota title wrong: %q", title)
	}
	if color, _ := embed["color"].(float64); int(color) != eventOrange {
		t.Errorf("quota color = %v, want %d", color, eventOrange)
	}
	desc, _ := embed["description"].(string)
	if !strings.Contains(desc, "429") {
		t.Errorf("quota description should mention 429 rejection: %q", desc)
	}
}

func TestPlatformBodyDiscordAudit(t *testing.T) {
	raw := marshalEvent("audit.create", map[string]any{
		"actor": "admin", "action": "create", "target_type": "organization",
		"target_id": "org1", "meta": "Acme", "created_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
	m := decodeBody(t, platformBody("discord", "audit.create", raw))
	embed := discordEmbed(t, m)
	if title, _ := embed["title"].(string); !strings.Contains(title, "Organization Created") {
		t.Errorf("audit title should humanize target+verb: %q", title)
	}
	if desc, _ := embed["description"].(string); !strings.Contains(desc, "admin") {
		t.Errorf("audit description should name actor: %q", desc)
	}
}

func TestPlatformBodyDiscordTestPing(t *testing.T) {
	raw := marshalEvent("test.ping", map[string]any{"webhook_id": "w1"})
	m := decodeBody(t, platformBody("discord", "test.ping", raw))
	embed := discordEmbed(t, m)
	if title, _ := embed["title"].(string); !strings.Contains(title, "Webhook Connected") {
		t.Errorf("test ping title wrong: %q", title)
	}
}

func TestPlatformBodyDiscordTruncates(t *testing.T) {
	big := strings.Repeat("x", 20000)
	raw := marshalEvent("billing.export", map[string]any{"actor": big, "range": "7d", "path": "/api/x"})
	got := platformBody("discord", "billing.export", raw)
	if len(got) > 6000 {
		t.Errorf("discord body must stay within 6000 chars, got %d", len(got))
	}
	if !json.Valid(got) {
		t.Fatal("truncated discord body invalid json")
	}
}

func TestPlatformBodySlack(t *testing.T) {
	raw := marshalEvent("key.created", map[string]any{"name": "ci-key"})
	m := decodeBody(t, platformBody("slack", "key.created", raw))
	text, ok := m["text"].(string)
	if !ok || !strings.Contains(text, "API Key Created") || !strings.Contains(text, "ci-key") {
		t.Fatalf("slack text should be human fallback with key name: %v", m)
	}
	if strings.Contains(text, `"name"`) {
		t.Errorf("slack text must not dump raw JSON: %q", text)
	}
	// Slack payloads must not leak the raw gateway envelope.
	if _, has := m["payload"]; has {
		t.Error("slack body should not include raw envelope fields")
	}
	blocks, _ := m["blocks"].([]any)
	if len(blocks) < 3 {
		t.Fatalf("expected header+section+context blocks, got %d", len(blocks))
	}
	header, _ := blocks[0].(map[string]any)
	if header["type"] != "header" {
		t.Errorf("first block should be a header: %v", header)
	}
	joined, _ := json.Marshal(blocks)
	if !strings.Contains(string(joined), "key.created") {
		t.Errorf("blocks should reference the event name: %s", joined)
	}
	if !strings.Contains(string(joined), "AI Gateway") {
		t.Errorf("blocks should brand the context: %s", joined)
	}
}

func TestPlatformBodyCaseInsensitive(t *testing.T) {
	raw := marshalEvent("test.ping", nil)
	if string(platformBody("DISCORD", "test.ping", raw)) == string(raw) {
		t.Error("DISCORD format should render an embed, not pass the envelope through")
	}
	if string(platformBody("Slack", "key.created", raw)) == string(raw) {
		t.Error("Slack format should render blocks, not pass the envelope through")
	}
}
