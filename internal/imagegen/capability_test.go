package imagegen

import "testing"

// TestImageOnlyFromModalities is the regression guard for the capability bug:
// models.dev marks image generators with `modalities.output`, and a model that
// also emits text is a legitimate chat model. Treating every image model as
// image-ONLY made the gateway refuse valid chat traffic.
func TestImageOnlyFromModalities(t *testing.T) {
	cases := []struct {
		name      string
		mods      string
		wantOnly  bool
		wantImage bool
	}{
		{"pure generator (dall-e/imagen/gpt-image-1)", `{"input":["text"],"output":["image"]}`, true, true},
		{"generator with pdf side output (grok-imagine)", `{"input":["text","image"],"output":["image","pdf"]}`, true, true},
		{"image-capable chat model (gemini image, gpt-image-1-mini)", `{"input":["text","image"],"output":["text","image"]}`, false, true},
		{"plain chat model", `{"input":["text","image"],"output":["text"]}`, false, false},
		{"empty", ``, false, false},
		{"no output key", `{"input":["text"]}`, false, false},
		{"malformed json", `not json`, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ImageOnlyFromModalities(c.mods); got != c.wantOnly {
				t.Errorf("ImageOnlyFromModalities(%s) = %v, want %v", c.mods, got, c.wantOnly)
			}
			if got := ImageModality(c.mods); got != c.wantImage {
				t.Errorf("ImageModality(%s) = %v, want %v", c.mods, got, c.wantImage)
			}
		})
	}
}

// TestCuratedTextOutputMatchesModelsDev locks the curated registry to the
// models.dev reality: Gemini image models narrate text (output text+image),
// while Imagen / DALL·E / gpt-image-1 / Grok are pure generators. A drift here
// means the gateway would either wrongly refuse chat or wrongly allow a
// generator into a chat body.
func TestCuratedTextOutputMatchesModelsDev(t *testing.T) {
	textCapable := []string{
		"gemini-2.5-flash-image",
		"gemini-3-pro-image-preview",
		"nano-banana",
		"nano-banana-pro",
	}
	imageOnly := []string{
		"gpt-image-1", "gpt-image-2", "dall-e-3", "dall-e-2",
		"imagen-4.0-generate-001", "imagen-3.0-generate-002",
		"grok-2-image", "grok-imagine-image-2.0",
	}
	for _, id := range textCapable {
		m, ok := Lookup(id)
		if !ok {
			t.Errorf("%s: not in registry", id)
			continue
		}
		if !m.TextOutput {
			t.Errorf("%s: TextOutput=false, but models.dev reports text+image output", id)
		}
		if IsImageOnlyModel(id) {
			t.Errorf("%s: reported image-only, but it is chat-capable", id)
		}
		if !ImageModality(m.ModalitiesJSON()) {
			t.Errorf("%s: ModalitiesJSON lost the image output", id)
		}
	}
	for _, id := range imageOnly {
		m, ok := Lookup(id)
		if !ok {
			t.Errorf("%s: not in registry", id)
			continue
		}
		if m.TextOutput {
			t.Errorf("%s: TextOutput=true, but models.dev reports image-only output", id)
		}
		if !IsImageOnlyModel(id) {
			t.Errorf("%s: should be image-only", id)
		}
		if !ImageOnlyFromModalities(m.ModalitiesJSON()) {
			t.Errorf("%s: ModalitiesJSON not recognized as image-only: %s", id, m.ModalitiesJSON())
		}
	}
	// A chat model is never image-only.
	if IsImageOnlyModel("gpt-4o") {
		t.Fatal("gpt-4o must not be image-only")
	}
}

// TestGrokVideoModelsAreNotImageModels is the root-cause regression guard for a
// real capability leak: the "grok-imagine" family prefix also matched xAI's
// VIDEO models, so "grok-imagine-video" resolved as an image-only model. The
// proxy consequently accepted it on POST /v1/images/generations and pointed
// chat traffic at the images API for a video model.
func TestGrokVideoModelsAreNotImageModels(t *testing.T) {
	for _, id := range []string{
		"grok-imagine-video", "grok-imagine-video-1.5", "grok-imagine-video-1.5-lite",
		"xai/grok-imagine-video",
	} {
		if _, ok := Lookup(id); ok {
			t.Errorf("%s must not resolve in the image registry (it is a video model)", id)
		}
		if IsImageModel(id) {
			t.Errorf("%s reported as an image model", id)
		}
		if IsImageOnlyModel(id) {
			t.Errorf("%s reported as an image-only model", id)
		}
	}
	// The genuine Grok image models must keep resolving.
	for _, id := range []string{"grok-2-image", "grok-imagine-image", "grok-imagine-image-2.0"} {
		if !IsImageModel(id) {
			t.Errorf("%s must stay an image model", id)
		}
	}
}

// TestImageCapabilityFromModalities proves the catalog-aware image capability
// predicates agree with the curated registry on the distinctions that matter to
// the endpoint guards.
//
// P1-4: the images endpoint used to consult only the curated registry, so a
// catalog row synced with image=1 (an operator-added or models.dev-discovered
// generator the registry has never heard of) was treated as "not an image
// model", and the guards disagreed with what /v1/models advertised.
func TestImageCapabilityFromModalities(t *testing.T) {
	cases := []struct {
		name          string
		mods          string
		wantImage     bool
		wantImageOnly bool
	}{
		{"pure image output", `{"input":["text"],"output":["image"]}`, true, true},
		{"text plus image output", `{"input":["text"],"output":["text","image"]}`, true, false},
		{"text only", `{"input":["text"],"output":["text"]}`, false, false},
		{"video only", `{"input":["text"],"output":["video"]}`, false, false},
		{"decision only", `{"input":["text"],"output":["decision"]}`, false, false},
		{"image plus pdf side output", `{"input":["text"],"output":["image","pdf"]}`, true, true},
		{"empty", ``, false, false},
		{"malformed", `{`, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ImageModality(c.mods); got != c.wantImage {
				t.Errorf("ImageModality(%s) = %v, want %v", c.mods, got, c.wantImage)
			}
			if got := ImageOnlyFromModalities(c.mods); got != c.wantImageOnly {
				t.Errorf("ImageOnlyFromModalities(%s) = %v, want %v", c.mods, got, c.wantImageOnly)
			}
		})
	}
}
