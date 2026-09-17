package antigravity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchQuota(t *testing.T) {
	codeAssist := map[string]any{
		"planInfo":                map[string]any{"planType": "GOOGLE_AI_PRO", "monthlyPromptCredits": 1000.0},
		"availablePromptCredits":  750.0,
		"cloudaicompanionProject": "proj-1",
	}
	models := map[string]any{
		"models": map[string]any{
			"gemini-3.8-flash-low": map[string]any{
				"displayName": "Gemini 3.8 Flash",
				"quotaInfo":   map[string]any{"remainingFraction": 0.8, "resetTime": "2099-01-01T00:00:00Z"},
			},
			"claude-sonnet-4-6": map[string]any{
				"displayName": "Claude Sonnet",
				"quotaInfo":   map[string]any{"remainingFraction": 0.0, "isExhausted": true, "resetTime": "2099-01-01T00:00:00Z"},
			},
			"chat_autocomplete": map[string]any{
				"quotaInfo": map[string]any{"remainingFraction": 1.0},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			_ = json.NewEncoder(w).Encode(codeAssist)
			return
		}
		_ = json.NewEncoder(w).Encode(models)
	}))
	defer srv.Close()

	q, err := FetchQuota("tok", "proj-1", srv.URL, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if q.PlanType != "GOOGLE_AI_PRO" {
		t.Fatalf("plan: %q", q.PlanType)
	}
	if q.PromptCredits == nil || q.PromptCredits.Available != 750 || q.PromptCredits.Monthly != 1000 {
		t.Fatalf("credits: %+v", q.PromptCredits)
	}
	if q.PromptCredits.UsedPct != 25 || q.PromptCredits.RemainingPct != 75 {
		t.Fatalf("credit pcts: %+v", q.PromptCredits)
	}
	if len(q.Models) != 2 {
		t.Fatalf("models = %+v", q.Models)
	}
	// Most-used first: exhausted sonnet before 20%-used flash.
	if q.Models[0].ModelID != "claude-sonnet-4-6" || q.Models[1].ModelID != "gemini-3.8-flash-low" {
		t.Fatalf("most-used-first order: %+v", q.Models)
	}
	byID := map[string]ModelQuota{}
	for _, m := range q.Models {
		byID[m.ModelID] = m
	}
	flash := byID["gemini-3.8-flash-low"]
	if flash.RemainingPct == nil || *flash.RemainingPct != 80 || *flash.UsedPct != 20 {
		t.Fatalf("flash: %+v", flash)
	}
	if flash.ResetsInMs == nil || *flash.ResetsInMs <= 0 {
		t.Fatalf("flash reset missing: %+v", flash)
	}
	if sonnet := byID["claude-sonnet-4-6"]; !sonnet.Exhausted {
		t.Fatalf("sonnet should be exhausted: %+v", sonnet)
	}
}

func TestFetchQuotaUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	t.Setenv("ANTIGRAVITY_BASE_URL", srv.URL)
	if _, err := FetchQuota("tok", "proj", "", nil); err == nil {
		t.Fatal("expected error when all endpoints fail")
	}
}
