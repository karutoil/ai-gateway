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
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/resilience"

	"github.com/go-chi/chi/v5"
)

// imageServerNoRetry is imageTestServer with the retry policy disabled. The
// dialect error tests assert on the terminal status after the candidate chain
// is exhausted, so retry backoff would only add seconds of wall clock without
// testing anything they care about.
func imageServerNoRetry(t *testing.T, provName string, typ models.ProviderType, upstream *httptest.Server) (*httptest.Server, *sql.DB, string) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	if _, err := ps.Create(provName, typ, upstream.URL+"/v1", "upstream-key"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("test-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0}
	r := chi.NewRouter()
	r.Post("/v1/images/generations", h.ImageGenerations)
	r.Post("/v1/images/edits", h.ImageEdits)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, database, k.Key
}

// TestImageGeminiErrorBodyIsRelayed proves the Google dialect maps a native
// error into a client-visible failure instead of answering 200 with Google's
// envelope (which an OpenAI SDK would fail to parse).
//
// It also pins the current translation behaviour: a Gemini 400 is relayed with
// its own status when the provider classifies it as client-caused, otherwise the
// body is passed through untouched. Either way the client must see an error.
func TestImageGeminiErrorBodyIsRelayed(t *testing.T) {
	cases := []struct {
		name         string
		upstreamCode int
		body         string
		wantStatus   int
	}{
		{
			name:         "google 400 invalid argument",
			upstreamCode: 400,
			body:         `{"error":{"code":400,"message":"API key not valid","status":"INVALID_ARGUMENT"}}`,
			wantStatus:   400,
		},
		{
			name:         "google 429 quota",
			upstreamCode: 429,
			body:         `{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED"}}`,
			wantStatus:   429,
		},
		{
			name:         "google 500 internal",
			upstreamCode: 500,
			body:         `{"error":{"code":500,"message":"internal","status":"INTERNAL"}}`,
			wantStatus:   500, // relayed as-is: the upstream really did fail
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.upstreamCode)
				_, _ = w.Write([]byte(c.body))
			}))
			defer upstream.Close()
			srv, _, key := imageServerNoRetry(t, "goog", models.ProviderGoogle, upstream)

			resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
				`{"model":"goog/gemini-2.5-flash-image","prompt":"a cat"}`)
			if resp.StatusCode < 400 {
				t.Fatalf("upstream %d must not become a client success: %d %s",
					c.upstreamCode, resp.StatusCode, body)
			}
			if c.wantStatus != 0 && resp.StatusCode != c.wantStatus {
				t.Fatalf("upstream %d -> client %d, want %d: %s", c.upstreamCode, resp.StatusCode, c.wantStatus, body)
			}
			if !strings.Contains(string(body), "error") {
				t.Fatalf("failure body is not an error envelope: %s", body)
			}
		})
	}
}

// TestImageGeminiResponseWithoutCandidatesIsNotAFakeSuccess proves the
// transform is a no-op (not a 200 with an empty data array) when Gemini answers
// 200 but returns no inline image — e.g. a safety block. The client must not
// receive `{"data":[]}` as if it had worked.
func TestImageGeminiResponseWithoutCandidatesIsNotAFakeSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A 200 with a promptFeedback block and no candidates: the model
		// refused to generate.
		_, _ = w.Write([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`))
	}))
	defer upstream.Close()
	srv, database, key := imageTestServer(t, "goog", models.ProviderGoogle, upstream)

	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
		`{"model":"goog/gemini-2.5-flash-image","prompt":"a cat"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	// The untranslatable body is relayed verbatim (GeminiToOpenAI returns nil),
	// NOT rewritten into an empty OpenAI data array.
	if strings.Contains(string(body), `"data":[]`) {
		t.Fatalf("a blocked Gemini response became an empty success: %s", body)
	}
	var cost float64
	if err := database.QueryRow(`SELECT COALESCE(cost_usd,0) FROM request_logs ORDER BY created_at DESC LIMIT 1`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost != 0 {
		t.Fatalf("a response with no image must not be billed: cost = %v", cost)
	}
}

// TestImageImagenErrorBodyIsRelayed is the Imagen (:predict) mirror.
func TestImageImagenErrorBodyIsRelayed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"prompt blocked by safety filter","status":"INVALID_ARGUMENT"}}`))
	}))
	defer upstream.Close()
	srv, _, key := imageTestServer(t, "goog", models.ProviderGoogle, upstream)
	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
		`{"model":"goog/imagen-4.0-generate-001","prompt":"a cat"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("imagen 400 -> %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "safety") {
		t.Fatalf("upstream message lost: %s", body)
	}
}

// TestImageOpenAICompatibleErrorIsRelayed proves the passthrough dialect relays
// the provider's verdict verbatim (a bad model name is the caller's error).
func TestImageOpenAICompatibleErrorIsRelayed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"unknown model: grok-2-image","type":"invalid_request_error"}}`))
	}))
	defer upstream.Close()
	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
		`{"model":"xaip/grok-2-image","prompt":"a cat"}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "unknown model") {
		t.Fatalf("passthrough error: %d %s", resp.StatusCode, body)
	}
}

