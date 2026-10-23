package mcp

// Regression: "revoke all" must revoke, not grant everything.
//
// Clearing a key's grants deleted every mcp_key_permissions row, which is
// byte-identical to a key that was never configured — and an unconfigured key is
// allowed every enabled tool. The most common admin action therefore produced
// the widest possible access while the dashboard showed the grants as removed.

import (
	"testing"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
)

func TestRevokeAllGrantsDeniesEverything(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewStore(database, make([]byte, 32))

	enabled := true
	srv, err := store.Create(ServerInput{
		Name: "demo", Slug: "demo", Transport: "http", URL: "http://127.0.0.1:9/mcp", Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	key := &models.GatewayKey{ID: "key-1", Prefix: "sk-gw-revoke"}

	// Untouched key: historical permissive default (this is what a fresh install
	// relies on, so it must keep working).
	access, err := store.AccessForKey(key)
	if err != nil {
		t.Fatalf("access (unconfigured): %v", err)
	}
	if access.Explicit {
		t.Error("a never-configured key must not be treated as an explicit allowlist")
	}

	// Grant one server, then revoke everything.
	if err := store.SetKeyPermissions(key.ID, []KeyPermission{{ServerID: srv.ID, Enabled: true}}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := store.SetKeyPermissions(key.ID, nil); err != nil {
		t.Fatalf("revoke all: %v", err)
	}

	access, err = store.AccessForKey(key)
	if err != nil {
		t.Fatalf("access (revoked): %v", err)
	}
	if !access.Explicit {
		t.Fatal("after revoking all grants the key fell back to the allow-everything default")
	}
	if len(access.Tools) != 0 || len(access.Servers) != 0 {
		t.Fatalf("revoked key still has %d servers / %d tools", len(access.Servers), len(access.Tools))
	}
}
