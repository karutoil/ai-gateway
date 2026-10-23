package handler

// Webhook management: CRUD + delivery test for the `webhooks` table.
// Gated by settings:write (gateway-level configuration).

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/audit"
	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/rbac"
	"ai-gateway/internal/webhook"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type WebhookHandler struct {
	DB         *sql.DB
	Dispatcher *webhook.DBDispatch
	Recorder   audit.Recorder
}

type webhookInput struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Events  string `json:"events"` // comma-separated; empty = all
	Secret  string `json:"secret"` // optional HMAC secret
	Format  string `json:"format"` // json (default) | discord | slack
	Enabled *bool  `json:"enabled"`
}

var validFormats = map[string]bool{"json": true, "discord": true, "slack": true}

func validEventCSV(csv string) bool {
	for _, e := range strings.Split(csv, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !validEventName(e) {
			return false
		}
	}
	return true
}

func validEventName(e string) bool {
	for _, r := range e {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.') {
			return false
		}
	}
	return strings.Contains(e, ".") && len(e) <= 64
}

func (h *WebhookHandler) Routes(r chi.Router) {
	// A webhook URL embeds the delivery token (Slack/Discord/Teams), so it is a
	// credential, not metadata: reading it is gated on the same permission as
	// mutating it. It used to be readable with logs:read (a default for member
	// and support), which let any member exfiltrate the URL and post arbitrary
	// content into the operator's channel.
	//
	// RequirePerm (not the handler-local check) so the RBAC middleware caches
	// the caller's effective set and per-user grants/revokes are honoured.
	r.With(middleware.RequirePerm(rbac.PermSettingsWrite)).Get("/webhooks", h.List)
	r.With(middleware.RequirePerm(rbac.PermSettingsWrite)).Post("/webhooks", h.Create)
	r.With(middleware.RequirePerm(rbac.PermSettingsWrite)).Put("/webhooks/{id}", h.Update)
	r.With(middleware.RequirePerm(rbac.PermSettingsWrite)).Delete("/webhooks/{id}", h.Delete)
	r.With(middleware.RequirePerm(rbac.PermSettingsWrite)).Post("/webhooks/{id}/test", h.Test)
}