// TestImageDialectSelectionByProviderType proves the provider TYPE wins over the
// model registry when choosing the upstream dialect: a Google provider serving a
// Gemini image model is called with generateContent, while the same model on an
// openai_compatible provider is called with the OpenAI Images path.
func TestImageDialectSelectionByProviderType(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []map[string]any{{"b64_json": tinyPNG}}})
	}))
	defer upstream.Close()

	// Google provider → native generateContent.
	srv, _, key := imageTestServer(t, "goog", models.ProviderGoogle, upstream)
	if resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
		`{"model":"goog/gemini-2.5-flash-image","prompt":"x"}`); resp.StatusCode != 200 {
		t.Fatalf("google: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(gotPath, ":generateContent") {
		t.Fatalf("google provider path = %q", gotPath)
	}

	// openai_compatible provider → OpenAI Images passthrough.
	srv2, _, key2 := imageTestServer(t, "compat", models.ProviderOpenAICompatible, upstream)
	if resp, body := postJSONImage(t, srv2.URL+"/v1/images/generations", key2,
		`{"model":"compat/gemini-2.5-flash-image","prompt":"x"}`); resp.StatusCode != 200 {
		t.Fatalf("compat: %d %s", resp.StatusCode, body)
	}
	if !strings.HasSuffix(gotPath, "/images/generations") {
		t.Fatalf("openai_compatible provider path = %q", gotPath)
	}
}

