package provider

import (
	"testing"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
)

// Mixed-case display names ("AIHubMix") must pin from qualified model IDs
// and hints in any casing — callers send "aihubmix/…" after lowercasing.
func TestGetByNameCIAndResolveMixedCase(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database, make([]byte, 32))

	prov, err := s.Create("AIHubMix", models.ProviderOpenAI, "https://api.example.com/v1", "sk-test")
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"AIHubMix", "aihubmix", "AIHUBMIX", "AiHuBmIx"} {
		got, err := s.GetByNameCI(name)
		if err != nil {
			t.Fatalf("GetByNameCI(%q) failed: %v", name, err)
		}
		if got.ID != prov.ID {
			t.Fatalf("GetByNameCI(%q) = %q, want %q", name, got.Name, prov.Name)
		}
	}

	for _, model := range []string{"AIHubMix/coding-glm-5.3", "aihubmix/coding-glm-5.3", "AIHUBMIX/coding-glm-5.3"} {
		got, err := s.Resolve(model, "")
		if err != nil {
			t.Fatalf("Resolve(%q) failed: %v", model, err)
		}
		if got.ID != prov.ID {
			t.Fatalf("Resolve(%q) = %q, want %q", model, got.Name, prov.Name)
		}
	}

	if got, err := s.Resolve("coding-glm-5.3", "aihubmix"); err != nil || got.ID != prov.ID {
		t.Fatalf("Resolve with hint aihubmix = %v, %v", got, err)
	}
}
