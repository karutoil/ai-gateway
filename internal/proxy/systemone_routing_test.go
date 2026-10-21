package proxy

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/lb"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

const validDecisionBody = `{"model":"clef","state":"x","questions":{"a":{"type":"noul","instructions":"y"}}}`

// TestSystemOneNoCandidatesBranches covers the three unroutable paths of the
// decision endpoint, none of which had coverage (they need specific routing
// configurations).
func TestSystemOneNoCandidatesBranches(t *testing.T) {
	newHandler := func(t *testing.T) (*Handler, *sql.DB, *httptest.Server) {
		t.Helper()
		database, err := db.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { database.Close() })
		h := New(provider.NewStore(database, make([]byte, 32)), database)
		srv := httptest.NewServer(http.HandlerFunc(h.SystemOne))
		t.Cleanup(srv.Close)
		return h, database, srv
	}

	t.Run("no decision provider configured", func(t *testing.T) {
		_, _, srv := newHandler(t)
		resp, body := postJSONImage(t, srv.URL, "k", validDecisionBody)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503: %s", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "no decision-capable provider") {
			t.Fatalf("message = %s", body)
		}
	})

	t.Run("strict routing, no rule for the model", func(t *testing.T) {
		h, database, srv := newHandler(t)
		h.LB = lb.NewStore(database)
		h.LegacyFallback = false
		resp, body := postJSONImage(t, srv.URL, "k", validDecisionBody)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "/v1/systemone") {
			t.Fatalf("no-route message must name the decision endpoint: %s", body)
		}
	})
}

