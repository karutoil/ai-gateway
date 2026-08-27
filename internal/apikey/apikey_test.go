package apikey

import (
	"testing"

	"ai-gateway/internal/db"
)

func TestGenerateHashPrefixVerify(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(database)
	k, err := s.Create("n")
	if err != nil {
		t.Fatal(err)
	}
	if k.Key == "" || Prefix(k.Key) != k.Prefix {
		t.Fatalf("key %+v", k)
	}
	got, ok := s.Verify(k.Key)
	if !ok || got.ID != k.ID {
		t.Fatal("verify")
	}
	if _, ok := s.Verify("sk-gw-" + "00"); ok {
		t.Fatal("bad key")
	}
	if err := s.Revoke(k.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(k.Key); ok {
		t.Fatal("revoked still valid")
	}
}

func TestIsModelAllowed(t *testing.T) {
	if !IsModelAllowed(nil, "gpt-4o") {
		t.Fatal("empty allowlist")
	}
	if !IsModelAllowed([]string{"openai/gpt-4o-mini"}, "gpt-4o-mini") {
		t.Fatal("suffix")
	}
	if !IsModelAllowed([]string{"gpt-4o-mini"}, "openai/gpt-4o-mini") {
		t.Fatal("qualified vs short")
	}
	if IsModelAllowed([]string{"gpt-4o"}, "claude-3") {
		t.Fatal("denied")
	}
	if !IsModelAllowed([]string{"gpt-4*"}, "gpt-4o-mini") {
		t.Fatal("wildcard")
	}
}
