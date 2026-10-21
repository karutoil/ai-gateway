package videogen

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestValidateWrapsParseRequest proves the exported Validate entry is wired to
// ParseRequest (it was previously uncovered and is the entry the OpenAPI
// validator and the proxy's generated schema both reference).
func TestValidateWrapsParseRequest(t *testing.T) {
	if err := Validate([]byte(`{"model":"sora-2","prompt":"x"}`)); err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}
	if err := Validate([]byte(`{"model":"sora-2"}`)); err == nil {
		t.Fatal("invalid body accepted")
	}
	if _, err := ParseVideo([]byte(`{"id":"v1","status":"queued"}`)); err != nil {
		t.Fatalf("ParseVideo: %v", err)
	}
}

// TestXAICreateBodyShapes covers the reference-image branches of the xAI create
// body (inline data URL, remote URL string, and object form), plus the
// aspect-ratio/resolution derivation from `size`.
func TestXAICreateBodyShapes(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes())

	t.Run("no reference", func(t *testing.T) {
		b, err := XAICreateBody(&Normalized{Model: "grok-imagine-video", Prompt: "p", Seconds: "8", Size: "1792x1024"})
		if err != nil {
			t.Fatal(err)
		}
		var req map[string]any
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatal(err)
		}
		if req["aspect_ratio"] != "16:9" || req["resolution"] != "1080p" {
			t.Fatalf("size-derived params = %v", req)
		}
		if _, ok := req["image"]; ok {
			t.Fatalf("no image expected: %v", req["image"])
		}
	})

	t.Run("inline data url", func(t *testing.T) {
		ref, _ := json.Marshal("data:image/png;base64," + b64)
		b, err := XAICreateBody(&Normalized{Model: "m", Prompt: "p", Seconds: "4", InputReference: ref})
		if err != nil {
			t.Fatal(err)
		}
		var req struct {
			Image struct {
				URL string `json:"url"`
			} `json:"image"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(req.Image.URL, "data:image/png;base64,") {
			t.Fatalf("inline image url = %q", req.Image.URL)
		}
	})

	t.Run("remote url string", func(t *testing.T) {
		ref, _ := json.Marshal("https://cdn.example/a.png")
		b, err := XAICreateBody(&Normalized{Model: "m", Prompt: "p", Seconds: "4", InputReference: ref})
		if err != nil {
			t.Fatal(err)
		}
		var req struct {
			Image struct {
				URL string `json:"url"`
			} `json:"image"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatal(err)
		}
		if req.Image.URL != "https://cdn.example/a.png" {
			t.Fatalf("remote image must be forwarded verbatim, got %q", req.Image.URL)
		}
	})

	t.Run("remote url object passthrough", func(t *testing.T) {
		ref := json.RawMessage(`{"url":"https://cdn.example/b.png","detail":"high"}`)
		b, err := XAICreateBody(&Normalized{Model: "m", Prompt: "p", Seconds: "4", InputReference: ref})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "cdn.example/b.png") {
			t.Fatalf("object reference lost: %s", b)
		}
	})

	t.Run("malformed base64", func(t *testing.T) {
		ref, _ := json.Marshal("!!!not-base64!!!")
		if _, err := XAICreateBody(&Normalized{Model: "m", Prompt: "p", InputReference: ref}); err == nil {
			t.Fatal("malformed base64 must error")
		}
	})
}

// TestVeoCreateBodyShapes covers the Veo create branches that were unreachable
// in the existing tests: remote gcsUri references and the durationSeconds /
// aspectRatio parameters.
func TestVeoCreateBodyShapes(t *testing.T) {
	// Remote reference → gcsUri.
	ref, _ := json.Marshal("https://storage.googleapis.com/bucket/a.png")
	b, err := VeoCreateBody(&Normalized{Model: "veo", Prompt: "p", Seconds: "8", Size: "720x1280", InputReference: ref})
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
	img, ok := req.Instances[0]["image"].(map[string]any)
	if !ok || img["gcsUri"] != "https://storage.googleapis.com/bucket/a.png" {
		t.Fatalf("remote reference = %v", req.Instances[0]["image"])
	}
	if req.Parameters["durationSeconds"].(float64) != 8 {
		t.Fatalf("durationSeconds = %v", req.Parameters["durationSeconds"])
	}
	if req.Parameters["aspectRatio"] != "9:16" {
		t.Fatalf("aspectRatio = %v", req.Parameters["aspectRatio"])
	}

	// No seconds / unknown size ⇒ no parameters block at all.
	b, _ = VeoCreateBody(&Normalized{Model: "veo", Prompt: "p", Size: "nope"})
	if strings.Contains(string(b), `"parameters"`) {
		t.Fatalf("parameters must be omitted when empty: %s", b)
	}

	// A malformed inline reference is an error.
	bad, _ := json.Marshal("!!!not-base64!!!")
	if _, err := VeoCreateBody(&Normalized{Model: "veo", Prompt: "p", InputReference: bad}); err == nil {
		t.Fatal("malformed reference must error")
	}
}

