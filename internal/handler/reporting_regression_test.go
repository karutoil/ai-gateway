package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
)

// Reporting charts must contain old traffic in a multi-day window and must
// not collapse a full week into one hourly bucket or return an empty series.
func TestReportingTrendsIncludeMultiDayTraffic(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	provider, err := h.ProviderStore.Create("reporting-regression", "openai", "http://127.0.0.1:1", "sk-t")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedLogRows(t, h, []logSeed{
		{model: "report-model", endpoint: "chat", status: 200, latency: 150, tokens: 15, cost: .12, stream: true, providerID: provider.ID, at: now.Add(-3 * time.Hour)},
		{model: "report-model", endpoint: "chat", status: 502, latency: 330, tokens: 10, cost: .04, stream: true, providerID: provider.ID, at: now.Add(-72 * time.Hour)},
		{model: "report-model", endpoint: "chat", status: 200, latency: 100, tokens: 99, cost: 9, stream: true, providerID: provider.ID, at: now.Add(-40 * 24 * time.Hour)},
	})
	for _, endpoint := range []string{"cost", "performance"} {
		t.Run(endpoint, func(t *testing.T) {
			code, report, _ := getJSON(t, newAnalyticsRouter(h), "/api/analytics/"+endpoint+"?range=7d")
			if code != 200 {
				t.Fatalf("status=%d", code)
			}
			points, ok := report["daily"].([]any)
			if !ok || len(points) != 2 {
				t.Fatalf("daily buckets=%v, want two distinct days", report["daily"])
			}
			var requests float64
			for _, point := range points {
				p := point.(map[string]any)
				if len(p["day"].(string)) != 10 {
					t.Fatalf("multi-day bucket not a date: %v", p["day"])
				}
				requests += p["requests"].(float64)
			}
			if requests != 2 {
				t.Fatalf("chart requests=%v, want 2", requests)
			}
			if endpoint == "cost" {
				totals := report["totals"].(map[string]any)
				if totals["cost_usd"].(float64) < .159 || totals["cost_usd"].(float64) > .161 {
					t.Fatalf("window cost=%v, want .16", totals["cost_usd"])
				}
			} else {
				if points[0].(map[string]any)["avg_ms"].(float64) != 330 {
					t.Fatalf("older latency omitted: %v", points[0])
				}
			}
		})
	}
}

func TestLogReportShorthandWindows(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	var providerID string
	if err := h.DB.QueryRow(db.Q("SELECT id FROM providers LIMIT 1")).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	seedLogRows(t, h, []logSeed{
		{model: "older-report-log", endpoint: "chat", status: 200, latency: 150, tokens: 10, cost: .01, providerID: providerID, at: time.Now().UTC().Add(-48 * time.Hour)},
	})
	for _, window := range []string{"24h", "7d", "30d", "90d", "365d"} {
		code, _, rows := getJSON(t, newLogsRouter(h), "/api/logs?since="+window)
		if code != 200 {
			t.Fatalf("since=%s status=%d", window, code)
		}
		want := 1
		if window == "24h" {
			want = 0
		}
		if len(rows) != want {
			t.Fatalf("since=%s rows=%d want=%d", window, len(rows), want)
		}
	}
}

func TestStatsErrorsRespectOrgScopeAndTopKeysByTokens(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	if _, err := h.DB.Exec(db.Q("INSERT INTO organizations(id,name,created_at) VALUES(?,?,?)"), "org-report", "Reporting org", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(db.Q("INSERT INTO organizations(id,name,created_at) VALUES(?,?,?)"), "org-other", "Other org", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	own, err := h.ProviderStore.Create("stats-org-owned", "openai", "http://127.0.0.1:1", "sk-t")
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.ProviderStore.Create("stats-org-other", "openai", "http://127.0.0.1:1", "sk-t")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ id, org string }{{own.ID, "org-report"}, {other.ID, "org-other"}} {
		if _, err := h.DB.Exec(db.Q("UPDATE providers SET org_id=? WHERE id=?"), p.org, p.id); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	seedLogRows(t, h, []logSeed{
		{model: "own", endpoint: "chat", status: 502, errMsg: "own failure", tokens: 200, keyPrefix: "high-tokens", providerID: own.ID, at: now.Add(-2 * time.Hour)},
		{model: "own", endpoint: "chat", status: 200, tokens: 2, keyPrefix: "many-requests", providerID: own.ID, at: now.Add(-3 * time.Hour)},
		{model: "own", endpoint: "chat", status: 200, tokens: 2, keyPrefix: "many-requests", providerID: own.ID, at: now.Add(-4 * time.Hour)},
		{model: "other", endpoint: "chat", status: 502, errMsg: "secret failure", tokens: 900, keyPrefix: "other", providerID: other.ID, at: now.Add(-time.Hour)},
	})
	// Apply a signed org claim through the real authentication middleware.
	token, err := auth.MakeTokenWithOrg(h.Config.JWTSecret, "operator", "org-report", "member")
	if err != nil {
		t.Fatal(err)
	}
	req := adminReq("/api/stats?range=7d")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	auth.AdminMiddleware(h.Config.JWTSecret)(http.HandlerFunc(h.Stats)).ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("stats status=%d body=%s", w.Code, w.Body.String())
	}
	var report map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report["range_requests"].(float64) != 3 {
		t.Fatalf("org traffic=%v, want 3", report["range_requests"])
	}
	errors := report["errors"].([]any)
	if len(errors) != 1 {
		t.Fatalf("org error rows=%v, want 1", errors)
	}
	row := errors[0].(map[string]any)
	if row["count"].(float64) != 1 || row["sample"].(string) != "own failure" {
		t.Fatalf("error row leaked/missed: %v", row)
	}
	keys := report["top_keys"].([]any)
	if len(keys) != 2 || keys[0].(map[string]any)["key_prefix"] != "high-tokens" {
		t.Fatalf("top keys should rank by tokens, got %v", keys)
	}
}
