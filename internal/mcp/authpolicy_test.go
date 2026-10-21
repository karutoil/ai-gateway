package mcp

// Regression tests for the per-key admission policy on the unified MCP
// surface. GatewayAuthWithJWTRevocation enforces the per-key IP allowlist
// and the calendar-month spend cap inline on /v1; the MCP endpoint verifies
// keys itself, so it must apply the SAME policy (middleware.AdmitKey) after
// verification — otherwise a restricted key sheds both controls by speaking
// MCP instead of plain HTTP.

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
)

// newAuthFixture builds a store/manager/endpoint on a dedicated SQLite
// database and returns the raw handle so spend rows can be seeded directly.
func newAuthFixture(t *testing.T) (*Store, *Manager, *apikey.Store, *Endpoint, *sql.DB) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 7)
	}
	store := NewStore(database, master)
	manager := NewManager(store, 20*time.Second)
	t.Cleanup(manager.Close)
	keys := apikey.NewStore(database)
	return store, manager, keys, NewEndpoint(store, manager, keys), database
}

func TestMCPAuthEnforcesIPAllowlistAndMonthlyBudget(t *testing.T) {
	_, _, keys, endpoint, database := newAuthFixture(t)

	// A key locked to the 10.0.0.0/8 range with a $1 monthly cap.
	locked, err := keys.Create("mcp-locked")
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.SetIPAllowlist(locked.ID, "10.0.0.0/8"); err != nil {
		t.Fatal(err)
	}
	if err := keys.SetMonthlyBudget(locked.ID, 1.00); err != nil {
		t.Fatal(err)
	}

	var called bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	mw := endpoint.Auth(next)

	do := func(key, ip string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("x-litellm-api-key", key)
		req.RemoteAddr = ip + ":5555"
		w := httptest.NewRecorder()
		called = false
		mw.ServeHTTP(w, req)
		return w
	}

	// Allowed IP, no spend -> through to the handler.
	if w := do(locked.Key, "10.1.2.3"); w.Code != http.StatusOK || !called {
		t.Fatalf("allowed IP: code=%d called=%v, want 200/true", w.Code, called)
	}

	// Disallowed IP -> 403, same envelope /v1 answers with.
	if w := do(locked.Key, "203.0.113.9"); w.Code != http.StatusForbidden || called {
		t.Fatalf("disallowed IP: code=%d called=%v, want 403/false", w.Code, called)
	}
	if w := do(locked.Key, "203.0.113.9"); !strings.Contains(w.Body.String(), "client IP not allowed") {
		t.Fatalf("403 body = %q, want the permission_error envelope", w.Body.String())
	}

	// Spend over the monthly budget -> 429 even from the allowed IP.
	// (MonthSpendUSD reads request_logs; seed directly, matching
	// TestGatewayAuthIPAllowlistAndBudget in the middleware package.)
	monthStart := time.Now().UTC().Format("2006-01") + "-01 00:00:00"
	if _, err := database.Exec(
		`INSERT INTO request_logs (id, key_id, created_at, cost_usd) VALUES ('mcp-spend-1', ?, ?, 5.0)`,
		locked.ID, monthStart); err != nil {
		t.Fatal(err)
	}
	if w := do(locked.Key, "10.1.2.3"); w.Code != http.StatusTooManyRequests || called {
		t.Fatalf("over budget: code=%d called=%v, want 429/false", w.Code, called)
	}

	// Raise the budget -> admitted again.
	if err := keys.SetMonthlyBudget(locked.ID, 100); err != nil {
		t.Fatal(err)
	}
	if w := do(locked.Key, "10.1.2.3"); w.Code != http.StatusOK || !called {
		t.Fatalf("raised budget: code=%d called=%v, want 200/true", w.Code, called)
	}

	// The unrestricted fixture key is unaffected from any IP.
	extra, err := keys.Create("mcp-extra")
	if err != nil {
		t.Fatal(err)
	}
	if w := do(extra.Key, "198.51.100.7"); w.Code != http.StatusOK || !called {
		t.Fatalf("unrestricted key: code=%d called=%v, want 200/true", w.Code, called)
	}
}

// TestMCPAuthAcceptsAlternateKeyHeaders proves every accepted credential
// header still authenticates (the x-litellm-api-key convention is what the
// policy test above rides on).
func TestMCPAuthAcceptsAlternateKeyHeaders(t *testing.T) {
	_, _, keys, endpoint, _ := newAuthFixture(t)
	rawKey, err := keys.Create("header-key")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, header string }{
		{"x-litellm-api-key", "x-litellm-api-key"},
		{"x-api-key", "x-api-key"},
		{"Authorization", "Authorization"},
	} {
		req := httptest.NewRequest("POST", "/mcp", nil)
		if tc.header == "Authorization" {
			req.Header.Set("Authorization", "Bearer "+rawKey.Key)
		} else {
			req.Header.Set(tc.header, rawKey.Key)
		}
		var called bool
		w := httptest.NewRecorder()
		endpoint.Auth(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { called = true })).ServeHTTP(w, req)
		if w.Code != http.StatusOK || !called {
			t.Fatalf("%s: code=%d called=%v, want 200/true", tc.name, w.Code, called)
		}
	}
}