// TestDecodeInputReferenceRemainingBranches covers the empty/null and
// object-without-bytes branches.
func TestDecodeInputReferenceRemainingBranches(t *testing.T) {
	if _, inline, err := DecodeInputReference(nil); err != nil || inline {
		t.Fatalf("nil reference: inline=%v err=%v", inline, err)
	}
	if _, inline, err := DecodeInputReference(json.RawMessage(`null`)); err != nil || inline {
		t.Fatalf("null reference: inline=%v err=%v", inline, err)
	}
	if _, inline, err := DecodeInputReference(json.RawMessage(`""`)); err != nil || inline {
		t.Fatalf("empty string reference: inline=%v err=%v", inline, err)
	}
	if _, _, err := DecodeInputReference(json.RawMessage(`{"mime_type":"image/png"}`)); err == nil {
		t.Fatal("object without bytes must error")
	}
	// Object with data + explicit mime.
	b64 := base64.StdEncoding.EncodeToString(pngBytes())
	img, inline, err := DecodeInputReference(json.RawMessage(`{"data":"` + b64 + `","media_type":"image/jpeg"}`))
	if err != nil || !inline || img.MimeType != "image/jpeg" {
		t.Fatalf("object data branch: img=%+v inline=%v err=%v", img, inline, err)
	}
}

// TestForNamespaceAndCatalogProjections covers the registry projections the
// catalog seeder and the /v1/models listing depend on.
func TestForNamespaceAndCatalogProjections(t *testing.T) {
	for _, ns := range []string{"openai", "google", "xai"} {
		got := ForNamespace(ns)
		if len(got) == 0 {
			t.Errorf("%s: no curated video models", ns)
		}
		for _, m := range got {
			if m.Namespace != ns {
				t.Errorf("ForNamespace(%q) returned %q", ns, m.Namespace)
			}
			if m.CatalogID() != ns+"/"+m.ID {
				t.Errorf("%s: catalog id = %q", m.ID, m.CatalogID())
			}
			if m.Description() == "" {
				t.Errorf("%s: empty description", m.ID)
			}
			if !VideoModality(m.ModalitiesJSON()) {
				t.Errorf("%s: modalities lack video output: %s", m.ID, m.ModalitiesJSON())
			}
		}
	}
	// Case-insensitive and trimmed, like every other namespace lookup.
	if len(ForNamespace("  GOOGLE ")) == 0 {
		t.Error("ForNamespace must normalize case/space")
	}
	if len(ForNamespace("nope")) != 0 {
		t.Error("unknown namespace should be empty")
	}
}

// TestSecondsToInt pins the helper the cost calculation uses.
func TestSecondsToInt(t *testing.T) {
	cases := map[string]int{"4": 4, "8": 8, "12": 12, "": 0, "abc": 0, "-4": 0}
	for in, want := range cases {
		if got := SecondsToInt(in); got != want {
			t.Errorf("SecondsToInt(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestValidateErrorFormatting pins the 400-message shape: the field name must
// prefix the message so the proxy can pass it straight to httperr.Invalid.
func TestValidateErrorFormatting(t *testing.T) {
	if got := (&ValidateError{Field: "seconds", Msg: "must be 4, 8, or 12"}).Error(); got != "seconds: must be 4, 8, or 12" {
		t.Fatalf("fielded error = %q", got)
	}
	if got := (&ValidateError{Msg: "body must be a valid JSON object"}).Error(); got != "body must be a valid JSON object" {
		t.Fatalf("fieldless error = %q", got)
	}
}
