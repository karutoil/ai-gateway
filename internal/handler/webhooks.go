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
	r.Get("/webhooks", h.List)
	r.With(h.RequireSettingsWrite()).Post("/webhooks", h.Create)
	r.With(h.RequireSettingsWrite()).Put("/webhooks/{id}", h.Update)
	r.With(h.RequireSettingsWrite()).Delete("/webhooks/{id}", h.Delete)
	r.With(h.RequireSettingsWrite()).Post("/webhooks/{id}/test", h.Test)
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
	if !requirePermFor(r, "settings:read") {
		// settings:read isn't in the catalog; treat logs:read as the read gate
		if !requirePermFor(r, "logs:read") && !requirePermFor(r, "settings:write") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
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
		Format   string     `json:"format"`
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
	json.NewEncoder(w).Encode(out)
}

func (h *WebhookHandler) Create(w http.ResponseWriter, r *http.Request) {
	var in webhookInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.URL) == "" {
		httperr.Invalid(w, "name and url are required")
		return
	}
	in.URL = strings.TrimSpace(in.URL)
	if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
		httperr.Invalid(w, "url must be http(s)")
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
	if in.URL != "" && !strings.HasPrefix(in.URL, "http") {
		httperr.Invalid(w, "url must be http(s)")
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
	if in.Format == "" {
		in.Format = "json"
	}
	enabled := 1
	if in.Enabled != nil && !*in.Enabled {
		enabled = 0
	}
	_, err := h.DB.Exec(db.Q(`UPDATE webhooks SET name=COALESCE(NULLIF(?,''), name), url=COALESCE(NULLIF(?,''), url), events=?, secret=CASE WHEN ? != '' THEN ? ELSE secret END, format=?, enabled=?, updated_at=? WHERE id=?`),
		strings.TrimSpace(in.Name), strings.TrimSpace(in.URL), strings.TrimSpace(in.Events), in.Secret, in.Secret, in.Format, enabled, time.Now().UTC(), id)
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
