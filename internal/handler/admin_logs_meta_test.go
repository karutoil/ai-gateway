package handler

// Tests for the extended usage-log API: GetLog returns finish reason /
// fallback chain / token detail, and Stats exposes hourly buckets, an error
// breakdown and cache-hit rate.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

// newTestAdminHandler wires a minimal AdminHandler on :memory: sqlite.

// adminReq wraps a request with admin auth context (RBAC read guards now
// resolve permissions from the request context).
func adminReq(path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r = r.WithContext(auth.WithRole(r.Context(), "admin"))
	return r.WithContext(auth.WithSubject(r.Context(), "admin"))
}

func newTestAdminHandler(t *testing.T) (*AdminHandler, func()) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	h := &AdminHandler{
		Config:        &config.Config{AdminPassword: "unused", JWTSecret: []byte("test-jwt-secret-which-is-long-enough")},
		DB:            database,
		ProviderStore: provider.NewStore(database, make([]byte, 32)),
	}
	if _, err := h.ProviderStore.Create("prov-1", models.ProviderOpenAICompatible, "http://127.0.0.1:1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	return h, func() { database.Close() }
}

// seedLog inserts one request_logs row with the extended metadata.
func seedLog(t *testing.T, h *AdminHandler, status int, model, finish, chain, errMsg string, cacheRead int, at time.Time) {
	t.Helper()
	_, err := h.DB.Exec(db.Q(`INSERT INTO request_logs(id,key_prefix,provider_id,model,endpoint,status,latency_ms,created_at,prompt_tokens,completion_tokens,total_tokens,cost_usd,is_stream,error,request_body,response_body,finish_reason,fallback_chain,cache_read_tokens,cache_write_tokens,reasoning_tokens) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		"log-"+model+"-"+time.Now().Format("150405.000000000"), "pfx-test", "prov-1", model, "chat", status, 100, at, 10, 5, 15, 0.01, false,
		nullStr(errMsg), nullStr(`{"messages":[{"role":"user","content":"hi"}]}`), nullStr(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`),
		nullStr(finish), nullStr(chain), cacheRead, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
}

func nullStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func TestGetLogReturnsUsageMetadata(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	chain := `[{"provider_id":"prov-2","name":"backup","status":502}]`
	seedLog(t, h, 200, "gpt-test", "stop", chain, "", 42, time.Now().UTC())

	// Direct lookup by the id we generated. Rows must be closed BEFORE
	// GetLog runs — :memory: sqlite is a single connection, so an open
	// cursor deadlocks the next query.
	rows, err := h.DB.Query(db.Q(`SELECT id FROM request_logs`))
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if rows.Next() {
		_ = rows.Scan(&id)
	}
	rows.Close()
	// GetLog reads the id via chi.URLParam, so it must run behind a real
	// chi router (a bare httptest request has no route context).
	r := chi.NewRouter()
	r.Get("/api/logs/{id}", h.GetLog)
	req := adminReq("/api/logs/" + id)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GetLog status = %d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if got, _ := out["finish_reason"].(string); got != "stop" {
		t.Fatalf("finish_reason = %v", out["finish_reason"])
	}
	if got, _ := out["cache_read_tokens"].(float64); got != 42 {
		t.Fatalf("cache_read_tokens = %v", out["cache_read_tokens"])
	}
	chainArr, _ := out["fallback_chain"].([]any)
	if len(chainArr) != 1 {
		t.Fatalf("fallback_chain should parse to 1 entry, got %v", out["fallback_chain"])
	}
	first, _ := chainArr[0].(map[string]any)
	if first["name"] != "backup" {
		t.Fatalf("chain entry wrong: %v", first)
	}
}

func TestStatsHourlyBucketsAndErrors(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	now := time.Now().UTC()
	seedLog(t, h, 200, "gpt-a", "stop", "", "", 0, now.Add(-1*time.Hour))
	// Most recent 502 (2h ago) — its message must be the sampled one.
	seedLog(t, h, 502, "gpt-a", "", "", "upstream connect failed again", 0, now.Add(-2*time.Hour))
	seedLog(t, h, 502, "gpt-a", "", "", "upstream connect failed", 0, now.Add(-3*time.Hour))

	req := adminReq("/api/stats?range=24h")
	w := httptest.NewRecorder()
	h.Stats(w, req)
	if w.Code != 200 {
		t.Fatalf("Stats status = %d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	// 24h range must return HOURLY buckets (keys contain "T"), not one day.
	daily, _ := out["daily"].([]any)
	if len(daily) == 0 {
		t.Fatal("no daily buckets")
	}
	hourly := 0
	for _, d := range daily {
		dm, _ := d.(map[string]any)
		if strings.Contains(dm["day"].(string), "T") {
			hourly++
		}
	}
	if hourly < 2 {
		t.Fatalf("expected >=2 hourly buckets for 24h range, got %d of %d", hourly, len(daily))
	}

	// Error breakdown: two 502s, sample message = most recent one.
	errs, _ := out["errors"].([]any)
	if len(errs) == 0 {
		t.Fatal("no error rows")
	}
	var row502 map[string]any
	for _, e := range errs {
		em, _ := e.(map[string]any)
		if em["status"].(float64) == 502 {
			row502 = em
		}
	}
	if row502 == nil {
		t.Fatal("missing 502 row")
	}
	if got, _ := row502["count"].(float64); got != 2 {
		t.Fatalf("502 count = %v", row502["count"])
	}
	if got, _ := row502["sample"].(string); !strings.Contains(got, "failed again") {
		t.Fatalf("sample should be the most recent error, got %q", got)
	}

	// 7d range must keep daily (date-only) buckets.
	req7 := adminReq("/api/stats?range=7d")
	w7 := httptest.NewRecorder()
	h.Stats(w7, req7)
	var out7 map[string]any
	_ = json.Unmarshal(w7.Body.Bytes(), &out7)
	for _, d := range out7["daily"].([]any) {
		dm, _ := d.(map[string]any)
		if strings.Contains(dm["day"].(string), "T") {
			t.Fatalf("7d range must use daily buckets, got %v", dm["day"])
		}
	}
}

func TestStatsCacheHitRate(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	now := time.Now().UTC()
	seedLog(t, h, 200, "gpt-cache", "stop", "", "", 80, now.Add(-1*time.Hour))
	seedLog(t, h, 200, "gpt-cache", "stop", "", "", 0, now.Add(-2*time.Hour))

	req := adminReq("/api/stats?range=24h")
	w := httptest.NewRecorder()
	h.Stats(w, req)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// prompt_tokens=10 on both rows → 80/20 = 4.0 (ratio uses raw prompt col)
	rate, _ := out["cache_hit_rate"].(float64)
	if rate <= 0 {
		t.Fatalf("cache_hit_rate = %v, want > 0", out["cache_hit_rate"])
	}
}

// silence unused import when models shape changes
var _ = models.RequestLog{}
