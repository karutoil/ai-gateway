package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/models"
)

// buildEdit builds a multipart edit body from ordered text fields and file
// parts, mirroring the shapes real OpenAI SDKs send.
type editFile struct {
	field, name, ctype string
	data               []byte
}

func buildEdit(t *testing.T, fields [][2]string, files []editFile) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		hdr := make(map[string][]string)
		hdr["Content-Disposition"] = []string{`form-data; name="` + f.field + `"; filename="` + f.name + `"`}
		if f.ctype != "" {
			hdr["Content-Type"] = []string{f.ctype}
		}
		part, err := mw.CreatePart(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func postMultipart(t *testing.T, url, key string, body *bytes.Buffer, ctype string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, body)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", ctype)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// TestImageGenerationsRejectsOverlongPrompt pins the prompt bound: an unbounded
// prompt must be refused locally (400) and never reach an upstream. 32000 is
// the OpenAI Images limit mirrored by imagegen.MaxPromptLen.
func TestImageGenerationsRejectsOverlongPrompt(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []map[string]any{{"b64_json": tinyPNG}}})
	}))
	defer upstream.Close()
	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)

	payload, err := json.Marshal(map[string]any{
		"model": "xaip/grok-2-image", "prompt": strings.Repeat("a", 32001),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key, string(payload))
	if resp.StatusCode != 400 || !strings.Contains(string(body), "prompt") {
		t.Fatalf("overlong prompt: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "32000") {
		t.Fatalf("refusal should name the limit: %s", body)
	}
	if calls != 0 {
		t.Fatalf("overlong prompt reached upstream %d times", calls)
	}

	// The boundary itself is accepted (and forwarded).
	payload, _ = json.Marshal(map[string]any{
		"model": "xaip/grok-2-image", "prompt": strings.Repeat("a", 32000),
	})
	resp, body = postJSONImage(t, srv.URL+"/v1/images/generations", key, string(payload))
	if resp.StatusCode == 400 {
		t.Fatalf("exactly-at-limit prompt must be accepted: %s", body)
	}
	if calls != 1 {
		t.Fatalf("at-limit prompt should reach upstream once, got %d", calls)
	}
}

// TestImageEditsMultipartValidation covers the multipart validation surface the
// existing tests skipped: missing boundary, malformed body, missing model, the
// reference-image count bound, and mask handling.
func TestImageEditsMultipartValidation(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []map[string]any{{"b64_json": tinyPNG}}})
	}))
	defer upstream.Close()
	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	png := decodeB64(t, tinyPNG)

	t.Run("missing boundary", func(t *testing.T) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/images/edits", strings.NewReader("garbage"))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "multipart/form-data")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 400 || !strings.Contains(string(body), "boundary") {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/images/edits", strings.NewReader("not-a-multipart-body"))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 400 || !strings.Contains(string(body), "malformed multipart") {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
	})

	t.Run("missing model", func(t *testing.T) {
		buf, ct := buildEdit(t, [][2]string{{"prompt", "x"}}, []editFile{{"image", "a.png", "image/png", png}})
		resp, body := postMultipart(t, srv.URL+"/v1/images/edits", key, buf, ct)
		if resp.StatusCode != 400 || !strings.Contains(string(body), "model") {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
	})

	t.Run("missing prompt", func(t *testing.T) {
		buf, ct := buildEdit(t, [][2]string{{"model", "xaip/grok-2-image"}}, []editFile{{"image", "a.png", "image/png", png}})
		resp, body := postMultipart(t, srv.URL+"/v1/images/edits", key, buf, ct)
		if resp.StatusCode != 400 || !strings.Contains(string(body), "prompt") {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
	})

	t.Run("too many reference images", func(t *testing.T) {
		files := make([]editFile, 0, 5)
		for i := 0; i < 5; i++ {
			files = append(files, editFile{"image", "a.png", "image/png", png})
		}
		buf, ct := buildEdit(t, [][2]string{{"model", "xaip/grok-2-image"}, {"prompt", "x"}}, files)
		resp, body := postMultipart(t, srv.URL+"/v1/images/edits", key, buf, ct)
		if resp.StatusCode != 400 || !strings.Contains(string(body), "too many reference images") {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
	})

	if calls != 0 {
		t.Fatalf("invalid edit requests reached the upstream %d times", calls)
	}
}

// TestImageEditsForwardsOptionsAndMask proves the non-image multipart fields
// (n/size/quality/style/response_format/aspect_ratio/resolution) and the mask
// part survive the rebuild to an OpenAI-compatible upstream.
func TestImageEditsForwardsOptionsAndMask(t *testing.T) {
	var got map[string][]string
	var gotFiles []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			t.Errorf("upstream multipart: %v", err)
			w.WriteHeader(400)
			return
		}
		got = r.MultipartForm.Value
		for name := range r.MultipartForm.File {
			gotFiles = append(gotFiles, name)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []map[string]any{{"b64_json": tinyPNG}}})
	}))
	defer upstream.Close()
	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	png := decodeB64(t, tinyPNG)

	buf, ct := buildEdit(t, [][2]string{
		{"model", "xaip/grok-2-image"},
		{"prompt", "make it blue"},
		{"n", "3"},
		{"size", "1024x1024"},
		{"quality", "high"},
		{"style", "vivid"},
		{"response_format", "b64_json"},
		{"aspect_ratio", "16:9"},
		{"resolution", "2k"},
	}, []editFile{
		{"image", "in.png", "image/png", png},
		{"mask", "mask.png", "image/png", png},
	})
	resp, body := postMultipart(t, srv.URL+"/v1/images/edits", key, buf, ct)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	want := map[string]string{
		"model": "grok-2-image", "prompt": "make it blue", "n": "3",
		"size": "1024x1024", "quality": "high", "style": "vivid",
		"response_format": "b64_json", "aspect_ratio": "16:9", "resolution": "2k",
	}
	for k, v := range want {
		if len(got[k]) == 0 || got[k][0] != v {
			t.Errorf("upstream field %s = %v, want %q", k, got[k], v)
		}
	}
	if len(gotFiles) != 2 {
		t.Fatalf("upstream file parts = %v, want image + mask", gotFiles)
	}
}

