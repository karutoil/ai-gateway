package handler

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/audit"
	"ai-gateway/internal/auth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/db"
	"ai-gateway/internal/discovery"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/resilience"
	"ai-gateway/internal/user"
	"ai-gateway/internal/webhook"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type AdminHandler struct {
	ProviderStore *provider.Store
	APIKeyStore   *apikey.Store
	Config        *config.Config
	DB            *sql.DB
	Discovery     *discovery.Service
	UserStore     *user.Store
	Breaker       resilience.CircuitBreaker
	// Recorder, when set, writes audit rows (logins, credential changes).
	Recorder audit.Recorder
	// AuthLimiter, when set, rate-limits the public credential endpoints.
	AuthLimiter *middleware.AuthRateLimiter
}

// auditLogin writes an action=login audit row for the given username. The
// Profile "Logins" feed queries exactly these rows; without them it silently
// fell back to a single last_login timestamp.
func (h *AdminHandler) auditLogin(username string) {
	if h.Recorder == nil || username == "" {
		return
	}
	_ = h.Recorder.Log(username, "login", "session", username, "dashboard login")
}

// GatewayVersion is the single source of truth for the reported app version
// (/health, /api/health, startup banner). It previously drifted: code said
// 1.6.0 while openapi.yaml said 1.7.0.
//
// Release builds inject the git tag via:
//
//	-ldflags "-X ai-gateway/internal/handler.GatewayVersion=v1.8.0"
//
// Source builds fall back to "dev" (+ short commit when available via the
// make target). The install.sh updater also reads this through `gateway version`.
var GatewayVersion = "1.7.0"

// GatewayCommit is injected at build time with the release's git SHA
// (-ldflags "-X ai-gateway/internal/handler.GatewayCommit=<sha>").
var GatewayCommit = "unknown"

func (h *AdminHandler) Routes(r chi.Router) {
	if h.AuthLimiter != nil {
		r.With(h.AuthLimiter.Middleware(middleware.AccountFromLoginBody)).Post("/auth/login", h.Login)
		r.With(h.AuthLimiter.Middleware(func(req *http.Request) string { return "" })).Post("/auth/oidc", h.OIDCLogin)
	} else {
		r.Post("/auth/login", h.Login)
		r.Post("/auth/oidc", h.OIDCLogin)
	}
	r.Post("/auth/logout", h.Logout)
	r.Get("/health", h.Health)
	// protected — WithRevocation so password/role/disable changes invalidate
	// outstanding sessions on THIS route group too (previously only group B
	// of the dashboard API checked revocation).
	r.Group(func(r chi.Router) {
		r.Use(auth.AdminMiddlewareWithRevocation(h.Config.JWTSecret, h.UserStore))
		r.Get("/providers", h.ListProviders)
		r.With(middleware.RequireRole("admin", "member", "support")).Post("/providers", h.CreateProvider)
		r.With(middleware.RequireRole("admin", "member", "support")).Put("/providers/{id}", h.UpdateProvider)
		r.With(middleware.RequireRole("admin", "member", "support")).Delete("/providers/{id}", h.DeleteProvider)
		r.With(middleware.RequireRole("admin", "member", "support", "readonly")).Post("/providers/test", h.TestProvider)

		r.Get("/keys", h.ListKeys)
		r.With(middleware.RequireRole("admin", "member", "support")).Post("/keys", h.CreateKey)
		r.With(middleware.RequireRole("admin", "member", "support")).Put("/keys/{id}", h.UpdateKey)
		r.With(middleware.RequireRole("admin", "member", "support")).Delete("/keys/{id}", h.DeleteKey)
		r.With(middleware.RequireRole("admin", "member", "support")).Put("/keys/{id}/rate-limit", h.UpdateKeyRateLimit)
		r.With(middleware.RequireRole("admin", "member", "support")).Put("/keys/{id}/limits", h.UpdateKeyLimits)
		r.Get("/stats", h.Stats)
		r.Get("/logs", h.Logs)
		r.Get("/logs/{id}", h.GetLog)
		r.Get("/billing/export", h.BillingExport)
	})
}

func (h *AdminHandler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": GatewayVersion})
}

