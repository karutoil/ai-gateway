package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
	"ai-gateway/internal/lb"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

// lbAPITestEnv wires an lb store + routing handler over a fresh in-memory DB.
func lbAPITestEnv(t *testing.T) (*httptest.Server, *lb.Store, *models.Provider, *models.Provider) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ps := provider.NewStore(database, make([]byte, 32))
	pa, err := ps.Create("prov-a", models.ProviderOpenAI, "http://localhost:9/v1", "sk-a")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := ps.Create("prov-b", models.ProviderAnthropic, "http://localhost:9", "sk-b")
	if err != nil {
		t.Fatal(err)
	}
	store := lb.NewStore(database)
	h := NewRoutingHandler(store)
	// Test runs as a global admin (no org scope).
	h.ProviderID = func(r *http.Request) string { return "" }
	h.Role = func(r *http.Request) string { return "admin" }

	r := chi.NewRouter()
	// Inject admin role context (RequireRole reads it via auth.GetRole);
	// normally set by auth.AdminMiddleware during dashboard JWT auth.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithRole(req.Context(), "admin")))
		})
	})
	h.Routes(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, store, pa, pb
}

func lbPut(t *testing.T, srv *httptest.Server, model, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("PUT", srv.URL+"/lb/rules/"+model, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 8192)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}

// Rich body: strategy + members with overrides/weights round-trips.
func TestPutRuleStrategyAndMembersRoundTrip(t *testing.T) {
	srv, _, pa, pb := lbAPITestEnv(t)
	body := `{"strategy":"weighted","members":[
		{"provider_id":"` + pa.ID + `","weight":70,"model_override":"gpt-4o-2024-11-20"},
		{"provider_id":"` + pb.ID + `","weight":30}
	]}`
	code, resp := lbPut(t, srv, "gpt-4o", body)
	if code != 201 {
		t.Fatalf("expected 201, got %d: %s", code, resp)
	}
	var rule lb.Rule
	if err := json.Unmarshal([]byte(resp), &rule); err != nil {
		t.Fatal(err)
	}
	if rule.Strategy != "weighted" {
		t.Fatalf("strategy round-trip failed: %q", rule.Strategy)
	}
	if len(rule.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(rule.Members))
	}
	if rule.Members[0].Weight != 70 || rule.Members[0].ModelOverride != "gpt-4o-2024-11-20" {
		t.Fatalf("member 0 round-trip failed: %+v", rule.Members[0])
	}
	if rule.Members[1].Weight != 30 {
		t.Fatalf("member 1 default weight failed: %+v", rule.Members[1])
	}
}

// Legacy providers-only body still works (back-compat: round_robin, no extras).
func TestPutRuleLegacyProvidersBody(t *testing.T) {
	srv, _, pa, pb := lbAPITestEnv(t)
	body := `{"providers":["` + pa.ID + `","` + pb.ID + `"]}`
	code, resp := lbPut(t, srv, "gpt-4o-mini", body)
	if code != 201 {
		t.Fatalf("expected 201, got %d: %s", code, resp)
	}
	var rule lb.Rule
	if err := json.Unmarshal([]byte(resp), &rule); err != nil {
		t.Fatal(err)
	}
	if rule.Strategy != "round_robin" {
		t.Fatalf("legacy body should default to round_robin, got %q", rule.Strategy)
	}
	if len(rule.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(rule.Members))
	}
}

// Invalid strategies and weights are rejected with 400.
func TestPutRuleValidation(t *testing.T) {
	srv, _, pa, _ := lbAPITestEnv(t)
	cases := []struct {
		name string
		body string
	}{
		{"bad strategy", `{"strategy":"cheapest","providers":["` + pa.ID + `"]}`},
		{"zero weight on weighted", `{"strategy":"weighted","members":[{"provider_id":"` + pa.ID + `","weight":0}]}`},
		{"weight over max", `{"strategy":"weighted","members":[{"provider_id":"` + pa.ID + `","weight":101}]}`},
	}
	for _, tc := range cases {
		code, body := lbPut(t, srv, "m", tc.body)
		if code != 400 {
			t.Errorf("%s: expected 400, got %d: %s", tc.name, code, body)
		}
	}
}
