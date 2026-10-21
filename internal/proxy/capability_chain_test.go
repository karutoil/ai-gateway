package proxy

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

// capabilityChainServer wires the production middleware chain (gateway auth,
// then the handler) over every new endpoint, so a test can prove the new
// surfaces are not an unscoped escape hatch: no key ⇒ 401, allowlist ⇒ 403,
// and the key's own allowlist is enforced before any upstream call.
func capabilityChainServer(t *testing.T) (*httptest.Server, *apikey.Store, *sql.DB, *int) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "systemone") || strings.Contains(r.URL.Path, "@cf/"):
			_, _ = w.Write([]byte(`{"model":"clef","answers":{"a":{"type":"noul","noul":0.9}},"usage":{"input_tokens":1}}`))
		case strings.Contains(r.URL.Path, "/images/"):
			_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"AAA"}]}`))
		case strings.HasSuffix(r.URL.Path, "/videos"):
			_, _ = w.Write([]byte(`{"id":"up_v1","object":"video","status":"queued","progress":0,"created_at":1700000000}`))
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		}
	}))
	t.Cleanup(upstream.Close)

	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	if _, err := ps.Create("cf", models.ProviderCloudflare, upstream.URL+"/ai/run", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Create("oa", models.ProviderOpenAI, upstream.URL+"/v1", "k"); err != nil {
		t.Fatal(err)
	}

	h := newLegacyHandler(ps, database)
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	r.Post("/v1/systemone", h.SystemOne)
	r.Post("/v1/images/generations", h.ImageGenerations)
	r.Post("/v1/videos", h.VideoCreate)
	r.Get("/v1/videos/{id}", h.VideoGet)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, ks, database, &hits
}

// newCapabilityKey creates a gateway key with the given allowlist (nil = any).
func newCapabilityKey(t *testing.T, ks *apikey.Store, allowed []string) string {
	t.Helper()
	k, err := ks.Create("chain-key")
	if err != nil {
		t.Fatal(err)
	}
	if allowed != nil {
		if err := ks.UpdateLimits(k.ID, nil, nil, nil, nil, &allowed); err != nil {
			t.Fatal(err)
		}
	}
	return k.Key
}

// newCapabilityEndpoints enumerates the new surfaces with a body that would
// succeed if the request were authorized.
func newCapabilityEndpoints(t *testing.T) []struct{ name, path, body string } {
	return []struct{ name, path, body string }{
		{"images", "/v1/images/generations", `{"model":"oa/gpt-image-1","prompt":"x"}`},
		{"videos", "/v1/videos", `{"model":"oa/sora-2","prompt":"x","seconds":"4"}`},
		{"systemone", "/v1/systemone", `{"model":"cf/clef","state":"x","questions":{"a":{"type":"noul","instructions":"y"}}}`},
	}
}

// TestNewEndpointsRequireGatewayAuth proves the new endpoints sit inside the
// authenticated gateway group: no Authorization header is a 401 and never
// reaches an upstream. (cmd/gateway/main.go registers them inside the
// GatewayAuth group — this pins the handler-level expectation.)
func TestNewEndpointsRequireGatewayAuth(t *testing.T) {
	srv, _, _, hits := capabilityChainServer(t)
	for _, c := range newCapabilityEndpoints(t) {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", srv.URL+c.path, strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unauthenticated %s = %d, want 401", c.path, resp.StatusCode)
			}
		})
	}
	if *hits != 0 {
		t.Fatalf("unauthenticated requests reached an upstream %d times", *hits)
	}
}

// TestModelAllowlistEnforcedOnNewEndpoints proves the per-key allowed_models
// allowlist is applied to all three new surfaces, with a 403 (not a 404 that
// would look like a routing problem) and no upstream traffic.
func TestModelAllowlistEnforcedOnNewEndpoints(t *testing.T) {
	srv, ks, _, hits := capabilityChainServer(t)
	key := newCapabilityKey(t, ks, []string{"gpt-4o-mini"})

	for _, c := range newCapabilityEndpoints(t) {
		t.Run(c.name, func(t *testing.T) {
			resp, body := postJSONImage(t, srv.URL+c.path, key, c.body)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("allowlisted-out model on %s = %d, want 403: %s", c.path, resp.StatusCode, body)
			}
			if !strings.Contains(string(body), "not allowed for this key") {
				t.Fatalf("message = %s", body)
			}
		})
	}
	if *hits != 0 {
		t.Fatalf("allowlist violations reached an upstream %d times", *hits)
	}
}

// TestAllowlistedCapabilityModelIsServed proves the allowlist is not a blanket
// deny: a key allowed to use the capability model gets through on each new
// endpoint.
func TestAllowlistedCapabilityModelIsServed(t *testing.T) {
	srv, ks, _, _ := capabilityChainServer(t)
	cases := []struct{ name, path, body, allow string }{
		{"images", "/v1/images/generations", `{"model":"oa/gpt-image-1","prompt":"x"}`, "gpt-image-1"},
		{"videos", "/v1/videos", `{"model":"oa/sora-2","prompt":"x","seconds":"4"}`, "sora-2"},
		{"systemone", "/v1/systemone", `{"model":"cf/clef","state":"x","questions":{"a":{"type":"noul","instructions":"y"}}}`, "clef"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := newCapabilityKey(t, ks, []string{c.allow})
			resp, body := postJSONImage(t, srv.URL+c.path, key, c.body)
			if resp.StatusCode != 200 {
				t.Fatalf("allowed %q on %s = %d: %s", c.allow, c.path, resp.StatusCode, body)
			}
		})
	}
}

// TestRevokedKeyCannotUseNewEndpoints proves revocation is honored on the new
// surfaces (no cached auth path bypasses the store).
func TestRevokedKeyCannotUseNewEndpoints(t *testing.T) {
	srv, ks, _, _ := capabilityChainServer(t)
	k, err := ks.Create("revoked-key")
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: works before revocation.
	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", k.Key, `{"model":"oa/gpt-image-1","prompt":"x"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("pre-revocation = %d: %s", resp.StatusCode, body)
	}
	if err := ks.Revoke(k.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range newCapabilityEndpoints(t) {
		t.Run(c.name, func(t *testing.T) {
			resp, body := postJSONImage(t, srv.URL+c.path, k.Key, c.body)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("revoked key on %s = %d, want 401: %s", c.path, resp.StatusCode, body)
			}
		})
	}
}

