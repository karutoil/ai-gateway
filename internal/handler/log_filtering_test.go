package handler

// Tests for the extended logs read surface: shared filters (log_filters.go),
// the group-by rollup endpoint (log_group.go) and the filter-aware CSV
// export (log_export.go).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"

	"github.com/go-chi/chi/v5"
)

// seedLogRows inserts a controlled set of request_logs rows and returns the
// ids of the inserted rows' key for filter assertions.
type logSeed struct {
	model      string
	endpoint   string
	status     int
	latency    int64
	tokens     int
	cost       float64
	stream     bool
	errMsg     string
	finish     string
	keyPrefix  string
	keyID      string
	providerID string
	at         time.Time
}

func seedLogRows(t *testing.T, h *AdminHandler, seeds []logSeed) {
	t.Helper()
	for i, s := range seeds {
		errStr := interface{}(nil)
		if s.errMsg != "" {
			errStr = s.errMsg
		}
		finish := interface{}(nil)
		if s.finish != "" {
			finish = s.finish
		}
		_, err := h.DB.Exec(db.Q(`INSERT INTO request_logs(id,key_prefix,key_id,provider_id,model,endpoint,status,latency_ms,ttft_ms,created_at,prompt_tokens,completion_tokens,total_tokens,cost_usd,is_stream,error,finish_reason,request_body) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
			fmt.Sprintf("seed-%d", i), s.keyPrefix, nullStr(s.keyID), nullStr(s.providerID), s.model, s.endpoint, s.status, s.latency, 30, s.at,
			s.tokens/2, s.tokens-s.tokens/2, s.tokens, s.cost, s.stream, errStr, finish, `{"messages":[{"role":"user","content":"findme-needle"}]}`)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

func newLogsRouter(h *AdminHandler) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/api/logs", h.Logs)
	r.Get("/api/logs/group", h.LogGroup)
	r.Get("/api/logs/export", h.LogsExport)
	return r
}

// getJSON issues a GET and unmarshals the response. Returns rows from
// "rows" (object envelope, e.g. group endpoint) or the top-level array
// (list endpoints like /api/logs).
func getJSON(t *testing.T, r *chi.Mux, path string) (int, map[string]any, []any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req = req.WithContext(auth.WithRole(req.Context(), "admin"))
	req = req.WithContext(auth.WithSubject(req.Context(), "admin"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		return w.Code, nil, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &obj); err == nil {
		rows, _ := obj["rows"].([]any)
		return w.Code, obj, rows
	}
	var arr []any
	if err := json.Unmarshal(w.Body.Bytes(), &arr); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return w.Code, nil, arr
}

func TestLogsFilters(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	ks := apikey.NewStore(h.DB)
	k, _ := ks.Create("filter-key")
	now := time.Now().UTC()
	ps := h.ProviderStore
	prov, _ := ps.Create("prov-x", "openai", "http://127.0.0.1:1", "sk-t")
	prov2, _ := ps.Create("prov-y", "openai", "http://127.0.0.1:2", "sk-t")
	// All seeds carry a real provider id — production rows are always routed
	// through a provider (the Logs list scan assumes this).
	pID, pID2 := prov.ID, prov2.ID

	seedLogRows(t, h, []logSeed{
		{model: "gpt-fast", endpoint: "chat", status: 200, latency: 100, tokens: 10, cost: 0.01, stream: false, keyPrefix: k.Prefix, keyID: k.ID, providerID: pID, at: now.Add(-time.Hour)},
		{model: "gpt-big", endpoint: "responses", status: 200, latency: 900, tokens: 500, cost: 0.50, stream: true, keyPrefix: k.Prefix, keyID: k.ID, providerID: pID2, at: now.Add(-2 * time.Hour)},
		{model: "gpt-fast", endpoint: "chat", status: 502, latency: 50, tokens: 0, cost: 0, stream: false, errMsg: "upstream exploded", keyPrefix: k.Prefix, keyID: k.ID, providerID: pID, at: now.Add(-3 * time.Hour)},
	})

	r := newLogsRouter(h)

	// key_id filter
	code, _, rows := getJSON(t, r, "/api/logs?key_id="+k.ID)
	if code != 200 || len(rows) != 3 {
		t.Fatalf("key_id filter: code=%d rows=%d, want 200/3", code, len(rows))
	}

	// provider_id filter
	code, _, rows = getJSON(t, r, "/api/logs?provider_id="+prov.ID)
	if code != 200 || len(rows) != 2 {
		t.Fatalf("provider_id filter: code=%d rows=%d, want 200/2", code, len(rows))
	}

	// stream=true / false
	code, _, rows = getJSON(t, r, "/api/logs?stream=true")
	if code != 200 || len(rows) != 1 {
		t.Fatalf("stream=true: code=%d rows=%d, want 200/1", code, len(rows))
	}
	code, _, rows = getJSON(t, r, "/api/logs?stream=false")
	if code != 200 || len(rows) != 2 {
		t.Fatalf("stream=false: code=%d rows=%d, want 200/2", code, len(rows))
	}

	// latency window
	code, _, rows = getJSON(t, r, "/api/logs?min_latency_ms=100&max_latency_ms=500")
	if code != 200 || len(rows) != 1 {
		t.Fatalf("latency window: code=%d rows=%d, want 200/1", code, len(rows))
	}

	// has_error
	code, _, rows = getJSON(t, r, "/api/logs?has_error=true")
	if code != 200 || len(rows) != 1 {
		t.Fatalf("has_error: code=%d rows=%d, want 200/1", code, len(rows))
	}

	// search over error text (default search includes error column)
	code, _, rows = getJSON(t, r, "/api/logs?q=exploded")
	if code != 200 || len(rows) != 1 {
		t.Fatalf("q=exploded: code=%d rows=%d, want 200/1", code, len(rows))
	}

	// search over model
	code, _, rows = getJSON(t, r, "/api/logs?q=gpt-big")
	if code != 200 || len(rows) != 1 {
		t.Fatalf("q=gpt-big: code=%d rows=%d, want 200/1", code, len(rows))
	}

	// search over request bodies requires search_bodies=true
	code, _, rows = getJSON(t, r, "/api/logs?q=findme-needle")
	if code != 200 || len(rows) != 0 {
		t.Fatalf("q over bodies without opt-in: code=%d rows=%d, want 200/0", code, len(rows))
	}
	code, _, rows = getJSON(t, r, "/api/logs?q=findme-needle&search_bodies=true")
	if code != 200 || len(rows) != 3 {
		t.Fatalf("q over bodies with opt-in: code=%d rows=%d, want 200/3", code, len(rows))
	}

	// combined filters: stream=false AND failed
	code, _, rows = getJSON(t, r, "/api/logs?stream=false&status=failed")
	if code != 200 || len(rows) != 1 {
		t.Fatalf("combined: code=%d rows=%d, want 200/1", code, len(rows))
	}

	// X-Total-Count still reflects filters
	req := adminReq("/api/logs?stream=true")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Header().Get("X-Total-Count") != "1" {
		t.Fatalf("X-Total-Count = %q, want 1", w.Header().Get("X-Total-Count"))
	}
}

func TestLogGroup(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	ks := apikey.NewStore(h.DB)
	k, _ := ks.Create("group-key")
	now := time.Now().UTC()
	seedLogRows(t, h, []logSeed{
		{model: "gpt-fast", endpoint: "chat", status: 200, latency: 100, tokens: 10, cost: 0.01, keyPrefix: k.Prefix, keyID: k.ID, finish: "stop", providerID: "prov-group", at: now.Add(-time.Hour)},
		{model: "gpt-fast", endpoint: "chat", status: 200, latency: 300, tokens: 20, cost: 0.02, keyPrefix: k.Prefix, keyID: k.ID, finish: "stop", providerID: "prov-group", at: now.Add(-2 * time.Hour)},
		{model: "gpt-big", endpoint: "chat", status: 502, latency: 900, tokens: 5, cost: 0.00, keyPrefix: k.Prefix, keyID: k.ID, finish: "error", providerID: "prov-group", at: now.Add(-3 * time.Hour)},
	})

	r := newLogsRouter(h)

	// group by model
	code, _, rows := getJSON(t, r, "/api/logs/group?group_by=model&range=24h")
	if code != 200 {
		t.Fatalf("group: code=%d", code)
	}
	if len(rows) != 2 {
		t.Fatalf("group by model rows=%d, want 2", len(rows))
	}
	// ordered by requests DESC: gpt-fast (2) first
	first := rows[0].(map[string]any)
	if first["group"] != "gpt-fast" {
		t.Fatalf("first group = %v, want gpt-fast", first["group"])
	}
	if got, _ := first["requests"].(float64); got != 2 {
		t.Fatalf("gpt-fast requests = %v, want 2", first["requests"])
	}
	if got, _ := first["tokens"].(float64); got != 30 {
		t.Fatalf("gpt-fast tokens = %v, want 30", first["tokens"])
	}
	// percentiles over [100,300]: p50 = 100 (ceil-index convention), p95 = 300
	if got, _ := first["p50_latency_ms"].(float64); got != 100 {
		t.Fatalf("p50 = %v, want 100", first["p50_latency_ms"])
	}
	// avg latency 200
	if got, _ := first["avg_latency_ms"].(float64); got != 200 {
		t.Fatalf("avg = %v, want 200", first["avg_latency_ms"])
	}

	// group by status
	_, _, rows = getJSON(t, r, "/api/logs/group?group_by=status&range=24h")
	if len(rows) != 2 {
		t.Fatalf("group by status rows=%d, want 2", len(rows))
	}

	// group by key with name enrichment
	_, _, rows2 := getJSON(t, r, "/api/logs/group?group_by=key&range=24h")
	if len(rows2) != 1 {
		t.Fatalf("group by key rows=%d, want 1", len(rows2))
	}
	fr := rows2[0].(map[string]any)
	if fr["name"] != "group-key" {
		t.Fatalf("key name enrichment = %v, want group-key", fr["name"])
	}

	// unknown group_by → 400
	code, _, _ = getJSON(t, r, "/api/logs/group?group_by=bogus")
	if code != 400 {
		t.Fatalf("unknown group_by: code=%d, want 400", code)
	}

	// order=cost
	_, _, rows = getJSON(t, r, "/api/logs/group?group_by=model&order=cost&range=24h")
	if len(rows) != 2 {
		t.Fatal("order=cost rows")
	}
	if rows[0].(map[string]any)["group"] != "gpt-fast" {
		t.Fatalf("order=cost first = %v", rows[0].(map[string]any)["group"])
	}
}

func TestLogsExportCSV(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	ks := apikey.NewStore(h.DB)
	k, _ := ks.Create("export-key")
	now := time.Now().UTC()
	seedLogRows(t, h, []logSeed{
		{model: "csv-a", endpoint: "chat", status: 200, latency: 100, tokens: 10, cost: 0.01, keyPrefix: k.Prefix, keyID: k.ID, finish: "stop", providerID: "prov-export", at: now.Add(-time.Hour)},
		{model: "csv-b", endpoint: "chat", status: 200, latency: 200, tokens: 20, cost: 0.02, keyPrefix: k.Prefix, keyID: k.ID, finish: "length", providerID: "prov-export", at: now.Add(-2 * time.Hour)},
		{model: "csv-c", endpoint: "chat", status: 502, latency: 300, tokens: 0, cost: 0, errMsg: "boom, with comma", keyPrefix: k.Prefix, keyID: k.ID, providerID: "prov-export", at: now.Add(-3 * time.Hour)},
	})

	r := newLogsRouter(h)

	// unfiltered export: header + 3 rows
	req := adminReq("/api/logs/export")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("export code=%d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content-type = %q", ct)
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("export lines=%d, want 4 (header+3): %v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "id,key_prefix,key_id,provider_id,model,endpoint,status") {
		t.Fatalf("csv header = %q", lines[0])
	}
	// RFC 4180: the comma-containing error must be quoted
	if !strings.Contains(w.Body.String(), `"boom, with comma"`) {
		t.Fatal("error text with comma not quoted")
	}
	// finish_reason column populated
	if !strings.Contains(w.Body.String(), ",stop,") || !strings.Contains(w.Body.String(), ",length,") {
		t.Fatal("finish_reason missing from export")
	}

	// filtered export: only the 502 row
	req = adminReq("/api/logs/export?status=failed")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	lines = strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("filtered export lines=%d, want 2", len(lines))
	}
	if !strings.Contains(lines[1], "csv-c") {
		t.Fatalf("filtered export row = %q", lines[1])
	}
}
