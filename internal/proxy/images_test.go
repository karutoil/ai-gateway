package proxy

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/imagegen"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func decodeB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("bad base64 in test fixture: %v", err)
	}
	return b
}

// imageTestServer wires a proxy handler with one provider of the given type
// pointing at a mock upstream, plus a gateway key and the image routes.
func imageTestServer(t *testing.T, provName string, typ models.ProviderType, upstream *httptest.Server) (*httptest.Server, *sql.DB, string) {
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
	if _, err := ps.Create(provName, typ, upstream.URL+"/v1", "upstream-key"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("test-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	// P1-12 tri-state: <0 = use the 300s default, 0 = DISABLED, >0 = that
	// budget. -1 is the "unset" sentinel, which is what the previous
	// `h.ImageTTFB = 0 // use DefaultImageTTFB` meant before 0 became meaningful.
	h.ImageTTFB = -1
	h.ImageTTFBDisabled = false
	r := chi.NewRouter()
	r.Post("/v1/images/generations", h.ImageGenerations)
	r.Post("/v1/images/edits", h.ImageEdits)
	r.Post("/v1/chat/completions", h.ChatCompletions)
	r.Get("/v1/models", h.Models)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { database.Close() })
	return srv, database, k.Key
}

func postJSONImage(t *testing.T, url, key, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

// TestImageGenerationsOpenAIPassthrough proves the [OI]-compatible path: the
// body is forwarded to {base}/images/generations with the provider prefix
// stripped from the model, and the upstream's [OI] Images response is relayed.
func TestImageGenerationsOpenAIPassthrough(t *testing.T) {
	var gotPath, gotModel, gotAuth string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		gotModel, _ = gotBody["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": 123,
			"data":    []map[string]any{{"b64_json": tinyPNG, "mime_type": "image/png"}},
		})
	}))
	defer upstream.Close()

	srv, database, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
		`{"model":"xaip/grok-2-image","prompt":"a cat","n":1}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotModel != "grok-2-image" {
		t.Fatalf("upstream model = %q, want the prefix stripped", gotModel)
	}
	if gotAuth != "Bearer upstream-key" {
		t.Fatalf("upstream auth = %q", gotAuth)
	}
	if !strings.Contains(string(body), tinyPNG) {
		t.Fatalf("response not relayed: %s", body)
	}
	// Per-image billing: grok-2-image is $0.07/image.
	var cost float64
	if err := database.QueryRow(`SELECT cost_usd FROM request_logs ORDER BY created_at DESC LIMIT 1`).Scan(&cost); err != nil {
		t.Fatalf("request_logs: %v", err)
	}
	if cost < 0.069 || cost > 0.071 {
		t.Fatalf("cost = %v, want ~0.07 per image", cost)
	}
}

// TestImageGenerationsGeminiTranslate proves the Google dialect: the request
// is rewritten into a generateContent body (IMAGE response modality) with the
// key in x-goog-api-key, and the native inlineData response is translated back
// into the [OI] Images shape.
func TestImageGenerationsGeminiTranslate(t *testing.T) {
	var gotPath, gotKeyHeader, gotAuth string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKeyHeader = r.Header.Get("x-goog-api-key")
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content": map[string]any{
					"role": "model",
					"parts": []map[string]any{
						{"text": "Here is your cat"},
						{"inlineData": map[string]any{"mimeType": "image/png", "data": tinyPNG}},
					},
				},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{"promptTokenCount": 9, "candidatesTokenCount": 1290, "totalTokenCount": 1299},
			"modelVersion":  "gemini-2.5-flash-image",
		})
	}))
	defer upstream.Close()

	srv, database, key := imageTestServer(t, "goog", models.ProviderGoogle, upstream)
	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
		`{"model":"goog/gemini-2.5-flash-image","prompt":"a cat","response_format":"b64_json"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if gotPath != "/v1beta/models/gemini-2.5-flash-image:generateContent" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotKeyHeader != "upstream-key" {
		t.Fatalf("x-goog-api-key = %q", gotKeyHeader)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization must not be sent to Google, got %q", gotAuth)
	}
	gc, _ := gotBody["generationConfig"].(map[string]any)
	mods, _ := gc["responseModalities"].([]any)
	if len(mods) != 2 || mods[0] != "TEXT" || mods[1] != "IMAGE" {
		t.Fatalf("responseModalities = %v", mods)
	}
	var out struct {
		Data []struct {
			B64JSON       string `json:"b64_json"`
			MimeType      string `json:"mime_type"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("translated response is not [OI] Images shape: %v (%s)", err, body)
	}
	if len(out.Data) != 1 || out.Data[0].B64JSON != tinyPNG {
		t.Fatalf("translated data = %+v", out.Data)
	}
	if out.Data[0].MimeType != "image/png" || out.Data[0].RevisedPrompt != "Here is your cat" {
		t.Fatalf("datum = %+v", out.Data[0])
	}
	if out.Usage == nil || out.Usage.InputTokens != 9 || out.Usage.OutputTokens != 1290 {
		t.Fatalf("usage = %+v", out.Usage)
	}
	var cost float64
	if err := database.QueryRow(`SELECT cost_usd FROM request_logs ORDER BY created_at DESC LIMIT 1`).Scan(&cost); err != nil {
		t.Fatalf("request_logs: %v", err)
	}
	if cost < 0.038 || cost > 0.040 {
		t.Fatalf("cost = %v, want ~0.039 per image", cost)
	}
}

// TestImageGenerationsImagenTranslate proves the Imagen :predict dialect.
func TestImageGenerationsImagenTranslate(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"predictions": []map[string]any{{"bytesBase64Encoded": tinyPNG, "mimeType": "image/png"}},
		})
	}))
	defer upstream.Close()

	srv, _, key := imageTestServer(t, "goog", models.ProviderGoogle, upstream)
	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
		`{"model":"goog/imagen-4.0-generate-001","prompt":"a cat","n":2,"aspect_ratio":"16:9"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if gotPath != "/v1beta/models/imagen-4.0-generate-001:predict" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	params, _ := gotBody["parameters"].(map[string]any)
	if params["sampleCount"].(float64) != 2 || params["aspectRatio"] != "16:9" {
		t.Fatalf("parameters = %v", params)
	}
	if !strings.Contains(string(body), tinyPNG) {
		t.Fatalf("translated body = %s", body)
	}
}

// TestImageGenerationsValidatesLocally proves malformed requests fail fast at
// the gateway without an upstream round trip.
func TestImageGenerationsValidatesLocally(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)

	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key, `{"model":"xaip/grok-2-image"}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "prompt") {
		t.Fatalf("missing prompt: status %d body %s", resp.StatusCode, body)
	}
	resp, body = postJSONImage(t, srv.URL+"/v1/images/generations", key, `{"model":"xaip/grok-2-image","prompt":"x","n":99}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "n") {
		t.Fatalf("bad n: status %d body %s", resp.StatusCode, body)
	}
	if calls != 0 {
		t.Fatalf("invalid requests must not reach upstream, got %d calls", calls)
	}
}

// TestImageEditsMultipartPassthrough proves multipart edits reach an
// [OI]-compatible upstream with the image part intact and the model prefix
// stripped.
func TestImageEditsMultipartPassthrough(t *testing.T) {
	var gotPath, gotModel string
	var gotImageLen int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			t.Errorf("upstream could not parse multipart: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotModel = r.FormValue("model")
		if f, _, err := r.FormFile("image"); err == nil {
			b, _ := io.ReadAll(f)
			gotImageLen = len(b)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []map[string]any{{"b64_json": tinyPNG}}})
	}))
	defer upstream.Close()

	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "xaip/grok-2-image")
	_ = mw.WriteField("prompt", "make it blue")
	fw, _ := mw.CreateFormFile("image", "in.png")
	_, _ = fw.Write(decodeB64(t, tinyPNG))
	_ = mw.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/images/edits", &buf)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if gotPath != "/v1/images/edits" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotModel != "grok-2-image" {
		t.Fatalf("upstream model = %q", gotModel)
	}
	if gotImageLen == 0 {
		t.Fatal("image part was lost in the multipart rebuild")
	}
}

