package proxy

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/otel"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

// videoLifecycleServer wires a video gateway whose mock upstream lets a test
// control every leg of the async lifecycle (create / poll / content) and count
// how often each leg is hit.
type videoLifecycle struct {
	polls        atomic.Int32
	creates      atomic.Int32
	contentCalls atomic.Int32
	// pollBody is served for GET {base}/videos/{id}; set before the request.
	pollBody string
	// createBody is served for POST {base}/videos.
	createBody string
	// contentStatus forces a non-2xx content response when non-zero.
	contentStatus int
	// pollStatus forces a non-2xx poll response when non-zero.
	pollStatus int
	// createStatus forces a non-2xx create response when non-zero.
	createStatus int
}

func newVideoLifecycleServer(t *testing.T) (*httptest.Server, *sql.DB, string, *videoLifecycle) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	lc := &videoLifecycle{
		createBody: `{"id":"up_v1","object":"video","status":"queued","progress":0,"created_at":1700000000}`,
		pollBody:   `{"id":"up_v1","object":"video","status":"in_progress","progress":40,"created_at":1700000000}`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/videos"):
			lc.creates.Add(1)
			if lc.createStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(lc.createStatus)
				_, _ = w.Write([]byte(lc.createBody))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(lc.createBody))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/content"):
			lc.contentCalls.Add(1)
			if lc.contentStatus != 0 {
				w.WriteHeader(lc.contentStatus)
				_, _ = w.Write([]byte(`{"error":{"message":"gone"}}`))
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("MP4"))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/videos/"):
			lc.polls.Add(1)
			if lc.pollStatus != 0 {
				w.WriteHeader(lc.pollStatus)
				_, _ = w.Write([]byte(`{"error":{"message":"upstream exploded"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(lc.pollBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	if _, err := ps.Create("oai", models.ProviderOpenAI, upstream.URL+"/v1", "upstream-key"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("video-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	r := chi.NewRouter()
	r.Post("/v1/videos", h.VideoCreate)
	r.Get("/v1/videos", h.VideoList)
	r.Get("/v1/videos/{id}", h.VideoGet)
	r.Get("/v1/videos/{id}/content", h.VideoContent)
	r.Delete("/v1/videos/{id}", h.VideoDelete)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, database, k.Key, lc
}

// createJob is a helper: create a job and return its gateway id.
func createJob(t *testing.T, srvURL, key, prefix string) string {
	t.Helper()
	resp, body := doReqAs(t, "POST", srvURL+"/v1/videos", key, prefix,
		`{"model":"oai/sora-2","prompt":"a cat","seconds":"8"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v["id"].(string)
}

// TestVideoCrossKeyOwnershipOnEveryRoute is the full ownership matrix for
// video_jobs: another gateway key must not be able to poll, download, list or
// delete a job it did not create. The existing scoping test covers GET,
// content and list; delete is the destructive one and was untested.
func TestVideoCrossKeyOwnershipOnEveryRoute(t *testing.T) {
	srv, _, key, _ := newVideoLifecycleServer(t)
	id := createJob(t, srv.URL, key, "prefix-A")

	// Owner can delete.
	resp, body := doReqAs(t, "DELETE", srv.URL+"/v1/videos/"+id, key, "prefix-A", "")
	if resp.StatusCode == 404 {
		t.Fatalf("owner delete rejected: %s", body)
	}
	// Recreate for the negative assertions.
	id = createJob(t, srv.URL, key, "prefix-A")

	for _, c := range []struct{ name, method, path string }{
		{"get", http.MethodGet, "/v1/videos/" + id},
		{"content", http.MethodGet, "/v1/videos/" + id + "/content"},
		{"delete", http.MethodDelete, "/v1/videos/" + id},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp, body := doReqAs(t, c.method, srv.URL+c.path, key, "prefix-B", "")
			if resp.StatusCode != 404 {
				t.Fatalf("cross-key %s = %d, want 404: %s", c.name, resp.StatusCode, body)
			}
			if strings.Contains(string(body), "forbidden") {
				t.Fatalf("cross-key must not confirm existence: %s", body)
			}
		})
	}
	// The job must still exist and still be the owner's.
	resp, body = doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "prefix-A", "")
	if resp.StatusCode != 200 {
		t.Fatalf("job disappeared after a cross-key delete attempt: %d %s", resp.StatusCode, body)
	}
}

