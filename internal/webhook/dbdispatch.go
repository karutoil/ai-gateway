// Multi-target webhook dispatch backed by the manageable `webhooks` table.
// Complements the env-configured dispatcher: on Emit, every enabled webhook
// whose event filter matches receives the payload. Delivery failures update
// last_status/last_delivery for the admin UI.
package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai-gateway/internal/db"
)

type DBDispatch struct {
	db      *sql.DB
	client  *http.Client
	mu      sync.Mutex
	started bool
}

func NewDBDispatch(database *sql.DB) *DBDispatch {
	return &DBDispatch{db: database, client: &http.Client{Timeout: 10 * time.Second}}
}

type webhookRow struct {
	id      string
	url     string
	secret  string
	events  string
	format  string
	enabled bool
}

// matchingWebhooks returns enabled webhooks subscribed to event (or to all).
func (d *DBDispatch) matching(event string) []webhookRow {
	rows, err := d.db.Query(db.Q(`SELECT id, url, COALESCE(secret,''), COALESCE(events,''), COALESCE(format,'json'), enabled FROM webhooks WHERE enabled = 1`))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []webhookRow
	for rows.Next() {
		var w webhookRow
		var enabled int
		if err := rows.Scan(&w.id, &w.url, &w.secret, &w.events, &w.format, &enabled); err != nil {
			continue
		}
		w.enabled = enabled == 1
		// Auto-detect platform formats from the URL unless an explicit
		// non-default format was chosen.
		if w.format == "" || w.format == "json" {
			if d := detectFormat(w.url); d != "json" {
				w.format = d
			}
		}
		if w.enabled && webhookWants(w.events, event) {
			out = append(out, w)
		}
	}
	return out
}

// detectFormat infers the payload format from the webhook URL so users
// never have to set it manually: Discord and Slack endpoints reject raw
// JSON envelopes with 400, so pointing at one implies its format.
func detectFormat(url string) string {
	u := strings.ToLower(url)
	if strings.Contains(u, "discord.com/api/webhooks") || strings.Contains(u, "discordapp.com/api/webhooks") {
		return "discord"
	}
	if strings.Contains(u, "hooks.slack.com") {
		return "slack"
	}
	return "json"
}

func webhookWants(eventsCSV, event string) bool {
	eventsCSV = strings.TrimSpace(eventsCSV)
	if eventsCSV == "" {
		return true // all events
	}
	for _, e := range strings.Split(eventsCSV, ",") {
		if strings.TrimSpace(e) == event {
			return true
		}
	}
	return false
}

// Emit delivers the event to every matching webhook asynchronously; the
// caller's hot path never blocks on webhook delivery.
func (d *DBDispatch) Emit(event string, payload any) {
	targets := d.matching(event)
	if len(targets) == 0 {
		return
	}
	body := marshalEvent(event, payload)
	for _, t := range targets {
		go d.deliver(t, event, body)
	}
}

// platformBody wraps a gateway event for platforms that reject arbitrary
// JSON. Discord requires {"content": ...} (optionally embeds); Slack
// requires {"text": ...}. "json" (default) sends the raw gateway envelope.
func platformBody(format, event string, body []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "discord":
		return discordBody(event, body)
	case "slack":
		return slackBody(event, body)
	default:
		return body
	}
}

// discordBody builds {"content": "...", embeds:[...]} — the text is a
// compact summary so it reads well in a channel; details are in the embed.
func discordBody(event string, raw []byte) []byte {
	var env struct {
		Payload json.RawMessage `json:"payload"`
		TS      string          `json:"ts"`
	}
	_ = json.Unmarshal(raw, &env)
	summary := summarizeEvent(event, env.Payload)
	content := "**" + event + "**"
	if summary != "" {
		content += "\n" + summary
	}
	// Discord caps content at 2000 chars.
	if len(content) > 1900 {
		content = content[:1900] + "…"
	}
	out, err := json.Marshal(map[string]any{
		"content": content,
		"embeds": []map[string]any{{
			"title":       event,
			"description": string(env.Payload),
			"timestamp":   env.TS,
		}},
	})
	if err != nil {
		return raw
	}
	// Discord rejects embeds over 6000 chars total; fall back to text-only.
	if len(out) > 5900 {
		return []byte(`{"content":` + mustJSONString(content[:1800]+"…") + `}`)
	}
	return out
}

