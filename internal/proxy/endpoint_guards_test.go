package proxy

import (
	"database/sql"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/catalog"
	"ai-gateway/internal/db"
	"ai-gateway/internal/imagegen"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/videogen"

	"github.com/go-chi/chi/v5"
)

// guardServer wires a gateway with a decision provider, a chat provider and an
// image provider, plus every protocol endpoint, so the endpoint-vs-model
// validity matrix can be exercised end to end.
func guardServer(t *testing.T) (*httptest.Server, *sql.DB, string) {
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
		w.Header().Set("Content-Type", "application/json")
		// Answer in whatever dialect the path implies.
		switch {
		case strings.Contains(r.URL.Path, "systemone") || strings.Contains(r.URL.Path, "@cf/"):
			_, _ = w.Write([]byte(`{"model":"clef","answers":{"a":{"type":"noul","noul":0.9}},"usage":{"input_tokens":1}}`))
		case strings.Contains(r.URL.Path, "images"):
			_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"AAA"}]}`))
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		}
	}))
	t.Cleanup(upstream.Close)

	if _, err := ps.Create("cf", models.ProviderCloudflare, upstream.URL+"/ai/run", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Create("oa", models.ProviderOpenAI, upstream.URL+"/v1", "k"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("test-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandlerWithCatalog(ps, catalog.NewStore(database), database)
	r := chi.NewRouter()
	r.Post("/v1/chat/completions", h.ChatCompletions)
	r.Post("/v1/completions", h.Completions)
	r.Post("/v1/embeddings", h.Embeddings)
	r.Post("/v1/messages", h.AnthropicMessages)
	r.Post("/v1/responses", h.Responses)
	r.Post("/v1/systemone", h.SystemOne)
	r.Post("/v1/images/generations", h.ImageGenerations)
	r.Post("/v1/images/edits", h.ImageEdits)
	r.Post("/v1/videos", h.VideoCreate)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { database.Close() })
	return srv, database, k.Key
}

// TestDecisionModelRefusedOnEveryTextEndpoint is the regression guard for the
// asymmetry bug: decision models were only rejected on the paths that reached
// the System One handler, while the text endpoints answered a generic
// 404 model_not_routed (unreachable actionable message behind unroutedModel()).
func TestDecisionModelRefusedOnEveryTextEndpoint(t *testing.T) {
	srv, _, key := guardServer(t)
	cases := []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"cf/clef","messages":[{"role":"user","content":"hi"}]}`},
		{"completions", "/v1/completions", `{"model":"cf/clef","prompt":"hi"}`},
		{"embeddings", "/v1/embeddings", `{"model":"cf/clef","input":"hi"}`},
		{"messages", "/v1/messages", `{"model":"cf/clef","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`},
		{"responses", "/v1/responses", `{"model":"cf/clef","input":"hi"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := postJSONImage(t, srv.URL+c.path, key, c.body)
			if resp.StatusCode != 400 {
				t.Fatalf("want 400, got %d: %s", resp.StatusCode, body)
			}
			s := string(body)
			if !strings.Contains(s, "decision model") || !strings.Contains(s, "/v1/systemone") {
				t.Fatalf("refusal must name the decision endpoint: %s", s)
			}
		})
	}
}

// TestImageOnlyModelRefusedOnTextEndpoints proves a pure generator is refused
// with a pointer to the images endpoint.
func TestImageOnlyModelRefusedOnTextEndpoints(t *testing.T) {
	srv, _, key := guardServer(t)
	for _, c := range []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"oa/dall-e-3","messages":[{"role":"user","content":"hi"}]}`},
		{"embeddings", "/v1/embeddings", `{"model":"oa/dall-e-3","input":"hi"}`},
		{"messages", "/v1/messages", `{"model":"oa/dall-e-3","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp, body := postJSONImage(t, srv.URL+c.path, key, c.body)
			if resp.StatusCode != 400 {
				t.Fatalf("want 400, got %d: %s", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), "image-only") || !strings.Contains(string(body), "/v1/images/generations") {
				t.Fatalf("refusal must name the images endpoint: %s", body)
			}
		})
	}
}

// TestImageCapableModelNotRefusedOnChat proves the capability fix: a model whose
// models.dev modalities output BOTH text and image (Gemini image models) is a
// legitimate chat model and must not be refused as image-only.
func TestImageCapableModelNotRefusedOnChat(t *testing.T) {
	srv, database, key := guardServer(t)
	if _, err := database.Exec(`INSERT INTO models_catalog(id, provider, name, image, modalities, updated_at)
		VALUES('gemini-2.5-flash-image','google','Nano Banana',1,?,CURRENT_TIMESTAMP)`,
		`{"input":["text","image"],"output":["text","image"]}`); err != nil {
		t.Fatal(err)
	}
	resp, body := postJSONImage(t, srv.URL+"/v1/chat/completions", key,
		`{"model":"oa/gemini-2.5-flash-image","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("image-capable model must chat: %d %s", resp.StatusCode, body)
	}
}