// TestVideoEmptyPrefixCannotDeleteForeignJob proves the ownership check fails
// CLOSED (P2-1): a request without a gateway key prefix — i.e. no gateway auth
// in front of the route, which the middleware never produces for a real caller —
// cannot read or delete anybody's job. Pre-fix an empty prefix saw everything.
func TestVideoEmptyPrefixCannotDeleteForeignJob(t *testing.T) {
	srv, _, key, _ := newVideoLifecycleServer(t)
	id := createJob(t, srv.URL, key, "prefix-A")

	resp, body := doReqAs(t, "DELETE", srv.URL+"/v1/videos/"+id, key, "", "")
	if resp.StatusCode != 404 {
		t.Fatalf("empty-prefix delete = %d, want 404: %s", resp.StatusCode, body)
	}
	resp, body = doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "", "")
	if resp.StatusCode != 404 {
		t.Fatalf("empty-prefix read = %d, want 404: %s", resp.StatusCode, body)
	}
	// The job is untouched.
	if resp, body := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "prefix-A", ""); resp.StatusCode != 200 {
		t.Fatalf("owner read after the attempt = %d: %s", resp.StatusCode, body)
	}
}

// TestVideoJobWithoutKeyPrefixIsNotPublic pins the second half of the hardening:
// a job row with an empty key_prefix (created before keys were recorded) is NOT
// readable by an arbitrary key. Fail-closed, not fail-open.
func TestVideoJobWithoutKeyPrefixIsNotPublic(t *testing.T) {
	srv, _, key, _ := newVideoLifecycleServer(t)
	id := createJob(t, srv.URL, key, "") // no prefix recorded
	resp, body := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "prefix-B", "")
	if resp.StatusCode != 404 {
		t.Fatalf("prefix-less job must not be publicly readable: %d %s", resp.StatusCode, body)
	}
}

// TestVideoExpiredJobIsGone proves expires_at is ENFORCED (P1-6): an expired job
// answers 410 Gone (not 200 with a stale completed status), the row is marked
// terminal locally, and neither the poll nor the content route hits the provider
// again.
func TestVideoExpiredJobIsGone(t *testing.T) {
	srv, database, key, lc := newVideoLifecycleServer(t)
	id := createJob(t, srv.URL, key, "p1")

	// Force the job terminal and expired an hour ago.
	lc.pollBody = `{"id":"up_v1","object":"video","status":"completed","progress":100,"created_at":1700000000,"completed_at":1700000060,"expires_at":1700000060,"error":null}`
	if _, body := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "p1", ""); !strings.Contains(string(body), "completed") {
		t.Fatalf("poll did not complete the job: %s", body)
	}
	pollsBefore := lc.polls.Load()
	contentBefore := lc.contentCalls.Load()

	resp, body := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "p1", "")
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("expired job poll = %d, want 410: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "expired") {
		t.Fatalf("410 must explain the expiry: %s", body)
	}
	// The row is now terminal-failed with the expiry reason.
	var status, code string
	if err := database.QueryRow(`SELECT status, COALESCE(error_code,'') FROM video_jobs WHERE id=?`, id).Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || code != "expired" {
		t.Fatalf("expired job row = %s/%s, want failed/expired", status, code)
	}

	// Content is a local 410 too, with no provider round trip.
	resp, body = doReqAs(t, "GET", srv.URL+"/v1/videos/"+id+"/content", key, "p1", "")
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("expired content = %d, want 410: %s", resp.StatusCode, body)
	}
	if lc.polls.Load() != pollsBefore || lc.contentCalls.Load() != contentBefore {
		t.Fatalf("expired job re-hit the provider (polls %d->%d, content %d->%d)",
			pollsBefore, lc.polls.Load(), contentBefore, lc.contentCalls.Load())
	}
}

