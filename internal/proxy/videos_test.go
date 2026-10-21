package proxy

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

// videoServer wires a gateway with an OpenAI-compatible provider whose mock
// upstream implements the async job lifecycle: create -> poll -> content.
func videoServer(t *testing.T) (*httptest.Server, *sql.DB, string, *int) {
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

	polls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/videos"):
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "up_video_1", "object": "video", "model": b["model"],
				"status": "queued", "progress": 0, "created_at": 1700000000,
				"size": b["size"], "seconds": b["seconds"], "prompt": b["prompt"],
				"completed_at": nil, "expires_at": nil, "error": nil,
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/content"):
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("FAKEMP4BYTES"))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/videos/"):
			polls++
			w.Header().Set("Content-Type", "application/json")
			// Completes on the first poll.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "up_video_1", "object": "video", "model": "sora-2",
				"status": "completed", "progress": 100, "created_at": 1700000000,
				"size": "1280x720", "seconds": "8", "prompt": "a cat",
				"completed_at": 1700000060, "expires_at": nil, "error": nil,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	if _, err := ps.Create("oai", models.ProviderOpenAI, upstream.URL+"/v1", "upstream-key"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("test-key")
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
	t.Cleanup(func() { database.Close() })
	return srv, database, k.Key, &polls
}

// doReq is doReqAs with the key prefix the auth middleware would derive for the
// key. videoJobVisible fails CLOSED on an empty prefix (P2-1), so a request
// without one is rejected; the helper therefore sends a stable per-key prefix
// exactly like production does.
func doReq(t *testing.T, method, url, key string, body string) (*http.Response, []byte) {
	return doReqAs(t, method, url, key, keyPrefixFor(key), body)
}

// keyPrefixFor mirrors apikey.Prefix for the test keys.
func keyPrefixFor(key string) string {
	if len(key) > 12 {
		return key[:12]
	}
	return key
}

