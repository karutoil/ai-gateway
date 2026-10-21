package catalog

import (
	"testing"

	"ai-gateway/internal/db"
	"ai-gateway/internal/imagegen"
)

// TestSeedImageModelsAndLookup locks in image-model catalog seeding: the
// curated registry must be listable, flagged as image models, and resolve from
// every spelling clients use (bare, namespaced, vendor-qualified).
func TestSeedImageModelsAndLookup(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)

	n, err := s.SeedImageModels()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n < 10 {
		t.Fatalf("expected the curated registry to seed many models, got %d", n)
	}

	for _, id := range []string{
		"openai/gpt-image-1", "dall-e-3", "google/gemini-2.5-flash-image",
		"google/gemini-3-pro-image-preview", "google/imagen-4.0-generate-001",
		"xai/grok-2-image", "grok-imagine-image-2.0",
	} {
		m, _, err := s.FindBestMatch(id)
		if err != nil {
			t.Errorf("%s lookup: %v", id, err)
			continue
		}
		if !m.Image {
			t.Errorf("%s not marked image", id)
		}
		if m.Decision {
			t.Errorf("%s wrongly marked decision", id)
		}
	}

	// A chat model must not be flagged as an image model.
	if _, err := s.SyncFromBytes([]byte(`{"openai":{"id":"openai","name":"[OI]","api":"openai","models":{"gpt-4o":{"id":"openai/gpt-4o","name":"GPT-4o","modalities":{"input":["text"],"output":["text"]}}}}}`)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	chat, _, err := s.FindBestMatch("openai/gpt-4o")
	if err != nil {
		t.Fatalf("gpt-4o lookup: %v", err)
	}
	if chat.Image {
		t.Fatal("gpt-4o must not be an image model")
	}

	// Idempotent: re-seeding must not duplicate rows.
	before, _ := s.Count()
	if _, err := s.SeedImageModels(); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	after, _ := s.Count()
	if after != before {
		t.Fatalf("re-seed changed row count: %d -> %d", before, after)
	}
}

// TestImageModalityFromUpstream proves a models.dev row that declares the
// image output modality is flagged even without a curated registry entry.
func TestImageModalityFromUpstream(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)
	body := []byte(`{"acme":{"id":"acme","name":"Acme","api":"openai","models":{"flux-9":{"id":"acme/flux-9","name":"Flux 9","modalities":{"input":["text","image"],"output":["image"]}}}}}`)
	if _, err := s.SyncFromBytes(body); err != nil {
		t.Fatalf("sync: %v", err)
	}
	m, _, err := s.FindBestMatch("acme/flux-9")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !m.Image {
		t.Fatal("flux-9 should be flagged as an image model from its modalities")
	}
	if !imagegen.ImageModality(`{"input":["text"],"output":["image"]}`) {
		t.Fatal("ImageModality should detect the image output modality")
	}
}