// Logout clears the HttpOnly session cookie client-side control cannot
// remove. Token remains valid until expiry (stateless JWT) but revocation on
// credential changes covers the abuse window.
func (h *AdminHandler) Logout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w, r)
	// Best-effort session revocation: bump the user's token_version so the
	// just-logged-out JWT (and any copies) stop working on both the dashboard
	// and the proxy plane, instead of living until expiry. Stateless-JWT
	// logouts previously only cleared the cookie.
	if h.UserStore != nil {
		if subject := auth.GetSubject(r); subject != "" {
			if u, _, err := h.UserStore.GetByUsername(subject); err == nil {
				_ = h.UserStore.BumpTokenVersion(u.ID)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *AdminHandler) Login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Password string `json:"password"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httperr.Invalid(w, "invalid json")
		return
	}

	verifyAndMint := func(u *user.DashboardUser, hash string) bool {
		if !auth.VerifyPasswordHash(hash, body.Password) {
			return false
		}
		// Uniform error for disabled accounts AFTER password verification so
		// attackers cannot enumerate disabled usernames.
		if u.Disabled {
			return false
		}
		tv, _ := h.UserStore.TokenVersionFor(u.Username)
		token, err := auth.MakeTokenFull(h.Config.JWTSecret, u.Username, h.orgClaimFor(u), string(u.Role), tv)
		if err != nil {
			return false
		}
		// Transparent bcrypt upgrade for legacy SHA-256 rows.
		if auth.NeedsRehash(hash) {
			_ = h.UserStore.UpgradePasswordHash(u.ID, body.Password)
		}
		_ = h.UserStore.UpdateLastLogin(u.ID)
		h.auditLogin(u.Username)
		w.Header().Set("Content-Type", "application/json")
		setSessionCookie(w, r, token)
		json.NewEncoder(w).Encode(map[string]string{"token": token, "username": u.Username, "role": string(u.Role)})
		return true
	}

	if h.UserStore != nil && strings.TrimSpace(body.Username) != "" {
		username := strings.ToLower(strings.TrimSpace(body.Username))
		if u, hash, err := h.UserStore.GetByUsername(username); err == nil {
			if verifyAndMint(u, hash) {
				return
			}
		}
		httperr.Auth(w, "invalid credentials")
		return
	}

	// Legacy single-password mode (bootstrap): only valid while no dashboard
	// users exist; config.Load refuses this in production.
	cnt := 0
	if h.UserStore != nil {
		cnt, _ = h.UserStore.Count()
	}
	if cnt > 0 || h.Config.AdminPassword == "" || !auth.PasswordEqual(body.Password, h.Config.AdminPassword) {
		httperr.Auth(w, "invalid credentials")
		return
	}
	tv := int64(0)
	subject := "admin"
	orgID := ""
	role := "admin"
	if h.UserStore != nil {
		if u, _, err := h.UserStore.GetByUsername(subject); err == nil {
			tv, _ = h.UserStore.TokenVersionFor(u.Username)
			subject = u.Username
			role = string(u.Role)
			orgID = h.orgClaimFor(u)
		}
	}
	token, err := auth.MakeTokenFull(h.Config.JWTSecret, subject, orgID, role, tv)
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "failed to create token", httperr.TypeProxy)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	setSessionCookie(w, r, token)
	h.auditLogin(subject)
	json.NewEncoder(w).Encode(map[string]string{"token": token, "username": subject, "role": role})
}

// orgClaimFor resolves the org scope minted into a dashboard JWT for a user:
// the user's first org membership ("" = global). Login, OIDC and passkey
// flows all share this path so tokens can never carry an org the user does
// not belong to.
func (h *AdminHandler) orgClaimFor(u *user.DashboardUser) string {
	if h.UserStore == nil || u == nil {
		return ""
	}
	return h.UserStore.PrimaryOrgID(u.ID)
}

func (h *AdminHandler) OIDCLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httperr.Invalid(w, "invalid json")
		return
	}
	oidcIssuer := os.Getenv("OIDC_ISSUER")
	if oidcIssuer == "" {
		// SECURITY: without a configured issuer there is nothing to verify.
		// Issuing tokens here would let anyone mint admin sessions — the
		// handler therefore refuses instead of trusting client-supplied claims.
		httperr.Write(w, http.StatusServiceUnavailable, "OIDC login is not configured (set OIDC_ISSUER)", httperr.TypeInvalid)
		return
	}
	if body.IDToken == "" {
		httperr.Invalid(w, "id_token required")
		return
	}
	claims, err := auth.VerifyOIDCToken(body.IDToken, oidcIssuer)
	if err != nil {
		// Never echo provider error details to clients.
		httperr.Auth(w, "invalid oidc token")
		return
	}
	subject, _ := claims["sub"].(string)
	if subject == "" {
		httperr.Auth(w, "oidc token missing subject claim")
		return
	}
	if subject != strings.ToLower(subject) {
		subject = strings.ToLower(subject)
	}

	// Role resolution is SERVER-SIDE ONLY:
	//  - existing dashboard user → stored role wins (admins manage roles in UI)
	//  - new subject             → member unless listed in OIDC_ADMIN_SUBJECTS
	adminSubjects := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("OIDC_ADMIN_SUBJECTS"), ",") {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			adminSubjects[s] = true
		}
	}

	existing, _, getErr := h.UserStore.GetByUsername(subject)
	role := "member"
	switch {
	case getErr == nil && existing.Disabled:
		httperr.Auth(w, "invalid credentials")
		return
	case getErr == nil:
		role = string(existing.Role)
	case getErr != nil && adminSubjects[subject]:
		role = "admin"
	}

	if getErr != nil {
		// Auto-provision with an unguessable unusable password hash.
		pwSeed, _ := auth.RandomSecret()
		if _, err := h.UserStore.Create(subject, pwSeed, role, ""); err != nil {
			httperr.Write(w, http.StatusInternalServerError, "failed to provision oidc user", httperr.TypeProxy)
			return
		}
	}

	u, _, err := h.UserStore.GetByUsername(subject)
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "failed to load provisioned user", httperr.TypeProxy)
		return
	}
	tv, _ := h.UserStore.TokenVersionFor(subject)
	token, err := auth.MakeTokenFull(h.Config.JWTSecret, subject, h.orgClaimFor(u), string(u.Role), tv)
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "failed to create token", httperr.TypeProxy)
		return
	}
	_ = h.UserStore.UpdateLastLogin(u.ID)
	h.auditLogin(subject)
	w.Header().Set("Content-Type", "application/json")
	setSessionCookie(w, r, token)
	json.NewEncoder(w).Encode(map[string]string{"token": token})
}

// resolveScope returns the org scope for the request and whether it may
// proceed. Rules (fail-closed):
//   - role admin  → global scope unless their JWT carries an org_id
//   - other roles → the JWT's org claim when present
//   - other roles WITHOUT an org claim → global READ scope is allowed only
//     when the deployment has no organizations at all (single-tenant
//     installs); once any org exists, unscoped non-admins are denied so they
//     can never see cross-org data. Mutations stay gated by
//     requireWriteTarget regardless.
//
// The scope derives exclusively from the verified JWT claim — never from
// client headers.
func (h *AdminHandler) resolveScope(r *http.Request) (string, bool) {
	role := auth.GetRole(r)
	org := auth.GetOrgID(r)
	if role == "admin" {
		return org, true
	}
	if org != "" {
		return org, true
	}
	return "", h.globalScopeAllowed()
}

// globalScopeAllowed reports whether unscoped non-admin reads may proceed:
// true only while no organizations exist (single-tenant deployment).
func (h *AdminHandler) globalScopeAllowed() bool {
	var cnt int
	// Missing table (org feature never used) → no orgs → allow.
	if err := h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM organizations`)).Scan(&cnt); err != nil {
		return true
	}
	return cnt == 0
}

// requireWriteTarget enforces ownership of an id-addressed resource for
// tenant-scoped callers (global admins pass through). Denies with 404.
func (h *AdminHandler) requireWriteTarget(w http.ResponseWriter, r *http.Request, table string, id string) bool {
	orgID := auth.GetOrgID(r)
	if orgID == "" {
		// Only admins can hold global scope (resolveScope guarantees this on
		// read paths); mutations additionally verify role.
		return auth.GetRole(r) == "admin"
	}
	var cnt int
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE id=? AND org_id=?", table)
	err := h.DB.QueryRow(db.Q(q), id, orgID).Scan(&cnt)
	if err != nil || cnt == 0 {
		httperr.NotFound(w, "not found")
		return false
	}
	return true
}

