package discovery

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
)

// openAIService stands up a provider whose /models endpoint returns the given
// ids, so Discover exercises the real upsert path.
func openAIService(t *testing.T, modelIDs []string) (*Service, *models.Provider) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[`))
		for i, id := range modelIDs {
			if i > 0 {
				w.Write([]byte(","))
			}
			w.Write([]byte(`{"id":"` + id + `","object":"model","owned_by":"openai"}`))
		}
		w.Write([]byte(`]}`))
	}))
	t.Cleanup(srv.Close)

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i + 3)
	}
	ps := provider.NewStore(database, mk)
	p, err := ps.Create("openai", models.ProviderOpenAI, srv.URL+"/v1", "sk-test")
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return &Service{db: database, providerStore: ps, client: srv.Client()}, p
}

func modelRow(t *testing.T, database *sql.DB, providerID, modelID string) (id, source string, ok bool) {
	t.Helper()
	err := database.QueryRow(`SELECT id, source FROM provider_models WHERE provider_id=? AND model_id=?`, providerID, modelID).Scan(&id, &source)
	if err == sql.ErrNoRows {
		return "", "", false
	}
	if err != nil {
		t.Fatalf("select %s: %v", modelID, err)
	}
	return id, source, true
}

func exclusionCount(t *testing.T, database *sql.DB, providerID, modelID string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM provider_model_exclusions WHERE provider_id=? AND model_id=?`, providerID, modelID).Scan(&n); err != nil {
		t.Fatalf("count exclusions: %v", err)
	}
	return n
}