// TestImageModelOverrideReachesUpstream covers P1-5: an lb_rules member with a
// model_override must send the override upstream (and bill it), not the alias.
// Pre-fix the prepare closures used the captured outer model, so the gateway
// billed one model and the provider ran another.
func TestImageModelOverrideReachesUpstream(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotModel, _ = b["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []map[string]any{{"b64_json": tinyPNG}}})
	}))
	defer upstream.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	p, err := ps.Create("xaip", models.ProviderXAI, upstream.URL+"/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	lbs := lb.NewStore(database)
	if err := lbs.ReplaceRule("img-alias", "", []lb.RuleMemberInput{
		{ProviderID: p.ID, ModelOverride: "grok-imagine-image-2.0"},
	}); err != nil {
		t.Fatalf("replace rule: %v", err)
	}
	k, _ := ks.Create("img-key")
	h := newLegacyHandler(ps, database)
	h.LB = lbs
	r := chi.NewRouter()
	r.Post("/v1/images/generations", h.ImageGenerations)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", k.Key,
		`{"model":"img-alias","prompt":"a cat"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if gotModel != "grok-imagine-image-2.0" {
		t.Fatalf("upstream model = %q, want the rule's model_override", gotModel)
	}
	// And the price must follow the model that actually ran: the override is
	// $0.05/image, the (unpriced) alias would have logged $0.
	var loggedModel string
	var cost float64
	if err := database.QueryRow(`SELECT model, COALESCE(cost_usd,0) FROM request_logs ORDER BY created_at DESC LIMIT 1`).Scan(&loggedModel, &cost); err != nil {
		t.Fatalf("request_logs: %v", err)
	}
	if loggedModel != "grok-imagine-image-2.0" {
		t.Fatalf("billed model = %q, want the override", loggedModel)
	}
	if cost <= 0 {
		t.Fatalf("cost = %v, want the override's per-image price", cost)
	}
}

// TestImageGenerationsIsAttemptedOnce covers P0-2: an image create is billable
// and NOT idempotent, so one client request must produce at most one upstream
// generation. Pre-fix the candidate chain retried 429/5xx and failed over to the
// next provider, billing twice for one request.
func TestImageGenerationsIsAttemptedOnce(t *testing.T) {
	attempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota","type":"rate_limit_error"}}`))
	}))
	defer upstream.Close()

	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
		`{"model":"xaip/grok-2-image","prompt":"a cat"}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream 429 relayed: %s", resp.StatusCode, body)
	}
	if attempts != 1 {
		t.Fatalf("upstream attempts = %d, want exactly 1 (billable create must not retry)", attempts)
	}
}

// TestImageEditsIsAttemptedOnce is the edits mirror of the P0-2 guard.
func TestImageEditsIsAttemptedOnce(t *testing.T) {
	attempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream exploded"}}`))
	}))
	defer upstream.Close()

	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	buf, ct := buildEdit(t, [][2]string{
		{"model", "xaip/grok-2-image"}, {"prompt", "x"},
	}, []editFile{{"image", "a.png", "image/png", decodeB64(t, tinyPNG)}})
	resp, body := postMultipart(t, srv.URL+"/v1/images/edits", key, buf, ct)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if attempts != 1 {
		t.Fatalf("upstream attempts = %d, want exactly 1", attempts)
	}
}

// TestImageRoutesEmitNoCacheHeader proves the image endpoints bypass the
// exact-match response cache (P2-2): a cached replay serves a stale re-roll and
// logs cost_usd=0. The second identical request must reach the upstream again.
func TestImageRoutesEmitNoCacheHeader(t *testing.T) {
	attempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []map[string]any{{"b64_json": tinyPNG}}})
	}))
	defer upstream.Close()

	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	for i := 0; i < 2; i++ {
		resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
			`{"model":"xaip/grok-2-image","prompt":"a cat"}`)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, body)
		}
	}
	if attempts != 2 {
		t.Fatalf("upstream attempts = %d, want 2 (generated media must not be cached)", attempts)
	}
}

// TestImageCreateRetryIsDisabled proves the retry half of P0-2 with the retry
// policy FORCED ON: even a handler configured to retry twice must attempt a
// billable image create exactly once, because the retry would buy a second
// generated (and billed) asset.
func TestImageCreateRetryIsDisabled(t *testing.T) {
	attempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"internal"}}`))
	}))
	defer upstream.Close()

	// A handler whose retry policy is deliberately eager: only opts.noRetry can
	// stop the retries in this test.
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	if _, err := ps.Create("xaip", models.ProviderXAI, upstream.URL+"/v1", "upstream-key"); err != nil {
		t.Fatal(err)
	}
	k, _ := ks.Create("retry-key")
	h := newLegacyHandler(ps, database)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 2, BaseDelay: time.Millisecond}
	router := chi.NewRouter()
	router.Post("/v1/images/generations", h.ImageGenerations)
	srv2 := httptest.NewServer(router)
	defer srv2.Close()

	resp, body := postJSONImage(t, srv2.URL+"/v1/images/generations", k.Key,
		`{"model":"xaip/grok-2-image","prompt":"a cat"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if attempts != 1 {
		t.Fatalf("upstream attempts = %d, want 1 even with retries enabled", attempts)
	}
}