func (h *AdminHandler) ListProviders(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.resolveScope(r)
	if !ok {
		httperr.Forbidden(w, "org scope required")
		return
	}
	var list []models.Provider
	var err error
	if orgID != "" {
		list, err = h.ProviderStore.ListForOrg(orgID)
	} else {
		list, err = h.ProviderStore.List()
	}
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "failed to list", httperr.TypeProxy)
		return
	}
	if list == nil {
		list = []models.Provider{}
	}
	if h.Breaker != nil {
		for i := range list {
			if h.Breaker.State(list[i].ID) == "open" {
				st := "circuit_open"
				list[i].HealthStatus = &st
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (h *AdminHandler) CreateProvider(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
		OrgID   string `json:"org_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
		httperr.Invalid(w, "invalid json")
		return
	}
	if body.Name == "" || body.APIKey == "" || body.Type == "" {
		httperr.Invalid(w, "name, type, api_key required")
		return
	}
	if len(body.Name) > 128 || len(body.BaseURL) > 2048 {
		httperr.Invalid(w, "name or base_url too long")
		return
	}
	// Tenant-scoped callers may only create providers inside their own org —
	// client-supplied org_id values are overridden by the verified claim for
	// non-admins (a member of org A must not plant providers in org B). Only
	// admins may create global (unscoped) providers or target another org.
	if auth.GetRole(r) != "admin" {
		body.OrgID = auth.GetOrgID(r)
	}
	if body.OrgID == "" && auth.GetRole(r) != "admin" {
		httperr.Forbidden(w, "org scope required")
		return
	}
	typ := models.ProviderType(body.Type)
	p, err := h.ProviderStore.CreateWithOrg(body.Name, typ, body.BaseURL, body.APIKey, body.OrgID)
	if err != nil {
		httperr.Invalid(w, err.Error())
		return
	}
	// auto-discover models in background
	if h.Discovery != nil {
		go func(id string) {
			// small delay to ensure commit
			// ignore error
			h.Discovery.Discover(id)
		}(p.ID)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(p)
}

func (h *AdminHandler) DeleteProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !h.requireWriteTarget(w, r, "providers", id) {
		return
	}
	if err := h.ProviderStore.Delete(id); err != nil {
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// UpdateProvider edits an existing provider (name, base_url, api_key). This
// is the credential-rotation path: previously the only way to replace a
// provider key was delete+recreate, which changed the provider ID and broke
// lb_rules member references and history joins. api_key is optional — when
// empty the stored credential is kept.
func (h *AdminHandler) UpdateProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !h.requireWriteTarget(w, r, "providers", id) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Name    *string `json:"name"`
		BaseURL *string `json:"base_url"`
		APIKey  *string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httperr.Invalid(w, "invalid json")
		return
	}
	if body.Name != nil {
		if strings.TrimSpace(*body.Name) == "" || len(*body.Name) > 128 {
			httperr.Invalid(w, "invalid name")
			return
		}
	}
	if body.BaseURL != nil && len(*body.BaseURL) > 2048 {
		httperr.Invalid(w, "base_url too long")
		return
	}
	p, err := h.ProviderStore.Update(id, body.Name, body.BaseURL, body.APIKey)
	if err != nil {
		httperr.Write(w, http.StatusBadRequest, err.Error(), httperr.TypeInvalid)
		return
	}
	p.APIKey = "***"
	p.APIKeyEnc = nil
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(p)
}

// ListAudit exposes the audit trail to admins (the trail previously had NO
// read API — rows were only reachable via the per-user profile feed).
// Params: limit (1-500, default 100), offset, actor.
func (h *AdminHandler) ListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 500 {
		limit = v
	}
	offset := 0
	if v, err := strconv.Atoi(q.Get("offset")); err == nil && v >= 0 {
		offset = v
	}
	where := ""
	args := []any{}
	if a := strings.TrimSpace(q.Get("actor")); a != "" {
		where = " WHERE actor=?"
		args = append(args, a)
	}
	var total int64
	_ = h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM audit_logs`+where), args...).Scan(&total)
	w.Header().Set("X-Total-Count", strconv.FormatInt(total, 10))
	rows, err := h.DB.Query(db.Q(`SELECT id, actor, action, target_type, target_id, meta, created_at FROM audit_logs`+where+` ORDER BY created_at DESC LIMIT ? OFFSET ?`), append(args, limit, offset)...)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type auditRow struct {
		ID         string    `json:"id"`
		Actor      string    `json:"actor"`
		Action     string    `json:"action"`
		TargetType string    `json:"target_type"`
		TargetID   string    `json:"target_id"`
		Meta       string    `json:"meta"`
		CreatedAt  time.Time `json:"created_at"`
	}
	out := []auditRow{}
	for rows.Next() {
		var a auditRow
		var created sql.NullTime
		var targetID, meta sql.NullString
		if err := rows.Scan(&a.ID, &a.Actor, &a.Action, &a.TargetType, &targetID, &meta, &created); err == nil && created.Valid {
			a.TargetID = targetID.String
			a.Meta = meta.String
			a.CreatedAt = created.Time.UTC()
			out = append(out, a)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (h *AdminHandler) TestProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type    string `json:"type"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	// simple test: try to hit /models
	base := body.BaseURL
	if base == "" {
		if body.Type == "anthropic" {
			base = "https://api.anthropic.com"
		} else {
			base = "https://api.openai.com/v1"
		}
	}
	// we won't actually call upstream in test without key validation; just check key non-empty and url parse
	if body.APIKey == "" {
		http.Error(w, `{"error":"api_key required"}`, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "provider config looks valid (live check skipped in Phase 1)"})
}

func (h *AdminHandler) ListKeys(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.resolveScope(r)
	if !ok {
		httperr.Forbidden(w, "org scope required")
		return
	}
	var list []models.GatewayKey
	var err error
	if orgID != "" {
		list, err = h.APIKeyStore.ListForOrg(orgID)
	} else {
		list, err = h.APIKeyStore.List()
	}
	if err != nil {
		http.Error(w, `{"error":"failed"}`, http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []models.GatewayKey{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (h *AdminHandler) CreateKey(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Name          string   `json:"name"`
		OrgID         string   `json:"org_id"`
		RPM           *int     `json:"rate_limit_rpm"`
		RPH           *int     `json:"rate_limit_rph"`
		RPD           *int     `json:"rate_limit_rpd"`
		TPM           *int     `json:"rate_limit_tpm"`
		AllowedModels []string `json:"allowed_models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
		httperr.Invalid(w, "invalid json")
		return
	}
	if len(body.AllowedModels) > 100 {
		httperr.Invalid(w, "allowed_models: too many entries (max 100)")
		return
	}
	if body.Name == "" {
		body.Name = "key-" + uuid.NewString()[:8]
	}
	if len(body.Name) > 64 {
		httperr.Invalid(w, "name too long (max 64)")
		return
	}
	// Same cross-org guard as CreateProvider: non-admins always get their own
	// verified claim org; admins may target any org or global.
	if auth.GetRole(r) != "admin" {
		body.OrgID = auth.GetOrgID(r)
	}
	if body.OrgID == "" && auth.GetRole(r) != "admin" {
		httperr.Forbidden(w, "org scope required")
		return
	}
	k, err := h.APIKeyStore.CreateWithOrg(body.Name, body.OrgID)
	if err != nil {
		http.Error(w, `{"error":"create failed"}`, http.StatusInternalServerError)
		return
	}
	// Apply optional limits/allowlist if provided. A failed limits update must
	// surface to the caller — silently returning an unlimited key is worse than
	// failing the create.
	if body.RPM != nil || body.RPH != nil || body.RPD != nil || body.TPM != nil || body.AllowedModels != nil {
		var amPtr *[]string
		if body.AllowedModels != nil {
			amPtr = &body.AllowedModels
		}
		if err := h.APIKeyStore.UpdateLimits(k.ID, body.RPM, body.RPH, body.RPD, body.TPM, amPtr); err != nil {
			// Roll back the half-created key rather than leaving an unbounded one.
			_ = h.APIKeyStore.Delete(k.ID)
			httperr.Write(w, http.StatusBadRequest, "failed to apply key limits: "+err.Error(), httperr.TypeInvalid)
			return
		}
		if updated, err := h.APIKeyStore.GetByID(k.ID); err == nil {
			k.RateLimitRPM = updated.RateLimitRPM
			k.RateLimitRPH = updated.RateLimitRPH
			k.RateLimitRPD = updated.RateLimitRPD
			k.RateLimitTPM = updated.RateLimitTPM
			k.AllowedModels = updated.AllowedModels
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(k)
}

func (h *AdminHandler) DeleteKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !h.requireWriteTarget(w, r, "gateway_keys", id) {
		return
	}
	if err := h.APIKeyStore.Delete(id); err != nil {
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *AdminHandler) Stats(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.resolveScope(r)
	if !ok {
		httperr.Forbidden(w, "org scope required")
		return
	}
	var providerCount, keyCount, logCount int
	if orgID != "" {
		h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM providers WHERE org_id=?`), orgID).Scan(&providerCount)
		h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM gateway_keys WHERE revoked_at IS NULL AND org_id=?`), orgID).Scan(&keyCount)
		h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM request_logs WHERE provider_id IN (SELECT id FROM providers WHERE org_id=?)`), orgID).Scan(&logCount)
	} else {
		h.DB.QueryRow(`SELECT COUNT(*) FROM providers`).Scan(&providerCount)
		h.DB.QueryRow(`SELECT COUNT(*) FROM gateway_keys WHERE revoked_at IS NULL`).Scan(&keyCount)
		h.DB.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&logCount)
	}
	var totalTokens sql.NullInt64
	var totalCost sql.NullFloat64
	// Cross-tenant guard: every request_logs aggregate below must honor the
	// same org scope as the counts above — previously only the counts were
	// filtered, leaking other orgs' tokens/cost/latency metadata to
	// org-scoped callers.
	orgFilter := ""
	orgArgs := []any{}
	if orgID != "" {
		orgFilter = " AND provider_id IN (SELECT id FROM providers WHERE org_id=?)"
		orgArgs = []any{orgID}
	}
	totArgs := append([]any{}, orgArgs...)
	h.DB.QueryRow(db.Q(`SELECT COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_usd),0) FROM request_logs WHERE 1=1`+orgFilter), totArgs...).Scan(&totalTokens, &totalCost)
	var catalogCount int
	h.DB.QueryRow(`SELECT COUNT(*) FROM models_catalog`).Scan(&catalogCount)
	var aliasCount int
	h.DB.QueryRow(`SELECT COUNT(*) FROM model_aliases`).Scan(&aliasCount)
	var providerModelsCount int
	h.DB.QueryRow(`SELECT COUNT(*) FROM provider_models`).Scan(&providerModelsCount)

	rng := strings.TrimSpace(r.URL.Query().Get("range"))
	if rng == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"providers":       providerCount,
			"keys":            keyCount,
			"requests":        logCount,
			"total_tokens":    totalTokens.Int64,
			"total_cost":      totalCost.Float64,
			"catalog":         catalogCount,
			"aliases":         aliasCount,
			"provider_models": providerModelsCount,
		})
		return
	}

	// range rollup: support 24h/7d/30d
	now := time.Now().UTC()
	var start time.Time
	switch rng {
	case "24h":
		start = now.Add(-24 * time.Hour)
	case "7d":
		start = now.Add(-7 * 24 * time.Hour)
	case "30d":
		start = now.Add(-30 * 24 * time.Hour)
	default:
		// try generic parse: e.g. "7d" or "24h"
		if strings.HasSuffix(rng, "h") {
			start = now.Add(-24 * time.Hour)
			rng = "24h"
		} else if strings.HasSuffix(rng, "d") {
			num := strings.TrimSuffix(rng, "d")
			switch num {
			case "7":
				start = now.Add(-7 * 24 * time.Hour)
				rng = "7d"
			case "30":
				start = now.Add(-30 * 24 * time.Hour)
				rng = "30d"
			default:
				start = now.Add(-7 * 24 * time.Hour)
				rng = "7d"
			}
		} else {
			start = now.Add(-7 * 24 * time.Hour)
			rng = "7d"
		}
	}

	// daily buckets GROUP BY day — hourly (strftime bucket) for 24h so the
	// chart isn't a single bar, daily otherwise. Hour bucket keys carry a
	// "T…Z" ISO fragment so clients can label them distinctly.
	bucketExpr := "date(created_at)"
	if rng == "24h" {
		bucketExpr = `strftime('%Y-%m-%dT%H:00:00Z', created_at)`
	}
	type daily struct {
		Day      string  `json:"day"`
		Tokens   int64   `json:"tokens"`
		Cost     float64 `json:"cost"`
		Requests int64   `json:"requests"`
	}
	var dailyRows []daily
	rows, err := h.DB.Query(db.Q(`SELECT `+bucketExpr+` as day, COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_usd),0), COUNT(*) FROM request_logs WHERE created_at >= ?`+orgFilter+` GROUP BY `+bucketExpr+` ORDER BY day`), append([]any{start}, orgArgs...)...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var d daily
			var daySql sql.NullString
			if err := rows.Scan(&daySql, &d.Tokens, &d.Cost, &d.Requests); err == nil {
				if daySql.Valid {
					d.Day = daySql.String
				}
				dailyRows = append(dailyRows, d)
			}
		}
	}
	if dailyRows == nil {
		dailyRows = []daily{}
	}

	// top models
	type topM struct {
		Model    string  `json:"model"`
		Tokens   int64   `json:"tokens"`
		Cost     float64 `json:"cost"`
		Requests int64   `json:"requests"`
	}
	var topModels []topM
	rows2, err := h.DB.Query(db.Q(`SELECT model, COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_usd),0), COUNT(*) FROM request_logs WHERE created_at >= ? AND model != ''`+orgFilter+` GROUP BY model ORDER BY SUM(total_tokens) DESC LIMIT 5`), append([]any{start}, orgArgs...)...)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var m topM
			rows2.Scan(&m.Model, &m.Tokens, &m.Cost, &m.Requests)
			topModels = append(topModels, m)
		}
	}
	if topModels == nil {
		topModels = []topM{}
	}

	// top keys
	type topK struct {
		KeyPrefix string  `json:"key_prefix"`
		Tokens    int64   `json:"tokens"`
		Cost      float64 `json:"cost"`
		Requests  int64   `json:"requests"`
	}
	var topKeys []topK
	rows3, err := h.DB.Query(db.Q(`SELECT key_prefix, COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_usd),0), COUNT(*) FROM request_logs WHERE created_at >= ? AND key_prefix != ''`+orgFilter+` GROUP BY key_prefix ORDER BY COUNT(*) DESC LIMIT 5`), append([]any{start}, orgArgs...)...)
	if err == nil {
		defer rows3.Close()
		for rows3.Next() {
			var k topK
			rows3.Scan(&k.KeyPrefix, &k.Tokens, &k.Cost, &k.Requests)
			topKeys = append(topKeys, k)
		}
	}
	if topKeys == nil {
		topKeys = []topK{}
	}

	// latency p50/p95
	var latencies []int64
	rows4, err := h.DB.Query(db.Q(`SELECT latency_ms FROM request_logs WHERE created_at >= ? AND latency_ms IS NOT NULL`+orgFilter+` ORDER BY latency_ms`), append([]any{start}, orgArgs...)...)
	if err == nil {
		defer rows4.Close()
		for rows4.Next() {
			var v sql.NullInt64
			if err := rows4.Scan(&v); err == nil && v.Valid {
				latencies = append(latencies, v.Int64)
			}
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	var p50, p95 int64
	var avg float64
	if len(latencies) > 0 {
		p50 = percentile(latencies, 50)
		p95 = percentile(latencies, 95)
		var sum int64
		for _, v := range latencies {
			sum += v
		}
		avg = float64(sum) / float64(len(latencies))
	}
	// ensure non-nil slices for json
	_ = math.Ceil // keep import used if not otherwise

	// totals for range
	var rangeTokens sql.NullInt64
	var rangeCost sql.NullFloat64
	var rangeCount int
	h.DB.QueryRow(db.Q(`SELECT COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_usd),0), COUNT(*) FROM request_logs WHERE created_at >= ?`+orgFilter), append([]any{start}, orgArgs...)...).Scan(&rangeTokens, &rangeCost, &rangeCount)

	// success vs failure for range
	var successful, failed int
	h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM request_logs WHERE created_at >= ? AND status < 400`+orgFilter), append([]any{start}, orgArgs...)...).Scan(&successful)
	h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM request_logs WHERE created_at >= ? AND status >= 400`+orgFilter), append([]any{start}, orgArgs...)...).Scan(&failed)

	// TTFT and TPS aggregates for range
	var avgTTFT sql.NullFloat64
	var avgTPS sql.NullFloat64
	h.DB.QueryRow(db.Q(`SELECT COALESCE(AVG(ttft_ms),0) FROM request_logs WHERE created_at >= ? AND ttft_ms > 0`+orgFilter), append([]any{start}, orgArgs...)...).Scan(&avgTTFT)
	h.DB.QueryRow(db.Q(`SELECT COALESCE(AVG(CASE WHEN latency_ms>0 THEN total_tokens*1000.0/latency_ms ELSE 0 END),0) FROM request_logs WHERE created_at >= ? AND total_tokens>0`+orgFilter), append([]any{start}, orgArgs...)...).Scan(&avgTPS)

	// overall success/failure
	var totalSuccessful, totalFailed int
	h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM request_logs WHERE status < 400`+orgFilter), orgArgs...).Scan(&totalSuccessful)
	h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM request_logs WHERE status >= 400`+orgFilter), orgArgs...).Scan(&totalFailed)

	// error breakdown for the range: count per status code with a sample
	// error message (most recent error text seen for that status).
	type errRow struct {
		Status int    `json:"status"`
		Count  int64  `json:"count"`
		Sample string `json:"sample,omitempty"`
	}
	var errorRows []errRow
	rows5, err5 := h.DB.Query(db.Q(`SELECT status, COUNT(*), (SELECT rl2.error FROM request_logs rl2 WHERE rl2.status=rl.status AND rl2.error IS NOT NULL AND rl2.created_at>=?`+orgFilter+` ORDER BY rl2.created_at DESC LIMIT 1) FROM request_logs rl WHERE rl.created_at >= ? AND rl.status >= 400`+orgFilter+` GROUP BY rl.status ORDER BY COUNT(*) DESC LIMIT 10`), append([]any{start, start}, orgArgs...)...)
	if err5 == nil {
		defer rows5.Close()
		for rows5.Next() {
			var e errRow
			var status sql.NullInt64
			var sample sql.NullString
			if err := rows5.Scan(&status, &e.Count, &sample); err == nil && status.Valid {
				e.Status = int(status.Int64)
				e.Sample = sample.String
				errorRows = append(errorRows, e)
			}
		}
	}
	if errorRows == nil {
		errorRows = []errRow{}
	}

	// cache hit rate over the range: share of billed prompt tokens served
	// from the provider's prompt cache (0 when no cache data exists).
	var cacheReadSum, promptSum sql.NullInt64
	h.DB.QueryRow(db.Q(`SELECT COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(prompt_tokens),0) FROM request_logs WHERE created_at >= ?`+orgFilter), append([]any{start}, orgArgs...)...).Scan(&cacheReadSum, &promptSum)
	cacheRate := 0.0
	if promptSum.Int64 > 0 {
		cacheRate = float64(cacheReadSum.Int64) / float64(promptSum.Int64)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"providers":       providerCount,
		"keys":            keyCount,
		"requests":        logCount,
		"total_tokens":    totalTokens.Int64,
		"total_cost":      totalCost.Float64,
		"catalog":         catalogCount,
		"aliases":         aliasCount,
		"provider_models": providerModelsCount,
		"range":           rng,
		"daily":           dailyRows,
		"top_models":      topModels,
		"top_keys":        topKeys,
		"latency": map[string]interface{}{
			"p50":   p50,
			"p95":   p95,
			"avg":   avg,
			"count": len(latencies),
		},
		"ttft": map[string]interface{}{
			"avg": avgTTFT.Float64,
		},
		"tps": map[string]interface{}{
			"avg": avgTPS.Float64,
		},
		"successful":        totalSuccessful,
		"failed":            totalFailed,
		"range_successful":  successful,
		"range_failed":      failed,
		"range_ttft_avg":    avgTTFT.Float64,
		"range_tps_avg":     avgTPS.Float64,
		"range_tokens":      rangeTokens.Int64,
		"range_cost":        rangeCost.Float64,
		"range_requests":    rangeCount,
		"errors":            errorRows,
		"cache_hit_rate":    cacheRate,
		"cache_read_tokens": cacheReadSum.Int64,
	})
}

func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	idx := int(math.Ceil(float64(p)/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// Logs lists recent request logs with pagination and filters. Response stays
// a JSON array (backward compatible); the total matching row count is exposed
// via the X-Total-Count header so clients can paginate without shape churn.
// Query params: limit (1-500, default 100), offset, model (substring),
// key (key_prefix prefix), endpoint (substring), status ("failed" or code),
// since (RFC3339 timestamp or Go duration like 24h).
func (h *AdminHandler) Logs(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.resolveScope(r)
	if !ok {
		httperr.Forbidden(w, "org scope required")
		return
	}
	q := r.URL.Query()
	limit := 100
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 500 {
		limit = v
	}
	offset := 0
	if v, err := strconv.Atoi(q.Get("offset")); err == nil && v >= 0 {
		offset = v
	}

	where := []string{}
	args := []any{}
	if orgID != "" {
		where = append(where, "p.org_id=?")
		args = append(args, orgID)
	}
	if m := strings.TrimSpace(q.Get("model")); m != "" {
		where = append(where, "(rl.model LIKE ? OR rl.model = ?)")
		args = append(args, "%"+m+"%", m)
	}
	if k := strings.TrimSpace(q.Get("key")); k != "" {
		where = append(where, "rl.key_prefix LIKE ?")
		args = append(args, k+"%")
	}
	if ep := strings.TrimSpace(q.Get("endpoint")); ep != "" {
		where = append(where, "rl.endpoint LIKE ?")
		args = append(args, "%"+ep+"%")
	}
	if s := strings.TrimSpace(q.Get("status")); s != "" {
		if s == "failed" {
			where = append(where, "rl.status >= 400")
		} else if code, err := strconv.Atoi(s); err == nil {
			where = append(where, "rl.status=?")
			args = append(args, code)
		}
	}
	if sv := strings.TrimSpace(q.Get("since")); sv != "" {
		if t, err := time.Parse(time.RFC3339, sv); err == nil {
			where = append(where, "rl.created_at >= ?")
			args = append(args, t.UTC())
		} else if d, err := time.ParseDuration(sv); err == nil && d > 0 {
			where = append(where, "rl.created_at >= ?")
			args = append(args, time.Now().UTC().Add(-d))
		}
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}
	from := " FROM request_logs rl LEFT JOIN providers p ON rl.provider_id = p.id" + whereSQL

	var total int64
	if err := h.DB.QueryRow(db.Q(`SELECT COUNT(*)`+from), args...).Scan(&total); err == nil {
		w.Header().Set("X-Total-Count", strconv.FormatInt(total, 10))
	}

	limArgs := append(append([]any{}, args...), limit, offset)
	rows, err := h.DB.Query(db.Q(`SELECT rl.id,rl.key_prefix,rl.provider_id,rl.model,rl.endpoint,rl.status,rl.latency_ms,rl.ttft_ms,rl.created_at,rl.prompt_tokens,rl.completion_tokens,rl.total_tokens,rl.cost_usd,rl.is_stream,rl.error`+from+` ORDER BY rl.created_at DESC LIMIT ? OFFSET ?`), limArgs...)
	if err != nil {
		// Fallback for DBs without ttft_ms/error columns.
		rows, err = h.DB.Query(db.Q(`SELECT rl.id,rl.key_prefix,rl.provider_id,rl.model,rl.endpoint,rl.status,rl.latency_ms,rl.created_at,rl.prompt_tokens,rl.completion_tokens,rl.total_tokens,rl.cost_usd,rl.is_stream`+from+` ORDER BY rl.created_at DESC LIMIT ? OFFSET ?`), limArgs...)
		if err != nil {
			http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		logs := []models.RequestLog{}
		for rows.Next() {
			var l models.RequestLog
			if rows.Scan(&l.ID, &l.KeyPrefix, &l.ProviderID, &l.Model, &l.Endpoint, &l.Status, &l.LatencyMs, &l.CreatedAt, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.CostUSD, &l.IsStream) == nil {
				logs = append(logs, l)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(logs)
		return
	}
	defer rows.Close()
	logs := []models.RequestLog{}
	for rows.Next() {
		var l models.RequestLog
		var ttft sql.NullInt64
		var errStr sql.NullString
		if err := rows.Scan(&l.ID, &l.KeyPrefix, &l.ProviderID, &l.Model, &l.Endpoint, &l.Status, &l.LatencyMs, &ttft, &l.CreatedAt, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.CostUSD, &l.IsStream, &errStr); err != nil {
			continue
		}
		if ttft.Valid {
			l.TTFTMs = ttft.Int64
		}
		if errStr.Valid {
			l.Error = errStr.String
		}
		logs = append(logs, l)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(logs)
}

func (h *AdminHandler) GetLog(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// resolveScope (not the raw claim) so unscoped non-admins are denied once
	// organizations exist — this detail view uniquely carries stored
	// request/response bodies.
	orgID, scopeOK := h.resolveScope(r)
	if !scopeOK {
		httperr.Forbidden(w, "org scope required")
		return
	}
	var l models.RequestLog
	var ttft sql.NullInt64
	var errStr, reqBody, respBody sql.NullString
	var finishReason, fallbackChain sql.NullString
	var cacheRead, cacheWrite, reasoning sql.NullInt64
	var q string
	if orgID != "" {
		q = db.Q(`SELECT rl.id,rl.key_prefix,rl.provider_id,rl.model,rl.endpoint,rl.status,rl.latency_ms,rl.ttft_ms,rl.created_at,rl.prompt_tokens,rl.completion_tokens,rl.total_tokens,rl.cost_usd,rl.is_stream,rl.error,rl.request_body,rl.response_body,rl.finish_reason,rl.fallback_chain,rl.cache_read_tokens,rl.cache_write_tokens,rl.reasoning_tokens FROM request_logs rl JOIN providers p ON rl.provider_id=p.id WHERE rl.id=? AND p.org_id=?`)
	} else {
		q = db.Q(`SELECT id,key_prefix,provider_id,model,endpoint,status,latency_ms,ttft_ms,created_at,prompt_tokens,completion_tokens,total_tokens,cost_usd,is_stream,error,request_body,response_body,finish_reason,fallback_chain,cache_read_tokens,cache_write_tokens,reasoning_tokens FROM request_logs WHERE id=?`)
	}
	args := []any{id}
	if orgID != "" {
		args = append(args, orgID)
	}
	err := h.DB.QueryRow(q, args...).Scan(&l.ID, &l.KeyPrefix, &l.ProviderID, &l.Model, &l.Endpoint, &l.Status, &l.LatencyMs, &ttft, &l.CreatedAt, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.CostUSD, &l.IsStream, &errStr, &reqBody, &respBody, &finishReason, &fallbackChain, &cacheRead, &cacheWrite, &reasoning)
	if err != nil {
		// Fallback without ttft/error — still org-scoped.
		var ferr error
		if orgID != "" {
			ferr = h.DB.QueryRow(db.Q(`SELECT rl.id,rl.key_prefix,rl.provider_id,rl.model,rl.endpoint,rl.status,rl.latency_ms,rl.created_at,rl.prompt_tokens,rl.completion_tokens,rl.total_tokens,rl.cost_usd,rl.is_stream FROM request_logs rl JOIN providers p ON rl.provider_id=p.id WHERE rl.id=? AND p.org_id=?`), id, orgID).Scan(&l.ID, &l.KeyPrefix, &l.ProviderID, &l.Model, &l.Endpoint, &l.Status, &l.LatencyMs, &l.CreatedAt, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.CostUSD, &l.IsStream)
		} else {
			ferr = h.DB.QueryRow(`SELECT id,key_prefix,provider_id,model,endpoint,status,latency_ms,created_at,prompt_tokens,completion_tokens,total_tokens,cost_usd,is_stream FROM request_logs WHERE id=?`, id).Scan(&l.ID, &l.KeyPrefix, &l.ProviderID, &l.Model, &l.Endpoint, &l.Status, &l.LatencyMs, &l.CreatedAt, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.CostUSD, &l.IsStream)
		}
		if ferr != nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
	} else {
		if ttft.Valid {
			l.TTFTMs = ttft.Int64
		}
		if errStr.Valid {
			l.Error = errStr.String
		}
		if reqBody.Valid {
			l.RequestBody = reqBody.String
		}
		if respBody.Valid {
			l.ResponseBody = respBody.String
		}
		if finishReason.Valid {
			l.FinishReason = finishReason.String
		}
		if fallbackChain.Valid {
			l.FallbackChain = fallbackChain.String
		}
		if cacheRead.Valid {
			l.CacheReadTokens = int(cacheRead.Int64)
		}
		if cacheWrite.Valid {
			l.CacheWriteTokens = int(cacheWrite.Int64)
		}
		if reasoning.Valid {
			l.ReasoningTokens = int(reasoning.Int64)
		}
	}
	// Enrich with provider name and key name
	var providerName sql.NullString
	h.DB.QueryRow(db.Q(`SELECT name FROM providers WHERE id=?`), l.ProviderID).Scan(&providerName)
	var keyName sql.NullString
	h.DB.QueryRow(db.Q(`SELECT name FROM gateway_keys WHERE prefix=?`), l.KeyPrefix).Scan(&keyName)
	extra := map[string]any{
		"log":           l,
		"provider_name": providerName.String,
		"key_name":      keyName.String,
		"tps":           0.0,
		"ttft_ms":       l.TTFTMs,
		"error":         l.Error,
		"request_body":  l.RequestBody,
		"response_body": l.ResponseBody,
		"finish_reason": l.FinishReason,
		"fallback_chain": func() any {
			if l.FallbackChain == "" {
				return nil
			}
			var chain []map[string]any
			if json.Unmarshal([]byte(l.FallbackChain), &chain) == nil {
				return chain
			}
			return l.FallbackChain
		}(),
		"cache_read_tokens":  l.CacheReadTokens,
		"cache_write_tokens": l.CacheWriteTokens,
		"reasoning_tokens":   l.ReasoningTokens,
	}
	if l.LatencyMs > 0 && l.TotalTokens > 0 {
		extra["tps"] = float64(l.TotalTokens) / (float64(l.LatencyMs) / 1000.0)
	}
	if l.TTFTMs > 0 && l.CompletionTokens > 0 {
		remaining := l.LatencyMs - l.TTFTMs
		if remaining > 0 {
			extra["tps_after_ttft"] = float64(l.CompletionTokens) / (float64(remaining) / 1000.0)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(extra)
}

func (h *AdminHandler) UpdateKeyRateLimit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !h.requireWriteTarget(w, r, "gateway_keys", id) {
		return
	}
	var body struct {
		RPM *int `json:"rpm"`
		RPH *int `json:"rph"`
		RPD *int `json:"rpd"`
		TPM *int `json:"tpm"`
		// support both camel and snake
		RateLimitRPM *int `json:"rate_limit_rpm"`
		RateLimitRPH *int `json:"rate_limit_rph"`
		RateLimitRPD *int `json:"rate_limit_rpd"`
		RateLimitTPM *int `json:"rate_limit_tpm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, 400)
		return
	}
	// coalesce aliases
	rpm := body.RPM
	if rpm == nil {
		rpm = body.RateLimitRPM
	}
	rph := body.RPH
	if rph == nil {
		rph = body.RateLimitRPH
	}
	rpd := body.RPD
	if rpd == nil {
		rpd = body.RateLimitRPD
	}
	tpm := body.TPM
	if tpm == nil {
		tpm = body.RateLimitTPM
	}
	if rpm == nil && rph == nil && rpd == nil && tpm == nil {
		http.Error(w, `{"error":"no rate limit fields provided"}`, 400)
		return
	}
	if err := h.APIKeyStore.UpdateLimits(id, rpm, rph, rpd, tpm, nil); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{"id": id}
	if rpm != nil {
		resp["rpm"] = *rpm
	}
	if rph != nil {
		resp["rph"] = *rph
	}
	if rpd != nil {
		resp["rpd"] = *rpd
	}
	if tpm != nil {
		resp["tpm"] = *tpm
	}
	json.NewEncoder(w).Encode(resp)
}