// doReqAs is doReq with an explicit gateway-key prefix, mirroring the header
// the auth middleware sets in production (the test router has no middleware).
func doReqAs(t *testing.T, method, url, key, prefix, body string) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Authorization", "Bearer "+key)
	if prefix != "" {
		req.Header.Set("X-Gateway-Key-Prefix", prefix)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

// TestVideoCreateReturnsQueuedJob proves create is async: it returns a job in
// state "queued" (not a video), with the OpenAI resource shape.
func TestVideoCreateReturnsQueuedJob(t *testing.T) {
	srv, database, key, _ := videoServer(t)
	resp, body := doReq(t, "POST", srv.URL+"/v1/videos", key,
		`{"model":"oai/sora-2","prompt":"a cat","seconds":"8","size":"1280x720"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if v["object"] != "video" || v["status"] != "queued" {
		t.Fatalf("unexpected job: %s", body)
	}
	if !strings.HasPrefix(v["id"].(string), "video_") {
		t.Fatalf("id = %v, want a video_ prefix", v["id"])
	}
	if v["progress"].(float64) != 0 {
		t.Fatalf("progress = %v", v["progress"])
	}
	// The upstream job handle must be persisted, not the gateway id.
	var upstreamID, dialect string
	if err := database.QueryRow(`SELECT upstream_id, dialect FROM video_jobs`).Scan(&upstreamID, &dialect); err != nil {
		t.Fatal(err)
	}
	if upstreamID != "up_video_1" || dialect != "openai" {
		t.Fatalf("persisted upstream=%q dialect=%q", upstreamID, dialect)
	}
	// Billing: 8 seconds of sora-2 at $0.10/s.
	var cost float64
	if err := database.QueryRow(`SELECT cost_usd FROM request_logs ORDER BY created_at DESC LIMIT 1`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost < 0.79 || cost > 0.81 {
		t.Fatalf("cost = %v, want ~0.80", cost)
	}
}

// TestVideoPollThenContent proves the poll -> download flow, and that a
// terminal job is served from storage without further upstream polls.
func TestVideoPollThenContent(t *testing.T) {
	srv, _, key, polls := videoServer(t)
	_, body := doReq(t, "POST", srv.URL+"/v1/videos", key, `{"model":"oai/sora-2","prompt":"a cat","seconds":"8"}`)
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	id := created["id"].(string)

	// First poll hits upstream and completes.
	resp, pollBody := doReq(t, "GET", srv.URL+"/v1/videos/"+id, key, "")
	if resp.StatusCode != 200 {
		t.Fatalf("poll status %d: %s", resp.StatusCode, pollBody)
	}
	var v map[string]any
	_ = json.Unmarshal(pollBody, &v)
	if v["status"] != "completed" || v["progress"].(float64) != 100 {
		t.Fatalf("poll = %s", pollBody)
	}
	afterFirst := *polls

	// Second poll must NOT hit upstream (terminal jobs are served locally).
	_, _ = doReq(t, "GET", srv.URL+"/v1/videos/"+id, key, "")
	if *polls != afterFirst {
		t.Fatalf("terminal job re-polled upstream (%d -> %d)", afterFirst, *polls)
	}

	// Content download.
	resp, content := doReq(t, "GET", srv.URL+"/v1/videos/"+id+"/content", key, "")
	if resp.StatusCode != 200 {
		t.Fatalf("content status %d: %s", resp.StatusCode, content)
	}
	if string(content) != "FAKEMP4BYTES" {
		t.Fatalf("content = %q", content)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "video/mp4") {
		t.Fatalf("content-type = %q", ct)
	}
}

// TestVideoContentBeforeReady proves downloading an unfinished job is a clear
// 409 rather than a confusing empty body.
func TestVideoContentBeforeReady(t *testing.T) {
	srv, database, key, _ := videoServer(t)
	_, body := doReq(t, "POST", srv.URL+"/v1/videos", key, `{"model":"oai/sora-2","prompt":"a cat"}`)
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	id := created["id"].(string)
	// Force the job back to a non-terminal state.
	if _, err := database.Exec(`UPDATE video_jobs SET status='in_progress' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	resp, out := doReq(t, "GET", srv.URL+"/v1/videos/"+id+"/content", key, "")
	if resp.StatusCode != 409 {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "not ready") {
		t.Fatalf("message = %s", out)
	}
}

// TestVideoFailedJobReportsError proves a failed job surfaces the provider's
// message on both the poll and the content route.
func TestVideoFailedJobReportsError(t *testing.T) {
	srv, database, key, _ := videoServer(t)
	_, body := doReq(t, "POST", srv.URL+"/v1/videos", key, `{"model":"oai/sora-2","prompt":"a cat"}`)
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	id := created["id"].(string)
	if _, err := database.Exec(`UPDATE video_jobs SET status='failed', error_code='invalid_argument', error_message='prompt rejected' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	_, pollBody := doReq(t, "GET", srv.URL+"/v1/videos/"+id, key, "")
	if !strings.Contains(string(pollBody), "prompt rejected") {
		t.Fatalf("poll should carry the error: %s", pollBody)
	}
	resp, out := doReq(t, "GET", srv.URL+"/v1/videos/"+id+"/content", key, "")
	if resp.StatusCode != 409 || !strings.Contains(string(out), "prompt rejected") {
		t.Fatalf("content on failed job: %d %s", resp.StatusCode, out)
	}
}

// TestVideoValidatesLocally proves malformed requests never reach upstream.
func TestVideoValidatesLocally(t *testing.T) {
	srv, _, key, polls := videoServer(t)
	cases := []struct{ body, want string }{
		{`{"model":"oai/sora-2"}`, "prompt"},
		{`{"prompt":"x"}`, "model"},
		{`{"model":"oai/sora-2","prompt":"x","seconds":"7"}`, "seconds"},
		{`{"model":"oai/sora-2","prompt":"x","size":"1920x1080"}`, "size"},
	}
	for _, c := range cases {
		resp, body := doReq(t, "POST", srv.URL+"/v1/videos", key, c.body)
		if resp.StatusCode != 400 || !strings.Contains(string(body), c.want) {
			t.Errorf("%s: status %d body %s", c.body, resp.StatusCode, body)
		}
	}
	if *polls != 0 {
		t.Fatalf("invalid requests reached upstream")
	}
}

// TestVideoListAndDelete proves listing is scoped and delete forgets the job.
func TestVideoListAndDelete(t *testing.T) {
	srv, _, key, _ := videoServer(t)
	_, body := doReq(t, "POST", srv.URL+"/v1/videos", key, `{"model":"oai/sora-2","prompt":"a cat"}`)
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	id := created["id"].(string)

	_, listBody := doReq(t, "GET", srv.URL+"/v1/videos", key, "")
	var list struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(listBody, &list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) != 1 || list.Data[0]["id"] != id {
		t.Fatalf("list = %s", listBody)
	}

	resp, delBody := doReq(t, "DELETE", srv.URL+"/v1/videos/"+id, key, "")
	if resp.StatusCode != 200 || !strings.Contains(string(delBody), `"deleted":true`) {
		t.Fatalf("delete = %d %s", resp.StatusCode, delBody)
	}
	resp, _ = doReq(t, "GET", srv.URL+"/v1/videos/"+id, key, "")
	if resp.StatusCode != 404 {
		t.Fatalf("deleted job should 404, got %d", resp.StatusCode)
	}
}

// TestVideoJobScopedToKey proves one gateway key cannot read another's job.
// The prefix header mirrors what the auth middleware sets in production.
func TestVideoJobScopedToKey(t *testing.T) {
	srv, _, key, _ := videoServer(t)
	_, body := doReqAs(t, "POST", srv.URL+"/v1/videos", key, "prefix-A", `{"model":"oai/sora-2","prompt":"a cat"}`)
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	id := created["id"].(string)

	// Same key: visible.
	if resp, _ := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "prefix-A", ""); resp.StatusCode != 200 {
		t.Fatalf("owner read = %d, want 200", resp.StatusCode)
	}
	// Different key: hidden (404, not 403 — do not confirm existence).
	if resp, _ := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id, key, "prefix-B", ""); resp.StatusCode != 404 {
		t.Fatalf("cross-key read = %d, want 404", resp.StatusCode)
	}
	// Content is scoped too.
	if resp, _ := doReqAs(t, "GET", srv.URL+"/v1/videos/"+id+"/content", key, "prefix-B", ""); resp.StatusCode != 404 {
		t.Fatalf("cross-key content = %d, want 404", resp.StatusCode)
	}
	// The list is scoped.
	_, listBody := doReqAs(t, "GET", srv.URL+"/v1/videos", key, "prefix-B", "")
	var list struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(listBody, &list)
	if len(list.Data) != 0 {
		t.Fatalf("cross-key list should be empty, got %d", len(list.Data))
	}
}

// TestVideoModelRefusedOnTextEndpoints proves video models are barred from the
// text protocols (and chat models from the video endpoint).
func TestVideoModelRefusedOnTextEndpoints(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	master := make([]byte, 32)
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)
	if _, err := ps.Create("oai", models.ProviderOpenAI, "https://api.openai.com/v1", "k"); err != nil {
		t.Fatal(err)
	}
	k, _ := ks.Create("test-key")
	h := newLegacyHandler(ps, database)
	r := chi.NewRouter()
	r.Post("/v1/chat/completions", h.ChatCompletions)
	r.Post("/v1/embeddings", h.Embeddings)
	r.Post("/v1/videos", h.VideoCreate)
	r.Post("/v1/images/generations", h.ImageGenerations)
	srv := httptest.NewServer(r)
	defer srv.Close()

	for _, ep := range []string{"/v1/chat/completions", "/v1/embeddings"} {
		resp, body := doReq(t, "POST", srv.URL+ep, k.Key, `{"model":"sora-2","messages":[{"role":"user","content":"hi"}],"input":"hi"}`)
		if resp.StatusCode != 400 || !strings.Contains(string(body), "video-generation model") {
			t.Errorf("%s: status %d body %s", ep, resp.StatusCode, body)
		}
	}
	// An image model on the videos endpoint is refused.
	resp, body := doReq(t, "POST", srv.URL+"/v1/videos", k.Key, `{"model":"dall-e-3","prompt":"x"}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "image") {
		t.Errorf("image model on videos: status %d body %s", resp.StatusCode, body)
	}
	// A video model on the images endpoint is refused.
	resp, body = doReq(t, "POST", srv.URL+"/v1/images/generations", k.Key, `{"model":"sora-2","prompt":"x"}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "video") {
		t.Errorf("video model on images: status %d body %s", resp.StatusCode, body)
	}
}

// TestVideoModelsListed proves the video flag persists on a provider model row
// (the /v1/models projection is covered by the image-listing tests, which share
// the same emission path).
func TestVideoModelsListed(t *testing.T) {
	_, database, _, _ := videoServer(t)
	if _, err := database.Exec(`INSERT INTO provider_models(id, provider_id, model_id, display_name, owned_by, video, modalities, source, created_at, updated_at)
		SELECT 'pmv', id, 'sora-2', 'Sora 2', 'oai', 1, '{"input":["text"],"output":["video"]}', 'video', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP FROM providers LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	var video, image int
	if err := database.QueryRow(`SELECT video, image FROM provider_models WHERE id='pmv'`).Scan(&video, &image); err != nil {
		t.Fatal(err)
	}
	if video != 1 || image != 0 {
		t.Fatalf("video=%d image=%d, want 1/0", video, image)
	}
}
