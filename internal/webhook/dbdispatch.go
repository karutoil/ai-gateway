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
	"math"
	"net/http"
	"sort"
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

// discordBody builds a professional Discord message: a titled, color-coded
// embed with a one-line summary and named fields — no raw JSON dumps.
// Shape: {"username": ..., "allowed_mentions": ..., "embeds": [{...}]}.
func discordBody(event string, raw []byte) []byte {
	var env struct {
		Payload json.RawMessage `json:"payload"`
		TS      string          `json:"ts"`
	}
	_ = json.Unmarshal(raw, &env)
	var p map[string]any
	_ = json.Unmarshal(env.Payload, &p)
	ts := normalizeTimestamp(env.TS)
	emoji, title, summary, color, fields := describeEvent(event, p)
	if title == "" {
		title = humanizeEvent(event)
		emoji = "📌"
	}
	fullTitle := truncateRunes(strings.TrimSpace(emoji+" "+title), 256)
	desc := truncateRunes(summary, 4000)
	embed := map[string]any{
		"title":     fullTitle,
		"color":     color,
		"timestamp": ts,
		"footer":    map[string]any{"text": truncateRunes("AI Gateway • "+event, 2048)},
	}
	if desc != "" {
		embed["description"] = desc
	}
	if len(fields) > 0 {
		fm := make([]map[string]any, 0, len(fields))
		for _, f := range fields {
			fm = append(fm, map[string]any{
				"name":   truncateRunes(f.name, 256),
				"value":  truncateRunes(discordFieldValue(f.value), 1024),
				"inline": true,
			})
		}
		embed["fields"] = fm
	}
	out, err := json.Marshal(map[string]any{
		"username":         discordUsername,
		"allowed_mentions": map[string]any{"parse": []string{}},
		"embeds":           []map[string]any{embed},
	})
	if err != nil {
		return raw
	}
	// Discord rejects embeds over 6000 chars total; retry without fields,
	// then fall back to a compact text-only message.
	if len(out) > 5900 {
		delete(embed, "fields")
		if desc != "" {
			embed["description"] = truncateRunes(summary, 1500)
		}
		out, err = json.Marshal(map[string]any{
			"username":         discordUsername,
			"allowed_mentions": map[string]any{"parse": []string{}},
			"embeds":           []map[string]any{embed},
		})
		if err != nil {
			return raw
		}
	}
	if len(out) > 5900 {
		fallback := fullTitle
		if summary != "" {
			fallback += "\n" + truncateRunes(summary, 1500)
		}
		out, err = json.Marshal(map[string]any{
			"username":         discordUsername,
			"allowed_mentions": map[string]any{"parse": []string{}},
			"content":          fallback,
		})
		if err != nil {
			return raw
		}
	}
	return out
}

// slackBody builds a professional Slack incoming-webhook message using
// Block Kit: header + summary + fields + context, with a plain-text
// fallback for notifications.
func slackBody(event string, raw []byte) []byte {
	var env struct {
		Payload json.RawMessage `json:"payload"`
		TS      string          `json:"ts"`
	}
	_ = json.Unmarshal(raw, &env)
	var p map[string]any
	_ = json.Unmarshal(env.Payload, &p)
	emoji, title, summary, _, fields := describeEvent(event, p)
	if title == "" {
		title = humanizeEvent(event)
		emoji = "📌"
	}
	header := truncateRunes(strings.TrimSpace(emoji+" "+title), 150)
	fallback := header
	if summary != "" {
		fallback += " — " + summary
	}
	fallback = truncateRunes(fallback, 2900)
	blocks := []map[string]any{
		{"type": "header", "text": map[string]any{"type": "plain_text", "text": header, "emoji": true}},
	}
	if summary != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": truncateRunes(summary, 2800)},
		})
	}
	if len(fields) > 0 {
		n := len(fields)
		if n > 10 {
			n = 10
		}
		sfields := make([]map[string]any, 0, n)
		for _, f := range fields[:n] {
			sfields = append(sfields, map[string]any{
				"type": "mrkdwn",
				"text": "*" + f.name + "*\n" + truncateRunes(slackFieldValue(f.value), 480),
			})
		}
		blocks = append(blocks, map[string]any{"type": "section", "fields": sfields})
	}
	ctx := "AI Gateway • `" + event + "`"
	if ts := prettyTimestamp(env.TS); ts != "" {
		ctx += " • " + ts
	}
	blocks = append(blocks, map[string]any{
		"type": "context",
		"elements": []map[string]any{
			{"type": "mrkdwn", "text": truncateRunes(ctx, 900)},
		},
	})
	out, err := json.Marshal(map[string]any{
		"username": discordUsername,
		"text":     fallback,
		"blocks":   blocks,
	})
	if err != nil {
		return raw
	}
	return out
}

