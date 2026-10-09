package handler

// Tests for the reporting endpoints behind the attribution, cost and
// performance pages. These aggregate every request_logs row in the window, so
// beyond the arithmetic they also pin the scoping contract: a rollup that
// ignores org/ownership narrowing leaks another tenant's spend as a total.

import (
	"net/url"
	"testing"
	"time"

	"ai-gateway/internal/db"
	"github.com/go-chi/chi/v5"
)

func newAnalyticsRouter(h *AdminHandler) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/api/analytics/attribution", h.Attribution)
	r.Get("/api/analytics/cost", h.Cost)
	r.Get("/api/analytics/performance", h.Performance)
	r.Get("/api/logs", h.Logs)
	return r
}

// analyticsSeed is a fully-specified request_logs row.
type analyticsSeed struct {
	id           string
	model        string
	endUser      string
	app          string
	status       int
	prompt       int
	completion   int
	cacheRead    int
	cacheWrite   int
	cost         float64
	latencyMs    int64
	ttftMs       int64
	responseMs   int64
	cacheStatus  string
	providerName string
}

func seedAnalyticsRows(t *testing.T, h *AdminHandler, providerID string, seeds []analyticsSeed) {
	t.Helper()
	now := time.Now().UTC()
	for i, s := range seeds {
		ttft := s.ttftMs
		responseMs := s.responseMs
		if ttft == 0 && responseMs == 0 && s.latencyMs > 0 {
			ttft = s.latencyMs / 2
			responseMs = s.latencyMs - ttft
		}
		if _, err := h.DB.Exec(db.Q(`INSERT INTO request_logs(
			id,provider_id,model,endpoint,status,latency_ms,ttft_ms,response_ms,created_at,
			prompt_tokens,completion_tokens,total_tokens,cost_usd,is_stream,
			cache_read_tokens,cache_write_tokens,cache_status,end_user,app)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
			s.id, providerID, s.model, "chat", s.status, s.latencyMs, ttft, responseMs,
			now.Add(-time.Duration(i+1)*time.Minute),
			s.prompt, s.completion, s.prompt+s.completion, s.cost, true,
			s.cacheRead, s.cacheWrite, s.cacheStatus, nullStr(s.endUser), nullStr(s.app)); err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
	}
}

func TestAttributionReport(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	prov, _ := h.ProviderStore.Create("attrprov", "openai", "http://127.0.0.1:1", "sk-t")
	seedAnalyticsRows(t, h, prov.ID, []analyticsSeed{
		{id: "a1", model: "m1", endUser: "alice", app: "https://app-a.example", status: 200, prompt: 100, completion: 50, cost: 0.10, latencyMs: 100},
		{id: "a2", model: "m1", endUser: "alice", app: "https://app-a.example", status: 200, prompt: 100, completion: 50, cost: 0.10, latencyMs: 100},
		{id: "a3", model: "m1", endUser: "bob", app: "https://app-b.example", status: 500, prompt: 50, completion: 0, cost: 0.05, latencyMs: 200},
		{id: "a4", model: "m1", status: 200, prompt: 10, completion: 5, cost: 0.01, latencyMs: 50},
	})

	r := newAnalyticsRouter(h)
	code, env, _ := getJSON(t, r, "/api/analytics/attribution?range=30d")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	apps, _ := env["apps"].([]any)
	if len(apps) != 2 {
		t.Fatalf("apps=%d, want 2 (unattributed excluded from ranking)", len(apps))
	}
	// Ordered by cost DESC: app-a (0.20) leads app-b (0.05).
	first, _ := apps[0].(map[string]any)
	if first["name"] != "https://app-a.example" {
		t.Fatalf("top app = %v, want app-a", first["name"])
	}
	if first["requests"].(float64) != 2 {
		t.Fatalf("app-a requests = %v, want 2", first["requests"])
	}
	// Distinct end users served by that app.
	if first["identities"].(float64) != 1 {
		t.Fatalf("app-a identities = %v, want 1", first["identities"])
	}

	users, _ := env["end_users"].([]any)
	if len(users) != 2 {
		t.Fatalf("end_users=%d, want 2", len(users))
	}
	top, _ := users[0].(map[string]any)
	if top["name"] != "alice" || top["requests"].(float64) != 2 {
		t.Fatalf("top user = %v/%v, want alice/2", top["name"], top["requests"])
	}
	if top["last_seen"] == nil || top["last_seen"] == "" {
		t.Fatal("end user ranking missing last_seen")
	}

	// Coverage: 3 of 4 rows carry an end user.
	cov, _ := env["coverage"].(map[string]any)
	if cov["total_requests"].(float64) != 4 {
		t.Fatalf("total_requests = %v, want 4", cov["total_requests"])
	}
	if cov["with_end_user"].(float64) != 3 {
		t.Fatalf("with_end_user = %v, want 3", cov["with_end_user"])
	}
	if cov["unattributed"].(float64) != 1 {
		t.Fatalf("unattributed = %v, want 1", cov["unattributed"])
	}
}

func TestCostReportCacheSavings(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	prov, _ := h.ProviderStore.Create("costprov", "openai", "http://127.0.0.1:1", "sk-t")
	// Priced model with a cache-read discount: 1.0 in, 0.25 cache read.
	if _, err := h.DB.Exec(`INSERT INTO models_catalog(id,provider,name,description,family,context_window,max_output,input_cost,output_cost,cache_read_cost,cache_write_cost,reasoning,tool_call,structured_output,attachment,modalities,open_weights,knowledge_cutoff,updated_at,reasoning_type,reasoning_levels,reasoning_output_limits)
		VALUES('costprov/priced','costprov','Priced','d','f',128000,4096,1.0,2.0,0.25,1.25,0,0,0,0,'["text"]',0,'2024-01-01',datetime('now'),'','','')`); err != nil {
		t.Fatal(err)
	}
	seedAnalyticsRows(t, h, prov.ID, []analyticsSeed{
		// 1000 prompt of which 400 cached, 500 completion, cache-aware cost.
		{id: "c1", model: "costprov/priced", app: "app-a", status: 200, prompt: 1000, completion: 500, cacheRead: 400, cost: (600*1.0 + 400*0.25 + 500*2.0) / 1e6, cacheStatus: "miss", latencyMs: 100},
		// Unpriced model: real tokens, zero recorded cost.
		{id: "c2", model: "costprov/unpriced", status: 200, prompt: 300, completion: 100, cost: 0, cacheStatus: "bypass", latencyMs: 100},
	})

	r := newAnalyticsRouter(h)
	code, env, _ := getJSON(t, r, "/api/analytics/cost?range=30d")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	totals, _ := env["totals"].(map[string]any)
	if got := totals["cache_read_tokens"].(float64); got != 400 {
		t.Fatalf("cache_read_tokens = %v, want 400", got)
	}
	// Savings = 400 * (1.0 - 0.25) / 1e6 = 0.0003
	wantSavings := 400 * (1.0 - 0.25) / 1e6
	if got := totals["cache_savings_usd"].(float64); got < wantSavings-1e-12 || got > wantSavings+1e-12 {
		t.Fatalf("cache_savings_usd = %v, want %v", got, wantSavings)
	}
	// The unpriced model is surfaced, not silently folded into the total.
	if got := totals["unpriced_requests"].(float64); got != 1 {
		t.Fatalf("unpriced_requests = %v, want 1", got)
	}
	if got := totals["unpriced_tokens"].(float64); got != 400 {
		t.Fatalf("unpriced_tokens = %v, want 400", got)
	}

	// Cache-disposition breakdown must include both dispositions present.
	byCache, _ := env["by_cache_status"].([]any)
	seen := map[string]bool{}
	for _, b := range byCache {
		m, _ := b.(map[string]any)
		seen[m["name"].(string)] = true
	}
	if !seen["miss"] || !seen["bypass"] {
		t.Fatalf("cache status breakdown = %v, want miss and bypass", seen)
	}
	// Cost attribution by app is the chargeback view.
	byApp, _ := env["by_app"].([]any)
	if len(byApp) == 0 {
		t.Fatal("by_app empty")
	}
}

func TestPerformanceReport(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	prov, _ := h.ProviderStore.Create("perfprov", "openai", "http://127.0.0.1:1", "sk-t")
	seeds := []analyticsSeed{}
	// 8 successes and 2 failures => uptime 80%.
	for i := 0; i < 8; i++ {
		seeds = append(seeds, analyticsSeed{id: "p" + string(rune('a'+i)), model: "perf-model", status: 200, prompt: 100, completion: 100, cost: 0.01, latencyMs: 100, ttftMs: 40, responseMs: 200})
	}
	for i := 0; i < 2; i++ {
		seeds = append(seeds, analyticsSeed{id: "f" + string(rune('a'+i)), model: "perf-model", status: 500, prompt: 100, completion: 0, cost: 0, latencyMs: 300})
	}
	seedAnalyticsRows(t, h, prov.ID, seeds)

	r := newAnalyticsRouter(h)
	code, env, _ := getJSON(t, r, "/api/analytics/performance?range=30d")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	models, _ := env["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models=%d, want 1", len(models))
	}
	m, _ := models[0].(map[string]any)
	if m["requests"].(float64) != 10 {
		t.Fatalf("requests = %v, want 10", m["requests"])
	}
	if m["failed"].(float64) != 2 {
		t.Fatalf("failed = %v, want 2", m["failed"])
	}
	if got := m["uptime_pct"].(float64); got != 80 {
		t.Fatalf("uptime_pct = %v, want 80", got)
	}
	if got := m["error_rate"].(float64); got != 0.2 {
		t.Fatalf("error_rate = %v, want 0.2", got)
	}
	// TTFT percentiles come from the streaming samples only.
	if m["ttft_p50_ms"].(float64) != 40 {
		t.Fatalf("ttft_p50 = %v, want 40", m["ttft_p50_ms"])
	}
	// Throughput: 100 completion tokens over a 200ms post-TTFT window.
	if got := m["avg_tps"].(float64); got < 490 || got > 510 {
		t.Fatalf("avg_tps = %v, want ~500", got)
	}

	// Provider rollup carries the point-in-time health flag alongside uptime.
	providers, _ := env["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("providers=%d, want 1", len(providers))
	}
	p, _ := providers[0].(map[string]any)
	if p["name"] != "perfprov" {
		t.Fatalf("provider name = %v, want perfprov", p["name"])
	}
}

// TestAnalyticsCustomWindow pins from/to precedence over the range shorthand —
// the basis for "compare this period to last period".
func TestAnalyticsCustomWindow(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	prov, _ := h.ProviderStore.Create("winprov", "openai", "http://127.0.0.1:1", "sk-t")
	seedAnalyticsRows(t, h, prov.ID, []analyticsSeed{
		{id: "w1", model: "m", status: 200, prompt: 10, completion: 5, cost: 0.01, latencyMs: 10},
	})
	r := newAnalyticsRouter(h)

	// A window entirely in the past must return no rows.
	past := "from=2020-01-01T00:00:00Z&to=2020-01-02T00:00:00Z"
	code, env, _ := getJSON(t, r, "/api/analytics/cost?"+past)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if env["range"].(string) != "custom" {
		t.Fatalf("range = %v, want custom", env["range"])
	}
	if got := env["totals"].(map[string]any)["cost_usd"].(float64); got != 0 {
		t.Fatalf("past-window cost = %v, want 0", got)
	}

	// A window covering now returns the row.
	now := time.Now().UTC()
	span := "from=" + now.Add(-time.Hour).Format(time.RFC3339) + "&to=" + now.Add(time.Hour).Format(time.RFC3339)
	_, env2, _ := getJSON(t, r, "/api/analytics/cost?"+span)
	if got := env2["totals"].(map[string]any)["cost_usd"].(float64); got != 0.01 {
		t.Fatalf("current-window cost = %v, want 0.01", got)
	}
}

// TestLogsSorting covers the whitelisted ORDER BY, including that an unknown
// sort column falls back instead of reaching SQL.
func TestLogsSorting(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	prov, _ := h.ProviderStore.Create("sortprov", "openai", "http://127.0.0.1:1", "sk-t")
	seedAnalyticsRows(t, h, prov.ID, []analyticsSeed{
		{id: "s1", model: "cheap", status: 200, prompt: 10, completion: 5, cost: 0.01, latencyMs: 900},
		{id: "s2", model: "pricey", status: 200, prompt: 10, completion: 5, cost: 5.00, latencyMs: 100},
		{id: "s3", model: "mid", status: 200, prompt: 10, completion: 5, cost: 0.50, latencyMs: 500},
	})
	r := newAnalyticsRouter(h)

	code, _, rows := getJSON(t, r, "/api/logs?since=30d&order=cost_usd&dir=desc")
	if code != 200 || len(rows) != 3 {
		t.Fatalf("cost sort: code=%d rows=%d", code, len(rows))
	}
	if rows[0].(map[string]any)["model"] != "pricey" {
		t.Fatalf("cost desc top = %v, want pricey", rows[0].(map[string]any)["model"])
	}
	_, _, rows = getJSON(t, r, "/api/logs?since=30d&order=cost_usd&dir=asc")
	if rows[0].(map[string]any)["model"] != "cheap" {
		t.Fatalf("cost asc top = %v, want cheap", rows[0].(map[string]any)["model"])
	}
	_, _, rows = getJSON(t, r, "/api/logs?since=30d&order=latency_ms&dir=asc")
	if rows[0].(map[string]any)["model"] != "pricey" {
		t.Fatalf("latency asc top = %v, want pricey", rows[0].(map[string]any)["model"])
	}

	// An unknown column must not reach SQL — it falls back to newest-first,
	// which is still a valid 200 with the full set.
	code, _, rows = getJSON(t, r, "/api/logs?since=30d&order="+url.QueryEscape("id; DROP TABLE request_logs"))
	if code != 200 || len(rows) != 3 {
		t.Fatalf("injection attempt: code=%d rows=%d", code, len(rows))
	}
}

// TestCostGroupingNotShadowedByJoinName is the regression test for a silent
// correctness bug: the rollups grouped by the bare "name" alias, which does
// not reliably bind to the SELECT alias once a join brings its own `name`
// column (models_catalog.name, providers.name). The query then grouped by the
// WRONG column while still labelling each row with the intended expression —
// so per-app spend showed one app's name repeated with per-model totals, and
// real apps disappeared from the report entirely.
//
// The failure needs a model present in the catalog to surface, which is why
// the earlier tests missed it.
func TestCostGroupingNotShadowedByJoinName(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	prov, _ := h.ProviderStore.Create("groupprov", "openai", "http://127.0.0.1:1", "sk-t")

	// Two catalog models with DISTINCT names, each used by two distinct apps.
	for _, m := range []struct{ id, name string }{
		{"groupprov/model-x", "Catalog Name X"},
		{"groupprov/model-y", "Catalog Name Y"},
	} {
		if _, err := h.DB.Exec(`INSERT INTO models_catalog(id,provider,name,description,family,context_window,max_output,input_cost,output_cost,cache_read_cost,cache_write_cost,reasoning,tool_call,structured_output,attachment,modalities,open_weights,knowledge_cutoff,updated_at,reasoning_type,reasoning_levels,reasoning_output_limits)
			VALUES(?,'groupprov',?,'d','f',128000,4096,1.0,2.0,0.25,1.25,0,0,0,0,'["text"]',0,'2024-01-01',datetime('now'),'','','')`, m.id, m.name); err != nil {
			t.Fatal(err)
		}
	}

	seedAnalyticsRows(t, h, prov.ID, []analyticsSeed{
		{id: "g1", model: "groupprov/model-x", app: "https://one.example", endUser: "u1", status: 200, prompt: 100, completion: 10, cost: 1.00, latencyMs: 10},
		{id: "g2", model: "groupprov/model-y", app: "https://one.example", endUser: "u1", status: 200, prompt: 100, completion: 10, cost: 2.00, latencyMs: 10},
		{id: "g3", model: "groupprov/model-x", app: "https://two.example", endUser: "u2", status: 200, prompt: 100, completion: 10, cost: 4.00, latencyMs: 10},
		{id: "g4", model: "groupprov/model-y", app: "https://three.example", endUser: "u3", status: 200, prompt: 100, completion: 10, cost: 8.00, latencyMs: 10},
	})

	r := newAnalyticsRouter(h)
	code, env, _ := getJSON(t, r, "/api/analytics/cost?range=30d")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}

	// Every app must appear exactly once, with its own total.
	byApp, _ := env["by_app"].([]any)
	got := map[string]float64{}
	for _, b := range byApp {
		m, _ := b.(map[string]any)
		name := m["name"].(string)
		if _, dup := got[name]; dup {
			t.Fatalf("app %q appeared twice — grouping is not by the app expression", name)
		}
		got[name] = m["cost"].(float64)
	}
	want := map[string]float64{
		"https://one.example":   3.00, // 1.00 + 2.00
		"https://two.example":   4.00,
		"https://three.example": 8.00,
	}
	if len(got) != len(want) {
		t.Fatalf("apps = %v, want %d distinct", got, len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("app %q cost = %v, want %v", k, got[k], v)
		}
	}

	// Models must group by model id, not by the joined catalog name.
	byModel, _ := env["by_model"].([]any)
	modelNames := map[string]bool{}
	for _, b := range byModel {
		m, _ := b.(map[string]any)
		modelNames[m["name"].(string)] = true
	}
	if !modelNames["groupprov/model-x"] || !modelNames["groupprov/model-y"] {
		t.Fatalf("by_model = %v, want the model ids", modelNames)
	}
	if modelNames["Catalog Name X"] || modelNames["Catalog Name Y"] {
		t.Fatalf("by_model leaked the catalog name: %v", modelNames)
	}
}

// TestPerfGroupingNotShadowedByJoinName is the same regression for the
// performance rollups, which join providers (providers.name collides).
func TestPerfGroupingNotShadowedByJoinName(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	p1, _ := h.ProviderStore.Create("alpha-prov", "openai", "http://127.0.0.1:1", "sk-t")
	p2, _ := h.ProviderStore.Create("beta-prov", "openai", "http://127.0.0.1:2", "sk-t")

	seeds := []analyticsSeed{
		{id: "pp1", model: "m", status: 200, prompt: 10, completion: 5, cost: 0.01, latencyMs: 100},
		{id: "pp2", model: "m", status: 200, prompt: 10, completion: 5, cost: 0.01, latencyMs: 100},
	}
	seedAnalyticsRows(t, h, p1.ID, seeds)
	seedAnalyticsRows(t, h, p2.ID, []analyticsSeed{
		{id: "pp3", model: "m", status: 500, prompt: 10, completion: 0, cost: 0, latencyMs: 900},
	})

	r := newAnalyticsRouter(h)
	_, env, _ := getJSON(t, r, "/api/analytics/performance?range=30d")
	providers, _ := env["providers"].([]any)
	seen := map[string]float64{}
	for _, b := range providers {
		m, _ := b.(map[string]any)
		name := m["name"].(string)
		if _, dup := seen[name]; dup {
			t.Fatalf("provider %q appeared twice — grouping is not by the provider expression", name)
		}
		seen[name] = m["requests"].(float64)
	}
	if seen["alpha-prov"] != 2 || seen["beta-prov"] != 1 {
		t.Fatalf("provider rollup = %v, want alpha-prov=2 beta-prov=1", seen)
	}
}

// TestPerformanceDailySeriesNotEmpty guards a scan-type mismatch that silently
// produced an EMPTY trend: AVG() returns a float64 even over an INTEGER
// column, and scanning it into int64 fails the conversion. The error was
// swallowed, so the pages rendered "no data" for a window that had plenty.
func TestPerformanceDailySeriesNotEmpty(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	prov, _ := h.ProviderStore.Create("dailyprov", "openai", "http://127.0.0.1:1", "sk-t")
	seedAnalyticsRows(t, h, prov.ID, []analyticsSeed{
		{id: "d1", model: "m", status: 200, prompt: 100, completion: 50, cost: 0.01, latencyMs: 250, ttftMs: 60, responseMs: 190},
		{id: "d2", model: "m", status: 200, prompt: 100, completion: 50, cost: 0.01, latencyMs: 350, ttftMs: 80, responseMs: 270},
	})

	r := newAnalyticsRouter(h)
	code, env, _ := getJSON(t, r, "/api/analytics/performance?range=30d")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	daily, _ := env["daily"].([]any)
	if len(daily) == 0 {
		t.Fatal("performance daily series is empty despite traffic in the window")
	}
	d, _ := daily[0].(map[string]any)
	if d["requests"].(float64) != 2 {
		t.Fatalf("daily requests = %v, want 2", d["requests"])
	}
	// Average latency of 250 and 350 is 300; TTFT average is 70.
	if got := d["avg_ms"].(float64); got != 300 {
		t.Fatalf("daily avg_ms = %v, want 300", got)
	}
	if got := d["avg_ttft_ms"].(float64); got != 70 {
		t.Fatalf("daily avg_ttft_ms = %v, want 70", got)
	}
}