// TestImageEditsGoogleInlinesImages proves Google edits are converted from
// multipart into an inlineData generateContent body (Gemini has no multipart
// edit endpoint).
func TestImageEditsGoogleInlinesImages(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("google edit must be JSON, got %q", ct)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{"content": map[string]any{"parts": []map[string]any{{"inlineData": map[string]any{"mimeType": "image/png", "data": tinyPNG}}}}}},
		})
	}))
	defer upstream.Close()

	srv, _, key := imageTestServer(t, "goog", models.ProviderGoogle, upstream)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "goog/gemini-2.5-flash-image")
	_ = mw.WriteField("prompt", "make it blue")
	fw, _ := mw.CreateFormFile("image", "in.png")
	_, _ = fw.Write(decodeB64(t, tinyPNG))
	_ = mw.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/images/edits", &buf)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if gotPath != "/v1beta/models/gemini-2.5-flash-image:generateContent" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	contents, _ := gotBody["contents"].([]any)
	parts, _ := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want text + inlineData", len(parts))
	}
	if _, ok := parts[1].(map[string]any)["inlineData"]; !ok {
		t.Fatalf("reference image was not inlined: %v", parts)
	}
}

// TestImageEditsRequiresImage proves an edit without a file is a 400.
func TestImageEditsRequiresImage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "xaip/grok-2-image")
	_ = mw.WriteField("prompt", "x")
	_ = mw.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/images/edits", &buf)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "image") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