// discordUsername is the display name for gateway alerts on chat platforms.
const discordUsername = "AI Gateway"

// Event color coding (Discord decimal colors).
const (
	eventGreen  = 5763719  // 0x57F287 — created, connected, healthy
	eventBlue   = 5793266  // 0x5865F2 — rotated, updated, exported
	eventRed    = 15548997 // 0xED4245 — revoked, disabled, deleted
	eventOrange = 15105570 // 0xE67E22 — quota warnings
	eventGrey   = 10070709 // 0x99AAB5 — audit trail, generic
	eventPurple = 10181046 // 0x9B59B6 — organizations
)

type eventField struct{ name, value string }

// describeEvent returns the human presentation for a gateway event:
// emoji, title, one-line summary, embed color, and named detail fields.
// Both Discord and Slack render from this so the two stay consistent.
func describeEvent(event string, p map[string]any) (emoji, title, summary string, color int, fields []eventField) {
	if p == nil {
		p = map[string]any{}
	}
	str := func(key string) string {
		if s, _ := p[key].(string); s != "" {
			return s
		}
		return ""
	}
	switch event {
	case "key.created":
		name, prefix, actor := str("name"), str("prefix"), str("actor")
		if name == "" {
			name = str("key")
		}
		summary = "API key " + codeTick(name) + " is ready to use."
		if actor != "" {
			summary += " Created by " + codeTick(actor) + "."
		}
		fields = nonEmptyFields(
			eventField{"Key", name},
			eventField{"Prefix", prefix},
			eventField{"Created by", actor},
		)
		return "🔑", "API Key Created", summary, eventGreen, fields
	case "key.rotated":
		name, prefix, actor := str("name"), str("prefix"), str("actor")
		if name == "" {
			name = str("key")
		}
		summary = "API key " + codeTick(name) + " was rotated."
		if actor != "" {
			summary += " Rotated by " + codeTick(actor) + "."
		}
		summary += " The previous secret stays valid briefly while you roll out the new one."
		fields = nonEmptyFields(
			eventField{"Key", name},
			eventField{"Prefix", prefix},
			eventField{"Rotated by", actor},
		)
		return "🔄", "API Key Rotated", summary, eventBlue, fields
	case "key.revoked":
		name := str("name")
		if name == "" {
			name = str("key")
		}
		summary = "API key " + codeTick(name) + " was revoked and can no longer authenticate."
		if actor := str("actor"); actor != "" {
			summary += " Revoked by " + codeTick(actor) + "."
		}
		fields = nonEmptyFields(
			eventField{"Key", name},
			eventField{"Revoked by", str("actor")},
		)
		return "🚫", "API Key Revoked", summary, eventRed, fields
	case "user.created":
		u, actor := str("username"), str("actor")
		summary = "Dashboard user " + codeTick(u) + " was created."
		if actor != "" {
			summary += " Created by " + codeTick(actor) + "."
		}
		fields = nonEmptyFields(
			eventField{"User", u},
			eventField{"Created by", actor},
		)
		return "👤", "User Created", summary, eventGreen, fields
	case "user.updated":
		u, actor := str("username"), str("actor")
		summary = "Dashboard user " + codeTick(u) + " was updated."
		if actor != "" {
			summary += " Updated by " + codeTick(actor) + "."
		}
		fields = nonEmptyFields(
			eventField{"User", u},
			eventField{"Updated by", actor},
		)
		return "✏️", "User Updated", summary, eventBlue, fields
	case "user.disabled":
		u, actor := str("username"), str("actor")
		summary = "Dashboard user " + codeTick(u) + " was disabled and can no longer sign in."
		if actor != "" {
			summary += " Disabled by " + codeTick(actor) + "."
		}
		fields = nonEmptyFields(
			eventField{"User", u},
			eventField{"Disabled by", actor},
		)
		return "⛔", "User Disabled", summary, eventRed, fields
	case "org.created":
		name, actor := str("name"), str("actor")
		summary = "Organization " + codeTick(name) + " was created."
		if actor != "" {
			summary += " Created by " + codeTick(actor) + "."
		}
		fields = nonEmptyFields(
			eventField{"Organization", name},
			eventField{"Created by", actor},
		)
		return "🏢", "Organization Created", summary, eventPurple, fields
	case "billing.over_quota":
		prefix, orgID, kind := str("prefix"), str("org_id"), str("limit_kind")
		limit, used := formatPayloadNumber(p["limit"]), formatPayloadNumber(p["used"])
		kindLabel := humanizeLabel(kind)
		if kindLabel == "" {
			kindLabel = "Quota"
		}
		summary = "Requests for key " + codeTick(prefix) + " are being rejected (429): " + kindLabel + " limit reached."
		fields = nonEmptyFields(
			eventField{"Key prefix", prefix},
			eventField{"Limit type", kindLabel},
			eventField{"Limit", limit},
			eventField{"Usage", used},
			eventField{"Org ID", orgID},
		)
		return "⚠️", "Quota Exceeded", summary, eventOrange, fields
	case "billing.export":
		summary = "Billing data was exported."
		if actor := str("actor"); actor != "" {
			summary += " Requested by " + codeTick(actor) + "."
		}
		fields = nonEmptyFields(
			eventField{"Requested by", str("actor")},
			eventField{"Range", str("range")},
			eventField{"Path", str("path")},
		)
		return "🧾", "Billing Export", summary, eventBlue, fields
	case "logs.export":
		summary = "Request logs were exported."
		if actor := str("actor"); actor != "" {
			summary += " Requested by " + codeTick(actor) + "."
		}
		fields = nonEmptyFields(
			eventField{"Requested by", str("actor")},
			eventField{"Filter", truncateRunes(str("filter"), 300)},
			eventField{"Path", str("path")},
		)
		return "📋", "Logs Export", summary, eventGrey, fields
	case "test.ping":
		return "✅", "Webhook Connected", "This channel will now receive AI Gateway alerts.", eventGreen, nil
	default:
		if strings.HasPrefix(event, "audit.") {
			return describeAuditEvent(p)
		}
		// Generic fallback: human title plus the most useful payload keys.
		return "📌", humanizeEvent(event), genericSummary(p), eventGrey, genericFields(p)
	}
}

