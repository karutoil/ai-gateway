package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/videogen"

	"github.com/go-chi/chi/v5"
)

// xaiVideoServer wires an xAI-typed provider whose mock implements the Grok
// Imagine video dialect: POST /videos/generations -> {request_id}, GET
// /videos/{id} -> {status:"done", video:{url}}, and a CDN content URL that is
// NOT on the provider origin (the gateway must not attach the provider key).
type xaiDialect struct {
	createPath, pollPath, gotAuth string
	pollCalls                     atomic.Int32
	contentCalls                  atomic.Int32
	cdnURL                        string
	cdnHits                       int32
	cdnAuth                       string
}

func newXAIVideoServer(t *testing.T) (*httptest.Server, *xaiDialect, string) {
	t.Helper()
	d := &xaiDialect{}

	// The CDN lives on a different origin; a request there must carry no key.
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&d.cdnHits, 1)
		d.cdnAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("CDNMP4"))
	}))
	t.Cleanup(cdn.Close)
	d.cdnURL = cdn.URL + "/x.mp4"

	providerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/videos/generations"):
			d.createPath = r.URL.Path
			d.gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"request_id":"xai_req_1"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/videos/"):
			d.pollCalls.Add(1)
			d.pollPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"done","model":"grok-imagine-video","video":{"url":"` + d.cdnURL + `","duration":6,"respect_moderation":true}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(providerSrv.Close)

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	if _, err := ps.Create("xai", models.ProviderXAI, providerSrv.URL+"/v1", "xai-secret"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("xai-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	r := chi.NewRouter()
	r.Post("/v1/videos", h.VideoCreate)
	r.Get("/v1/videos/{id}", h.VideoGet)
	r.Get("/v1/videos/{id}/content", h.VideoContent)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, d, k.Key
}

// TestVideoXAIDialectCreatePollContent proves the xAI dialect end to end:
//   - create goes to /videos/generations with the Bearer key and the
//     aspect_ratio/resolution derived from `size`;
//   - the request_id becomes the stored upstream_id;
//   - "done" is normalized to "completed";
//   - content is fetched from the provider-supplied CDN URL WITHOUT the
//     provider key (it is a third-party origin).
func TestVideoXAIDialectCreatePollContent(t *testing.T) {
	srv, d, key := newXAIVideoServer(t)

	resp, body := doReqAs(t, "POST", srv.URL+"/v1/videos", key, "p1",
		`{"model":"xai/grok-imagine-video","prompt":"a cat","seconds":"8","size":"1280x720"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if d.createPath != "/v1/videos/generations" {
		t.Fatalf("create path = %q", d.createPath)
	}
	if d.gotAuth != "Bearer xai-secret" {
		t.Fatalf("create auth = %q", d.gotAuth)
	}
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	id := created["id"].(string)

	// Poll: upstream "done" must surface as "completed".
	_, pollBody := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "p1", "")
	var polled map[string]any
	if err := json.Unmarshal(pollBody, &polled); err != nil {
		t.Fatalf("poll body: %s", pollBody)
	}
	if polled["status"] != "completed" {
		t.Fatalf("status = %v, want completed: %s", polled["status"], pollBody)
	}
	if d.pollPath != "/v1/videos/xai_req_1" {
		t.Fatalf("poll path = %q (must use the upstream request_id)", d.pollPath)
	}
	// The terminal job must not be re-polled upstream.
	after := d.pollCalls.Load()
	_, _ = doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "p1", "")
	if d.pollCalls.Load() != after {
		t.Fatal("terminal xAI job re-polled upstream")
	}

	// Content: the CDN URL is NOT on the provider origin, so the gateway must
	// neither fetch it server-side nor attach the provider credential — it
	// redirects the client there instead (P1-1). The redirect is not followed
	// here: the point is that the GATEWAY makes no request at all.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/videos/"+id+"/content", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Gateway-Key-Prefix", "p1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("foreign-origin content = %d, want 302: %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, d.cdnURL) {
		t.Fatalf("redirect location = %q, want the provider-supplied CDN URL", loc)
	}
	if d.cdnHits != 0 {
		t.Fatalf("gateway fetched a third-party content URL %d time(s)", d.cdnHits)
	}
	if d.cdnAuth != "" {
		t.Fatalf("provider key leaked to a third-party CDN: %q", d.cdnAuth)
	}
}

// TestVideoXAIModerationRejectionIsFailed proves the poll normalizer turns an
// xAI "done" response that carries no video URL (moderation block) into a
// failed job, so the content route answers 409 with the reason instead of a
// completed job whose download 502s.
func TestVideoXAIModerationRejectionIsFailed(t *testing.T) {
	h := &Handler{}
	job := &videoJob{Dialect: videogen.DialectXAI, ID: "video_x", Model: "grok-imagine-video"}
	out := h.normalizeVideoPoll([]byte(`{"status":"done","video":{"url":"","respect_moderation":false}}`), job)
	if out == nil {
		t.Fatal("poll response not recognized")
	}
	if out.Status != videogen.StatusFailed || out.Error == nil {
		t.Fatalf("status=%q error=%v, want failed with a reason", out.Status, out.Error)
	}
	if !strings.Contains(strings.ToLower(out.Error.Message), "moderation") {
		t.Fatalf("error message should mention moderation: %+v", out.Error)
	}
}

// veoServer wires a Google provider whose mock implements the Veo
// predictLongRunning dialect.
type veoDialect struct {
	url                                string
	createPath, pollPath, gotKeyHeader string
	contentAuth                        string
	contentCalls                       atomic.Int32
}

func newVeoVideoServer(t *testing.T) (*httptest.Server, *veoDialect, string) {
	t.Helper()
	d := &veoDialect{}
	providerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ":predictLongRunning"):
			d.createPath = r.URL.Path
			d.gotKeyHeader = r.Header.Get("x-goog-api-key")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"models/veo-3.1/operations/op-1"}`))
		case strings.Contains(r.URL.Path, "/operations/"):
			d.pollPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			// A finished long-running operation: done + one generated sample.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "models/veo-3.1/operations/op-1",
				"done": true,
				"response": map[string]any{
					"generateVideoResponse": map[string]any{
						"generatedSamples": []any{
							map[string]any{"video": map[string]any{"uri": d.origin(r) + "/v1beta/files/x:download"}},
						},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, ":download"):
			d.contentCalls.Add(1)
			d.contentAuth = firstNonEmptyCredential(r)
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("VEOMP4"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(providerSrv.Close)
	d.url = providerSrv.URL

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	if _, err := ps.Create("goog", models.ProviderGoogle, providerSrv.URL, "goog-key"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("veo-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	r := chi.NewRouter()
	r.Post("/v1/videos", h.VideoCreate)
	r.Get("/v1/videos/{id}", h.VideoGet)
	r.Get("/v1/videos/{id}/content", h.VideoContent)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, d, k.Key
}

// firstNonEmptyCredential returns whichever provider credential header the
// request carries, so a test can assert presence without pinning the scheme.
func firstNonEmptyCredential(r *http.Request) string {
	if v := r.Header.Get("x-goog-api-key"); v != "" {
		return v
	}
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// origin is a tiny helper so the mock can embed its own base URL in the URIs it
// returns (the struct is populated with the server URL in the constructor).
func (d *veoDialect) origin(r *http.Request) string {
	if d.url != "" {
		return d.url
	}
	return "http://" + r.Host
}

// TestVideoVeoOperationTargets pins the URL builders for the Veo dialect, which
// had no coverage at all (googleVeoTarget / googleVeoOperationURL at 0%).
func TestVideoVeoOperationTargets(t *testing.T) {
	cases := []struct{ base, want string }{
		{"https://generativelanguage.googleapis.com", "https://generativelanguage.googleapis.com/v1beta/models/veo-3.1-generate-preview:predictLongRunning"},
		{"https://generativelanguage.googleapis.com/v1beta", "https://generativelanguage.googleapis.com/v1beta/models/veo-3.1-generate-preview:predictLongRunning"},
		{"https://generativelanguage.googleapis.com/v1beta/models", "https://generativelanguage.googleapis.com/v1beta/models/veo-3.1-generate-preview:predictLongRunning"},
		{"https://generativelanguage.googleapis.com/v1", "https://generativelanguage.googleapis.com/v1beta/models/veo-3.1-generate-preview:predictLongRunning"},
	}
	for _, c := range cases {
		if got := googleVeoTarget(c.base, "veo-3.1-generate-preview"); got != c.want {
			t.Errorf("googleVeoTarget(%q) = %q, want %q", c.base, got, c.want)
		}
	}
	op := []struct{ base, name, want string }{
		{"https://generativelanguage.googleapis.com", "models/veo-3.1/operations/abc",
			"https://generativelanguage.googleapis.com/v1beta/models/veo-3.1/operations/abc"},
		{"https://generativelanguage.googleapis.com/v1beta", "/models/veo-3.1/operations/abc",
			"https://generativelanguage.googleapis.com/v1beta/models/veo-3.1/operations/abc"},
		{"https://generativelanguage.googleapis.com/v1beta/models", "models/x/operations/y",
			"https://generativelanguage.googleapis.com/v1beta/models/x/operations/y"},
	}
	for _, c := range op {
		if got := googleVeoOperationURL(c.base, c.name); got != c.want {
			t.Errorf("googleVeoOperationURL(%q,%q) = %q, want %q", c.base, c.name, got, c.want)
		}
	}
}

// TestVideoVeoDialectEndToEnd proves create -> operation poll -> content for the
// Google dialect, including x-goog-api-key placement (never Bearer) and that a
// Veo model on a Google-typed provider selects the Veo dialect.
func TestVideoVeoDialectEndToEnd(t *testing.T) {
	srv, d, key := newVeoVideoServer(t)

	resp, body := doReqAs(t, "POST", srv.URL+"/v1/videos", key, "p1",
		`{"model":"goog/veo-3.1-generate-preview","prompt":"a cat","seconds":"8","size":"1280x720"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if !strings.HasSuffix(d.createPath, "veo-3.1-generate-preview:predictLongRunning") {
		t.Fatalf("create path = %q", d.createPath)
	}
	if d.gotKeyHeader != "goog-key" {
		t.Fatalf("x-goog-api-key = %q", d.gotKeyHeader)
	}
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	id := created["id"].(string)

	_, pollBody := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "p1", "")
	var polled map[string]any
	if err := json.Unmarshal(pollBody, &polled); err != nil {
		t.Fatalf("poll: %s", pollBody)
	}
	if polled["status"] != "completed" || polled["progress"].(float64) != 100 {
		t.Fatalf("veo poll = %s", pollBody)
	}
	if !strings.Contains(d.pollPath, "/v1beta/models/veo-3.1/operations/op-1") {
		t.Fatalf("operation poll path = %q", d.pollPath)
	}

	resp, content := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id+"/content", key, "p1", "")
	if resp.StatusCode != 200 || string(content) != "VEOMP4" {
		t.Fatalf("content: %d %q", resp.StatusCode, content)
	}
	// A same-origin download must carry the provider credential exactly once.
	// NOTE: which header is used depends on isGoogleNativeAPI, which matches the
	// host or a :generateContent/:predict path — a Google file URI (`…/files/x:download`)
	// on the real host matches, but on a non-google-named host it falls back to
	// Bearer. Documented as a P3 observation in .review/tests.md.
	if d.contentAuth != "goog-key" {
		t.Fatalf("same-origin content download must carry the provider key, got %q", d.contentAuth)
	}
}