// TestImageEditsRejectsNonMultipart proves a JSON body on the edits endpoint is
// refused with a clear message rather than being misparsed.
func TestImageEditsRejectsNonMultipart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	resp, body := postJSONImage(t, srv.URL+"/v1/images/edits", key, `{"model":"xaip/grok-2-image","prompt":"x"}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "multipart") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

// TestChatRejectsImageModel proves an image model cannot be sent chat traffic
// (and vice versa the images endpoint refuses a chat model).
func TestChatRejectsImageModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)

	resp, body := postJSONImage(t, srv.URL+"/v1/chat/completions", key,
		`{"model":"xaip/grok-2-image","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "image-only") {
		t.Fatalf("chat with image-only model: status %d body %s", resp.StatusCode, body)
	}

	// A decision (System One) model is positively not an image model.
	resp, body = postJSONImage(t, srv.URL+"/v1/images/generations", key,
		`{"model":"clef","prompt":"a cat"}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "decision") {
		t.Fatalf("images with decision model: status %d body %s", resp.StatusCode, body)
	}
	// An unknown id is allowed through: resellers expose image models the
	// gateway cannot enumerate, and the endpoint must stay compatible.
	resp, _ = postJSONImage(t, srv.URL+"/v1/images/generations", key,
		`{"model":"xaip/flux-1.1-pro","prompt":"a cat"}`)
	if resp.StatusCode == 400 {
		t.Fatalf("unknown model must be forwarded, not rejected with 400")
	}
}

// TestModelsListingFlagsImageModels proves /v1/models surfaces the image
// capability, the kind marker and the per-image price.
func TestModelsListingFlagsImageModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"grok-2-image","object":"model"},{"id":"grok-3","object":"model"}]}`))
	}))
	defer upstream.Close()

	srv, database, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	// Seed provider_models the way discovery would.
	if _, err := database.Exec(`INSERT INTO provider_models(id, provider_id, model_id, display_name, owned_by, image, source, created_at, updated_at)
		SELECT 'pm1', id, 'grok-2-image', 'Grok 2 Image', 'xaip', 1, 'image', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP FROM providers LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO provider_models(id, provider_id, model_id, display_name, owned_by, image, source, created_at, updated_at)
		SELECT 'pm2', id, 'grok-3', 'Grok 3', 'xaip', 0, 'discovered', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP FROM providers LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	var sawImage, sawChat bool
	for _, m := range list.Data {
		switch m["model_id"] {
		case "grok-2-image":
			sawImage = true
			if m["image"] != true || m["kind"] != "image" {
				t.Fatalf("image model row = %v", m)
			}
			if _, ok := m["image_cost"]; !ok {
				t.Fatalf("image model row missing image_cost: %v", m)
			}
		case "grok-3":
			sawChat = true
			if m["image"] == true {
				t.Fatalf("chat model flagged as image: %v", m)
			}
		}
	}
	if !sawImage || !sawChat {
		t.Fatalf("models listing incomplete: image=%v chat=%v (%s)", sawImage, sawChat, body)
	}
}

// TestImageGenerationCountsMultipleImages proves cost scales with n.
func TestImageGenerationCountsMultipleImages(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": 1,
			"data": []map[string]any{
				{"b64_json": tinyPNG}, {"b64_json": tinyPNG}, {"b64_json": tinyPNG},
			},
		})
	}))
	defer upstream.Close()
	srv, database, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
		`{"model":"xaip/grok-2-image","prompt":"a cat","n":3}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var cost float64
	if err := database.QueryRow(`SELECT cost_usd FROM request_logs ORDER BY created_at DESC LIMIT 1`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost < 0.20 || cost > 0.22 {
		t.Fatalf("cost = %v, want ~0.21 (3 x 0.07)", cost)
	}
}

// TestParseEditMultipartAcceptsNestedImageField guards the field-name handling
// (image vs images) that clients disagree on.
func TestParseEditMultipartAcceptsImagesField(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "gpt-image-1")
	_ = mw.WriteField("prompt", "edit")
	fw, _ := mw.CreateFormFile("images", "a.png")
	_, _ = fw.Write(decodeB64(t, tinyPNG))
	_ = mw.Close()

	norm, files, err := parseEditMultipart(buf.Bytes(), mw.FormDataContentType())
	if err != nil {
		t.Fatal(err)
	}
	if len(norm.Images) != 1 || len(files) != 1 {
		t.Fatalf("images=%d files=%d", len(norm.Images), len(files))
	}
	if norm.N != 1 {
		t.Fatalf("default n = %d", norm.N)
	}
}