// TestVideoPromptBoundAndOversizeBody covers the two unbounded-input guards on
// the video create route: the protocol prompt cap and the proxy body cap.
func TestVideoPromptBoundAndOversizeBody(t *testing.T) {
	srv, _, key, lc := newVideoLifecycleServer(t)

	// Over-long prompt (videogen.MaxPromptLen = 32000).
	payload, _ := json.Marshal(map[string]any{
		"model": "oai/sora-2", "prompt": strings.Repeat("a", 32001),
	})
	resp, body := doReqAs(t, "POST", srv.URL+"/v1/videos", key, "p1", string(payload))
	if resp.StatusCode != 400 || !strings.Contains(string(body), "prompt") {
		t.Fatalf("overlong video prompt: %d %s", resp.StatusCode, body)
	}
	if lc.creates.Load() != 0 {
		t.Fatal("overlong prompt reached upstream")
	}

	// Body over the default 64 MiB proxy cap is a clean 413.
	big := `{"model":"oai/sora-2","prompt":"` + strings.Repeat("a", int(DefaultMaxProxyRequestBodyBytes)+16) + `"}`
	resp, body = doReqAs(t, "POST", srv.URL+"/v1/videos", key, "p1", big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize video body = %d, want 413: %.120s", resp.StatusCode, body)
	}
	if lc.creates.Load() != 0 {
		t.Fatal("oversize body reached upstream")
	}
}

// TestSystemOneQuestionBoundAndOversizeBody pins the decision model's unbounded
// input guards: the 64-question cap is a local 400, and the proxy body cap is a
// 413 — neither reaches an upstream.
func TestSystemOneQuestionBoundAndOversizeBody(t *testing.T) {
	srv, ks, _, hits := capabilityChainServer(t)
	key := newCapabilityKey(t, ks, nil)

	// 65 questions exceeds decision.MaxQuestions.
	qs := make([]string, 0, 65)
	for i := 0; i < 65; i++ {
		qs = append(qs, `"q`+itoa(i)+`":{"type":"noul","instructions":"x"}`)
	}
	body := `{"model":"cf/clef","state":"x","questions":{` + strings.Join(qs, ",") + `}}`
	resp, respBody := postJSONImage(t, srv.URL+"/v1/systemone", key, body)
	if resp.StatusCode != 400 || !strings.Contains(string(respBody), "at most 64") {
		t.Fatalf("65 questions: %d %s", resp.StatusCode, respBody)
	}
	if *hits != 0 {
		t.Fatal("an over-limit decision body reached upstream")
	}

	// A state payload over the proxy body cap is a 413, not an upstream 400.
	big := `{"model":"cf/clef","state":"` + strings.Repeat("a", int(DefaultMaxProxyRequestBodyBytes)+16) +
		`","questions":{"a":{"type":"noul","instructions":"y"}}}`
	resp, respBody = postJSONImage(t, srv.URL+"/v1/systemone", key, big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize systemone body = %d, want 413: %.120s", resp.StatusCode, respBody)
	}
}