func (h *AdminHandler) UpdateKeyLimits(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !h.requireWriteTarget(w, r, "gateway_keys", id) {
		return
	}
	var body struct {
		RPM           *int      `json:"rpm"`
		RPH           *int      `json:"rph"`
		RPD           *int      `json:"rpd"`
		TPM           *int      `json:"tpm"`
		RateLimitRPM  *int      `json:"rate_limit_rpm"`
		RateLimitRPH  *int      `json:"rate_limit_rph"`
		RateLimitRPD  *int      `json:"rate_limit_rpd"`
		RateLimitTPM  *int      `json:"rate_limit_tpm"`
		AllowedModels *[]string `json:"allowed_models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, 400)
		return
	}
	rpm := body.RPM
	if rpm == nil {
		rpm = body.RateLimitRPM
	}
	rph := body.RPH
	if rph == nil {
		rph = body.RateLimitRPH
	}
	rpd := body.RPD
	if rpd == nil {
		rpd = body.RateLimitRPD
	}
	tpm := body.TPM
	if tpm == nil {
		tpm = body.RateLimitTPM
	}
	if rpm == nil && rph == nil && rpd == nil && tpm == nil && body.AllowedModels == nil {
		http.Error(w, `{"error":"no fields to update"}`, 400)
		return
	}
	if err := h.APIKeyStore.UpdateLimits(id, rpm, rph, rpd, tpm, body.AllowedModels); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), 400)
		return
	}
	updated, err := h.APIKeyStore.GetByID(id)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": id})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(updated)
}

func (h *AdminHandler) UpdateKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !h.requireWriteTarget(w, r, "gateway_keys", id) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, 400)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		http.Error(w, `{"error":"name required"}`, 400)
		return
	}
	if len(name) > 64 {
		http.Error(w, `{"error":"name too long (max 64)"}`, 400)
		return
	}
	if err := h.APIKeyStore.UpdateName(id, name); err != nil {
		http.Error(w, `{"error":"update failed"}`, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id": id, "name": name})
}

func (h *AdminHandler) BillingExport(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.resolveScope(r)
	if !ok {
		httperr.Forbidden(w, "org scope required")
		return
	}
	// billing export webhook (Phase 3): emit audit-style webhook for exports
	if webhook.Global != nil {
		webhook.Global.Emit("billing.export", map[string]any{
			"actor":  "admin",
			"action": "export",
			"range":  r.URL.Query().Get("range"),
			"path":   r.URL.Path,
			"time":   time.Now().UTC().Format(time.RFC3339Nano),
		})
	}
	rangeStr := r.URL.Query().Get("range")
	if rangeStr == "" {
		rangeStr = "7d"
	}
	var start time.Time
	switch rangeStr {
	case "24h":
		start = time.Now().UTC().Add(-24 * time.Hour)
	case "7d":
		start = time.Now().UTC().Add(-7 * 24 * time.Hour)
	case "30d":
		start = time.Now().UTC().Add(-30 * 24 * time.Hour)
	default:
		start = time.Now().UTC().Add(-7 * 24 * time.Hour)
	}
	var rows *sql.Rows
	var err error
	if orgID != "" {
		rows, err = h.DB.Query(db.Q(`SELECT rl.id, rl.key_prefix, rl.provider_id, rl.model, rl.endpoint, rl.status, rl.latency_ms, rl.created_at, rl.prompt_tokens, rl.completion_tokens, rl.total_tokens, rl.cost_usd FROM request_logs rl JOIN providers p ON rl.provider_id=p.id WHERE p.org_id=? AND rl.created_at >= ? ORDER BY rl.created_at DESC`), orgID, start)
	} else {
		rows, err = h.DB.Query(db.Q(`SELECT id, key_prefix, provider_id, model, endpoint, status, latency_ms, created_at, prompt_tokens, completion_tokens, total_tokens, cost_usd FROM request_logs WHERE created_at >= ? ORDER BY created_at DESC`), start)
	}
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=\"billing-"+rangeStr+".csv\"")
	w.WriteHeader(http.StatusOK)
	// RFC 4180 quoting: model names / error-ish fields may contain commas.
	q := func(s string) string {
		if strings.ContainsAny(s, ",\"\n") {
			return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
		}
		return s
	}
	w.Write([]byte("id,key_prefix,provider_id,model,endpoint,status,latency_ms,created_at,prompt_tokens,completion_tokens,total_tokens,cost_usd\n"))
	for rows.Next() {
		var id, keyPrefix, providerID, model, endpoint string
		var createdAt sql.NullTime
		var status int
		var latencyMs int64
		var promptTokens, completionTokens, totalTokens int
		var costUSD float64
		// DATETIME columns must scan into time values; scanning into string
		// errors on every row and previously shipped header-only CSVs.
		if err := rows.Scan(&id, &keyPrefix, &providerID, &model, &endpoint, &status, &latencyMs, &createdAt, &promptTokens, &completionTokens, &totalTokens, &costUSD); err == nil {
			ts := ""
			if createdAt.Valid {
				ts = createdAt.Time.UTC().Format(time.RFC3339)
			}
			line := fmt.Sprintf("%s,%s,%s,%s,%s,%d,%d,%s,%d,%d,%d,%.6f\n",
				q(id), q(keyPrefix), q(providerID), q(model), q(endpoint), status, latencyMs, ts, promptTokens, completionTokens, totalTokens, costUSD)
			w.Write([]byte(line))
		}
	}
}
