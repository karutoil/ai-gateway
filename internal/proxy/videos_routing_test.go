package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
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
	"ai-gateway/internal/videogen"

	"github.com/go-chi/chi/v5"
)

// TestImageNoRouteBranches covers imageNoRoute, which had zero coverage because
// reaching it through HTTP needs a configuration with no matching route. The
// three branches produce very different advice, so each is asserted directly.
func TestImageNoRouteBranches(t *testing.T) {
	rule := &lb.Rule{Model: "gpt-image-1"}
	cases := []struct {
		name     string
		h        *Handler
		model    string
		rawModel string
		rule     *lb.Rule
		want     int
		wantBody []string
	}{
		{
			name:  "routing rule with no eligible member",
			h:     &Handler{},
			model: "gpt-image-1", rawModel: "gpt-image-1", rule: rule,
			want: 503, wantBody: []string{"OAuth quota threshold"},
		},
		{
			name:  "unrouted model (strict routing)",
			h:     &Handler{LB: lb.NewStore(nil), LegacyFallback: false},
			model: "gpt-image-1", rawModel: "gpt-image-1",
			want: http.StatusNotFound, wantBody: []string{"has no route on this endpoint", "model_not_routed", "/v1/images/generations"},
		},
		{
			name:  "unknown model, no providers at all",
			h:     &Handler{},
			model: "totally-unknown", rawModel: "totally-unknown",
			want: 400, wantBody: []string{"is not an image model", "/v1/images/generations"},
		},
		{
			name:  "known image model, no image provider",
			h:     &Handler{},
			model: "gpt-image-1", rawModel: "gpt-image-1",
			want: 503, wantBody: []string{"no image-capable provider"},
		},
		{
			name:  "known image model via the raw (unprefixed) id",
			h:     &Handler{},
			model: "prefix/gpt-image-1", rawModel: "gpt-image-1",
			want: 503, wantBody: []string{"no image-capable provider"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.h.imageNoRoute(rec, c.model, c.rawModel, c.rule)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, c.want, rec.Body.String())
			}
			for _, want := range c.wantBody {
				if !strings.Contains(rec.Body.String(), want) {
					t.Errorf("body missing %q: %s", want, rec.Body.String())
				}
			}
		})
	}
}

// TestVideoNoRouteBranches is the video mirror of the above.
func TestVideoNoRouteBranches(t *testing.T) {
	cases := []struct {
		name  string
		h     *Handler
		model string
		rule  *lb.Rule
		want  int
		body  []string
	}{
		{
			name: "rule with no eligible member", h: &Handler{}, model: "sora-2",
			rule: &lb.Rule{Model: "sora-2"}, want: 503, body: []string{"OAuth quota threshold"},
		},
		{
			name: "unrouted model", h: &Handler{LB: lb.NewStore(nil)}, model: "sora-2",
			want: http.StatusNotFound, body: []string{"video-generation model", "/v1/videos", "model_not_routed"},
		},
		{
			name: "no video provider", h: &Handler{}, model: "sora-2",
			want: 503, body: []string{"no video-capable provider"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.h.videoNoRoute(rec, c.model, c.rule)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, c.want, rec.Body.String())
			}
			for _, want := range c.body {
				if !strings.Contains(rec.Body.String(), want) {
					t.Errorf("body missing %q: %s", want, rec.Body.String())
				}
			}
		})
	}
}