// TestSystemOneModelAliasIsResolved proves an alias pointing at a decision model
// is rewritten to the target before the upstream call (the candidate resolution
// and the model check both use the resolved id).
func TestSystemOneModelAliasIsResolved(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotModel, _ = b["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"clef","answers":{"a":{"type":"noul","noul":0.5}},"usage":{"input_tokens":1}}`))
	}))
	defer upstream.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	if _, err := ps.Create("cf", models.ProviderCloudflare, upstream.URL+"/ai/run", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO model_aliases(alias,target,created_at) VALUES(?,?,CURRENT_TIMESTAMP)`, "my-decider", "clef"); err != nil {
		t.Fatal(err)
	}
	k, _ := ks.Create("alias-key")
	h := newLegacyHandler(ps, database)
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/systemone", h.SystemOne)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, body := postJSONImage(t, srv.URL+"/v1/systemone", k.Key,
		`{"model":"my-decider","state":"x","questions":{"a":{"type":"noul","instructions":"y"}}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if gotModel != "clef" {
		t.Fatalf("upstream model = %q, want the alias target", gotModel)
	}
}

// TestSystemOneCloudflarePathAddressing proves the Cloudflare dialect puts the
// model in the URL and the short selector in the body.
func TestSystemOneCloudflarePathAddressing(t *testing.T) {
	var gotPath, gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotModel, _ = b["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"a":{"type":"noul","noul":0.7}}}`))
	}))
	defer upstream.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	if _, err := ps.Create("cf", models.ProviderCloudflare,
		upstream.URL+"/client/v4/accounts/abc/ai/run", "k"); err != nil {
		t.Fatal(err)
	}
	k, _ := ks.Create("cf-key")
	h := newLegacyHandler(ps, database)
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/systemone", h.SystemOne)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, body := postJSONImage(t, srv.URL+"/v1/systemone", k.Key,
		`{"model":"@cf/cloudflare/clef-flash","state":"x","questions":{"a":{"type":"noul","instructions":"y"}}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if !strings.HasSuffix(gotPath, "/ai/run/@cf/cloudflare/clef-flash") {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotModel != "clef-flash" {
		t.Fatalf("upstream model = %q, want the short selector", gotModel)
	}
}

// TestNewEndpointsEnforceTPM proves the per-key token limit applies to the new
// surfaces — an image/video/decision body must not bypass TPM accounting.
func TestNewEndpointsEnforceTPM(t *testing.T) {
	newServer := func(t *testing.T) (*httptest.Server, string) {
		t.Helper()
		hits := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.Contains(r.URL.Path, "systemone"):
				_, _ = w.Write([]byte(`{"answers":{},"usage":{"input_tokens":1}}`))
			case strings.Contains(r.URL.Path, "/images/"):
				_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"AAA"}]}`))
			default:
				_, _ = w.Write([]byte(`{"id":"up_v1","object":"video","status":"queued","created_at":1}`))
			}
		}))
		t.Cleanup(upstream.Close)

		database, err := db.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { database.Close() })
		ps := provider.NewStore(database, make([]byte, 32))
		ks := apikey.NewStore(database)
		if _, err := ps.Create("oa", models.ProviderOpenAI, upstream.URL+"/v1", "k"); err != nil {
			t.Fatal(err)
		}
		ks2, err := ks.Create("tpm-key")
		if err != nil {
			t.Fatal(err)
		}
		tpm := 1 // one token: any real body exceeds it
		if err := ks.UpdateLimits(ks2.ID, nil, nil, nil, &tpm, nil); err != nil {
			t.Fatal(err)
		}
		h := newLegacyHandler(ps, database)
		h.RateLimiter = middleware.NewRateLimiter()
		r := chi.NewRouter()
		r.Use(middleware.GatewayAuth(ks))
		r.Post("/v1/images/generations", h.ImageGenerations)
		r.Post("/v1/videos", h.VideoCreate)
		r.Post("/v1/systemone", h.SystemOne)
		srv := httptest.NewServer(r)
		t.Cleanup(srv.Close)
		return srv, ks2.Key
	}

	srv, key := newServer(t)
	cases := []struct{ name, path, body string }{
		{"images", "/v1/images/generations", `{"model":"oa/gpt-image-1","prompt":"` + strings.Repeat("a", 200) + `"}`},
		{"videos", "/v1/videos", `{"model":"oa/sora-2","prompt":"` + strings.Repeat("a", 200) + `"}`},
		{"systemone", "/v1/systemone", `{"model":"clef","state":"` + strings.Repeat("a", 200) + `","questions":{"a":{"type":"noul","instructions":"y"}}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := postJSONImage(t, srv.URL+c.path, key, c.body)
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429: %s", resp.StatusCode, body)
			}
			if resp.Header.Get("Retry-After") == "" {
				t.Error("429 must carry Retry-After")
			}
			if !strings.Contains(string(body), "rate limit") {
				t.Fatalf("message = %s", body)
			}
		})
	}
}

// TestImageTTFPSentinelPlumbing pins the IMAGE_TIMEOUT_SECONDS contract end to
// end inside the proxy: <0 falls back to DefaultImageTTFB (300s, NOT the 85s
// chat budget), an explicit positive value wins, and 0 means DISABLED (P1-12:
// the old code resolved 0 to the 300s default while the boot log said
// "disabled").
func TestImageTTFPSentinelPlumbing(t *testing.T) {
	if DefaultImageTTFB != 300*time.Second {
		t.Fatalf("DefaultImageTTFB = %v, want 300s", DefaultImageTTFB)
	}
	if DefaultImageTTFB <= DefaultTimeouts().TTFB {
		t.Fatalf("image budget (%v) must exceed the chat first-byte budget (%v)",
			DefaultImageTTFB, DefaultTimeouts().TTFB)
	}
	cases := []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{"unset falls back to the default", -1, DefaultImageTTFB},
		{"explicit value wins", 42 * time.Second, 42 * time.Second},
		{"one hour", time.Hour, time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &Handler{ImageTTFB: c.set}
			if got := h.imageTTFB(); got != c.want {
				t.Fatalf("imageTTFB() = %v, want %v", got, c.want)
			}
			// ...and the proxy options must carry the resolved budget with the
			// disable flag OFF, so resolveTTFBBudget keeps it.
			opts := h.imageProxyOpts(nil)
			if opts.ttfbDisable {
				t.Fatal("a non-zero ImageTTFB must not disable the watchdog")
			}
			if budget, disabled := h.resolveTTFBBudget("images.generations", opts); disabled || budget != c.want {
				t.Fatalf("resolveTTFBBudget = (%v, %v), want (%v, false)", budget, disabled, c.want)
			}
		})
	}
}

// TestImageTTFBDisabledIsHonoured proves the DISABLED state is carried by its own
// flag end to end: imageTTFB reports 0, the proxy options set ttfbDisable, and
// resolveTTFBBudget returns a disabled controller budget rather than falling back
// to the 300s default. Pre-fix IMAGE_TIMEOUT_SECONDS=0 silently resolved to 300s
// while the boot log claimed "disabled".
func TestImageTTFBDisabledIsHonoured(t *testing.T) {
	// Both disable signals must behave identically: the documented ==0 sentinel
	// that main.go sets, and the explicit flag the shared machinery also reads.
	for _, h := range []*Handler{{ImageTTFB: 0}, {ImageTTFBDisabled: true}} {
		if got := h.imageTTFB(); got != 0 {
			t.Fatalf("imageTTFB() with %+v = %v, want 0", h, got)
		}
		opts := h.imageProxyOpts(nil)
		if !opts.ttfbDisable {
			t.Fatalf("%+v must set ttfbDisable", h)
		}
		if budget, disabled := h.resolveTTFBBudget("images.edits", opts); !disabled || budget != 0 {
			t.Fatalf("%+v resolveTTFBBudget = (%v, %v), want (0, true)", h, budget, disabled)
		}
	}

	// The flag still wins over a positive duration.
	h := &Handler{ImageTTFBDisabled: true, ImageTTFB: time.Hour}
	if got := h.imageTTFB(); got != 0 {
		t.Fatalf("imageTTFB() with the flag and a positive value = %v, want 0", got)
	}
	opts := h.imageProxyOpts(nil)
	if !opts.ttfbDisable {
		t.Fatal("imageProxyOpts must set ttfbDisable when the watchdog is off")
	}
	if budget, disabled := h.resolveTTFBBudget("images.generations", opts); !disabled || budget != 0 {
		t.Fatalf("resolveTTFBBudget = (%v, %v), want (0, true)", budget, disabled)
	}
	// And chat is untouched.
	if _, disabled := h.resolveTTFBBudget("chat.completions", proxyCandidatesOpts{}); disabled {
		t.Fatal("the image disable switch must not reach chat")
	}
}