// TestImageCreateDoesNotFailOver proves the failover half of P0-2: with a
// failover routing rule over two providers, a failing primary must NOT advance
// to the second provider (that would generate and bill a second asset).
func TestImageCreateDoesNotFailOver(t *testing.T) {
	var hitsA, hitsB int
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"a exploded"}}`))
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []map[string]any{{"b64_json": tinyPNG}}})
	}))
	defer upB.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	pa, err := ps.Create("xa", models.ProviderXAI, upA.URL+"/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := ps.Create("xb", models.ProviderXAI, upB.URL+"/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	lbs := lb.NewStore(database)
	if err := lbs.ReplaceRule("grok-2-image", string(lb.StrategyFailover), []lb.RuleMemberInput{
		{ProviderID: pa.ID}, {ProviderID: pb.ID},
	}); err != nil {
		t.Fatalf("replace rule: %v", err)
	}
	k, _ := ks.Create("fo-key")
	h := newLegacyHandler(ps, database)
	h.LB = lbs
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0}
	router := chi.NewRouter()
	router.Post("/v1/images/generations", h.ImageGenerations)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", k.Key,
		`{"model":"grok-2-image","prompt":"a cat"}`)
	if resp.StatusCode < 400 {
		t.Fatalf("a failing primary must not be masked by a failover: %d %s", resp.StatusCode, body)
	}
	if hitsA != 1 {
		t.Fatalf("primary attempts = %d, want 1", hitsA)
	}
	if hitsB != 0 {
		t.Fatalf("secondary was billed for a failed create (%d hits); failover must be disabled", hitsB)
	}
}

// TestImageEditsModelOverrideReachesUpstream is the edits mirror of
// TestImageModelOverrideReachesUpstream.
//
// The override travels on the candidate and is applied with replaceModelInBody,
// a JSON rewriter — which is a no-op on a MULTIPART body. The edits prepare
// closure therefore re-derived the upstream model from the (unmodified) form and
// sent the client's alias, so /v1/images/edits silently ignored
// lb_rules.model_override while /v1/images/generations honoured it: the provider
// answered 400 for the alias, and when the alias happened to be a valid id the
// request was priced from the alias (unpriced → $0) while the log recorded the
// override. The fix threads the router-resolved candidate model into prepareFn.
func TestImageEditsModelOverrideReachesUpstream(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(8 << 20); err == nil {
			gotModel = r.FormValue("model")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []map[string]any{{"b64_json": tinyPNG}}})
	}))
	defer upstream.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	p, err := ps.Create("xaip", models.ProviderXAI, upstream.URL+"/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	lbs := lb.NewStore(database)
	if err := lbs.ReplaceRule("img-alias", "", []lb.RuleMemberInput{
		{ProviderID: p.ID, ModelOverride: "grok-imagine-image-2.0"},
	}); err != nil {
		t.Fatalf("replace rule: %v", err)
	}
	k, _ := ks.Create("img-edit-key")
	h := newLegacyHandler(ps, database)
	h.LB = lbs
	r := chi.NewRouter()
	r.Post("/v1/images/edits", h.ImageEdits)
	srv := httptest.NewServer(r)
	defer srv.Close()

	buf, ct := buildEdit(t, [][2]string{
		{"model", "img-alias"}, {"prompt", "add a hat"},
	}, []editFile{{"image", "a.png", "image/png", decodeB64(t, tinyPNG)}})
	resp, body := postMultipart(t, srv.URL+"/v1/images/edits", k.Key, buf, ct)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if gotModel != "grok-imagine-image-2.0" {
		t.Fatalf("upstream model = %q, want the rule's model_override (multipart body was not rewritten)", gotModel)
	}
	var loggedModel string
	var cost float64
	if err := database.QueryRow(`SELECT model, COALESCE(cost_usd,0) FROM request_logs ORDER BY created_at DESC LIMIT 1`).Scan(&loggedModel, &cost); err != nil {
		t.Fatalf("request_logs: %v", err)
	}
	if loggedModel != "grok-imagine-image-2.0" {
		t.Fatalf("billed model = %q, want the override", loggedModel)
	}
	if cost <= 0 {
		t.Fatalf("cost = %v, want the override's per-image price", cost)
	}
}
