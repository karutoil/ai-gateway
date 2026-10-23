package handler

// Tenancy regression tests for the key lifecycle endpoints.
//
// These exist because the mutation audit found that deleting the org predicate
// from RotateKey (or the tenant clamp from CreateKey) left the whole suite green
// while handing any dashboard user a working credential for another tenant's
// provider spend. Each test drives the real handler and asserts the DATABASE
// outcome, not just the status code.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
	"ai-gateway/internal/rbac"
	"ai-gateway/internal/user"

	"github.com/go-chi/chi/v5"
)

// seedOrg inserts an organizations row so org-scoped keys satisfy the FK.
func seedOrg(t *testing.T, h *AdminHandler, id string) {
	t.Helper()
	if _, err := h.DB.Exec(db.Q(`INSERT INTO organizations(id,name,created_at) VALUES(?,?,?)`), id, id, time.Now().UTC()); err != nil {
		t.Fatalf("seed org %s: %v", id, err)
	}
}

// keyReq builds a request with a verified org claim + role + perms, routed
// through chi so URL params resolve.
func keyReq(t *testing.T, method, path, urlParamID, orgID, role, subject string, perms map[string]bool, body string) *http.Request {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	ctx := auth.WithRole(req.Context(), role)
	ctx = auth.WithSubject(ctx, subject)
	if orgID != "" {
		ctx = auth.WithOrgID(ctx, orgID)
	}
	if perms != nil {
		ctx = auth.WithPerms(ctx, perms)
	}
	req = req.WithContext(ctx)
	rctx := chi.NewRouteContext()
	// The caller passes the id explicitly: it is not always the last segment
	// (/api/keys/{id}/limits).
	rctx.URLParams.Add("id", urlParamID)
	return req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
}

// TestRotateKeyRefusesAnotherOrg is the IDOR gate: `keys:rotate` is a DEFAULT
// permission for the `member` role, and rotation returns a live sk-gw-… secret.
// Without an org predicate any member could rotate another tenant's key and walk
// away with a working credential.
func TestRotateKeyRefusesAnotherOrg(t *testing.T) {
	h, cleanup := newTestAdminHandler(t)
	defer cleanup()
	h.APIKeyStore = apikey.NewStore(h.DB)
	h.UserStore = user.NewStore(h.DB)
	seedOrg(t, h, "org-victim")
	seedOrg(t, h, "org-attacker")

	victim, err := h.APIKeyStore.CreateWithOrg("victim-key", "org-victim", "")
	if err != nil {
		t.Fatal(err)
	}
	var before string
	if err := h.DB.QueryRow(db.Q(`SELECT hash FROM gateway_keys WHERE id=?`), victim.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}

	// A member of a DIFFERENT org, holding keys:rotate.
	req := keyReq(t, http.MethodPost, "/api/keys/"+victim.ID+"/rotate", victim.ID, "org-attacker", rbac.RoleMember, "mallory",
		map[string]bool{rbac.PermKeysRotate: true, rbac.PermKeysReadOwn: true}, "")
	w := httptest.NewRecorder()
	h.RotateKey(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("RotateKey returned 200 for another org's key — cross-tenant credential takeover: %s", w.Body.String())
	}
	var after string
	if err := h.DB.QueryRow(db.Q(`SELECT hash FROM gateway_keys WHERE id=?`), victim.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("RotateKey rewrote another org's key secret despite the refusal")
	}
	if strings.Contains(w.Body.String(), "sk-gw-") {
		t.Fatalf("refused rotation still leaked a plaintext secret: %s", w.Body.String())
	}
}

// TestRotateKeyAllowsOwnOrg is the positive control: the same permission set
// inside the key's own org must succeed, so the guard above cannot be satisfied
// by refusing everything.
func TestRotateKeyAllowsOwnOrg(t *testing.T) {
	h, cleanup := newTestAdminHandler(t)
	defer cleanup()
	h.APIKeyStore = apikey.NewStore(h.DB)
	h.UserStore = user.NewStore(h.DB)
	seedOrg(t, h, "org-mine")

	k, err := h.APIKeyStore.CreateWithOrg("own-key", "org-mine", "")
	if err != nil {
		t.Fatal(err)
	}
	req := keyReq(t, http.MethodPost, "/api/keys/"+k.ID+"/rotate", k.ID, "org-mine", rbac.RoleMember, "alice",
		map[string]bool{rbac.PermKeysRotate: true}, "")
	w := httptest.NewRecorder()
	h.RotateKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("RotateKey in own org = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "sk-gw-") {
		t.Fatalf("rotation returned no new secret: %s", w.Body.String())
	}
}