// TestWrongModelOnSystemOneNamesRightEndpoint proves /v1/systemone explains
// which endpoint a non-decision model belongs to, and that the model check runs
// BEFORE routing (it used to be unreachable behind unroutedModel()).
func TestWrongModelOnSystemOneNamesRightEndpoint(t *testing.T) {
	srv, _, key := guardServer(t)
	cases := []struct{ model, want string }{
		{"gpt-4o", "/v1/chat/completions"},
		{"dall-e-3", "/v1/images/generations"},
	}
	for _, c := range cases {
		body := `{"model":"` + c.model + `","state":"x","questions":{"a":{"type":"noul","instructions":"y"}}}`
		resp, respBody := postJSONImage(t, srv.URL+"/v1/systemone", key, body)
		if resp.StatusCode != 400 {
			t.Fatalf("%s: want 400, got %d: %s", c.model, resp.StatusCode, respBody)
		}
		if !strings.Contains(string(respBody), c.want) {
			t.Fatalf("%s: refusal must point at %s, got %s", c.model, c.want, respBody)
		}
	}
}

// TestRealDecisionModelStillRoutes proves the guard did not break the happy
// path: an actual decision model reaches its provider.
func TestRealDecisionModelStillRoutes(t *testing.T) {
	srv, _, key := guardServer(t)
	body := `{"model":"cf/clef","state":"x","questions":{"a":{"type":"noul","instructions":"y"}}}`
	resp, respBody := postJSONImage(t, srv.URL+"/v1/systemone", key, body)
	if resp.StatusCode != 200 {
		t.Fatalf("decision model must route: %d %s", resp.StatusCode, respBody)
	}
	var out struct {
		Answers map[string]any `json:"answers"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil || len(out.Answers) == 0 {
		t.Fatalf("unexpected decision response: %s", respBody)
	}
}

// TestDecisionModelRefusedOnImages proves the images endpoint refuses decision
// models with an actionable message.
func TestDecisionModelRefusedOnImages(t *testing.T) {
	srv, _, key := guardServer(t)
	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key, `{"model":"cf/clef","prompt":"x"}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "decision") {
		t.Fatalf("want 400 naming decision: %d %s", resp.StatusCode, body)
	}
}

// TestNoRouteForIsCapabilityAware proves the no-route message names the right
// endpoint for models with a distinctive capability, while a genuinely unknown
// model keeps the generic routing advice. Tested directly because reaching
// noRouteFor for an image model requires a config with no matching route, which
// is awkward to construct through the HTTP surface.
func TestNoRouteForIsCapabilityAware(t *testing.T) {
	cases := []struct {
		model string
		want  []string
	}{
		{"gemini-2.5-flash-image", []string{"image-generation model", "/v1/images/generations"}},
		{"cf/clef", []string{"decision model", "/v1/systemone"}},
		{"totally-unknown-xyz", []string{"is not routed", "routing rule"}},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		noRouteFor(rec, c.model)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", c.model, rec.Code)
		}
		body := rec.Body.String()
		for _, want := range c.want {
			if !strings.Contains(body, want) {
				t.Errorf("%s: message missing %q: %s", c.model, want, body)
			}
		}
	}
}