// slackBody builds {"text": "..."} in Slack's incoming-webhook shape.
func slackBody(event string, raw []byte) []byte {
	var env struct {
		Payload json.RawMessage `json:"payload"`
		TS      string          `json:"ts"`
	}
	_ = json.Unmarshal(raw, &env)
	text := "*" + event + "*"
	if s := summarizeEvent(event, env.Payload); s != "" {
		text += "\n" + s
	}
	detail := string(env.Payload)
	if len(detail) > 1500 {
		detail = detail[:1500] + "…"
	}
	out, err := json.Marshal(map[string]any{"text": text + "\n```" + detail + "```"})
	if err != nil {
		return raw
	}
	return out
}

// summarizeEvent makes a human-readable one-liner from known payloads.
func summarizeEvent(event string, payload json.RawMessage) string {
	var p map[string]any
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	switch {
	case strings.HasPrefix(event, "key."):
		name, _ := p["name"].(string)
		prefix, _ := p["prefix"].(string)
		actor, _ := p["actor"].(string)
		s := "key " + name
		if prefix != "" {
			s += " (" + prefix + ")"
		}
		if actor != "" {
			s += " by " + actor
		}
		return s
	case strings.HasPrefix(event, "user."):
		u, _ := p["username"].(string)
		return "user " + u
	default:
		return ""
	}
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (d *DBDispatch) deliver(w webhookRow, event string, body []byte) {
	body = platformBody(w.format, event, body)
	req, err := http.NewRequest(http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		d.record(w.id, "invalid URL: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Event", event)
	if w.secret != "" {
		req.Header.Set("X-Webhook-Signature", SignPayload(w.secret, body))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		d.record(w.id, "error: "+err.Error())
		return
	}
	defer resp.Body.Close()
	d.record(w.id, fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
}

func (d *DBDispatch) record(id, status string) {
	_, _ = d.db.Exec(db.Q(`UPDATE webhooks SET last_status = ?, last_delivery = ? WHERE id = ?`),
		status, time.Now().UTC(), id)
}

// SignPayload produces the X-Webhook-Signature value (same convention as
// the env-configured dispatcher): "sha256=" + HMAC-SHA256 hex of the body.
func SignPayload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Test sends a probe event to a specific webhook (from the "Test" button).
func (d *DBDispatch) Test(id string) (string, error) {
	var url, secret string
	var enabled int
	err := d.db.QueryRow(db.Q(`SELECT url, COALESCE(secret,''), enabled FROM webhooks WHERE id = ?`), id).Scan(&url, &secret, &enabled)
	if err != nil {
		return "", err
	}
	raw := marshalEvent("test.ping", map[string]any{"webhook_id": id, "sent_at": time.Now().UTC()})
	var fmtField, testURL string
	if err := d.db.QueryRow(db.Q(`SELECT COALESCE(format,'json'), url FROM webhooks WHERE id=?`), id).Scan(&fmtField, &testURL); err != nil {
		return "", err
	}
	if fmtField == "" || fmtField == "json" {
		if d2 := detectFormat(testURL); d2 != "json" {
			fmtField = d2
		}
	}
	body := platformBody(fmtField, "test.ping", raw)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		d.record(id, "invalid URL: "+err.Error())
		return "invalid URL", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Event", "test.ping")
	if secret != "" {
		req.Header.Set("X-Gateway-Signature", SignPayload(secret, body))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		d.record(id, "test error: "+err.Error())
		return "error: " + err.Error(), nil
	}
	defer resp.Body.Close()
	status := strconv.Itoa(resp.StatusCode)
	d.record(id, "test: "+status)
	return status, nil
}
