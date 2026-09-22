package pgsmoke

import (
	"os"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/catalog"
	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/pat"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/user"
)

// TestPostgresStores exercises the real store layer (placeholders, BYTEA,
// booleans, timestamps, LIKE search) against Postgres.
func TestPostgresStores(t *testing.T) {
	dsn := os.Getenv("GATEWAY_PG_DSN")
	if dsn == "" {
		t.Skip("GATEWAY_PG_DSN not set; skipping Postgres smoke test")
	}
	t.Setenv("DATABASE_URL", dsn)
	database, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("open/migrate: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i + 1)
	}

	// Providers: encrypted BYTEA key round-trip + reads.
	ps := provider.NewStore(database, mk)
	p, err := ps.CreateWithOrg("openai-stores", models.ProviderOpenAI, "https://api.openai.com/v1", "sk-test-123", "")
	if err != nil {
		t.Fatalf("provider create: %v", err)
	}
	got, err := ps.GetByID(p.ID)
	if err != nil {
		t.Fatalf("provider get: %v", err)
	}
	dec, err := provider.Decrypt(got.APIKeyEnc, mk)
	if err != nil || string(dec) != "sk-test-123" {
		t.Fatalf("provider key round-trip: %v %q", err, dec)
	}
	if _, err := ps.GetByName("openai-stores"); err != nil {
		t.Fatalf("provider get-by-name: %v", err)
	}
	if _, err := ps.List(); err != nil {
		t.Fatalf("provider list: %v", err)
	}
	_ = ps.ListForModel("gpt-4o")

	// API keys: hash insert + prefix verify + list.
	ks := apikey.NewStore(database)
	key, err := ks.CreateWithOrg("ci", "")
	if err != nil {
		t.Fatalf("key create: %v", err)
	}
	if _, ok := ks.Verify(key.Key); !ok {
		t.Fatal("key verify failed")
	}
	if _, err := ks.List(); err != nil {
		t.Fatalf("key list: %v", err)
	}

	// Catalog: bulk sync + filtered list (LIKE) + short-id lookup.
	cs := catalog.NewStore(database)
	body := []byte(`{"openai":{"id":"openai","name":"OpenAI","api":"openai","models":{"gpt-4o":{"id":"openai/gpt-4o","name":"GPT-4o","description":"d","family":"gpt","attachment":true,"reasoning":false,"tool_call":true,"structured_output":true,"temperature":true,"knowledge":"2024","modalities":{"input":["text"],"output":["text"]},"open_weights":false,"limit":{"context":128000,"output":16384},"cost":{"input":2.5,"output":10,"cache_read":1.25,"cache_write":2.5}}}}}`)
	if _, err := cs.SyncFromBytes(body); err != nil {
		t.Fatalf("catalog sync: %v", err)
	}
	list, err := cs.List("gpt", "", false, 10, 0)
	if err != nil || len(list) == 0 {
		t.Fatalf("catalog list: %v (n=%d)", err, len(list))
	}
	if _, _, err := cs.FindBestMatch("gpt-4o"); err != nil {
		t.Fatalf("catalog best-match lookup: %v", err)
	}

	// Users + PATs (INTEGER flags, timestamptz, FK).
	us := user.NewStore(database)
	u, err := us.Create("pgadmin", "long-enough-password", "admin", "PG Admin")
	if err != nil {
		t.Fatalf("user create: %v", err)
	}
	if cnt, err := us.Count(); err != nil || cnt == 0 {
		t.Fatalf("user count: %v (n=%d)", err, cnt)
	}
	if _, err := us.List(); err != nil {
		t.Fatalf("user list: %v", err)
	}
	pats := pat.NewStore(database)
	if _, _, err := pats.Create(u.ID, "ci-token", nil, "keys:read"); err != nil {
		t.Fatalf("pat create: %v", err)
	}
}