// TestVideoDialectFor pins dialect selection: the PROVIDER TYPE decides the
// wire protocol, the curated registry is consulted only when the provider type
// is unclassified, and an unknown model defaults to the OpenAI shape.
//
// Regression (see .review/tests.md item 6): the registry used to win for every
// non-Google/non-xAI provider, so an OpenAI-compatible reseller carrying
// veo-3.1-fast was called with Google's :predictLongRunning and answered 400
// "video model must be called through the video endpoint", while a
// grok-imagine id was called at xAI's /videos/generations and answered 404.
// Every such model was unusable from the playground.
func TestVideoDialectFor(t *testing.T) {
	cases := []struct {
		name  string
		p     *models.Provider
		model string
		want  videogen.Dialect
	}{
		{"nil provider", nil, "sora-2", videogen.DialectOpenAI},
		{"google provider", &models.Provider{Type: models.ProviderGoogle}, "veo-3.1-generate-preview", videogen.DialectVeo},
		{"xai provider", &models.Provider{Type: models.ProviderXAI}, "grok-imagine-video", videogen.DialectXAI},
		// A reseller exposes ONE wire protocol for every model it carries,
		// whatever vendor the id names. These four are the regression above.
		{"openai provider with a veo model", &models.Provider{Type: models.ProviderOpenAI}, "veo-3.1-generate-preview", videogen.DialectOpenAI},
		{"reseller with a veo model", &models.Provider{Type: models.ProviderOpenAICompatible}, "veo-3.1-generate-preview", videogen.DialectOpenAI},
		{"reseller with an xai model", &models.Provider{Type: models.ProviderOpenAICompatible}, "grok-imagine-video", videogen.DialectOpenAI},
		{"reseller with an unknown model", &models.Provider{Type: models.ProviderOpenAICompatible}, "acme/video-1", videogen.DialectOpenAI},
		{"azure speaks the OpenAI shape", &models.Provider{Type: models.ProviderAzure}, "sora-2", videogen.DialectOpenAI},
		// Only an UNCLASSIFIED provider falls back to the registry, which is
		// then the sole evidence of a wire protocol.
		{"untyped provider falls back to the registry (veo)", &models.Provider{}, "veo-3.1-generate-preview", videogen.DialectVeo},
		{"untyped provider falls back to the registry (xai)", &models.Provider{}, "grok-imagine-video", videogen.DialectXAI},
		{"untyped provider with an unknown model", &models.Provider{}, "acme/video-1", videogen.DialectOpenAI},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := videoDialectFor(c.p, c.model); got != c.want {
				t.Fatalf("dialect = %q, want %q", got, c.want)
			}
		})
	}
}

// TestVideoCreateRequestShapes covers the three dialects' URL/body builders,
// including the input_reference branch of the OpenAI passthrough (which was
// untested and silently drops the reference if the re-marshal fails).
func TestVideoCreateRequestShapes(t *testing.T) {
	n := &videogen.Normalized{
		Model: "sora-2", Prompt: "a cat", Seconds: "8", Size: "1280x720",
		InputReference: []byte(`"aGVsbG8="`),
	}

	t.Run("openai passthrough with reference", func(t *testing.T) {
		p := &models.Provider{Type: models.ProviderOpenAI, BaseURL: "https://api.openai.com/v1"}
		target, body, err := videoCreateRequest(p, videogen.DialectOpenAI, n, "sora-2")
		if err != nil {
			t.Fatal(err)
		}
		if target != "https://api.openai.com/v1/videos" {
			t.Fatalf("target = %q", target)
		}
		s := string(body)
		for _, want := range []string{`"model":"sora-2"`, `"prompt":"a cat"`, `"seconds":"8"`, `"size":"1280x720"`, `"input_reference"`} {
			if !strings.Contains(s, want) {
				t.Errorf("body missing %s: %s", want, s)
			}
		}
	})

	t.Run("azure adds the deployment path", func(t *testing.T) {
		p := &models.Provider{Type: models.ProviderAzure, BaseURL: "https://x.openai.azure.com"}
		target, _, err := videoCreateRequest(p, videogen.DialectOpenAI, n, "sora-2")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(target, "/openai/deployments/sora-2/videos") {
			t.Fatalf("azure target = %q", target)
		}
		if !strings.Contains(target, "api-version=") {
			t.Fatalf("azure target is missing the api-version: %q", target)
		}
	})

	t.Run("xai", func(t *testing.T) {
		p := &models.Provider{Type: models.ProviderXAI, BaseURL: "https://api.x.ai/v1/"}
		target, body, err := videoCreateRequest(p, videogen.DialectXAI, n, "grok-imagine-video")
		if err != nil {
			t.Fatal(err)
		}
		if target != "https://api.x.ai/v1/videos/generations" {
			t.Fatalf("xai target = %q", target)
		}
		if !strings.Contains(string(body), `"aspect_ratio":"16:9"`) {
			t.Fatalf("xai body = %s", body)
		}
	})

	t.Run("veo", func(t *testing.T) {
		p := &models.Provider{Type: models.ProviderGoogle, BaseURL: "https://generativelanguage.googleapis.com"}
		target, body, err := videoCreateRequest(p, videogen.DialectVeo, n, "veo-3.1-generate-preview")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(target, "/v1beta/models/veo-3.1-generate-preview:predictLongRunning") {
			t.Fatalf("veo target = %q", target)
		}
		if !strings.Contains(string(body), `"instances"`) || !strings.Contains(string(body), `"durationSeconds":8`) {
			t.Fatalf("veo body = %s", body)
		}
	})

	t.Run("malformed reference fails the request", func(t *testing.T) {
		bad := &videogen.Normalized{Model: "grok-imagine-video", Prompt: "p", Seconds: "4", InputReference: []byte(`"!!!not-base64!!!"`)}
		p := &models.Provider{Type: models.ProviderXAI, BaseURL: "https://api.x.ai/v1"}
		if _, _, err := videoCreateRequest(p, videogen.DialectXAI, bad, "grok-imagine-video"); err == nil {
			t.Fatal("malformed input_reference must fail before the upstream call")
		}
	})
}

