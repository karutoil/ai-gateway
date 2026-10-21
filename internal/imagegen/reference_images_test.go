package imagegen

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestParseRequestImageShapes covers the reference-image shapes decodeImage
// accepts, which the existing tests only exercised for one data URL and one
// b64_json object. Each branch is a separate client SDK convention.
func TestParseRequestImageShapes(t *testing.T) {
	raw := pngBytes()
	b64 := base64.StdEncoding.EncodeToString(raw)

	cases := []struct {
		name     string
		image    string
		wantMime string
		wantLen  int
	}{
		{"bare base64", `"` + b64 + `"`, "image/png", len(raw)},
		{"data url", `"data:image/png;base64,` + b64 + `"`, "image/png", len(raw)},
		{"data url jpeg mime", `"data:image/jpeg;base64,` + b64 + `"`, "image/jpeg", len(raw)},
		{"object data", `{"data":"` + b64 + `"}`, "image/png", len(raw)},
		{"object b64_json", `{"b64_json":"` + b64 + `"}`, "image/png", len(raw)},
		{"object b64_json with mime", `{"b64_json":"` + b64 + `","mime_type":"image/webp"}`, "image/webp", len(raw)},
		{"object url as base64", `{"url":"` + b64 + `"}`, "image/png", len(raw)},
		{"object image_url as base64", `{"image_url":"` + b64 + `"}`, "image/png", len(raw)},
		{"object data url", `{"url":"data:image/jpeg;base64,` + b64 + `"}`, "image/jpeg", len(raw)},
		{"object media_type", `{"data":"` + b64 + `","media_type":"image/gif"}`, "image/gif", len(raw)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"model":"gpt-image-1","prompt":"x","image":` + c.image + `}`
			n, err := ParseRequest([]byte(body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(n.Images) != 1 {
				t.Fatalf("images = %d, want 1", len(n.Images))
			}
			if n.Images[0].MimeType != c.wantMime {
				t.Errorf("mime = %q, want %q", n.Images[0].MimeType, c.wantMime)
			}
			if len(n.Images[0].Data) != c.wantLen {
				t.Errorf("data length = %d, want %d", len(n.Images[0].Data), c.wantLen)
			}
		})
	}
}

// TestParseRequestImageErrors covers the malformed reference shapes: every one
// must be a field-named ValidateError so the proxy can answer a useful 400.
func TestParseRequestImageErrors(t *testing.T) {
	cases := []struct{ name, image, wantField, wantMsg string }{
		{"empty string", `""`, "images[0]", "must not be empty"},
		{"malformed data url", `"data:image/png;base64"`, "images[0]", ""},
		{"bad base64", `"!!!!"`, "images[0]", "data URL or base64"},
		{"malformed data url payload", `"data:image/png;base64,!!!!"`, "images[0]", "invalid base64"},
		{"object without bytes", `{"mime_type":"image/png"}`, "images[0]", "requires url, b64_json, or data"},
		{"remote url object", `{"url":"https://evil.example/a.png"}`, "images[0]", "remote image URLs"},
		{"remote image_url object", `{"image_url":"http://evil.example/a.png"}`, "images[0]", "remote image URLs"},
		{"images not array", `5`, "images", "must be an array of images"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := "image"
			if strings.HasPrefix(c.name, "images") {
				key = "images"
			}
			body := `{"model":"m","prompt":"x","` + key + `":` + c.image + `}`
			_, err := ParseRequest([]byte(body))
			if err == nil {
				t.Fatal("expected an error")
			}
			ve, ok := err.(*ValidateError)
			if !ok {
				t.Fatalf("expected ValidateError, got %T (%v)", err, err)
			}
			if ve.Field != c.wantField {
				t.Errorf("field = %q, want %q (%v)", ve.Field, c.wantField, err)
			}
			if c.wantMsg != "" && !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("message %q missing %q", err.Error(), c.wantMsg)
			}
		})
	}
}

// TestParseRequestMask covers the optional mask edge cases.
func TestParseRequestMask(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes())
	n, err := ParseRequest([]byte(`{"model":"m","prompt":"x","mask":"` + b64 + `"}`))
	if err != nil {
		t.Fatalf("mask: %v", err)
	}
	if n.Mask == nil || n.Mask.MimeType != "image/png" {
		t.Fatalf("mask = %+v", n.Mask)
	}
	// null mask is simply absent.
	n, err = ParseRequest([]byte(`{"model":"m","prompt":"x","mask":null}`))
	if err != nil || n.Mask != nil {
		t.Fatalf("null mask: %+v err=%v", n.Mask, err)
	}
	// A bad mask is a mask-field error, not an images-field one.
	_, err = ParseRequest([]byte(`{"model":"m","prompt":"x","mask":"https://e.example/m.png"}`))
	ve, ok := err.(*ValidateError)
	if !ok || ve.Field != "mask" {
		t.Fatalf("bad mask error = %v", err)
	}
}

// TestParseRequestReferenceBoundary pins MaxReferenceImages exactly: the limit
// is allowed, one over is not.
func TestParseRequestReferenceBoundary(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes())
	imgs := make([]string, 0, MaxReferenceImages)
	for i := 0; i < MaxReferenceImages; i++ {
		imgs = append(imgs, `"`+b64+`"`)
	}
	body := `{"model":"m","prompt":"x","images":[` + strings.Join(imgs, ",") + `]}`
	if _, err := ParseRequest([]byte(body)); err != nil {
		t.Fatalf("%d reference images must be accepted: %v", MaxReferenceImages, err)
	}
	imgs = append(imgs, `"`+b64+`"`)
	body = `{"model":"m","prompt":"x","images":[` + strings.Join(imgs, ",") + `]}`
	if _, err := ParseRequest([]byte(body)); err == nil {
		t.Fatalf("%d reference images must be rejected", MaxReferenceImages+1)
	}
}

// TestValidateAcceptsSameAsParseRequest proves Validate (the exported entry the
// proxy's generated docs and the SDK wrappers call) is wired to ParseRequest.
func TestValidateAcceptsSameAsParseRequest(t *testing.T) {
	if err := Validate([]byte(`{"model":"gpt-image-1","prompt":"x"}`)); err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}
	if err := Validate([]byte(`{"prompt":"x"}`)); err == nil {
		t.Fatal("invalid body accepted")
	}
}

// TestImagenRequestBodyFull proves the :predict body carries every parameter
// the API supports (sampleCount + aspectRatio from size or explicit ratio).
func TestImagenRequestBodyFull(t *testing.T) {
	n := &Normalized{Prompt: "a cat", N: 3, Size: "1792x1024"}
	b, err := ImagenRequestBody(n)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Instances  []map[string]any `json:"instances"`
		Parameters map[string]any   `json:"parameters"`
	}
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Instances) != 1 || req.Instances[0]["prompt"] != "a cat" {
		t.Fatalf("instances = %v", req.Instances)
	}
	if req.Parameters["sampleCount"].(float64) != 3 {
		t.Fatalf("sampleCount = %v", req.Parameters["sampleCount"])
	}
	if req.Parameters["aspectRatio"] != "16:9" {
		t.Fatalf("aspectRatio = %v (must derive from size)", req.Parameters["aspectRatio"])
	}

	// "auto" and an empty ratio are dropped rather than forwarded.
	for _, size := range []string{"auto", ""} {
		n := &Normalized{Prompt: "x", N: 1, Size: size}
		b, _ := ImagenRequestBody(n)
		if strings.Contains(string(b), "aspectRatio") {
			t.Errorf("size %q must not emit aspectRatio: %s", size, b)
		}
	}
	// A missing prompt is an error (Imagen cannot edit).
	if _, err := ImagenRequestBody(&Normalized{N: 1}); err == nil {
		t.Fatal("imagen without a prompt must error")
	}
}

// TestDialectForModelDefaults pins the dialect fallback used by the proxy when a
// model is not in the curated registry.
func TestDialectForModelDefaults(t *testing.T) {
	if d := DialectForModel("totally-unknown-model"); d != DialectOpenAI {
		t.Fatalf("unknown model dialect = %q, want openai", d)
	}
	if d := DialectForModel("imagen-4.0-generate-001"); d != DialectImagen {
		t.Fatalf("imagen dialect = %q", d)
	}
	if d := DialectForModel("gemini-2.5-flash-image"); d != DialectGemini {
		t.Fatalf("gemini dialect = %q", d)
	}
}

// TestAllCoversRegistry proves the exported All() is a stable copy (the catalog
// seeder iterates it) and that every entry is usable.
func TestAllCoversRegistry(t *testing.T) {
	all := All()
	if len(all) < 5 {
		t.Fatalf("registry has %d entries", len(all))
	}
	// Mutating the returned slice must not corrupt the registry.
	if len(all) > 0 {
		all[0].ID = "mutated"
	}
	if All()[0].ID == "mutated" {
		t.Fatal("All() exposes the registry slice for mutation")
	}
	for _, m := range All() {
		if m.ID == "" || m.Namespace == "" || m.Dialect == "" {
			t.Errorf("incomplete registry entry: %+v", m)
		}
		if m.CostPerImage <= 0 {
			t.Errorf("%s has no per-image price", m.ID)
		}
		if m.CatalogID() == "" || m.ModalitiesJSON() == "" {
			t.Errorf("%s missing catalog projections", m.ID)
		}
	}
}

// TestSniffMime covers the magic-byte detector for the formats clients send.
func TestSniffMime(t *testing.T) {
	cases := []struct{ data, want string }{
		{"\x89PNG\r\n\x1a\n", "image/png"},
		{"\xff\xd8\xff\xe0", "image/jpeg"},
		{"RIFF0000WEBP", "image/webp"},
		{"GIF89a", "image/gif"},
		{"GIF87a", "image/gif"},
		{"plain text", ""},
	}
	for _, c := range cases {
		if got := sniffMime([]byte(c.data)); got != c.want {
			t.Errorf("sniffMime(%q) = %q, want %q", c.data, got, c.want)
		}
	}
}

// TestParseRequestEmptyImagesArray document the accepted boundary: an explicitly
// empty `images` array is not an error (it means "no references"), and the
// multipart edits endpoint is the one that insists on a file.
func TestParseRequestEmptyImagesArray(t *testing.T) {
	n, err := ParseRequest([]byte(`{"model":"m","prompt":"x","images":[]}`))
	if err != nil {
		t.Fatalf("empty images array must parse: %v", err)
	}
	if len(n.Images) != 0 {
		t.Fatalf("images = %d, want 0", len(n.Images))
	}
}
