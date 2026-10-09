package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
	"ai-gateway/internal/rbac"
	"ai-gateway/internal/user"
)

// The correlated error sample must use the same ownership scope as its count.
// In particular a more recent error for someone else's key cannot be exposed.
func TestStatsErrorsRespectOwnKeyScope(t *testing.T) {
	h, _ := newTestAdminHandler(t)
	h.UserStore = user.NewStore(h.DB)
	owner, err := h.UserStore.Create("report-owner", "not-a-real-password", "member", "")
	if err != nil {
		t.Fatal(err)
	}
	keys := apikey.NewStore(h.DB)
	owned, err := keys.CreateWithOrg("owned", "", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := keys.Create("other")
	if err != nil {
		t.Fatal(err)
	}
	var providerID string
	if err := h.DB.QueryRow(db.Q("SELECT id FROM providers LIMIT 1")).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedLogRows(t, h, []logSeed{
		{model: "owned-model", endpoint: "chat", status: 502, errMsg: "owner error", tokens: 10, keyID: owned.ID, keyPrefix: owned.Prefix, providerID: providerID, at: now.Add(-2 * time.Hour)},
		{model: "other-model", endpoint: "chat", status: 502, errMsg: "secret other error", tokens: 100, keyID: other.ID, keyPrefix: other.Prefix, providerID: providerID, at: now.Add(-time.Hour)},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/stats?range=7d", nil)
	req = req.WithContext(auth.WithRole(req.Context(), "member"))
	req = req.WithContext(auth.WithSubject(req.Context(), owner.Username))
	req = req.WithContext(auth.WithPerms(req.Context(), map[string]bool{rbac.PermKeysReadOwn: true}))
	w := httptest.NewRecorder()
	h.Stats(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var report map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report["range_requests"].(float64) != 1 {
		t.Fatalf("owned requests=%v", report["range_requests"])
	}
	errors := report["errors"].([]any)
	if len(errors) != 1 || errors[0].(map[string]any)["sample"] != "owner error" {
		t.Fatalf("owned error breakdown=%v", errors)
	}
}