// TestVideoUpstreamIDPerDialect pins the job-handle extraction, which decides
// what the poll URL is built from.
func TestVideoUpstreamIDPerDialect(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		dialect videogen.Dialect
		want    string
	}{
		{"openai id", `{"id":"video_abc"}`, videogen.DialectOpenAI, "video_abc"},
		{"openai without id", `{"object":"video"}`, videogen.DialectOpenAI, ""},
		{"openai garbage", `not json`, videogen.DialectOpenAI, ""},
		{"xai request_id", `{"request_id":"req-1"}`, videogen.DialectXAI, "req-1"},
		{"xai id fallback", `{"id":"req-2"}`, videogen.DialectXAI, "req-2"},
		{"veo operation", `{"name":"models/veo/operations/op"}`, videogen.DialectVeo, "models/veo/operations/op"},
		{"veo without name", `{"done":false}`, videogen.DialectVeo, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := videoUpstreamID([]byte(c.body), c.dialect); got != c.want {
				t.Fatalf("upstream id = %q, want %q", got, c.want)
			}
		})
	}
}

// TestVideoContentURLResolution covers the three ways the content URL is found:
// a stored provider URL, the OpenAI content route, and a re-poll for dialects
// that only reveal the URL on the poll response.
func TestVideoContentURLResolution(t *testing.T) {
	h := &Handler{}

	t.Run("stored url wins", func(t *testing.T) {
		job := &videoJob{Dialect: videogen.DialectXAI, VideoURL: "https://cdn/x.mp4"}
		got, err := h.videoContentURL(nil, job, &models.Provider{BaseURL: "https://api.x.ai/v1"}, "k")
		if err != nil || got != "https://cdn/x.mp4" {
			t.Fatalf("url = %q err=%v", got, err)
		}
	})

	t.Run("openai content route", func(t *testing.T) {
		job := &videoJob{Dialect: videogen.DialectOpenAI, UpstreamID: "up_1", Model: "sora-2"}
		got, err := h.videoContentURL(nil, job, &models.Provider{Type: models.ProviderOpenAI, BaseURL: "https://api.openai.com/v1"}, "k")
		if err != nil {
			t.Fatal(err)
		}
		if got != "https://api.openai.com/v1/videos/up_1/content" {
			t.Fatalf("content url = %q", got)
		}
	})

	t.Run("veo re-polls for the uri", func(t *testing.T) {
		dbase, err := db.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer dbase.Close()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "op",
				"done": true,
				"response": map[string]any{
					"generateVideoResponse": map[string]any{
						"generatedSamples": []any{
							map[string]any{"video": map[string]any{"uri": "https://files/x"}},
						},
					},
				},
			})
		}))
		defer upstream.Close()
		hh := newLegacyHandler(nil, dbase)
		job := &videoJob{ID: "video_1", Dialect: videogen.DialectVeo, UpstreamID: "op", Model: "veo", Status: "completed"}
		p := &models.Provider{Type: models.ProviderGoogle, BaseURL: upstream.URL}
		got, err := hh.videoContentURL(httptest.NewRequest("GET", "/", nil), job, p, "k")
		if err != nil {
			t.Fatal(err)
		}
		if got != "https://files/x" {
			t.Fatalf("re-polled uri = %q", got)
		}
	})
}