// TestVideoUpstreamGoneTerminatesJob proves a provider 404/410 on poll or
// content transitions the job to a terminal failed state instead of looping
// (P1-6). Pre-fix the stored job stayed "completed" forever and every poll and
// content GET re-hit the provider.
func TestVideoUpstreamGoneTerminatesJob(t *testing.T) {
	for _, gone := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run("poll "+http.StatusText(gone), func(t *testing.T) {
			srv, database, key, lc := newVideoLifecycleServer(t)
			id := createJob(t, srv.URL, key, "p1")
			lc.pollBody = `{"id":"up_v1","status":"completed","progress":100,"created_at":1700000000,"completed_at":1700000060,"error":null}`
			if _, body := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "p1", ""); !strings.Contains(string(body), "completed") {
				t.Fatalf("setup poll: %s", body)
			}
			lc.contentStatus = gone
			resp, body := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id+"/content", key, "p1", "")
			if resp.StatusCode != http.StatusGone {
				t.Fatalf("content after a provider %d = %d: %s", gone, resp.StatusCode, body)
			}
			var status, code string
			if err := database.QueryRow(`SELECT status, COALESCE(error_code,'') FROM video_jobs WHERE id=?`, id).Scan(&status, &code); err != nil {
				t.Fatal(err)
			}
			if status != "failed" || code != "expired" {
				t.Fatalf("row after a provider %d = %s/%s, want failed/expired", gone, status, code)
			}
			// The terminal row now answers from storage: a 410 (the artefact is
			// gone), never another provider call.
			calls := lc.contentCalls.Load()
			resp, body = doReqAs(t, "GET", srv.URL+"/v1/videos/"+id+"/content", key, "p1", "")
			if resp.StatusCode != http.StatusGone {
				t.Fatalf("second content fetch = %d, want 410 from the row: %s", resp.StatusCode, body)
			}
			if lc.contentCalls.Load() != calls {
				t.Fatal("a terminal job re-hit the provider content route")
			}
		})
	}

	t.Run("live poll", func(t *testing.T) {
		srv, database, key, lc := newVideoLifecycleServer(t)
		id := createJob(t, srv.URL, key, "p1")
		lc.pollStatus = http.StatusNotFound
		resp, body := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "p1", "")
		if resp.StatusCode != 200 {
			t.Fatalf("poll = %d, want the stored (now terminal) row: %s", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "failed") {
			t.Fatalf("a provider 404 must terminate the job: %s", body)
		}
		var status string
		if err := database.QueryRow(`SELECT status FROM video_jobs WHERE id=?`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "failed" {
			t.Fatalf("row status = %q, want failed", status)
		}
		// And it stops polling upwards.
		polls := lc.polls.Load()
		_, _ = doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "p1", "")
		if lc.polls.Load() != polls {
			t.Fatal("a terminally failed job kept polling the provider")
		}
	})
}

// TestVideoContentUpstreamErrorsAreMapped proves a provider content failure is
// mapped onto an outbound status (5xx stays 5xx, and an out-of-range status is
// normalized to 502) instead of being relayed raw or answered 200.
func TestVideoContentUpstreamErrorsAreMapped(t *testing.T) {
	for _, upstream := range []int{http.StatusInternalServerError, http.StatusForbidden} {
		t.Run(http.StatusText(upstream), func(t *testing.T) {
			srv, _, key, lc := newVideoLifecycleServer(t)
			id := createJob(t, srv.URL, key, "p1")
			lc.pollBody = `{"id":"up_v1","object":"video","status":"completed","progress":100,"created_at":1700000000,"completed_at":1700000060,"error":null}`
			if _, body := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "p1", ""); !strings.Contains(string(body), "completed") {
				t.Fatalf("setup poll: %s", body)
			}
			lc.contentStatus = upstream
			resp, body := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id+"/content", key, "p1", "")
			if resp.StatusCode != upstream {
				t.Fatalf("content upstream %d -> client %d: %s", upstream, resp.StatusCode, body)
			}
			if !strings.Contains(string(body), "proxy_error") {
				t.Fatalf("content failure must use the httperr envelope: %s", body)
			}
		})
	}
}

// TestVideoPollUpstreamFailureServesLastKnownState proves a transient provider
// poll failure does not break the client's poll: the stored state is returned
// instead of a 5xx, and the job stays pollable.
func TestVideoPollUpstreamFailureServesLastKnownState(t *testing.T) {
	srv, _, key, lc := newVideoLifecycleServer(t)
	id := createJob(t, srv.URL, key, "p1")

	lc.pollStatus = http.StatusBadGateway
	resp, body := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "p1", "")
	if resp.StatusCode != 200 {
		t.Fatalf("poll with failing upstream = %d, want last-known 200: %s", resp.StatusCode, body)
	}
	var v map[string]any
	_ = json.Unmarshal(body, &v)
	if v["status"] != "queued" {
		t.Fatalf("status = %v, want the stored queued state", v["status"])
	}

	// Once upstream recovers the job advances (nothing was wedged).
	lc.pollStatus = 0
	lc.pollBody = `{"id":"up_v1","object":"video","status":"in_progress","progress":55,"created_at":1700000000}`
	_, body = doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "p1", "")
	if !strings.Contains(string(body), `"progress":55`) {
		t.Fatalf("recovered poll = %s", body)
	}
}

