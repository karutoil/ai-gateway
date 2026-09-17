package webhook

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDetectFormat(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://discord.com/api/webhooks/123/abc", "discord"},
		{"https://discordapp.com/api/webhooks/123/abc", "discord"},
		{"https://ptb.discord.com/api/webhooks/123/x", "discord"},
		{"https://canary.discord.com/api/webhooks/123/x", "discord"},
		{"https://hooks.slack.com/services/T00/B00/xyz", "slack"},
		{"https://example.com/hook", "json"},
		{"", "json"},
	}
	for _, c := range cases {
		if got := detectFormat(c.url); got != c.want {
			t.Errorf("detectFormat(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestDiscordBodyShape(t *testing.T) {
	raw := marshalEvent("key.rotated", map[string]any{
		"name":   "prod-key",
		"prefix": "ab12cd34",
		"actor":  "admin",
	})
	body := platformBody("discord", "key.rotated", raw)
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("invalid discord body: %v\n%s", err, body)
	}
	if u, _ := m["username"].(string); u == "" {
		t.Error("discord body should set a username")
	}
	embeds, _ := m["embeds"].([]any)
	if len(embeds) != 1 {
		t.Fatalf("expected 1 embed, got %d", len(embeds))
	}
	embed := embeds[0].(map[string]any)
	title, _ := embed["title"].(string)
	desc, _ := embed["description"].(string)
	fieldsJSON, _ := json.Marshal(embed["fields"])
	combined := title + "\n" + desc + "\n" + string(fieldsJSON)
	if !strings.Contains(combined, "prod-key") || !strings.Contains(combined, "admin") {
		t.Errorf("embed should name the key and actor: %s", combined)
	}
	if strings.Contains(desc, `"prefix"`) {
		t.Errorf("embed description must be human text, not raw JSON: %q", desc)
	}
}

func TestSlackBodyShape(t *testing.T) {
	raw := marshalEvent("key.created", map[string]any{"name": "ci"})
	body := platformBody("slack", "key.created", raw)
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	text, _ := m["text"].(string)
	if !strings.Contains(text, "API Key Created") || !strings.Contains(text, "ci") {
		t.Fatalf("slack text should be a human summary: %q", text)
	}
}