// TestSameOriginCredentialScope proves the provider key is only attached to
// same-host downloads (a CDN URL on another host must not receive it).
func TestSameOriginCredentialScope(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://api.x.ai/videos/x.mp4", "https://api.x.ai/v1", true},
		{"https://API.X.AI/x", "https://api.x.ai/v1", true},
		{"https://cdn.x.ai/x.mp4", "https://api.x.ai/v1", false},
		{"not a url", "https://api.x.ai", false},
		{"https://api.x.ai", "not a url", false},
	}
	for _, c := range cases {
		if got := sameOrigin(c.a, c.b); got != c.want {
			t.Errorf("sameOrigin(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestVideoCostPricing proves billing is derived from the curated per-second
// price and defaults to zero for unknown models (no silent overcharge).
func TestVideoCostPricing(t *testing.T) {
	h := &Handler{}
	if got := h.videoCost("sora-2", "8"); got < 0.79 || got > 0.81 {
		t.Fatalf("sora-2 8s = %v, want ~0.80", got)
	}
	if got := h.videoCost("sora-2", "12"); got < 1.19 || got > 1.21 {
		t.Fatalf("sora-2 12s = %v, want ~1.20", got)
	}
	if got := h.videoCost("totally-unknown-video", "8"); got != 0 {
		t.Fatalf("unknown model cost = %v, want 0", got)
	}
	if got := h.videoCost("sora-2", "not-a-number"); got != 0 {
		t.Fatalf("unparseable seconds cost = %v, want 0", got)
	}
}

// TestSameOriginRequiresSchemeAndHost is the P1-1 regression guard: the origin
// comparison used to look at the Host only, so an http:// URL (credential sent in
// cleartext) or a scheme-less protocol-relative URL (scheme "" after url.Parse)
// was treated as the provider's own origin.
func TestSameOriginRequiresSchemeAndHost(t *testing.T) {
	base := "https://api.provider.com/v1"
	cases := []struct {
		url  string
		want bool
	}{
		{"https://api.provider.com/video.mp4", true},
		{"https://API.PROVIDER.COM/video.mp4", true},
		{"http://api.provider.com/video.mp4", false},  // cleartext: never attach the key
		{"//api.provider.com/video.mp4", false},       // empty scheme
		{"api.provider.com/video.mp4", false},         // relative: no origin at all
		{"ftp://api.provider.com/video.mp4", false},   // scheme mismatch
		{"https://10.0.0.5/video.mp4", false},         // private range, not the provider
		{"https://cdn.provider.com/video.mp4", false}, // third-party CDN
		{"https://api.provider.com:8443/v.mp4", false},
		{"", false},
	}
	for _, c := range cases {
		if got := sameOrigin(c.url, base); got != c.want {
			t.Errorf("sameOrigin(%q, %q) = %v, want %v", c.url, base, got, c.want)
		}
	}
}

// TestVideoContentForeignOriginIsRedirected proves a provider-supplied content
// URL that is not on the provider's origin is never fetched server-side: the
// client gets a 302 to it instead (P1-1). Pre-fix the gateway fetched ANY
// absolute URL the provider returned, so a reseller could turn the content route
// into an authenticated-network read.
func TestVideoContentForeignOriginIsRedirected(t *testing.T) {
	for _, foreign := range []string{
		"http://api.provider.com/x.mp4", // same host, wrong scheme
		"//api.provider.com/x.mp4",      // empty scheme
		"http://10.0.0.5:8080/admin",    // RFC1918
		"https://169.254.169.254/latest/meta-data/",
		"https://cdn.thirdparty.example/x.mp4",
	} {
		t.Run(foreign, func(t *testing.T) {
			hit := false
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hit = true
				w.Header().Set("Content-Type", "video/mp4")
				_, _ = w.Write([]byte("SECRET"))
			}))
			defer target.Close()

			database, err := db.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			h := newLegacyHandler(nil, database)
			job := &videoJob{ID: "video_1", Model: "sora-2", Status: "completed", Dialect: videogen.DialectXAI}
			p := &models.Provider{Type: models.ProviderXAI, BaseURL: "https://api.provider.com/v1"}

			rec := httptest.NewRecorder()
			// The URL under test is the provider-supplied one; the local mock
			// exists only to prove nothing dials it when the case is foreign.
			status := h.streamVideoContent(rec, httptest.NewRequest("GET", "/v1/videos/video_1/content", nil),
				foreign, "provider-secret", p, job)
			if status != http.StatusFound {
				t.Fatalf("foreign origin = %d, want 302: %s", status, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != foreign {
				t.Fatalf("Location = %q, want the provider URL", loc)
			}
			if strings.Contains(rec.Body.String(), "SECRET") || hit {
				t.Fatal("gateway fetched a foreign URL server-side")
			}
		})
	}
}

// TestPrivateFetchRangesAreDenied pins the private-range denylist used for any
// remaining server-side content fetch.
func TestPrivateFetchRangesAreDenied(t *testing.T) {
	denied := []string{
		"127.0.0.1", "10.1.2.3", "192.168.0.1", "172.16.5.4",
		"169.254.169.254", "100.100.100.200", "::1", "fd00:ec2::254", "fc00::1",
	}
	for _, s := range denied {
		if !isPrivateFetchIP(net.ParseIP(s)) {
			t.Errorf("%s must be denied", s)
		}
	}
	if isPrivateFetchIP(net.ParseIP("8.8.8.8")) {
		t.Error("a public address must be allowed")
	}
}

// TestVideoCreateBoundsAndSanitizesUpstreamErrors covers P2-3: the raw provider
// error body used to be relayed verbatim.
func TestVideoCreateBoundsAndSanitizesUpstreamErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		// A huge, control-character-laden body carrying a credential shape.
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","type":"invalid_request_error","message":"` +
			"sk-abcdefghijklmnop" + strings.Repeat("A", 64<<10) + "\u0007\u0000" + `"}}`))
	}))
	defer upstream.Close()

	lc := &videoLifecycle{createStatus: http.StatusBadRequest, createBody: "ignored"}
	_ = lc
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	if _, err := ps.Create("oai", models.ProviderOpenAI, upstream.URL+"/v1", "k"); err != nil {
		t.Fatal(err)
	}
	k, _ := ks.Create("err-key")
	h := newLegacyHandler(ps, database)
	router := chi.NewRouter()
	router.Post("/v1/videos", h.VideoCreate)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, body := doReq(t, "POST", srv.URL+"/v1/videos", k.Key, `{"model":"oai/sora-2","prompt":"x"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("relayed status = %d: %s", resp.StatusCode, body)
	}
	if len(body) > MaxVideoCreateErrorBytes+1024 {
		t.Fatalf("relayed body is unbounded: %d bytes", len(body))
	}
	if strings.Contains(string(body), "sk-abcdefghijklmnop") {
		t.Fatalf("credential-shaped text was relayed: %s", body)
	}
	if strings.ContainsAny(string(body), "\x00\x07") {
		t.Fatal("control characters were relayed")
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("relayed error is not the standard envelope: %s", body)
	}
}

// TestVideoProgressAndStatusAreMonotonic covers P2-5: progress is clamped to
// [0,100], the status never regresses out of a later state, and a terminal job
// is never left.
func TestVideoProgressAndStatusAreMonotonic(t *testing.T) {
	h := &Handler{}
	base := func() *videoJob {
		return &videoJob{ID: "video_1", Status: videogen.StatusInProgress, Progress: 60}
	}

	// Out-of-range and regressing progress are clamped.
	job := base()
	h.applyVideoPollState(job, &normalizedVideoPoll{Status: videogen.StatusInProgress, Progress: -5})
	if job.Progress != 60 {
		t.Fatalf("progress regressed to %d", job.Progress)
	}
	h.applyVideoPollState(job, &normalizedVideoPoll{Status: videogen.StatusInProgress, Progress: 400})
	if job.Progress != 100 {
		t.Fatalf("progress = %d, want the clamp at 100", job.Progress)
	}

	// Queued never wins over in_progress.
	job = base()
	h.applyVideoPollState(job, &normalizedVideoPoll{Status: videogen.StatusQueued, Progress: 10})
	if job.Status != videogen.StatusInProgress {
		t.Fatalf("status regressed to %q", job.Status)
	}
	if job.Progress != 60 {
		t.Fatalf("progress regressed to %d", job.Progress)
	}

	// completed_at is stamped once, on the transition, and never drifts.
	job = base()
	h.applyVideoPollState(job, &normalizedVideoPoll{Status: videogen.StatusCompleted, Progress: 100})
	if job.Status != videogen.StatusCompleted || job.Completed == nil {
		t.Fatalf("completion not recorded: %+v", job)
	}
	first := *job.Completed
	// A later poll reporting anything else must not move the stamp or the state.
	h.applyVideoPollState(job, &normalizedVideoPoll{Status: videogen.StatusInProgress, Progress: 10})
	if job.Status != videogen.StatusCompleted {
		t.Fatalf("left a terminal state: %q", job.Status)
	}
	if !job.Completed.Equal(first) {
		t.Fatalf("completed_at drifted: %v -> %v", first, *job.Completed)
	}
	if job.Progress != 100 {
		t.Fatalf("completed progress = %d", job.Progress)
	}
}

// TestXAIAndVeoCreateBodiesCarryTheSentModel covers P2-4 (the xAI create body
// never set the stripped upstream id) and pins that completed_at is no longer
// synthesized by the dialect normalizers (P1-7).
func TestXAIAndVeoCreateBodiesCarryTheSentModel(t *testing.T) {
	n := &videogen.Normalized{Model: "alias-name", Prompt: "p", Seconds: "8", Size: "1280x720"}
	p := &models.Provider{Type: models.ProviderXAI, BaseURL: "https://api.x.ai/v1"}
	_, body, err := videoCreateRequest(p, videogen.DialectXAI, n, "grok-imagine-video-1.5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"model":"grok-imagine-video-1.5"`) {
		t.Fatalf("xAI create body model = %s, want the sent id", body)
	}
	if strings.Contains(string(body), "alias-name") {
		t.Fatalf("xAI create body kept the requested alias: %s", body)
	}

	pv := &models.Provider{Type: models.ProviderGoogle, BaseURL: "https://g.example"}
	_, vbody, err := videoCreateRequest(pv, videogen.DialectVeo, n, "veo-3.1-generate-preview")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(vbody), "alias-name") {
		t.Fatalf("veo body carries the alias: %s", vbody)
	}

	// The dialect normalizers must not invent completed_at: the job state
	// machine stamps it once on the transition.
	if v := videogen.VeoPollToVideo([]byte(`{"done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"https://x/v.mp4"}}]}}}`), "id", "veo", "p", "8", "1280x720"); v.CompletedAt != nil {
		t.Fatalf("VeoPollToVideo synthesized completed_at: %v", v.CompletedAt)
	}
	if v := videogen.XAIPollToVideo([]byte(`{"status":"done","video":{"url":"https://x/v.mp4"}}`), "id", "m", "p", "8", "1280x720"); v.CompletedAt != nil {
		t.Fatalf("XAIPollToVideo synthesized completed_at: %v", v.CompletedAt)
	}
}

