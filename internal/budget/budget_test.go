package budget

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
)

func TestMemoryLimiterOverQuota(t *testing.T) {
	m := NewMemoryLimiter(100)
	if err := m.Check("abc", 50); err != nil {
		t.Fatal(err)
	}
	m.RecordUsage("abc", 50, 0, time.Now())
	if err := m.Check("abc", 51); err == nil || !IsOverQuota(err) {
		t.Fatalf("expected over_quota got %v", err)
	}
}

func TestWriteOverQuotaType(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteOverQuota(rec)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d", rec.Code)
	}
	var body map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"]["type"] != "over_quota_error" {
		t.Fatalf("%v", body)
	}
}

func TestDBLimiterDailyTokens(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ks := apikey.NewStore(database)
	k, err := ks.Create("q")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE gateway_keys SET daily_token_limit=100 WHERE prefix=?`, k.Prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO request_logs(id,key_prefix,provider_id,model,endpoint,status,latency_ms,created_at,prompt_tokens,completion_tokens,total_tokens,cost_usd,is_stream) VALUES('l1',?,?, 'm','chat',200,1,datetime('now'),50,51,101,0,0)`, k.Prefix, "p"); err != nil {
		t.Fatal(err)
	}
	lim := NewDBLimiter(database)
	if err := lim.Check(k.Prefix, 1); err == nil || !IsOverQuota(err) {
		t.Fatalf("expected over_quota got %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Gateway-Key-Prefix", k.Prefix)
	Middleware(lim)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})).ServeHTTP(rec, req)
	if rec.Code != 429 {
		t.Fatalf("middleware status %d %s", rec.Code, rec.Body.String())
	}
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatal("json")
	}
}