func TestGoogleImageTarget(t *testing.T) {
	cases := []struct {
		base, model string
		dialect     imagegen.Dialect
		want        string
	}{
		{"https://generativelanguage.googleapis.com", "gemini-2.5-flash-image", imagegen.DialectGemini, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash-image:generateContent"},
		{"https://generativelanguage.googleapis.com/v1beta", "imagen-4.0-generate-001", imagegen.DialectImagen, "https://generativelanguage.googleapis.com/v1beta/models/imagen-4.0-generate-001:predict"},
		{"https://generativelanguage.googleapis.com/v1beta/", "gemini-3-pro-image-preview", imagegen.DialectGemini, "https://generativelanguage.googleapis.com/v1beta/models/gemini-3-pro-image-preview:generateContent"},
	}
	for _, c := range cases {
		got := googleImageTarget(c.base, c.model, c.dialect)
		if got != c.want {
			t.Errorf("googleImageTarget(%q,%q) = %q, want %q", c.base, c.model, got, c.want)
		}
	}
}

// TestEditMultipartContentTypeReachesUpstream proves the multipart boundary is
// preserved on the upstream request (a stale application/json would break it).
func TestEditMultipartContentTypeReachesUpstream(t *testing.T) {
	var gotCT string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []map[string]any{{"b64_json": tinyPNG}}})
	}))
	defer upstream.Close()
	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "xaip/grok-2-image")
	_ = mw.WriteField("prompt", "x")
	fw, _ := mw.CreateFormFile("image", "in.png")
	_, _ = fw.Write(decodeB64(t, tinyPNG))
	_ = mw.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/images/edits", &buf)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	mt, _, err := mime.ParseMediaType(gotCT)
	if err != nil || mt != "multipart/form-data" {
		t.Fatalf("upstream content-type = %q (err %v), want multipart/form-data", gotCT, err)
	}
}