// TestVeoDoneWithoutSampleIsTerminalFailure covers P1-7: a "done" operation with
// no generatedSamples must not report completed, and must not leave the job
// pollable forever either.
func TestVeoDoneWithoutSampleIsTerminalFailure(t *testing.T) {
	h := &Handler{}
	job := &videoJob{Dialect: videogen.DialectVeo, ID: "video_v", Model: "veo-3.1-generate-preview"}
	out := h.normalizeVideoPoll([]byte(`{"name":"op","done":true,"response":{"generateVideoResponse":{"generatedSamples":[]}}}`), job)
	if out == nil {
		t.Fatal("poll response not recognized")
	}
	if out.Status == videogen.StatusCompleted {
		t.Fatal("done without a sample reported completed")
	}
	if out.Status != videogen.StatusFailed || out.Error == nil || out.Error.Message == "" {
		t.Fatalf("want failed with an actionable message, got %+v", out)
	}

	// And the end-to-end shape: the content route answers 409 with the reason
	// instead of a 502 loop.
	job.Status = out.Status
	job.ErrCode, job.ErrMessage = out.Error.Code, out.Error.Message
	rec := httptest.NewRecorder()
	writeVideoJSON(rec, job.toVideo())
	if !strings.Contains(rec.Body.String(), "no_output") {
		t.Fatalf("poll payload = %s", rec.Body.String())
	}
}

