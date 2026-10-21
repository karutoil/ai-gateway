package apikey

import (
	"testing"
	"time"

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

// TestMonthSpendAttribution covers the P1-2 fix.
//
// Dashboard-session traffic carries a VIRTUAL key ("sess"+subject) that has no
// gateway_keys row, so gatewayKeyID returns "" and request_logs.key_id stays
// NULL on those rows. Before the prefix term, MonthSpendUSD summed nothing for a
// logged-in user and the MonthlyBudgetUSD branch could never bind — while a
// single 12s Veo clip costs $4.80. These cases pin that spend is attributable.
func TestMonthSpendAttribution(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	s := NewStore(database)
	dbh := database

	key, err := s.Create("budgeted")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	// Stamp a request the way logRequestExtendedBodies does once key_id resolves.
	insertLog := func(id, keyID, prefix string, cost float64) {
		t.Helper()
		var kid any
		if keyID != "" {
			kid = keyID
		}
		if _, err := dbh.Exec(`INSERT INTO request_logs(id,key_prefix,key_id,provider_id,model,endpoint,status,latency_ms,created_at,cost_usd) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			id, prefix, kid, "p", "m", "chat.completions", 200, 1, time.Now().UTC(), cost); err != nil {
			t.Fatalf("insert log %s: %v", id, err)
		}
	}

	// A normal key: id and prefix agree.
	insertLog("l1", key.ID, key.Prefix, 1.50)
	if got := s.MonthSpendUSD(key.ID); got != 1.50 {
		t.Errorf("id-only spend = %v, want 1.50", got)
	}
	if got := s.MonthSpendUSDForPrefix(key.ID, key.Prefix); got != 1.50 {
		t.Errorf("id+prefix spend = %v, want 1.50 (must not double-count)", got)
	}

	// A session-style row: prefix only, key_id NULL.
	insertLog("l2", "", "sessalice", 4.80)
	// Id-only cannot see it (this is the old behaviour).
	if got := s.MonthSpendUSD("session:alice"); got != 0 {
		t.Errorf("session id-only spend = %v, want 0", got)
	}
	// Prefix-aware does see it — the fix.
	if got := s.MonthSpendUSDForPrefix("session:alice", "sessalice"); got != 4.80 {
		t.Errorf("session prefix spend = %v, want 4.80", got)
	}

	// A row written before the key_id backfill still counts under its prefix.
	insertLog("l3", "", key.Prefix, 0.25)
	if got := s.MonthSpendUSDForPrefix(key.ID, key.Prefix); got != 1.75 {
		t.Errorf("prefix includes unattributed rows: got %v, want 1.75", got)
	}
}