// TestImageCapableModelStillChats is the regression guard for the capability
// bug: a model that generates images AND text (models.dev modalities output
// ["text","image"]) must remain usable on the chat endpoint, while a pure
// generator (output ["image"]) is refused with a pointer to the images API.
func TestImageCapableModelStillChats(t *testing.T) {
	var chatHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			chatHits++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	srv, database, key := imageTestServer(t, "goog", models.ProviderGoogle, upstream)
	// Catalog rows mirroring models.dev: a chat-capable image model and a
	// pure generator, both flagged image.
	for _, row := range []struct{ id, mods string }{
		{"gemini-2.5-flash-image", `{"input":["text","image"],"output":["text","image"]}`},
		{"imagen-4.0-generate-001", `{"input":["text"],"output":["image"]}`},
	} {
		if _, err := database.Exec(`INSERT INTO models_catalog(id, provider, name, image, modalities, updated_at) VALUES(?,?,?,?,?,CURRENT_TIMESTAMP)`,
			row.id, "google", row.id, 1, row.mods); err != nil {
			t.Fatalf("seed catalog %s: %v", row.id, err)
		}
	}
	// The chat-capable model is routed through an openai_compatible provider
	// so the chat path is exercised end to end.
	if _, err := database.Exec(`UPDATE providers SET type='openai_compatible', base_url=? WHERE name='goog'`, upstream.URL+"/v1"); err != nil {
		t.Fatal(err)
	}

	// Image-capable -> chat is allowed and reaches the upstream.
	resp, body := postJSONImage(t, srv.URL+"/v1/chat/completions", key,
		`{"model":"goog/gemini-2.5-flash-image","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("image-capable model must still chat: status %d body %s", resp.StatusCode, body)
	}
	if chatHits == 0 {
		t.Fatal("chat request never reached the upstream")
	}

	// Image-only -> chat is refused, naming the images endpoint.
	resp, body = postJSONImage(t, srv.URL+"/v1/chat/completions", key,
		`{"model":"goog/imagen-4.0-generate-001","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 400 {
		t.Fatalf("image-only model must be refused on chat: status %d body %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "image-only") || !strings.Contains(string(body), "/v1/images/generations") {
		t.Fatalf("refusal must name the images endpoint: %s", body)
	}
}

// TestModelsListingExposesImageOnly proves /v1/models surfaces the
// image/image_only distinction the UI needs to pick models per mode.
func TestModelsListingExposesImageOnly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()
	srv, database, key := imageTestServer(t, "goog", models.ProviderGoogle, upstream)
	for _, row := range []struct {
		id, mods string
	}{
		{"gemini-2.5-flash-image", `{"input":["text","image"],"output":["text","image"]}`},
		{"imagen-4.0-generate-001", `{"input":["text"],"output":["image"]}`},
	} {
		if _, err := database.Exec(`INSERT INTO provider_models(id, provider_id, model_id, display_name, owned_by, image, modalities, source, created_at, updated_at)
			SELECT ?, id, ?, ?, 'goog', 1, ?, 'image', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP FROM providers LIMIT 1`,
			"pm-"+row.id, row.id, row.id, row.mods); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range list.Data {
		if m["image"] == true {
			got[m["model_id"].(string)] = m["image_only"] == true
		}
	}
	if only, ok := got["gemini-2.5-flash-image"]; !ok || only {
		t.Errorf("gemini-2.5-flash-image: present=%v image_only=%v, want present and not image-only", ok, only)
	}
	if only, ok := got["imagen-4.0-generate-001"]; !ok || !only {
		t.Errorf("imagen-4.0-generate-001: present=%v image_only=%v, want present and image-only", ok, only)
	}
}