// TestVideoTTFBBudgets pins the P1-8 budgets: create gets DefaultVideoTTFB,
// polls the normal handler budget, and content a first-byte budget plus an idle
// watchdog that is actually armed.
func TestVideoTTFBBudgets(t *testing.T) {
	h := &Handler{Timeouts: DefaultTimeouts()}
	if got := h.videoTTFB(); got != DefaultVideoTTFB {
		t.Fatalf("videoTTFB = %v, want %v", got, DefaultVideoTTFB)
	}
	if got := h.contentTTFB(); got != DefaultTimeouts().TTFB {
		t.Fatalf("contentTTFB = %v, want the handler budget %v", got, DefaultTimeouts().TTFB)
	}
	// The create budget must never exceed the configured upstream header budget.
	h.Timeouts.UpstreamHeader = 30 * time.Second
	if got := h.videoTTFB(); got != 30*time.Second {
		t.Fatalf("videoTTFB = %v, want it capped at the upstream header budget", got)
	}

	// The idle watchdog must fire on a body that stalls after its first chunk,
	// instead of pinning the handler goroutine.
	h = &Handler{Timeouts: TimeoutsConfig{StreamIdle: 25 * time.Millisecond}}
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("first-chunk"))
		// never write again, never close
	}()
	rec := httptest.NewRecorder()
	start := time.Now()
	_, err := h.copyVideoBody(rec, pr)
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("stalled download must fail with an idle error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("idle watchdog took %v", elapsed)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("the bytes received before the stall must be relayed")
	}
	_ = pw.Close()
}

