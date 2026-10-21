package pgsmoke

import (
	"testing"

	"ai-gateway/internal/catalog"
	"ai-gateway/internal/db"
	"ai-gateway/internal/discovery"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
)

// TestCapabilityFlagWritesAgainstPostgres is the regression guard for
//
//	pq: invalid input syntax for type integer: "false"
//
// The decision/image/video capability columns are INTEGER (not BOOLEAN), while
// the Go layer carries those flags as bool. lib/pq refuses to coerce a bool
// into an integer column whereas SQLite accepts either, so every write path
// must normalize through db.BoolInt. This test exercises each path against a
// real Postgres — SQLite cannot catch the bug, which is exactly why it shipped.
func TestCapabilityFlagWritesAgainstPostgres(t *testing.T) {
	database := openPG(t)

	// --- catalog seeding (one path per flag) ---
	cs := catalog.NewStore(database)
	if _, err := cs.SeedDecisionModels(); err != nil {
		t.Errorf("SeedDecisionModels: %v", err)
	}
	if _, err := cs.SeedImageModels(); err != nil {
		t.Errorf("SeedImageModels: %v", err)
	}
	if _, err := cs.SeedVideoModels(); err != nil {
		t.Errorf("SeedVideoModels: %v", err)
	}

	// --- catalog sync (derives all three flags from models.dev modalities) ---
	sync := []byte(`{"captest":{"id":"captest","name":"OpenAI","api":"openai","models":{
		"gpt-image-1":{"id":"gpt-image-1","name":"gpt-image-1","modalities":{"input":["text"],"output":["image"]}},
		"sora-2":{"id":"sora-2","name":"Sora 2","modalities":{"input":["text"],"output":["video"]}},
		"gpt-4o":{"id":"gpt-4o","name":"GPT-4o","modalities":{"input":["text"],"output":["text"]}}}}}`)
	if _, err := cs.SyncFromBytes(sync); err != nil {
		t.Fatalf("SyncFromBytes: %v", err)
	}

	// --- manual add (INSERT with all three flags) ---
	mk := make([]byte, 32)
	ps := provider.NewStore(database, mk)
	p, err := ps.Create("capflags", models.ProviderOpenAI, "https://api.openai.com/v1", "k")
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	svc := discovery.New(database, ps, cs)
	id, err := svc.AddManual(p.ID, "manual-image-model", models.ProviderModel{
		DisplayName: "Manual", Image: true,
	})
	if err != nil {
		t.Fatalf("AddManual: %v", err)
	}
	// The INSERT must have stored image=1 (this is the assertion that fails
	// with the pq error when the flag is written as a Go bool).
	var image, video, decision int
	if err := database.QueryRow(db.Q(`SELECT image, video, decision FROM provider_models WHERE id=?`), id).
		Scan(&image, &video, &decision); err != nil {
		t.Fatalf("read back flags: %v", err)
	}
	if image != 1 || video != 0 || decision != 0 {
		t.Errorf("AddManual flags image=%d video=%d decision=%d, want 1/0/0", image, video, decision)
	}

	// --- update (UPDATE with all three flags) ---
	if err := svc.UpdateManual(id, models.ProviderModel{
		DisplayName: "Manual 2", Image: true, Video: true, Decision: true,
	}); err != nil {
		t.Errorf("UpdateManual: %v", err)
	}
	if err := database.QueryRow(db.Q(`SELECT image, video, decision FROM provider_models WHERE id=?`), id).
		Scan(&image, &video, &decision); err != nil {
		t.Fatal(err)
	}
	if image != 1 || video != 1 || decision != 1 {
		t.Errorf("UpdateManual flags image=%d video=%d decision=%d, want 1/1/1", image, video, decision)
	}

	// --- enrich (UPDATE re-deriving the flags from the catalog) ---
	if err := svc.Enrich(id); err != nil {
		t.Errorf("Enrich: %v", err)
	}

	// --- delete + restore through the recycling bin (INSERT with snap.*) ---
	if err := svc.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	excluded, err := svc.ListExcluded(p.ID, "")
	if err != nil {
		t.Fatalf("ListExcluded: %v", err)
	}
	if len(excluded) == 0 {
		t.Fatal("expected a recycling-bin entry")
	}
	restoredID, err := svc.Restore(excluded[0].ID)
	if err != nil {
		t.Errorf("Restore: %v", err)
	}
	if restoredID == "" {
		t.Error("Restore returned no id")
	}

}

// TestCapabilityFlagsRoundTripFromCatalog proves the flags written by the sync
// path are readable as integers (the read paths scan sql.NullInt64).
func TestCapabilityFlagsRoundTripFromCatalog(t *testing.T) {
	database := openPG(t)
	cs := catalog.NewStore(database)
	sync := []byte(`{"captest":{"id":"captest","name":"OpenAI","api":"openai","models":{
		"gpt-image-1":{"id":"gpt-image-1","name":"gpt-image-1","modalities":{"input":["text"],"output":["image"]}},
		"sora-2":{"id":"sora-2","name":"Sora 2","modalities":{"input":["text"],"output":["video"]}},
		"gpt-4o":{"id":"gpt-4o","name":"GPT-4o","modalities":{"input":["text"],"output":["text"]}}}}}`)
	if _, err := cs.SyncFromBytes(sync); err != nil {
		t.Fatalf("sync: %v", err)
	}
	cases := []struct {
		id                 string
		image, video, text int
	}{
		{"captest/gpt-image-1", 1, 0, 0},
		{"captest/sora-2", 0, 1, 0},
		{"captest/gpt-4o", 0, 0, 0},
	}
	for _, c := range cases {
		var image, video int
		if err := database.QueryRow(db.Q(`SELECT image, video FROM models_catalog WHERE id=?`), c.id).
			Scan(&image, &video); err != nil {
			t.Errorf("%s: %v", c.id, err)
			continue
		}
		if image != c.image || video != c.video {
			t.Errorf("%s: image=%d video=%d, want %d/%d", c.id, image, video, c.image, c.video)
		}
	}
}