// describeAuditEvent presents audit.* rows (admin writes + API mutations).
func describeAuditEvent(p map[string]any) (emoji, title, summary string, color int, fields []eventField) {
	actor, action, targetType, targetID, meta := strOf(p, "actor"), strOf(p, "action"), strOf(p, "target_type"), strOf(p, "target_id"), strOf(p, "meta")
	target := humanizeLabel(targetType)
	if target == "" {
		target = "Resource"
	}
	color = eventGrey
	emoji = "📝"
	var verb string
	switch strings.ToLower(action) {
	case "post", "create":
		verb = "Created"
		color = eventGreen
	case "put", "patch", "update":
		verb = "Updated"
		color = eventBlue
	case "delete":
		verb = "Deleted"
		color = eventRed
	case "revoke":
		verb = "Revoked"
		color = eventRed
	case "rotate":
		verb = "Rotated"
		color = eventBlue
	case "add_member":
		title = "Member Added"
		summary = "A member was added to an organization."
		if actor != "" {
			summary += " Added by " + actor + "."
		}
		return emoji, title, summary, eventGreen, nonEmptyFields(
			eventField{"Added by", actor},
			eventField{"Details", meta},
		)
	case "login":
		title = "Dashboard Login"
		summary = codeTick(actor) + " signed in to the dashboard."
		return emoji, title, summary, eventGrey, nonEmptyFields(
			eventField{"User", actor},
			eventField{"Details", meta},
		)
	default:
		if strings.HasPrefix(strings.ToLower(action), "audit") || action == "" {
			title = "Audit Event"
			summary = "An audited change was recorded."
		} else {
			verb = humanizeLabel(action)
			if verb == "" {
				verb = "Changed"
			}
		}
	}
	if title == "" {
		title = target + " " + verb
	}
	if summary == "" {
		if actor != "" {
			summary = codeTick(actor) + " " + strings.ToLower(verb) + " " + strings.ToLower(target)
			if targetID != "" && targetID != meta {
				summary += " " + codeTick(truncateRunes(targetID, 120)) + "."
			} else {
				summary += "."
			}
		} else {
			summary = target + " " + strings.ToLower(verb) + "."
		}
	}
	fields = nonEmptyFields(
		eventField{"Actor", actor},
		eventField{"Target", targetLabel(target, targetID)},
		eventField{"Details", truncateRunes(meta, 500)},
	)
	return emoji, title, summary, color, fields
}

// summarizeEvent keeps the old one-liner helper (used for nothing critical
// now) backed by the shared presenter so any caller stays consistent.
func summarizeEvent(event string, payload json.RawMessage) string {
	var p map[string]any
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	_, _, summary, _, _ := describeEvent(event, p)
	return stripMarkdown(summary)
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// truncateRunes cuts s to at most max runes, appending … on truncation.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

// codeTick wraps single-token identifiers in backticks for chat markdown.
func codeTick(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "—"
	}
	if strings.ContainsAny(s, "`\n") {
		return s
	}
	return "`" + s + "`"
}

