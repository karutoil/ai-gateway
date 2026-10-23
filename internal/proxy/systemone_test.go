package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/discovery"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

// setupSystemOneServer wires a proxy handler with one systemone provider
// pointing at a mock upstream that only answers POST <base>/v1/systemone, so a
// 200 proves both routing and path construction.
func setupSystemOneServer(t *testing.T) (*httptest.Server, *Handler, string) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i)
	}
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/systemone") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var b map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model": b["model"],
			"answers": map[string]interface{}{
				"urgent": map[string]interface{}{"type": "noul", "noul": 0.91},
				"team": map[string]interface{}{
					"type": "choice", "choice": "billing",
					"probabilities": map[string]float64{"billing": 0.7, "technical": 0.3},
					"confidence":    0.4,
				},
			},
			"usage": map[string]int{"input_tokens": 128, "output_tokens": 0},
		})
	}))
	t.Cleanup(upstream.Close)

	if _, err := ps.Create("kev-local", models.ProviderSystemOne, upstream.URL+"/v1", ""); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("test-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/systemone", h.SystemOne)
	r.Post("/v1/chat/completions", h.ChatCompletions)
	r.Get("/v1/models", h.Models)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, h, k.Key
}

const systemOneBody = `{
	"model": "kev-4b",
	"state": "Shoes arrived late and in the wrong size.",
	"questions": {
		"urgent": {"type": "noul", "instructions": "Is this urgent?"},
		"team": {"type": "choice", "instructions": "Which team?", "criteria": {"billing": "payments", "technical": "outages"}}
	}
}`

func TestSystemOneProxiesDecisionRequest(t *testing.T) {
	srv, _, key := setupSystemOneServer(t)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/systemone", strings.NewReader(systemOneBody))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"noul":0.91`) || !strings.Contains(string(body), `"choice":"billing"`) {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestSystemOneQualifiedModelStripsProviderPrefix(t *testing.T) {
	// The dashboard Playground sends the provider-pinned form
	// ("provider/model"); the upstream must receive the bare selector.
	srv, _, key := setupSystemOneServer(t)
	body := `{"model":"kev-local/kev-4b","state":"x","questions":{"u":{"type":"noul","instructions":"Is it urgent?"}}}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/systemone", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	respBody, _ := io.ReadAll(resp.Body)
	// The mock echoes the model it received upstream.
	if !strings.Contains(string(respBody), `"model":"kev-4b"`) {
		t.Fatalf("expected upstream model kev-4b, got: %s", respBody)
	}
}

func TestSystemOneRejectsMalformedBody(t *testing.T) {
	srv, _, key := setupSystemOneServer(t)
	// choice with a single option is invalid per the System One schema.
	bad := `{"model":"kev-4b","state":"x","questions":{"t":{"type":"choice","instructions":"q","criteria":{"only":"one"}}}}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/systemone", strings.NewReader(bad))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "at least 2 options") {
		t.Fatalf("unexpected error body: %s", body)
	}
}

func TestChatEndpointDoesNotRouteToDecisionProvider(t *testing.T) {
	srv, _, key := setupSystemOneServer(t)
	body := `{"model":"kev-4b","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatalf("chat request must not be served by a decision provider")
	}
}

func TestDecisionModelsAreListed(t *testing.T) {
	srv, h, key := setupSystemOneServer(t)
	// Populate provider_models via discovery (upstream 404s /models, so the
	// curated registry is seeded).
	providers, _ := h.ProviderStore.List()
	if len(providers) == 0 {
		t.Fatal("no providers")
	}
	svc := discovery.New(h.DB, h.ProviderStore, h.CatalogStore)
	if _, err := svc.Discover(providers[0].ID); err != nil {
		t.Fatalf("discover: %v", err)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"decision":true`) {
		t.Fatalf("expected a decision model in listing: %s", body)
	}
	if !strings.Contains(string(body), `"kev-4b"`) {
		t.Fatalf("expected kev-4b in listing: %s", body)
	}
}