// RequireSettingsWrite returns a tiny middleware using the handler's own
// permission check (keeps route wiring local).
func (h *WebhookHandler) RequireSettingsWrite() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !requirePermFor(r, "settings:write") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (h *WebhookHandler) List(w http.ResponseWriter, r *http.Request) {
	if !requirePermFor(r, rbac.PermSettingsWrite) {
		httperr.Forbidden(w, "settings:write required to read webhook endpoints")
		return
	}
	rows, err := h.DB.Query(db.Q(`SELECT id, name, url, COALESCE(events,''), COALESCE(format,'json'), enabled, created_at, updated_at, COALESCE(last_status,''), last_delivery FROM webhooks ORDER BY created_at DESC`))
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type wh struct {
		ID           string     `json:"id"`
		Name         string     `json:"name"`
		URL          string     `json:"url"`
		Events       string     `json:"events"`
		Format       string     `json:"format"`
		Enabled      bool       `json:"enabled"`
		CreatedAt    time.Time  `json:"created_at"`
		UpdatedAt    time.Time  `json:"updated_at"`
		LastStatus   string     `json:"last_status,omitempty"`
		LastDelivery *time.Time `json:"last_delivery,omitempty"`
	}
	out := []wh{}
	for rows.Next() {
		var w2 wh
		var enabled int
		if err := rows.Scan(&w2.ID, &w2.Name, &w2.URL, &w2.Events, &w2.Format, &enabled, &w2.CreatedAt, &w2.UpdatedAt, &w2.LastStatus, &w2.LastDelivery); err == nil {
			w2.Enabled = enabled == 1
			out = append(out, w2)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func validWebhookURL(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

func (h *WebhookHandler) Create(w http.ResponseWriter, r *http.Request) {
	var in webhookInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.URL) == "" {
		httperr.Invalid(w, "name and url are required")
		return
	}
	in.URL = strings.TrimSpace(in.URL)
	if !validWebhookURL(in.URL) {
		httperr.Invalid(w, "url must be http(s)")
		return
	}
	if len(in.URL) > 2048 || len(strings.TrimSpace(in.Name)) > 128 {
		httperr.Invalid(w, "name or url too long")
		return
	}
	if !validEventCSV(in.Events) {
		httperr.Invalid(w, "events must be comma-separated lowercase event names like key.created")
		return
	}
	if in.Format == "" {
		in.Format = "json"
	}
	if !validFormats[in.Format] {
		httperr.Invalid(w, "format must be one of: json, discord, slack")
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	_, err := h.DB.Exec(db.Q(`INSERT INTO webhooks (id, name, url, events, secret, format, enabled, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?)`),
		id, strings.TrimSpace(in.Name), in.URL, strings.TrimSpace(in.Events), in.Secret, in.Format, boolToInt(enabled), now, now)
	if err != nil {
		http.Error(w, `{"error":"create failed"}`, http.StatusInternalServerError)
		return
	}
	if h.Recorder != nil {
		h.Recorder.Log(auth.GetSubject(r), "create", "webhook", id, in.Name+" -> "+in.URL)
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"id": id})
}

func (h *WebhookHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in webhookInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		httperr.Invalid(w, "invalid json")
		return
	}
	// Same strict scheme rule as Create: the old check accepted any
	// "http"-prefixed string (e.g. "httpjunk:...").
	if in.URL != "" && !validWebhookURL(strings.TrimSpace(in.URL)) {
		httperr.Invalid(w, "url must be http(s)")
		return
	}
	if len(in.URL) > 2048 || (in.Name != "" && len(strings.TrimSpace(in.Name)) > 128) {
		httperr.Invalid(w, "name or url too long")
		return
	}
	if !validEventCSV(in.Events) {
		httperr.Invalid(w, "invalid events")
		return
	}
	if in.Format != "" && !validFormats[in.Format] {
		httperr.Invalid(w, "format must be json, discord, or slack")
		return
	}
	// Partial-update semantics: fields ABSENT from the body keep their
	// stored value. The previous statement unconditionally overwrote
	// events, format and enabled, so renaming a webhook also cleared its
	// event filter (silently widening delivery to ALL events), reset its
	// format to json, and re-enabled a disabled webhook. enabled uses a
	// nullable arg so "omitted" (nil) is distinguishable from false.
	enabledArg := any(nil)
	if in.Enabled != nil {
		enabledArg = boolToInt(*in.Enabled)
	}
	_, err := h.DB.Exec(db.Q(`UPDATE webhooks SET name=COALESCE(NULLIF(?,''), name), url=COALESCE(NULLIF(?,''), url), events=COALESCE(NULLIF(?,''), events), secret=CASE WHEN ? != '' THEN ? ELSE secret END, format=COALESCE(NULLIF(?,''), format), enabled=COALESCE(?, enabled), updated_at=? WHERE id=?`),
		strings.TrimSpace(in.Name), strings.TrimSpace(in.URL), strings.TrimSpace(in.Events), in.Secret, in.Secret, strings.TrimSpace(in.Format), enabledArg, time.Now().UTC(), id)
	if err != nil {
		http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
		return
	}
	if h.Recorder != nil {
		h.Recorder.Log(auth.GetSubject(r), "update", "webhook", id, "")
	}
	w.WriteHeader(200)
}

func (h *WebhookHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := h.DB.Exec(db.Q(`DELETE FROM webhooks WHERE id=?`), id); err != nil {
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	if h.Recorder != nil {
		h.Recorder.Log(auth.GetSubject(r), "delete", "webhook", id, "")
	}
	w.WriteHeader(http.StatusNoContent)
}

// Test sends a probe event to the webhook and records the result.
func (h *WebhookHandler) Test(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	status, err := h.Dispatcher.Test(id)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": status})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// emitWebhook fans a gateway event out to all matching webhooks via the
// process-wide dispatcher (no-op when boot hasn't installed one).
func emitWebhook(event string, payload map[string]any) {
	if webhook.Global != nil {
		webhook.Global.Emit(event, payload)
	}
}
