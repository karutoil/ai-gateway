package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
)

func TestClientIPAllowed(t *testing.T) {
	cases := []struct {
		ip, allowlist string
		want          bool
	}{
		{"1.2.3.4", "", true},
		{"10.0.0.5", "10.0.0.5", true},
		{"10.0.0.6", "10.0.0.5", false},
		{"10.42.0.7", "10.0.0.0/8", true},
		{"11.0.0.1", "10.0.0.0/8", false},
		{"10.1.2.3", "192.168.1.1, 10.0.0.0/8", true},
		{"10.1.2.3", "192.168.1.1, 192.168.2.1", false},
		{"::1", "::1", true},
		{"fe80::1", "fe80::/10", true},
		{"2001:db8::1", "fe80::/10", false},
		{"not-an-ip", "10.0.0.0/8", false},
	}
	for _, c := range cases {
		if got := clientIPAllowed(c.ip, c.allowlist); got != c.want {
			t.Errorf("clientIPAllowed(%q, %q) = %v, want %v", c.ip, c.allowlist, got, c.want)
		}
	}
}

func TestGatewayAuthIPAllowlistAndBudget(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ks := apikey.NewStore(database)

	k, _ := ks.Create("locked")
	if err := ks.SetIPAllowlist(k.ID, "10.0.0.0/8"); err != nil {
		t.Fatal(err)
	}
	if err := ks.SetMonthlyBudget(k.ID, 1.00); err != nil {
		t.Fatal(err)
	}

	var called bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	mw := GatewayAuthWithJWT(ks, nil)

	do := func(ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer "+k.Key)
		req.RemoteAddr = ip + ":5555"
		w := httptest.NewRecorder()
		called = false
		mw(next).ServeHTTP(w, req)
		return w
	}

	// Allowed IP, no spend -> through
	if w := do("10.1.2.3"); w.Code != 200 || !called {
		t.Fatalf("allowed IP: code=%d called=%v", w.Code, called)
	}

	// Disallowed IP -> 403
	if w := do("203.0.113.9"); w.Code != http.StatusForbidden || called {
		t.Fatalf("disallowed IP: code=%d called=%v, want 403", w.Code, called)
	}

	// Spend over budget -> 429 even from allowed IP.
	// (MonthSpendUSD reads request_logs; seed directly.)
	monthStart := time.Now().UTC().Format("2006-01") + "-01 00:00:00"
	if _, err := database.Exec(
		`INSERT INTO request_logs (id, key_id, created_at, cost_usd) VALUES ('spend-1', ?, ?, 5.0)`,
		k.ID, monthStart); err != nil {
		t.Fatal(err)
	}
	w := do("10.1.2.3")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over budget: code=%d, want 429", w.Code)
	}

	// Raise budget -> allowed again
	if err := ks.SetMonthlyBudget(k.ID, 100); err != nil {
		t.Fatal(err)
	}
	if w := do("10.1.2.3"); w.Code != 200 || !called {
		t.Fatalf("raised budget should admit: code=%d called=%v", w.Code, called)
	}
}
