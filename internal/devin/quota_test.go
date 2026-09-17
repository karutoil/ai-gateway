package devin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchQuota(t *testing.T) {
	payload := map[string]any{
		"userStatus": map[string]any{
			"planStatus": map[string]any{
				"daily_quota_remaining_percent":  60.0,
				"weekly_quota_remaining_percent": 80.0,
				"daily_quota_reset_at_unix":      1893456000.0,
				"weekly_quota_reset_at_unix":     1894060800.0,
				"overage_balance_micros":         2500000.0,
			},
		},
		"planInfo": map[string]any{
			"planName":        "Devin Pro",
			"hideDailyQuota":  false,
			"hideWeeklyQuota": true,
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != quotaPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("connect-protocol-version") == "" {
			t.Error("missing connect-protocol-version")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		meta, _ := body["metadata"].(map[string]any)
		if meta["apiKey"] != "devin-session-token$abc" {
			t.Errorf("session token not normalized: %v", meta["apiKey"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	q, err := FetchQuota("abc", srv.URL, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if q.Plan != "Devin Pro" {
		t.Fatalf("plan: %q", q.Plan)
	}
	if q.DailyRemaining == nil || *q.DailyRemaining != 60 || q.DailyUsed == nil || *q.DailyUsed != 40 {
		t.Fatalf("daily: %+v", q)
	}
	if q.WeeklyRemaining == nil || *q.WeeklyRemaining != 80 || q.WeeklyUsed == nil || *q.WeeklyUsed != 20 {
		t.Fatalf("weekly: %+v", q)
	}
	if q.DailyResetAt == nil || *q.DailyResetAt != 1893456000 {
		t.Fatalf("daily reset: %+v", q)
	}
	if !q.HideWeekly || q.HideDaily {
		t.Fatalf("hide flags: %+v", q)
	}
	if q.ExtraBalanceUSD != 2.5 {
		t.Fatalf("balance: %v", q.ExtraBalanceUSD)
	}
}

func TestFetchQuotaInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"userStatus":{}}`))
	}))
	defer srv.Close()
	if _, err := FetchQuota("abc", srv.URL, nil); err == nil {
		t.Fatal("missing plan should fail")
	}
}

func TestFetchQuotaHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	if _, err := FetchQuota("abc", srv.URL, nil); err == nil {
		t.Fatal("5xx should fail")
	}
}