// TestVideoStatusNormalizedFromDialectVocabularies proves a reseller answering
// "succeeded"/"processing" is normalized on the way into storage (so the
// terminal fast-path and the content gate both work).
func TestVideoStatusNormalizedFromDialectVocabularies(t *testing.T) {
	cases := []struct{ upstream, want string }{
		{"succeeded", "completed"},
		{"processing", "in_progress"},
		{"pending", "queued"},
		{"error", "failed"},
		{"a-vocabulary-nobody-has-seen", "in_progress"},
	}
	for _, c := range cases {
		t.Run(c.upstream, func(t *testing.T) {
			srv, _, key, lc := newVideoLifecycleServer(t)
			id := createJob(t, srv.URL, key, "p1")
			lc.pollBody = `{"id":"up_v1","object":"video","status":"` + c.upstream + `","progress":10,"created_at":1700000000}`
			_, body := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "p1", "")
			var v map[string]any
			if err := json.Unmarshal(body, &v); err != nil {
				t.Fatalf("poll body %s: %v", body, err)
			}
			if v["status"] != c.want {
				t.Fatalf("upstream %q -> %v, want %q", c.upstream, v["status"], c.want)
			}
		})
	}
}

// TestVideoCreateUpstreamErrorIsRelayed proves two distinct create-failure
// contracts: (a) a provider that rejects the request has its own status and
// message relayed verbatim, and (b) a 2xx create that carries no job handle is
// a gateway-side 502 rather than a persisted unusable job.
func TestVideoCreateUpstreamErrorIsRelayed(t *testing.T) {
	t.Run("upstream refusal relayed", func(t *testing.T) {
		srv, database, key, lc := newVideoLifecycleServer(t)
		lc.createStatus = http.StatusBadRequest
		lc.createBody = `{"error":{"message":"model does not exist","type":"invalid_request_error"}}`
		resp, body := doReqAs(t, "POST", srv.URL+"/v1/videos", key, "p1",
			`{"model":"oai/sora-2","prompt":"a cat"}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("upstream 400 -> %d, want 400 relayed: %s", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "model does not exist") {
			t.Fatalf("upstream message lost: %s", body)
		}
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM video_jobs`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("a failed enqueue persisted %d video job(s)", n)
		}
	})

	t.Run("missing job handle is a 502", func(t *testing.T) {
		srv, database, key, lc := newVideoLifecycleServer(t)
		lc.createBody = `{"object":"video","status":"queued"}`
		resp, body := doReqAs(t, "POST", srv.URL+"/v1/videos", key, "p1",
			`{"model":"oai/sora-2","prompt":"a cat"}`)
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("create without a job handle = %d, want 502: %s", resp.StatusCode, body)
		}
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM video_jobs`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("a handle-less create persisted %d video job(s)", n)
		}
	})
}

// videoMetrics is a minimal otel.Metrics sink that records the video routes'
// endpoints and latencies (the shared recordingMetrics in zerocost_test.go
// records zero-cost reasons instead).
type videoMetrics struct {
	mu      sync.Mutex
	reqs    []string
	latency []string
}

func (m *videoMetrics) IncRequests(provider, model, endpoint string, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs = append(m.reqs, endpoint)
}

func (m *videoMetrics) ObserveLatency(provider, endpoint string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latency = append(m.latency, endpoint)
}

func (m *videoMetrics) IncCacheHit(bool)                   {}
func (m *videoMetrics) IncZeroCost(string)                 {}
func (m *videoMetrics) ObserveRequest(otel.RequestMetrics) {}

func (m *videoMetrics) sawEndpoint(endpoint string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.reqs {
		if e == endpoint {
			return true
		}
	}
	return false
}

func (m *videoMetrics) sawLatency(endpoint string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.latency {
		if e == endpoint {
			return true
		}
	}
	return false
}

func (m *videoMetrics) endpoints() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.reqs...)
}