// upsertCatalogRow inserts a synthetic catalog row (the same shape discovery and
// the curated seeders write) so the capability guards can be exercised against
// catalog evidence rather than the curated registry.
func upsertCatalogRow(t *testing.T, database *sql.DB, id string, decision, image, video bool, modalities string) {
	t.Helper()
	if _, err := database.Exec(`INSERT INTO models_catalog(
		id,provider,name,description,family,context_window,max_output,
		input_cost,output_cost,cache_read_cost,cache_write_cost,
		reasoning,tool_call,structured_output,attachment,modalities,
		open_weights,knowledge_cutoff,updated_at,
		reasoning_type,reasoning_levels,reasoning_output_limits,
		decision,image,video)
		VALUES(?,?,?,?,?,0,0,0,0,0,0,0,0,0,0,?,0,'',CURRENT_TIMESTAMP,'none','[]','{}',?,?,?)`,
		id, "acme", id, "synthetic row", "acme", modalities,
		boolToInt(decision), boolToInt(image), boolToInt(video)); err != nil {
		t.Fatalf("insert catalog row %s: %v", id, err)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestCatalogDecisionRowAcceptedOnSystemOne is the P1-4 regression guard: a row
// synced with decision=1 is advertised by /v1/models as a decision model, so
// /v1/systemone must accept it. Pre-fix only the curated registry was consulted
// and the request was rejected with "is not a decision model".
func TestCatalogDecisionRowAcceptedOnSystemOne(t *testing.T) {
	srv, database, key := guardServer(t)
	upsertCatalogRow(t, database, "acme/acme-decide", true, false, false,
		`{"input":["text"],"output":["decision"]}`)

	resp, body := postJSONImage(t, srv.URL+"/v1/systemone", key,
		`{"model":"acme/acme-decide","state":"x","questions":{"a":{"type":"noul","instructions":"y"}}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("catalog-only decision model on /v1/systemone = %d: %s", resp.StatusCode, body)
	}

	// The same evidence refuses the model on the text endpoints, so the listing
	// and the guards agree. The catalog-aware guard is a method on the handler;
	// internal/proxy/proxy.go still calls the package-level (curated-only)
	// variant at its text call sites, which is owned by another agent — see the
	// integration note in the completion report. Asserting the method here pins
	// the behaviour that call site must adopt.
	rec := httptest.NewRecorder()
	h := hForDB(t, database)
	if !h.rejectDecisionModelOnText(rec, "acme/acme-decide", "/v1/chat/completions") {
		t.Fatalf("catalog decision row must be refused on the text endpoints: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "/v1/systemone") {
		t.Fatalf("refusal must be a 400 naming /v1/systemone: %d %s", rec.Code, rec.Body.String())
	}
}

// TestCatalogImageRowAcceptedOnImages is the image half of P1-4: a catalog row
// with image=1 is an image model even when the curated registry has never heard
// of it, so the images guard must not refuse it. Pre-fix only the curated
// registry was consulted, so an operator-added or models.dev-discovered
// generator was rejected as "not an image model" while /v1/models advertised it
// as one.
func TestCatalogImageRowAcceptedOnImages(t *testing.T) {
	srv, database, key := guardServer(t)
	upsertCatalogRow(t, database, "acme/acme-image-9", false, true, false,
		`{"input":["text"],"output":["image"]}`)

	// The endpoint no longer answers the "not an image model" 400 (it may still
	// have no route in this fixture, which is a different, honest answer).
	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
		`{"model":"acme/acme-image-9","prompt":"x"}`)
	if resp.StatusCode == 400 && strings.Contains(string(body), "not an image model") {
		t.Fatalf("catalog image row refused on images: %d %s", resp.StatusCode, body)
	}

	// Directly: the guard accepts it, and refuses the curated video/decision
	// models as before (the curated registry remains the fallback).
	h := hForDB(t, database)
	if rec := httptest.NewRecorder(); h.rejectNonImageModelOnImages(rec, "acme/acme-image-9") {
		t.Fatalf("catalog image row must pass the images guard: %s", rec.Body.String())
	}
	for model, want := range map[string]string{"cf/clef": "decision", "sora-2": "video"} {
		rec := httptest.NewRecorder()
		if !h.rejectNonImageModelOnImages(rec, model) {
			t.Fatalf("%s must still be refused on images", model)
		}
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("refusal for %s must name %q: %s", model, want, rec.Body.String())
		}
	}

	// Catalog-only image rows are image-ONLY (output has no text), so the text
	// endpoints must refuse them: catalog modalities are authoritative for the
	// image-only distinction.
	rec := httptest.NewRecorder()
	if !h.rejectImageModelOnText(rec, "acme/acme-image-9", "/v1/chat/completions") {
		t.Fatalf("catalog image-only row must be refused on chat: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/v1/images/generations") {
		t.Fatalf("refusal must name the images endpoint: %s", rec.Body.String())
	}
}

// TestImageEditsRunsCapabilityGuard proves the multipart edit path applies the
// same capability guard as the JSON path (P1-3). Pre-fix a decision model and a
// text model both reached the provider's edit endpoint.
func TestImageEditsRunsCapabilityGuard(t *testing.T) {
	srv, database, key := guardServer(t)
	var hits int
	// A fresh upstream would be needed to count hits; instead assert only the
	// local verdict, which is what the guard produces before any routing.
	_ = hits
	upsertCatalogRow(t, database, "acme/plain-text", false, false, false,
		`{"input":["text"],"output":["text"]}`)

	cases := []struct {
		name, model, want string
	}{
		{"decision model", "cf/clef", "decision"},
		{"catalog text row", "acme/plain-text", "text model"},
		{"video model", "sora-2", "video"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf, ct := buildEdit(t, [][2]string{
				{"model", c.model}, {"prompt", "x"},
			}, []editFile{{"image", "a.png", "image/png", decodeB64(t, tinyPNG)}})
			resp, body := postMultipart(t, srv.URL+"/v1/images/edits", key, buf, ct)
			if resp.StatusCode != 400 {
				t.Fatalf("%s on edits = %d, want 400: %s", c.name, resp.StatusCode, body)
			}
			if !strings.Contains(string(body), c.want) {
				t.Fatalf("%s message must name %q: %s", c.name, c.want, body)
			}
		})
	}
}

// TestGoogleTargetsEscapeAndRejectModelIDs covers P3-2: the model id is
// client-influenced and used to be concatenated raw into Google/Cloudflare
// native URLs, so it could inject path or query structure. It is now a single
// escaped path segment, and an id that cannot be one is refused up front.
func TestGoogleTargetsEscapeAndRejectModelIDs(t *testing.T) {
	if got := googleImageTarget("https://g.example", "a b/c", imagegen.DialectGemini); !strings.Contains(got, "a%20b%2Fc") {
		t.Fatalf("googleImageTarget did not escape the id: %q", got)
	}
	if got := googleVeoTarget("https://g.example", "veo/3 1"); !strings.Contains(got, "veo%2F3%201") {
		t.Fatalf("googleVeoTarget did not escape the id: %q", got)
	}
	if got := googleVeoOperationURL("https://g.example", "models/veo/operations/a b"); !strings.Contains(got, "a%20b") || !strings.Contains(got, "/operations/") {
		t.Fatalf("googleVeoOperationURL did not escape the operation: %q", got)
	}
	if got := cloudflareDecisionTarget("https://cf.example/ai/run", "clef/../admin"); strings.Contains(got, "../") {
		t.Fatalf("cloudflareDecisionTarget left a traversal: %q", got)
	}

	for _, bad := range []string{"m?x=1", "m#frag", "a/b/c"} {
		if err := validateGoogleModelID(bad); err == nil {
			t.Errorf("validateGoogleModelID(%q) must refuse the id", bad)
		}
	}
	for _, good := range []string{"gemini-2.5-flash-image", "google/veo-3.1-generate-preview"} {
		if err := validateGoogleModelID(good); err != nil {
			t.Errorf("validateGoogleModelID(%q) = %v, want nil", good, err)
		}
	}
	if err := validateDecisionSelector("clef/../x"); err == nil {
		t.Error("validateDecisionSelector must refuse a selector with a slash")
	}
}

// TestGoogleModelIDInjectionIsRefused proves the handler-level rejection: a
// model id carrying query/fragment syntax never reaches the upstream URL
// builder on the Veo dialect.
func TestGoogleModelIDInjectionIsRefused(t *testing.T) {
	p := &models.Provider{Type: models.ProviderGoogle, BaseURL: "https://generativelanguage.googleapis.com"}
	n := &videogen.Normalized{Model: "veo", Prompt: "p", Seconds: "8", Size: "1280x720"}
	if _, _, err := videoCreateRequest(p, videogen.DialectVeo, n, "veo?x=1"); err == nil {
		t.Fatal("a model id with a query separator must be refused before the upstream call")
	}
}

// TestMultipartContentTypeIsTokenized covers P3-1: the part Content-Type is
// copied from the client into a rebuilt upstream part header, so CR/LF and
// structural characters must never survive.
func TestMultipartContentTypeIsTokenized(t *testing.T) {
	cases := []struct{ in, want string }{
		{"image/png", "image/png"},
		{"image/png\r\nX-Injected: 1", "image/pngX-Injected:1"},
		{"image/png\n", "image/png"},
		{"  ", "application/octet-stream"},
		{"", "application/octet-stream"},
		{"text/plain; boundary=evil", "text/plainboundary=evil"},
	}
	for _, c := range cases {
		if got := sanitizeMultipartToken(c.in); got != c.want {
			t.Errorf("sanitizeMultipartToken(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// And end to end: the rebuilt part must carry exactly one Content-Type
	// header, with no injected second header line.
	files := []multipartFile{{Field: "image", FileName: "a.png", ContentType: "image/png\r\nX-Evil: 1", Data: []byte("x")}}
	n := &imagegen.Normalized{Prompt: "p", N: 1}
	body, ctype, err := buildEditMultipart(n, files, "m")
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		t.Fatalf("rebuilt content type %q: %v", ctype, err)
	}
	// The rebuilt form leads with the model/prompt fields, so scan to the file
	// part — that is the one that carries the client-supplied Content-Type.
	mr := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
	var part *multipart.Part
	for {
		p, perr := mr.NextPart()
		if perr == io.EOF {
			t.Fatal("rebuilt body has no file part")
		}
		if perr != nil {
			t.Fatalf("rebuilt body is not parseable: %v", perr)
		}
		if p.FormName() == "image" {
			part = p
			break
		}
	}
	if got := part.Header.Get("Content-Type"); strings.ContainsAny(got, "\r\n") {
		t.Fatalf("part Content-Type carries a header break: %q", got)
	}
	if len(part.Header.Values("Content-Type")) != 1 || part.Header.Get("X-Evil") != "" {
		t.Fatalf("part header was injected: %v", part.Header)
	}
}

// hForDB builds a legacy handler sharing an existing database (so a catalog row
// inserted by a test is visible to the guards).
func hForDB(t *testing.T, database *sql.DB) *Handler {
	t.Helper()
	return newLegacyHandlerWithCatalog(provider.NewStore(database, make([]byte, 32)), catalog.NewStore(database), database)
}
