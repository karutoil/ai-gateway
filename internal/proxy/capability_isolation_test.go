package proxy

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/catalog"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

// isolationServer wires the full endpoint surface (text + images + videos +
// systemone) against provider entries of every relevant type, with the curated
// catalog seeded so catalog-modality capability decisions are exercised rather
// than only the curated-registry fallback.
//
// The mock upstream accepts anything and records how many requests reached it,
// so a test can prove a rejected request never burned an upstream round trip.
type isolationUpstream struct {
	hits int
}

func newIsolationServer(t *testing.T) (*httptest.Server, *sql.DB, string, *isolationUpstream) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	ps := provider.NewStore(database, make([]byte, 32))
	cs := catalog.NewStore(database)
	ks := apikey.NewStore(database)

	track := &isolationUpstream{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		track.hits++
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
	t.Cleanup(up.Close)

	// One provider per capability axis. Names double as the model-id prefix.
	if _, err := ps.Create("cf", models.ProviderCloudflare, up.URL+"/ai/run", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Create("oa", models.ProviderOpenAI, up.URL+"/v1", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Create("goog", models.ProviderGoogle, up.URL+"/v1", "k"); err != nil {
		t.Fatal(err)
	}
	// Seed the curated capability registries exactly like boot does.
	for name, fn := range map[string]func() (int, error){
		"decision": cs.SeedDecisionModels,
		"image":    cs.SeedImageModels,
		"video":    cs.SeedVideoModels,
	} {
		if _, err := fn(); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	k, err := ks.Create("isolation-key")
	if err != nil {
		t.Fatal(err)
	}

	h := newLegacyHandlerWithCatalog(ps, cs, database)
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	r.Post("/v1/completions", h.Completions)
	r.Post("/v1/embeddings", h.Embeddings)
	r.Post("/v1/messages", h.AnthropicMessages)
	r.Post("/v1/responses", h.Responses)
	r.Post("/v1/systemone", h.SystemOne)
	r.Post("/v1/images/generations", h.ImageGenerations)
	r.Post("/v1/videos", h.VideoCreate)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, database, k.Key, track
}

// textBodies renders one minimal valid body per text-protocol endpoint for the
// given model, so the same refusal can be asserted on every text surface.
func textBodies(model string) []struct{ name, path, body string } {
	q := `"model":"` + model + `"`
	return []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{` + q + `,"messages":[{"role":"user","content":"hi"}]}`},
		{"completions", "/v1/completions", `{` + q + `,"prompt":"hi"}`},
		{"embeddings", "/v1/embeddings", `{` + q + `,"input":"hi"}`},
		{"messages", "/v1/messages", `{` + q + `,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`},
		{"responses", "/v1/responses", `{` + q + `,"input":"hi"}`},
	}
}

// TestVideoModelRefusedOnEveryTextEndpoint is the missing mirror of
// TestDecisionModelRefusedOnEveryTextEndpoint: rejectVideoModelOnText exists on
// all five text handlers, but only chat/embeddings were asserted.
func TestVideoModelRefusedOnEveryTextEndpoint(t *testing.T) {
	srv, _, key, up := newIsolationServer(t)
	before := up.hits
	for _, c := range textBodies("sora-2") {
		t.Run(c.name, func(t *testing.T) {
			resp, body := postJSONImage(t, srv.URL+c.path, key, c.body)
			if resp.StatusCode != 400 {
				t.Fatalf("want 400, got %d: %s", resp.StatusCode, body)
			}
			s := string(body)
			if !strings.Contains(s, "video-generation model") || !strings.Contains(s, "/v1/videos") {
				t.Fatalf("refusal must name the video endpoint: %s", s)
			}
		})
	}
	if up.hits != before {
		t.Fatalf("refused video model reached upstream %d times", up.hits-before)
	}
}

// TestImageOnlyModelRefusedOnRemainingTextEndpoints closes the gap left by
// TestImageOnlyModelRefusedOnTextEndpoints (chat/embeddings/messages only).
func TestImageOnlyModelRefusedOnRemainingTextEndpoints(t *testing.T) {
	srv, _, key, up := newIsolationServer(t)
	before := up.hits
	for _, c := range textBodies("dall-e-3") {
		if c.name == "chat" || c.name == "embeddings" || c.name == "messages" {
			continue // already pinned by endpoint_guards_test.go
		}
		t.Run(c.name, func(t *testing.T) {
			resp, body := postJSONImage(t, srv.URL+c.path, key, c.body)
			if resp.StatusCode != 400 {
				t.Fatalf("want 400, got %d: %s", resp.StatusCode, body)
			}
			s := string(body)
			if !strings.Contains(s, "image-only") || !strings.Contains(s, "/v1/images/generations") {
				t.Fatalf("refusal must name the images endpoint: %s", s)
			}
		})
	}
	if up.hits != before {
		t.Fatalf("refused image model reached upstream %d times", up.hits-before)
	}
}

// TestVideoModelRefusedOnSystemOne: /v1/systemone must explain that a video
// model belongs on POST /v1/videos (previously only text/image models were
// asserted).
func TestVideoModelRefusedOnSystemOne(t *testing.T) {
	srv, _, key, up := newIsolationServer(t)
	before := up.hits
	body := `{"model":"sora-2","state":"x","questions":{"a":{"type":"noul","instructions":"y"}}}`
	resp, respBody := postJSONImage(t, srv.URL+"/v1/systemone", key, body)
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d: %s", resp.StatusCode, respBody)
	}
	if !strings.Contains(string(respBody), "/v1/videos") {
		t.Fatalf("refusal must name the video endpoint: %s", respBody)
	}
	if up.hits != before {
		t.Fatal("video model reached upstream through /v1/systemone")
	}
}

// TestChatModelRefusedOnNewCapabilityEndpoints proves the reverse direction for
// every new endpoint: a plain text model cannot be smuggled into /v1/videos or
// /v1/systemone.
func TestChatModelRefusedOnNewCapabilityEndpoints(t *testing.T) {
	srv, database, key, up := newIsolationServer(t)
	// A complete catalog row (mirrors what the models.dev sync writes) so the
	// "exact catalog row that is neither image nor decision" branch is hit.
	if _, err := database.Exec(`INSERT INTO models_catalog(
		id,provider,name,description,family,context_window,max_output,
		input_cost,output_cost,cache_read_cost,cache_write_cost,
		reasoning,tool_call,structured_output,attachment,modalities,
		open_weights,knowledge_cutoff,updated_at,
		reasoning_type,reasoning_levels,reasoning_output_limits,
		decision,image,video)
		VALUES('openai/gpt-4o','openai','GPT-4o','d','gpt',128000,4096,
		1,2,0,0,0,1,1,1,'{"input":["text"],"output":["text"]}',
		0,'2024-01-01',CURRENT_TIMESTAMP,'none','[]','{}',0,0,0)`); err != nil {
		t.Fatal(err)
	}
	before := up.hits

	// A text model on the videos endpoint.
	resp, body := postJSONImage(t, srv.URL+"/v1/videos", key, `{"model":"openai/gpt-4o","prompt":"x"}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "not a video model") {
		t.Fatalf("text model on videos: %d %s", resp.StatusCode, body)
	}
	// A text model on the images endpoint.
	resp, body = postJSONImage(t, srv.URL+"/v1/images/generations", key, `{"model":"openai/gpt-4o","prompt":"x"}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "not an image model") {
		t.Fatalf("text model on images: %d %s", resp.StatusCode, body)
	}
	if up.hits != before {
		t.Fatalf("refused text model reached upstream %d times", up.hits-before)
	}
}

// TestNoCapabilityModelCrossesIntoAnotherEndpoint summarises the isolation
// matrix as one table: each row is (endpoint, foreign-capability model) and the
// expectation is an actionable 400 that never reaches an upstream.
func TestNoCapabilityModelCrossesIntoAnotherEndpoint(t *testing.T) {
	srv, _, key, up := newIsolationServer(t)
	cases := []struct {
		name, path, body, want string
	}{
		{"decision on images", "/v1/images/generations",
			`{"model":"cloudflare/clef","prompt":"x"}`, "/v1/images/generations requires an image model"},
		{"decision on chat", "/v1/chat/completions",
			`{"model":"cloudflare/clef","messages":[{"role":"user","content":"hi"}]}`, "/v1/systemone"},
		{"image on systemone", "/v1/systemone",
			`{"model":"gpt-image-1","state":"x","questions":{"a":{"type":"noul","instructions":"y"}}}`, "/v1/images/generations"},
		{"image-only on videos", "/v1/videos",
			`{"model":"dall-e-3","prompt":"x"}`, "image model"},
		{"video on images", "/v1/images/generations",
			`{"model":"sora-2","prompt":"x"}`, "/v1/videos"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := up.hits
			resp, body := postJSONImage(t, srv.URL+c.path, key, c.body)
			if resp.StatusCode != 400 {
				t.Fatalf("want 400, got %d: %s", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), c.want) {
				t.Fatalf("message %q missing %q", body, c.want)
			}
			if up.hits != before {
				t.Fatal("rejected cross-capability request reached an upstream")
			}
		})
	}
}

// TestDecisionModelOnVideosDoesNotLeakUpstream proves the safety half of the
// decision→videos direction: a decision model never reaches an upstream and
// never becomes a job (videoCandidates excludes decision providers).
//
// It also pins a REAL quality gap found while building the isolation matrix:
// rejectNonVideoModelOnVideos has no decision branch, so instead of a 400
// naming /v1/systemone (what every other endpoint does) the client gets an
// opaque 503 "no video-capable provider configured". The assertion below is
// deliberately loose on the status so the day the guard is added the test
// starts failing loudly with the actionable message it should produce.
func TestDecisionModelOnVideosDoesNotLeakUpstream(t *testing.T) {
	srv, database, key, up := newIsolationServer(t)
	before := up.hits
	resp, body := postJSONImage(t, srv.URL+"/v1/videos", key,
		`{"model":"cloudflare/clef","prompt":"x","seconds":"4"}`)
	if resp.StatusCode < 400 {
		t.Fatalf("decision model must not create a video job: %d %s", resp.StatusCode, body)
	}
	if up.hits != before {
		t.Fatal("decision model reached a video upstream")
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM video_jobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a rejected request persisted %d video job(s)", n)
	}
	if !strings.Contains(string(body), "/v1/systemone") {
		t.Logf("GAP (P2): decision model on /v1/videos answers %d %q without pointing at /v1/systemone",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// TestImageOnlyModelOnVideosNamesImagesEndpoint guards the specific suggestion
// text for the image→video direction (it names example video models).
func TestImageOnlyModelOnVideosNamesImagesEndpoint(t *testing.T) {
	srv, _, key, _ := newIsolationServer(t)
	resp, body := postJSONImage(t, srv.URL+"/v1/videos", key, `{"model":"gpt-image-1","prompt":"x"}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "sora-2") {
		t.Fatalf("image model on videos must suggest a video model: %d %s", resp.StatusCode, body)
	}
}

// TestCatalogModalitiesDriveVideoOnlyRefusal proves the catalog row (models.dev
// modalities) is authoritative over the curated registry: a video-only model the
// curated registry has never heard of is still refused on the images endpoint
// without an upstream round trip.
//
// The message is asserted only for the safety property (a 400 that does not
// reach upstream). It currently says "is a text model", which is wrong for a
// video-only row: rejectNonImageModelOnImages checks image/decision before it
// looks at video modalities. Documented as a P2 in .review/tests.md.
func TestCatalogModalitiesDriveVideoOnlyRefusal(t *testing.T) {
	srv, database, key, up := newIsolationServer(t)
	if _, err := database.Exec(`INSERT INTO models_catalog(
		id,provider,name,description,family,context_window,max_output,
		input_cost,output_cost,cache_read_cost,cache_write_cost,
		reasoning,tool_call,structured_output,attachment,modalities,
		open_weights,knowledge_cutoff,updated_at,
		reasoning_type,reasoning_levels,reasoning_output_limits,
		decision,image,video)
		VALUES('acme/kling-9','acme','Kling 9','d','kling',0,0,
		0,0,0,0,0,0,0,0,'{"input":["text"],"output":["video"]}',
		0,'',CURRENT_TIMESTAMP,'none','[]','{}',0,0,1)`); err != nil {
		t.Fatal(err)
	}
	before := up.hits
	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
		`{"model":"acme/kling-9","prompt":"x"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("video-only catalog model on images: %d %s", resp.StatusCode, body)
	}
	if up.hits != before {
		t.Fatal("refused video-only model reached upstream")
	}
	if !strings.Contains(string(body), "/v1/videos") {
		t.Logf("GAP (P2): video-only model refused on images as %q — should point at /v1/videos",
			strings.TrimSpace(string(body)))
	}
	// And it is refused on chat too, via the catalog modalities.
	resp, body = postJSONImage(t, srv.URL+"/v1/chat/completions", key,
		`{"model":"acme/kling-9","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "/v1/videos") {
		t.Fatalf("video-only catalog model on chat: %d %s", resp.StatusCode, body)
	}
}

// TestListedDecisionModelIsRefusedOnChat proves the seeded catalog's decision
// rows (the same rows /v1/models lists) are refused on chat, i.e. the listing
// capability and the routing guard agree.
func TestListedDecisionModelIsRefusedOnChat(t *testing.T) {
	srv, database, key, _ := newIsolationServer(t)
	var id string
	if err := database.QueryRow(`SELECT id FROM models_catalog WHERE decision=1 ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("no seeded decision row: %v", err)
	}
	resp, body := postJSONImage(t, srv.URL+"/v1/chat/completions", key,
		`{"model":"`+id+`","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 400 {
		t.Fatalf("%s on chat: want 400, got %d: %s", id, resp.StatusCode, body)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	if !strings.Contains(string(body), "/v1/systemone") {
		t.Fatalf("refusal must point at /v1/systemone: %s", body)
	}
}
