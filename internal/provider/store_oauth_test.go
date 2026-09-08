package provider

import (
	"context"
	"testing"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/oauth"
	_ "ai-gateway/internal/oauth/providers"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return NewStore(database, bytes32())
}

func bytes32() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}

func TestAntigravityCreateWithoutKey(t *testing.T) {
	s := openTestStore(t)
	p, err := s.CreateWithOrg("antigravity", models.ProviderAntigravity, "", "", "")
	if err != nil {
		t.Fatalf("create antigravity without key: %v", err)
	}
	if p.BaseURL == "" {
		t.Fatal("default base_url missing")
	}
}

func TestOAuthTokenRoundTrip(t *testing.T) {
	s := openTestStore(t)
	p, err := s.CreateWithOrg("agy", models.ProviderAntigravity, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	full, err := s.GetByID(p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	tok := &oauth.Tokens{Access: "acc-123", Refresh: "ref-123", ExpiresAt: 9999999999999, Email: "u@example.com", ProjectID: "proj-1"}
	if err := s.SetOAuthTokens(full.ID, "antigravity", tok); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, defID, err := s.OAuthTokens(full)
	if err != nil {
		t.Fatalf("get tokens: %v", err)
	}
	if defID != "antigravity" || got.Refresh != "ref-123" || got.Access != "acc-123" {
		t.Fatalf("mismatch: %+v %q", got, defID)
	}
	enriched, _ := s.GetByID(full.ID)
	if !enriched.OAuthConnected || enriched.OAuthEmail != "u@example.com" {
		t.Fatalf("enrich failed: %+v", enriched)
	}
	if err := s.ClearOAuth(full.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, _, err := s.OAuthTokens(full); err == nil {
		t.Fatal("tokens should be gone after disconnect")
	}
	_ = context.Background
}
