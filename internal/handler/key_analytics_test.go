package handler

// Tests for per-API-key analytics (GET /api/keys/{id}/analytics).

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"

	"github.com/go-chi/chi/v5"
)

// seedKeyAnalytics creates a gateway key and attributes request_logs rows to
// it via key_id (the path the proxy writes since migration 012).
func seedKeyAnalytics(t *testing.T, h *AdminHandler, name string) (keyID, prefix string) {
	t.Helper()
	ks := apikey.NewStore(h.DB)
	k, err := ks.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	return k.ID, k.Prefix
}

// seedAttrLog inserts one request_logs row attributed to the given key.
func seedAttrLog(t *testing.T, h *AdminHandler, keyID, prefix, model, endpoint string, status int, tokens int, cost float64, errMsg string, latency int64, at time.Time) {
	t.Helper()
	_, err := h.DB.Exec(db.Q(`INSERT INTO request_logs(id,key_prefix,key_id,provider_id,model,endpoint,status,latency_ms,ttft_ms,created_at,prompt_tokens,completion_tokens,total_tokens,cost_usd,is_stream,error) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		"kalog-"+model+"-"+time.Now().Format("150405.000000000"), prefix, keyID, "prov-1", model, endpoint, status, latency, 50, at, tokens/2, tokens-tokens/2, tokens, cost, false,
		nullStr(errMsg))
	if err != nil {
		t.Fatal(err)
	}
}

func TestKeyAnalyticsAggregates(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	keyID, prefix := seedKeyAnalytics(t, h, "prod-key")
	now := time.Now().UTC()

	seedAttrLog(t, h, keyID, prefix, "gpt-a", "chat", 200, 100, 0.10, "", 100, now.Add(-1*time.Hour))
	seedAttrLog(t, h, keyID, prefix, "gpt-a", "chat", 200, 200, 0.20, "", 200, now.Add(-2*time.Hour))
	seedAttrLog(t, h, keyID, prefix, "gpt-b", "embeddings", 502, 0, 0, "upstream exploded", 40, now.Add(-3*time.Hour))
	// Another key's traffic must not leak in.
	otherID, otherPrefix := seedKeyAnalytics(t, h, "other-key")
	seedAttrLog(t, h, otherID, otherPrefix, "gpt-a", "chat", 200, 999, 9.99, "", 100, now.Add(-1*time.Hour))
	// Unattributed legacy row (NULL key_id, prefix set) counts toward prefix fallback.
	_, err := h.DB.Exec(db.Q(`INSERT INTO request_logs(id,key_prefix,provider_id,model,endpoint,status,latency_ms,created_at,total_tokens,cost_usd,is_stream) VALUES(?,?,?,?,?,?,?,?,?,?,?)`),
		"legacy-1", prefix, "prov-1", "gpt-c", "chat", 200, 150, now.Add(-4*time.Hour), 50, 0.05, false)
	if err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Get("/api/keys/{id}/analytics", h.KeyAnalytics)
	req := httptest.NewRequest(http.MethodGet, "/api/keys/"+keyID+"/analytics?range=24h", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("KeyAnalytics status = %d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	// 4 attributable rows for this key: 2 modern + 1 failed + 1 legacy
	// prefix-fallback row; the other key's 999-token row must not be included.
	if got, _ := out["range_requests"].(float64); got != 4 {
		t.Fatalf("range_requests = %v, want 4", out["range_requests"])
	}
	if got, _ := out["range_tokens"].(float64); got != 350 {
		t.Fatalf("range_tokens = %v, want 350", out["range_tokens"])
	}
	// Success rate: 3 of 4.
	if got, _ := out["range_successful"].(float64); got != 3 {
		t.Fatalf("range_successful = %v, want 3", out["range_successful"])
	}
	// All-time equals in-window here.
	allTime, _ := out["all_time"].(map[string]any)
	if got, _ := allTime["requests"].(float64); got != 4 {
		t.Fatalf("all_time.requests = %v, want 4", allTime["requests"])
	}

	// Top models: gpt-a leads (2 requests, 300 tokens).
	models, _ := out["top_models"].([]any)
	if len(models) == 0 {
		t.Fatal("no top_models")
	}
	first, _ := models[0].(map[string]any)
	if first["model"] != "gpt-a" {
		t.Fatalf("top model = %v, want gpt-a", first["model"])
	}

	// Errors: one 502 with the sampled message.
	errs, _ := out["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly one row", errs)
	}
	e0, _ := errs[0].(map[string]any)
	if e0["status"].(float64) != 502 {
		t.Fatalf("error status = %v", e0["status"])
	}
	if s, _ := e0["sample"].(string); s != "upstream exploded" {
		t.Fatalf("error sample = %q", s)
	}

	// Latency percentiles: 4 samples {40,100,150,200} sorted.
	lat, _ := out["latency"].(map[string]any)
	if got, _ := lat["p50"].(float64); got != 100 {
		t.Fatalf("p50 = %v, want 100", lat["p50"])
	}
	if got, _ := lat["p95"].(float64); got != 200 {
		t.Fatalf("p95 = %v, want 200", lat["p95"])
	}

	// Hourly buckets for the 24h range (same contract as Stats).
	daily, _ := out["daily"].([]any)
	if len(daily) == 0 {
		t.Fatal("no daily buckets")
	}
	hourly := 0
	for _, d := range daily {
		dm, _ := d.(map[string]any)
		if s, ok := dm["day"].(string); ok && len(s) > 10 && s[10] == 'T' {
			hourly++
		}
	}
	if hourly == 0 {
		t.Fatalf("24h range must produce hourly buckets, got %v", daily)
	}
}

func TestKeyAnalyticsUnknownKey(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	r := chi.NewRouter()
	r.Get("/api/keys/{id}/analytics", h.KeyAnalytics)
	req := httptest.NewRequest(http.MethodGet, "/api/keys/does-not-exist/analytics", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestKeyAnalyticsLegacyRowsWithoutKeyIDStillCount(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	keyID, prefix := seedKeyAnalytics(t, h, "legacy-key")
	// Row with key_prefix set but key_id NULL (pre-migration-012 data).
	_, err := h.DB.Exec(db.Q(`INSERT INTO request_logs(id,key_prefix,provider_id,model,endpoint,status,latency_ms,created_at,total_tokens,cost_usd,is_stream) VALUES(?,?,?,?,?,?,?,?,?,?,?)`),
		"old-1", prefix, "prov-1", "gpt-x", "chat", 200, 120, time.Now().UTC().Add(-time.Hour), 80, 0.01, false)
	if err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Get("/api/keys/{id}/analytics", h.KeyAnalytics)
	req := httptest.NewRequest(http.MethodGet, "/api/keys/"+keyID+"/analytics?range=7d", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if got, _ := out["range_requests"].(float64); got != 1 {
		t.Fatalf("range_requests = %v, want 1 (prefix fallback)", out["range_requests"])
	}
}

func TestBackfillKeyIDs(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	keyID, prefix := seedKeyAnalytics(t, h, "backfill-key")
	_, err := h.DB.Exec(db.Q(`INSERT INTO request_logs(id,key_prefix,provider_id,model,endpoint,status,latency_ms,created_at,total_tokens,cost_usd,is_stream) VALUES(?,?,?,?,?,?,?,?,?,?,?)`),
		"bf-1", prefix, "prov-1", "gpt-z", "chat", 200, 100, time.Now().UTC(), 10, 0.01, false)
	if err != nil {
		t.Fatal(err)
	}

	n, err := db.BackfillKeyIDs(h.DB)
	if err != nil {
		t.Fatalf("BackfillKeyIDs: %v", err)
	}
	if n != 1 {
		t.Fatalf("backfilled %d rows, want 1", n)
	}
	var got sql.NullString
	_ = h.DB.QueryRow(`SELECT key_id FROM request_logs WHERE id='bf-1'`).Scan(&got)
	if !got.Valid || got.String != keyID {
		t.Fatalf("key_id = %v, want %s", got, keyID)
	}

	// Idempotent: second run backfills nothing.
	n, err = db.BackfillKeyIDs(h.DB)
	if err != nil || n != 0 {
		t.Fatalf("second backfill n=%d err=%v, want 0/nil", n, err)
	}
}