func TestDecisionTargets(t *testing.T) {
	// The llama.cpp spelling (/v1/systemone) for typed systemone providers.
	for _, c := range []struct{ base, path, want string }{
		{"https://api.typesafe.ai", "/v1/systemone", "https://api.typesafe.ai/v1/systemone"},
		{"http://localhost:8009/v1/", "/v1/systemone", "http://localhost:8009/v1/systemone"},
		{"http://localhost:8009/v1/systemone", "/v1/systemone", "http://localhost:8009/v1/systemone"},
		// The Jev spelling (/v1/decisions) for resellers that mirror it.
		{"https://api.surplusintelligence.ai/v1", "/v1/decisions", "https://api.surplusintelligence.ai/v1/decisions"},
		{"https://api.surplusintelligence.ai", "/v1/decisions", "https://api.surplusintelligence.ai/v1/decisions"},
		// An operator override may be a bare path or a whole URL.
		{"https://api.example/v1", "decide", "https://api.example/v1/decide"},
		{"https://api.example/v1", "https://other.example/v2/decide", "https://other.example/v2/decide"},
	} {
		if got := appendEndpointPath(c.base, c.path); got != c.want {
			t.Errorf("appendEndpointPath(%q, %q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
	if got := cloudflareDecisionTarget("https://api.cloudflare.com/client/v4/accounts/abc/ai/run", "clef-flash"); got != "https://api.cloudflare.com/client/v4/accounts/abc/ai/run/@cf/cloudflare/clef-flash" {
		t.Errorf("cloudflare target = %q", got)
	}
	if got := cloudflareDecisionTarget("https://api.cloudflare.com/client/v4/accounts/abc", "clef"); got != "https://api.cloudflare.com/client/v4/accounts/abc/@cf/cloudflare/clef" {
		t.Errorf("cloudflare base-only target = %q", got)
	}
}

// TestDecisionPathPerProviderDialect pins the endpoint each provider dialect
// gets: llama.cpp/typed-systemone keeps /v1/systemone, a generic decision
// reseller gets the Jev dialect's /v1/decisions, and the system_config override
// replaces the path on the provider's origin.
func TestDecisionPathPerProviderDialect(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	h := hForDB(t, database)

	cases := []struct {
		name string
		p    *models.Provider
		want string
	}{
		{"typed systemone", &models.Provider{Name: "kev", Type: models.ProviderSystemOne, BaseURL: "http://localhost:8009/v1"}, "http://localhost:8009/v1/systemone"},
		{"systemone base", &models.Provider{Name: "llama", Type: models.ProviderOpenAI, BaseURL: "http://localhost:8009/v1/systemone"}, "http://localhost:8009/v1/systemone"},
		{"jev reseller", &models.Provider{Name: "surplus", Type: models.ProviderOpenAI, BaseURL: "https://api.surplusintelligence.ai/v1"}, "https://api.surplusintelligence.ai/v1/decisions"},
		{"prefixed base", &models.Provider{Name: "mirror", Type: models.ProviderOpenAI, BaseURL: "https://gw.example/openai/v1"}, "https://gw.example/openai/v1/decisions"},
	}
	for _, c := range cases {
		if got := h.decisionTarget(c.p); got != c.want {
			t.Errorf("%s: decisionTarget = %q, want %q", c.name, got, c.want)
		}
	}

	// A per-provider override replaces the path on the origin, so it is not
	// re-prefixed by a gateway base path.
	if _, err := database.Exec(`INSERT INTO system_config(key,value,updated_at) VALUES('provider.surplus.decision_path','/v2/decide',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	p := &models.Provider{Name: "surplus", Type: models.ProviderOpenAI, BaseURL: "https://api.surplusintelligence.ai/v1"}
	if got := h.decisionTarget(p); got != "https://api.surplusintelligence.ai/v2/decide" {
		t.Errorf("override target = %q, want https://api.surplusintelligence.ai/v2/decide", got)
	}

	// A full URL override wins outright.
	if _, err := database.Exec(`UPDATE system_config SET value='https://other.example/v2/decide' WHERE key='provider.surplus.decision_path'`); err != nil {
		t.Fatal(err)
	}
	if got := h.decisionTarget(p); got != "https://other.example/v2/decide" {
		t.Errorf("full URL override = %q", got)
	}
}

func TestIsDecisionProvider(t *testing.T) {
	yes := []*models.Provider{
		{Type: models.ProviderSystemOne},
		{Type: models.ProviderCloudflare},
		{Type: models.ProviderAzure, BaseURL: "https://api.cloudflare.com/client/v4/accounts/x/ai/run"},
		{Type: models.ProviderAzure, BaseURL: "https://x.example/v1/systemone"},
	}
	for _, p := range yes {
		if !isDecisionProvider(p) {
			t.Errorf("expected decision provider: %+v", p)
		}
	}
	no := []*models.Provider{
		nil,
		{Type: models.ProviderAzure, BaseURL: "https://example.openai.azure.com"},
		{Type: models.ProviderAnthropic, BaseURL: "https://api.anthropic.com"},
	}
	for _, p := range no {
		if isDecisionProvider(p) {
			t.Errorf("expected non-decision provider: %+v", p)
		}
	}
}