// TestImageEditsRejectsInvalidScalarFields proves the multipart path now applies
// the JSON path's scalar validation instead of silently swallowing bad values.
//
// Pre-fix a non-numeric `n` was dropped (the upstream then chose its own count),
// an out-of-range `n` was forwarded, an unknown `size`/`response_format` reached
// the provider, and an over-long prompt was accepted — all with a 200. Each is
// a 400 that names the field, and none of them reaches the upstream.
func TestImageEditsRejectsInvalidScalarFields(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []map[string]any{{"b64_json": tinyPNG}}})
	}))
	defer upstream.Close()
	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	png := decodeB64(t, tinyPNG)

	cases := []struct{ name, field, value, want string }{
		{"non-numeric n", "n", "abc", "n"},
		{"n too large", "n", "99", "n"},
		{"n zero", "n", "0", "n"},
		{"unknown size", "size", "1920x1080", "size"},
		{"bad response_format", "response_format", "png", "response_format"},
		{"over-long prompt", "prompt", strings.Repeat("a", 32001), "prompt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf, ct := buildEdit(t, [][2]string{
				{"model", "xaip/grok-2-image"}, {"prompt", "x"}, {c.field, c.value},
			}, []editFile{{"image", "a.png", "image/png", png}})
			resp, body := postMultipart(t, srv.URL+"/v1/images/edits", key, buf, ct)
			if resp.StatusCode != 400 {
				t.Fatalf("status %d, want 400: %s", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), c.want) {
				t.Fatalf("400 must name %q: %s", c.want, body)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("invalid edits reached the upstream %d times", calls)
	}
}

// TestImageEditsRejectsOversizedAndOverlongBodies proves the two new structural
// caps: a part-count cap and an aggregate byte cap, both 400s that name the
// limit. Pre-fix the only bound was the whole-request body cap.
func TestImageEditsRejectsOversizedAndOverlongBodies(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	defer upstream.Close()
	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	png := decodeB64(t, tinyPNG)

	t.Run("part count", func(t *testing.T) {
		fields := [][2]string{{"model", "xaip/grok-2-image"}, {"prompt", "x"}}
		for i := 0; i < MaxImageEditParts; i++ {
			fields = append(fields, [2]string{"quality", "high"})
		}
		buf, ct := buildEdit(t, fields, []editFile{{"image", "a.png", "image/png", png}})
		resp, body := postMultipart(t, srv.URL+"/v1/images/edits", key, buf, ct)
		if resp.StatusCode != 400 || !strings.Contains(string(body), "part limit") {
			t.Fatalf("part-count cap: %d %s", resp.StatusCode, body)
		}
	})

	t.Run("aggregate bytes", func(t *testing.T) {
		// Four reference images within the count limit but far over the byte cap.
		big := make([]byte, MaxImageEditBytes/2)
		files := []editFile{
			{"image", "a.png", "image/png", big},
			{"image", "b.png", "image/png", big},
		}
		buf, ct := buildEdit(t, [][2]string{{"model", "xaip/grok-2-image"}, {"prompt", "x"}}, files)
		resp, body := postMultipart(t, srv.URL+"/v1/images/edits", key, buf, ct)
		if resp.StatusCode != 400 || !strings.Contains(string(body), "aggregate limit") {
			t.Fatalf("byte cap: %d %s", resp.StatusCode, body)
		}
	})

	if calls != 0 {
		t.Fatalf("over-limit edits reached the upstream %d times", calls)
	}
}

// TestImageEditsRemoteImagePartIsRejectedInJSONPath proves the JSON generation
// path refuses remote reference URLs (no server-side fetch), while the
// multipart path can only carry bytes.
func TestImageEditsRemoteImagePartIsRejectedInJSONPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
	resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
		`{"model":"xaip/grok-2-image","prompt":"x","image":"https://evil.example/a.png"}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "remote image URLs") {
		t.Fatalf("remote reference: %d %s", resp.StatusCode, body)
	}
}