// discordFieldValue keeps identifiers scannable inside embed fields.
func discordFieldValue(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "—"
	}
	if strings.Contains(s, "\n") || strings.Contains(s, "`") || len([]rune(s)) > 200 || strings.Contains(s, " ") {
		return s
	}
	return "`" + s + "`"
}

// slackFieldValue mirrors discordFieldValue for Slack mrkdwn fields.
func slackFieldValue(s string) string {
	return discordFieldValue(s)
}

// nonEmptyFields drops fields with blank values so embeds stay compact.
func nonEmptyFields(fields ...eventField) []eventField {
	out := fields[:0]
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			continue
		}
		f.value = strings.TrimSpace(f.value)
		out = append(out, f)
	}
	return out
}

func strOf(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	s, _ := p[key].(string)
	return strings.TrimSpace(s)
}

// humanizeLabel turns snake/camel keys into Title Case words.
func humanizeLabel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "-", " ")
	s = strings.ReplaceAll(s, ".", " ")
	words := strings.Fields(s)
	for i, w := range words {
		words[i] = titleWord(w)
	}
	return strings.Join(words, " ")
}

// humanizeEvent turns "billing.over_quota" into "Billing Over Quota".
func humanizeEvent(event string) string {
	return humanizeLabel(strings.ReplaceAll(event, ".", " "))
}

func titleWord(w string) string {
	if w == "" {
		return ""
	}
	r := []rune(w)
	if len(r) == 1 {
		return strings.ToUpper(w)
	}
	return strings.ToUpper(string(r[:1])) + strings.ToLower(string(r[1:]))
}

// targetLabel combines a pretty target type with its id/path.
func targetLabel(target, id string) string {
	target = strings.TrimSpace(target)
	id = strings.TrimSpace(id)
	if target == "" {
		return id
	}
	if id == "" {
		return target
	}
	return target + " " + id
}

// genericSummary builds a plain one-liner from unknown payloads.
func genericSummary(p map[string]any) string {
	if len(p) == 0 {
		return "An event was recorded."
	}
	if actor := strOf(p, "actor"); actor != "" {
		return "Recorded for " + codeTick(actor) + "."
	}
	return "An event was recorded — see details below."
}

// genericFields surfaces up to 6 payload keys as named fields.
func genericFields(p map[string]any) []eventField {
	if len(p) == 0 {
		return nil
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []eventField
	for _, k := range keys {
		if k == "actor" {
			continue
		}
		v := shortValue(p[k])
		if v == "" || v == "—" {
			continue
		}
		out = append(out, eventField{humanizeLabel(k), v})
		if len(out) >= 6 {
			break
		}
	}
	if actor := strOf(p, "actor"); actor != "" {
		out = append([]eventField{{"Actor", actor}}, out...)
	}
	return out
}

// shortValue renders one JSON value as compact plain text.
func shortValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "—"
	case string:
		t = strings.TrimSpace(t)
		if t == "" {
			return "—"
		}
		return truncateRunes(t, 200)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return formatFloatGrouped(t)
	case int:
		return formatIntGrouped(int64(t))
	case int64:
		return formatIntGrouped(t)
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return formatIntGrouped(i)
		}
		return t.String()
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "—"
		}
		return truncateRunes(string(b), 200)
	}
}

// formatPayloadNumber renders limit/usage counters without decimals.
func formatPayloadNumber(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case float64:
		if math.Trunc(t) == t && math.Abs(t) < 1e15 {
			return formatIntGrouped(int64(t))
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return formatIntGrouped(int64(t))
	case int64:
		return formatIntGrouped(t)
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return formatIntGrouped(i)
		}
		return t.String()
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func formatFloatGrouped(f float64) string {
	if math.Trunc(f) == f && math.Abs(f) < 1e15 {
		return formatIntGrouped(int64(f))
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func formatIntGrouped(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// stripMarkdown removes chat code ticks for plain-text fallbacks.
func stripMarkdown(s string) string {
	return strings.ReplaceAll(s, "`", "")
}

// normalizeTimestamp returns a Discord-safe timestamp (now on bad input).
func normalizeTimestamp(ts string) string {
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// prettyTimestamp renders "2006-01-02 15:04 UTC" for Slack context lines.
func prettyTimestamp(ts string) string {
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t.UTC().Format("2006-01-02 15:04 UTC")
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.UTC().Format("2006-01-02 15:04 UTC")
	}
	return ""
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
	req.Header.Set("X-Webhook-Event", "test.ping")
	if secret != "" {
		req.Header.Set("X-Webhook-Signature", SignPayload(secret, body))
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
