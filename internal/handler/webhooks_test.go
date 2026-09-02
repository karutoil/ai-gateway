package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
	"ai-gateway/internal/webhook"

	"github.com/go-chi/chi/v5"
)

func newWebhookEnv(t *testing.T) (*WebhookHandler, *chi.Mux, func()) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	h := &WebhookHandler{
		DB:         database,
		Dispatcher: webhook.NewDBDispatch(database),
	}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		h.Routes(r)
	})
	return h, r, func() { database.Close() }
}

func whReq(method, path, body string) *http.Request {
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	r2 := httptest.NewRequest(method, path, rd)
	req := r2 // keep name continuity
	req.Header.Set("Content-Type", "application/json")
	// requirePermFor falls back to role defaults; admin bypasses.
	ctx := auth.WithRole(r2.Context(), "admin")
	ctx = auth.WithSubject(ctx, "admin")
	return req.WithContext(ctx)
}

func TestWebhookCRUD(t *testing.T) {
	_, r, closer := newWebhookEnv(t)
	defer closer()

	// Create
	w := httptest.NewRecorder()
	r.ServeHTTP(w, whReq("POST", "/api/webhooks", `{"name":"relay","url":"https://example.com/hook","events":"test.ping,logs.export","secret":"s3cret"}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.ID == "" {
		t.Fatal("no id returned")
	}

	// List
	w = httptest.NewRecorder()
	r.ServeHTTP(w, whReq("GET", "/api/webhooks", ""))
	if w.Code != 200 {
		t.Fatalf("list: %d", w.Code)
	}
	var list []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || list[0]["name"] != "relay" {
		t.Fatalf("list = %v", list)
	}

	// Update (disable)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, whReq("PUT", "/api/webhooks/"+created.ID, `{"name":"relay2","enabled":false}`))
	if w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}

	// Create with invalid URL
	w = httptest.NewRecorder()
	r.ServeHTTP(w, whReq("POST", "/api/webhooks", `{"name":"x","url":"ftp://bad"}`))
	if w.Code != 400 {
		t.Fatalf("invalid url accepted: %d", w.Code)
	}

	// Create with invalid events
	w = httptest.NewRecorder()
	r.ServeHTTP(w, whReq("POST", "/api/webhooks", `{"name":"x","url":"https://ok.example","events":"BAD EVENT!"}`))
	if w.Code != 400 {
		t.Fatalf("invalid events accepted: %d", w.Code)
	}

	// Delete
	w = httptest.NewRecorder()
	r.ServeHTTP(w, whReq("DELETE", "/api/webhooks/"+created.ID, ""))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}

	// List empty again
	w = httptest.NewRecorder()
	r.ServeHTTP(w, whReq("GET", "/api/webhooks", ""))
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 0 {
		t.Fatalf("expected empty after delete, got %v", list)
	}
}