// TestCreateKeyCannotTargetAnotherOrg pins the tenant clamp on creation. A
// tenant admin (admin role WITH an org claim) that supplied another org's id used
// to have it honoured, planting a key it could then use against that org's
// providers.
func TestCreateKeyCannotTargetAnotherOrg(t *testing.T) {
	h, cleanup := newTestAdminHandler(t)
	defer cleanup()
	h.APIKeyStore = apikey.NewStore(h.DB)
	h.UserStore = user.NewStore(h.DB)
	seedOrg(t, h, "org-mine")
	seedOrg(t, h, "org-victim")

	for _, tc := range []struct {
		name  string
		role  string
		orgID string
	}{
		{"member", rbac.RoleMember, "org-mine"},
		{"tenant-admin", rbac.RoleAdmin, "org-mine"},
	} {
		body := `{"name":"planted","org_id":"org-victim"}`
		req := keyReq(t, http.MethodPost, "/api/keys", "", tc.orgID, tc.role, "eve",
			map[string]bool{rbac.PermKeysCreate: true}, body)
		w := httptest.NewRecorder()
		h.CreateKey(w, req)
		if w.Code != http.StatusOK && w.Code != http.StatusCreated {
			t.Fatalf("%s: CreateKey = %d: %s", tc.name, w.Code, w.Body.String())
		}
		var org string
		if err := h.DB.QueryRow(db.Q(`SELECT COALESCE(org_id,'') FROM gateway_keys WHERE name='planted'`)).Scan(&org); err != nil {
			t.Fatalf("%s: key not created: %v", tc.name, err)
		}
		if org != "org-mine" {
			t.Fatalf("%s: key created in org %q, want the caller's own org %q (cross-org escalation)", tc.name, org, "org-mine")
		}
		_, _ = h.DB.Exec(db.Q(`DELETE FROM gateway_keys WHERE name='planted'`))
	}
}

// TestUpdateKeyLimitsPersistsEveryField pins the operator's budget knob. Every
// field of PUT /api/keys/{id}/limits was at 0% coverage: silently ignoring
// monthly_budget_usd left the key uncapped while the dashboard said otherwise.
func TestUpdateKeyLimitsPersistsEveryField(t *testing.T) {
	h, cleanup := newTestAdminHandler(t)
	defer cleanup()
	h.APIKeyStore = apikey.NewStore(h.DB)

	k, err := h.APIKeyStore.CreateWithOrg("limited", "", "")
	if err != nil {
		t.Fatal(err)
	}
	body := `{
		"monthly_budget_usd": 42.5,
		"rate_limit_rpm": 11,
		"rate_limit_rph": 22,
		"rate_limit_rpd": 33,
		"rate_limit_tpm": 44,
		"ip_allowlist": "10.0.0.0/8, 192.168.1.1",
		"allowed_models": ["gpt-4o","claude-3-5-sonnet"]
	}`
	req := keyReq(t, http.MethodPut, "/api/keys/"+k.ID+"/limits", k.ID, "", rbac.RoleAdmin, "root", nil, body)
	w := httptest.NewRecorder()
	h.UpdateKeyLimits(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateKeyLimits = %d: %s", w.Code, w.Body.String())
	}

	var budget float64
	var rpm, rph, rpd, tpm int
	var allowlist, allowedModels string
	if err := h.DB.QueryRow(db.Q(`SELECT COALESCE(monthly_budget_usd,0), COALESCE(rate_limit_rpm,0), COALESCE(rate_limit_rph,0), COALESCE(rate_limit_rpd,0), COALESCE(rate_limit_tpm,0), COALESCE(ip_allowlist,''), COALESCE(allowed_models,'') FROM gateway_keys WHERE id=?`), k.ID).
		Scan(&budget, &rpm, &rph, &rpd, &tpm, &allowlist, &allowedModels); err != nil {
		t.Fatal(err)
	}
	if budget != 42.5 {
		t.Errorf("monthly_budget_usd = %v, want 42.5 — the operator's cost cap was silently dropped", budget)
	}
	if rpm != 11 || rph != 22 || rpd != 33 || tpm != 44 {
		t.Errorf("rate limits = (%d,%d,%d,%d), want (11,22,33,44)", rpm, rph, rpd, tpm)
	}
	if !strings.Contains(allowlist, "10.0.0.0/8") {
		t.Errorf("ip_allowlist = %q, want it persisted", allowlist)
	}
	var models []string
	if err := json.Unmarshal([]byte(allowedModels), &models); err != nil {
		t.Fatalf("allowed_models not JSON: %q", allowedModels)
	}
	if len(models) != 2 {
		t.Errorf("allowed_models = %v, want 2 entries", models)
	}
}
