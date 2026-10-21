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
	if got := systemOneTarget("https://api.typesafe.ai"); got != "https://api.typesafe.ai/v1/systemone" {
		t.Errorf("typesafe target = %q", got)
	}
	if got := systemOneTarget("http://localhost:8009/v1/"); got != "http://localhost:8009/v1/systemone" {
		t.Errorf("v1 target = %q", got)
	}
	if got := systemOneTarget("http://localhost:8009/v1/systemone"); got != "http://localhost:8009/v1/systemone" {
		t.Errorf("idempotent target = %q", got)
	}
	if got := cloudflareDecisionTarget("https://api.cloudflare.com/client/v4/accounts/abc/ai/run", "clef-flash"); got != "https://api.cloudflare.com/client/v4/accounts/abc/ai/run/@cf/cloudflare/clef-flash" {
		t.Errorf("cloudflare target = %q", got)
	}
	if got := cloudflareDecisionTarget("https://api.cloudflare.com/client/v4/accounts/abc", "clef"); got != "https://api.cloudflare.com/client/v4/accounts/abc/@cf/cloudflare/clef" {
		t.Errorf("cloudflare base-only target = %q", got)
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