// Removing a discovered model must survive the next discovery run, and only a
// manual add may bring that exact model back.
func TestDeleteExcludesFromRediscovery(t *testing.T) {
	s, p := openAIService(t, []string{"gpt-keep", "gpt-drop"})
	n, err := s.Discover(p.ID)
	if err != nil || n != 2 {
		t.Fatalf("discover = %d, %v; want 2, nil", n, err)
	}
	dropID, _, ok := modelRow(t, s.db, p.ID, "gpt-drop")
	if !ok {
		t.Fatal("gpt-drop not discovered")
	}

	if err := s.Delete(dropID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, ok := modelRow(t, s.db, p.ID, "gpt-drop"); ok {
		t.Fatal("gpt-drop still present after delete")
	}
	if exclusionCount(t, s.db, p.ID, "gpt-drop") != 1 {
		t.Fatal("deletion did not record an exclusion")
	}

	n, err = s.Discover(p.ID)
	if err != nil {
		t.Fatalf("rediscover: %v", err)
	}
	if _, _, ok := modelRow(t, s.db, p.ID, "gpt-drop"); ok {
		t.Fatal("excluded model came back on rediscovery")
	}
	if _, _, ok := modelRow(t, s.db, p.ID, "gpt-keep"); !ok {
		t.Fatal("unrelated model was lost on rediscovery")
	}

	// Deleting a model that is already gone is a no-op, not an error, and
	// must not invent a second exclusion row.
	if err := s.Delete(dropID); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if exclusionCount(t, s.db, p.ID, "gpt-drop") != 1 {
		t.Fatal("repeated delete duplicated the exclusion")
	}

	// A manual add is the only way back, and it clears the exclusion so later
	// discovery treats the model as an operator-owned row.
	newID, err := s.AddManual(p.ID, "gpt-drop", models.ProviderModel{DisplayName: "My Drop"})
	if err != nil {
		t.Fatalf("add manual: %v", err)
	}
	if exclusionCount(t, s.db, p.ID, "gpt-drop") != 0 {
		t.Fatal("manual add did not clear the exclusion")
	}
	if _, source, ok := modelRow(t, s.db, p.ID, "gpt-drop"); !ok || source != "manual" {
		t.Fatalf("manual row = present:%v source:%q", ok, source)
	}

	if _, err := s.Discover(p.ID); err != nil {
		t.Fatalf("rediscover after manual add: %v", err)
	}
	id, source, ok := modelRow(t, s.db, p.ID, "gpt-drop")
	if !ok || id != newID || source != "manual" {
		t.Fatalf("manual row after rediscovery = %q/%q present:%v, want %q/manual", id, source, ok, newID)
	}
	var display string
	if err := s.db.QueryRow(`SELECT display_name FROM provider_models WHERE id=?`, newID).Scan(&display); err != nil {
		t.Fatalf("select display: %v", err)
	}
	if display != "My Drop" {
		t.Errorf("manual display clobbered: %q", display)
	}
}

// Removing a model parks it in the recycling bin with its row intact, and
// restoring puts that exact row back while letting discovery resume.
func TestRecyclingBinRestoresSnapshot(t *testing.T) {
	s, p := openAIService(t, []string{"gpt-bin"})
	if _, err := s.Discover(p.ID); err != nil {
		t.Fatalf("discover: %v", err)
	}
	rowID, _, ok := modelRow(t, s.db, p.ID, "gpt-bin")
	if !ok {
		t.Fatal("gpt-bin not discovered")
	}
	if _, err := s.db.Exec(`UPDATE provider_models SET display_name=?, input_cost=?, context_window=? WHERE id=?`, "Fancy Bin", 1.25, 128000, rowID); err != nil {
		t.Fatalf("customize row: %v", err)
	}
	if err := s.Delete(rowID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	bin, err := s.ListExcluded("", "")
	if err != nil {
		t.Fatalf("list excluded: %v", err)
	}
	if len(bin) != 1 || bin[0].ModelID != "gpt-bin" || bin[0].ProviderName == "" {
		t.Fatalf("bin = %+v, want the one removed model", bin)
	}
	if bin[0].Snapshot == nil || bin[0].Snapshot.DisplayName != "Fancy Bin" || bin[0].Snapshot.InputCost != 1.25 || bin[0].Snapshot.ContextWindow != 128000 {
		t.Fatalf("snapshot = %+v, want the removed row", bin[0].Snapshot)
	}
	// The provider filter and search both narrow the bin, including the
	// snapshotted display name.
	if narrowed, err := s.ListExcluded(p.ID, "fancy"); err != nil || len(narrowed) != 1 {
		t.Fatalf("search by display name = %+v %v, want the row", narrowed, err)
	}
	if narrowed, err := s.ListExcluded(p.ID, "gpt-bin"); err != nil || len(narrowed) != 1 {
		t.Fatalf("search by model id = %+v %v, want the row", narrowed, err)
	}
	if narrowed, err := s.ListExcluded(p.ID, "nope"); err != nil || len(narrowed) != 0 {
		t.Fatalf("search miss = %+v %v, want empty", narrowed, err)
	}
	if narrowed, err := s.ListExcluded("no-such-provider", ""); err != nil || len(narrowed) != 0 {
		t.Fatalf("other provider's bin = %+v %v, want empty", narrowed, err)
	}

	// Still excluded from discovery while it sits in the bin.
	if _, err := s.Discover(p.ID); err != nil {
		t.Fatalf("rediscover: %v", err)
	}
	if _, _, ok := modelRow(t, s.db, p.ID, "gpt-bin"); ok {
		t.Fatal("binned model came back on discovery")
	}

	restoredID, err := s.Restore(bin[0].ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	id, source, ok := modelRow(t, s.db, p.ID, "gpt-bin")
	if !ok || id != restoredID {
		t.Fatalf("restored row = %q present:%v, want %q", id, ok, restoredID)
	}
	var display string
	var cost float64
	var ctx int
	if err := s.db.QueryRow(`SELECT display_name, input_cost, context_window, source FROM provider_models WHERE id=?`, restoredID).Scan(&display, &cost, &ctx, &source); err != nil {
		t.Fatalf("select restored: %v", err)
	}
	if display != "Fancy Bin" || cost != 1.25 || ctx != 128000 || source == "" {
		t.Errorf("restored row = %q/%.2f/%d/%q, want Fancy Bin/1.25/128000/<source>", display, cost, ctx, source)
	}
	if exclusionCount(t, s.db, p.ID, "gpt-bin") != 0 {
		t.Fatal("restore left the exclusion in place")
	}
	if again, err := s.ListExcluded("", ""); err != nil || len(again) != 0 {
		t.Fatalf("bin after restore = %+v %v, want empty", again, err)
	}

	// Back in circulation: discovery may refresh it, and it is no longer skipped.
	if _, err := s.Discover(p.ID); err != nil {
		t.Fatalf("rediscover after restore: %v", err)
	}
	if _, _, ok := modelRow(t, s.db, p.ID, "gpt-bin"); !ok {
		t.Fatal("restored model disappeared on the next discovery")
	}
}

// An exclusion is per provider: the same model id on another provider is
// unaffected, and removing one model never blocks its siblings.
func TestExclusionIsScopedToProviderAndModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"shared","object":"model","owned_by":"openai"},{"id":"other","object":"model","owned_by":"openai"}]}`))
	}))
	t.Cleanup(srv.Close)

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i + 5)
	}
	ps := provider.NewStore(database, mk)
	p, err := ps.Create("openai", models.ProviderOpenAI, srv.URL+"/v1", "sk-test")
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	other, err := ps.Create("openai-2", models.ProviderOpenAI, srv.URL+"/v1", "sk-test")
	if err != nil {
		t.Fatalf("create second provider: %v", err)
	}
	s := &Service{db: database, providerStore: ps, client: srv.Client()}
	if _, err := s.Discover(p.ID); err != nil {
		t.Fatalf("discover: %v", err)
	}
	sharedID, _, ok := modelRow(t, database, p.ID, "shared")
	if !ok {
		t.Fatal("shared model not discovered")
	}
	if err := s.Delete(sharedID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if s.isExcluded(other.ID, "shared") {
		t.Fatal("exclusion leaked to another provider")
	}
	if s.isExcluded(p.ID, "other") {
		t.Fatal("exclusion leaked to a sibling model")
	}
	if !s.isExcluded(p.ID, "shared") {
		t.Fatal("exclusion missing for the removed model")
	}
	// The other provider still discovers the same model id normally.
	if _, err := s.Discover(other.ID); err != nil {
		t.Fatalf("discover other: %v", err)
	}
	if _, _, ok := modelRow(t, database, other.ID, "shared"); !ok {
		t.Fatal("same model id was excluded on a different provider")
	}
}