// TestVideoRoutesEmitMetrics covers P2-6: poll/list/content/delete emitted no
// request or latency instruments at all, so the read half of the video API was
// invisible in Prometheus.
func TestVideoRoutesEmitMetrics(t *testing.T) {
	srv, _, key, _ := newVideoLifecycleServer(t)
	_ = srv
	// Use a handler with a recording metrics sink and drive the routes directly.
	m := &videoMetrics{}
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ps := provider.NewStore(database, make([]byte, 32))
	h := newLegacyHandler(ps, database)
	h.Metrics = m

	job := &videoJob{
		ID: "video_1", ProviderID: "p1", KeyPrefix: "p1", Dialect: videogen.DialectOpenAI,
		Model: "sora-2", UpstreamID: "up_1", Status: videogen.StatusQueued,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := h.insertVideoJob(job); err != nil {
		t.Fatal(err)
	}

	newReq := func(method, id string) *http.Request {
		r := httptest.NewRequest(method, "/v1/videos/"+id, nil)
		r.Header.Set("X-Gateway-Key-Prefix", "p1")
		return withChiID(r, id)
	}

	// poll (queued -> provider lookup fails, which still emits)
	h.VideoGet(httptest.NewRecorder(), newReq("GET", job.ID))
	// list
	h.VideoList(httptest.NewRecorder(), newReq("GET", ""))
	// content (not ready -> 409)
	h.VideoContent(httptest.NewRecorder(), newReq("GET", job.ID))
	// delete
	h.VideoDelete(httptest.NewRecorder(), newReq("DELETE", job.ID))

	for _, want := range []string{"videos.poll", "videos.list", "videos.content", "videos.delete"} {
		if !m.sawEndpoint(want) {
			t.Errorf("no request metrics emitted for %s (saw %v)", want, m.endpoints())
		}
	}
	if !m.sawLatency("videos.poll") {
		t.Error("no latency observed for videos.poll")
	}
	_ = key
}

// withChiID attaches the chi route parameter the handlers read.
func withChiID(r *http.Request, id string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// TestVideoCreateHonoursModelOverride covers P1-5 for videos: the upstream must
// receive (and the gateway must bill) the routing rule's model_override, not the
// alias the client asked for. Pre-fix the create path captured the requested
// model before candidate resolution.
func TestVideoCreateHonoursModelOverride(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotModel, _ = b["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"up_v1","object":"video","status":"queued","created_at":1700000000}`))
	}))
	defer upstream.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	p, err := ps.Create("oai", models.ProviderOpenAI, upstream.URL+"/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	lbs := lb.NewStore(database)
	if err := lbs.ReplaceRule("vid-alias", "", []lb.RuleMemberInput{
		{ProviderID: p.ID, ModelOverride: "sora-2-pro"},
	}); err != nil {
		t.Fatalf("replace rule: %v", err)
	}
	k, _ := ks.Create("vid-key")
	h := newLegacyHandler(ps, database)
	h.LB = lbs
	r := chi.NewRouter()
	r.Post("/v1/videos", h.VideoCreate)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, body := doReq(t, "POST", srv.URL+"/v1/videos", k.Key, `{"model":"vid-alias","prompt":"x","seconds":"8"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if gotModel != "sora-2-pro" {
		t.Fatalf("upstream model = %q, want the rule override", gotModel)
	}
	// Billing must follow the model actually sent: sora-2-pro is $0.30/s.
	var model string
	var cost float64
	if err := database.QueryRow(`SELECT model, COALESCE(cost_usd,0) FROM request_logs ORDER BY created_at DESC LIMIT 1`).Scan(&model, &cost); err != nil {
		t.Fatalf("request_logs: %v", err)
	}
	if model != "sora-2-pro" {
		t.Fatalf("billed model = %q, want the override", model)
	}
	if cost < 2.39 || cost > 2.41 {
		t.Fatalf("cost = %v, want ~2.40 (8s at sora-2-pro rates)", cost)
	}
	// The stored job records the model actually used.
	var jobModel string
	if err := database.QueryRow(`SELECT model FROM video_jobs LIMIT 1`).Scan(&jobModel); err != nil {
		t.Fatal(err)
	}
	if jobModel != "sora-2-pro" {
		t.Fatalf("job model = %q", jobModel)
	}
}

// TestVideoGetChecksOrgScope covers P2-1's org half: an org-scoped key must not
// poll or download a job whose provider belongs to a different org (the provider
// key would otherwise be decrypted and used on its behalf). The auth middleware
// is exercised for real, since the org scope comes from the authenticated key.
func TestVideoGetChecksOrgScope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"up_v1","status":"in_progress","progress":10}`))
	}))
	defer upstream.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	// A real org row: gateway_keys.org_id is a foreign key.
	if _, err := database.Exec(`INSERT INTO organizations(id,name,created_at) VALUES('org-a','Org A',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("create org: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO organizations(id,name,created_at) VALUES('org-b','Org B',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("create org: %v", err)
	}
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	p, err := ps.Create("oai", models.ProviderOpenAI, upstream.URL+"/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	// Reassign the provider to another org: org-a's job now points at org-b's
	// provider.
	if _, err := database.Exec(`UPDATE providers SET org_id='org-b' WHERE id=?`, p.ID); err != nil {
		t.Fatal(err)
	}
	k, err := ks.CreateWithOrg("org-key", "org-a")
	if err != nil {
		t.Fatalf("create org key: %v", err)
	}
	h := newLegacyHandler(ps, database)
	job := &videoJob{
		ID: "video_1", ProviderID: p.ID, KeyPrefix: k.Prefix, Dialect: videogen.DialectOpenAI,
		Model: "sora-2", UpstreamID: "up_1", Status: videogen.StatusInProgress,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := h.insertVideoJob(job); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Get("/v1/videos/{id}", h.VideoGet)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, body := doReq(t, "GET", srv.URL+"/v1/videos/"+job.ID, k.Key, "")
	if resp.StatusCode != 404 {
		t.Fatalf("cross-org poll = %d, want 404 (fail closed): %s", resp.StatusCode, body)
	}
}

// TestPrivateHostFetchPolicy proves the private-range denylist applies to the
// one remaining server-side fetch: a content URL on a private address is refused
// unless the operator pointed the PROVIDER at that same internal host (homelab
// vLLM / local gateways), which is the only legitimate case.
func TestPrivateHostFetchPolicy(t *testing.T) {
	cases := []struct {
		name       string
		contentURL string
		baseURL    string
		want       bool
	}{
		{"public provider, public content", "https://api.provider.com/v.mp4", "https://api.provider.com/v1", true},
		{"loopback provider, loopback content", "http://127.0.0.1:9000/v.mp4", "http://127.0.0.1:9000", true},
		{"homelab provider, homelab content", "http://192.168.1.9:8080/v.mp4", "http://192.168.1.9:8080/v1", true},
		{"homelab provider, loopback content", "http://127.0.0.1:8080/v.mp4", "http://192.168.1.9:8080/v1", false},
		{"empty", "", "https://api.provider.com", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := privateFetchPermitted(c.contentURL, c.baseURL); got != c.want {
				t.Fatalf("privateFetchPermitted(%q,%q) = %v, want %v", c.contentURL, c.baseURL, got, c.want)
			}
		})
	}

	// And end to end: a same-origin-looking URL that resolves into a private
	// range is refused instead of fetched and relayed.
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	h := newLegacyHandler(nil, database)
	job := &videoJob{ID: "video_1", Model: "sora-2", Status: "completed"}
	p := &models.Provider{Type: models.ProviderXAI, BaseURL: "http://169.254.169.254/v1"}
	rec := httptest.NewRecorder()
	status := h.streamVideoContent(rec, httptest.NewRequest("GET", "/v1/videos/video_1/content", nil),
		"http://169.254.169.254/latest/meta-data/", "secret", p, job)
	if status != http.StatusBadGateway {
		t.Fatalf("metadata content fetch = %d, want 502 refusal: %s", status, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ami-") {
		t.Fatal("metadata response was relayed")
	}
}
