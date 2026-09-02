package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/audit"
	"ai-gateway/internal/auth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/pat"
	"ai-gateway/internal/rbac"
	"ai-gateway/internal/user"

	"github.com/go-chi/chi/v5"
)

type UsersHandler struct {
	Store       *user.Store
	Config      *config.Config
	DB          *sql.DB
	Recorder    audit.Recorder
	APIKeyStore *apikey.Store // for per-user key counts in ListUsers
}

func (h *UsersHandler) Routes(r chi.Router) {
	r.Get("/admin/users", h.ListUsers)
	r.Post("/admin/users", h.CreateUser)
	r.Get("/admin/users/me", h.GetMe)
	r.Get("/admin/users/{id}/permissions", h.GetUserPermissions)
	r.Put("/admin/users/{id}/permissions", h.PutUserPermissions)
	r.Put("/admin/users/{id}", h.UpdateUser)
	r.Delete("/admin/users/{id}", h.DeleteUser)
	r.Post("/admin/users/{id}/reset-password", h.ResetPassword)
}

// requirePermFor checks one permission against the caller's effective set,
// resolving live when the RBAC middleware ran (it caches perms in context)
// and falling back to the caller's role defaults otherwise. Admins always
// pass. This keeps users-handler behavior identical to the legacy
// role=="admin" checks for default roles while letting overrides in.
func requirePermFor(r *http.Request, permission string) bool {
	role := auth.GetRole(r)
	if role == rbac.RoleAdmin {
		// Admins pass everything, but a PAT still narrows to its scopes.
		if scopes := auth.PATScopes(r); scopes != "" {
			return pat.CheckScopes(rbac.Effective(rbac.RoleAdmin, nil), scopes)[permission]
		}
		return true
	}
	perms := auth.GetPerms(r)
	if perms == nil {
		// No RBAC middleware cache (e.g. PAT path or bare handler): role defaults.
		perms = rbac.Defaults(role)
	}
	if scopes := auth.PATScopes(r); scopes != "" {
		perms = pat.CheckScopes(perms, scopes)
	}
	return rbac.Has(perms, permission)
}

func (h *UsersHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	// RBAC: users:read (admin always passes; overrides honored)
	if !requirePermFor(r, "users:read") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	list, err := h.Store.List()
	if err != nil {
		http.Error(w, `{"error":"failed"}`, http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []user.DashboardUser{}
	}
	// Per-user assigned-key counts (keys:read_own ownership view). Non-revoked
	// keys created by each user; nil-safe when no key store is wired (tests).
	keyCounts := map[string]int{}
	if h.APIKeyStore != nil {
		keyCounts, _ = h.APIKeyStore.CountByOwner()
	}
	out := make([]map[string]any, len(list))
	for i, u := range list {
		b, _ := json.Marshal(u)
		m := map[string]any{}
		_ = json.Unmarshal(b, &m)
		m["key_count"] = keyCounts[u.ID]
		out[i] = m
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (h *UsersHandler) GetMe(w http.ResponseWriter, r *http.Request) {
	sub := auth.GetSubject(r)
	role := auth.GetRole(r)
	encodeWithPerms := func(u *user.DashboardUser) {
		// Effective permissions ride along so the UI can gate itself without
		// a second call. RBAC middleware caches the resolved set in context;
		// fall back to role defaults (e.g. bare-handler paths).
		perms := auth.GetPerms(r)
		if perms == nil {
			perms = rbac.Defaults(string(u.Role))
			if role == rbac.RoleAdmin {
				perms = rbac.Effective(rbac.RoleAdmin, nil)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": u.ID, "username": u.Username, "role": u.Role,
			"display_name": u.DisplayName, "created_at": u.CreatedAt, "updated_at": u.UpdatedAt,
			"last_login_at": u.LastLoginAt, "login_count": u.LoginCount,
			"passkey_enabled": u.PasskeyEnabled, "has_recovery_code": u.HasRecoveryCode,
			"disabled": u.Disabled, "passkey_count": u.PasskeyCount,
			"permissions": rbac.Sorted(perms),
		})
	}
	if sub == "" || sub == "admin" {
		// try to find admin user
		u, _, err := h.Store.GetByUsername("admin")
		if err == nil {
			encodeWithPerms(u)
			return
		}
		// fallback to token subject
		json.NewEncoder(w).Encode(map[string]any{"username": sub, "role": role})
		return
	}
	u, _, err := h.Store.GetByUsername(sub)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	encodeWithPerms(u)
}

// GetUserPermissions returns a user's effective permissions plus their raw
// overrides (admin-only surface; requires users:read).
func (h *UsersHandler) GetUserPermissions(w http.ResponseWriter, r *http.Request) {
	if !requirePermFor(r, "users:read") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	id := chi.URLParam(r, "id")
	u, err := h.Store.GetByID(id)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	overrides, err := h.Store.PermissionOverrides(u.ID)
	if err != nil {
		http.Error(w, `{"error":"failed"}`, http.StatusInternalServerError)
		return
	}
	effective, err := h.Store.EffectivePermissions(u.ID, string(u.Role))
	if err != nil {
		http.Error(w, `{"error":"failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"user_id":   u.ID,
		"username":  u.Username,
		"role":      u.Role,
		"effective": rbac.Sorted(effective),
		"overrides": overrides,
		"catalog":   rbac.All,
	})
}

// PutUserPermissions upserts a batch of overrides. Keys absent from the body
// keep their previous override (or fall back to the role default once
// cleared via null → remove override).
func (h *UsersHandler) PutUserPermissions(w http.ResponseWriter, r *http.Request) {
	if !requirePermFor(r, "users:write") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.Store.GetByID(id); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body map[string]*bool
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httperr.Invalid(w, "invalid json: expected {permission: true|false|null}")
		return
	}
	// Guard: you cannot strip permissions from the last active admin — role
	// admin short-circuits everything anyway, but blocking the write keeps
	// the audit trail honest about what overrides mean.
	if target, err := h.Store.GetByID(id); err == nil && target.Role == rbac.RoleAdmin {
		http.Error(w, `{"error":"admin permissions are fixed (admins always allow)"}`, http.StatusBadRequest)
		return
	}
	for perm, granted := range body {
		if granted == nil {
			// null = clear the override; the role default applies again.
			if err := h.Store.ClearPermission(id, perm); err != nil {
				http.Error(w, `{"error":"failed to clear override"}`, http.StatusInternalServerError)
				return
			}
			continue
		}
		if err := h.Store.SetPermission(id, perm, *granted); err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
	}
	// Audited like other credential/authorization changes.
	if h.Recorder != nil {
		actor := auth.GetSubject(r)
		h.Recorder.Log(actor, "update", "user_permissions", id, "permissions updated")
	}
	effective, _ := h.Store.EffectivePermissions(id, "")
	// Re-resolve role for the effective set.
	if u, err := h.Store.GetByID(id); err == nil {
		effective, _ = h.Store.EffectivePermissions(id, string(u.Role))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"effective": rbac.Sorted(effective),
	})
}

func (h *UsersHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	if !requirePermFor(r, "users:write") {
		http.Error(w, `{"error":"forbidden: admin only"}`, http.StatusForbidden)
		return
	}
	var body struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		Role        string `json:"role"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	body.Username = strings.ToLower(strings.TrimSpace(body.Username))
	if body.Username == "" || body.Password == "" {
		http.Error(w, `{"error":"username and password required"}`, http.StatusBadRequest)
		return
	}
	if !user.IsValidRole(body.Role) {
		http.Error(w, `{"error":"valid role required (admin|support|member|readonly)"}`, http.StatusBadRequest)
		return
	}
	u, err := h.Store.Create(body.Username, body.Password, body.Role, body.DisplayName)
	if err != nil {
		// Store create errors are policy messages (validation/uniqueness).
		httperr.Invalid(w, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(u)
	emitWebhook("user.created", map[string]any{
		"username": u.Username,
		"actor":    auth.GetSubject(r),
	})
}

func (h *UsersHandler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	if !requirePermFor(r, "users:write") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	id := chi.URLParam(r, "id")
	var body struct {
		Role        *string `json:"role"`
		DisplayName *string `json:"display_name"`
		Password    *string `json:"password"`
		Disabled    *bool   `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	// Last-admin guard: demoting or disabling the sole active admin would
	// lock every operator out of user management (DeleteUser already guards
	// this — UpdateUser previously did not).
	if (body.Role != nil && *body.Role != "admin") || (body.Disabled != nil && *body.Disabled) {
		if target, err := h.Store.GetByID(id); err == nil && target.Role == "admin" && !target.Disabled {
			users, _ := h.Store.List()
			adminCount := 0
			for _, u := range users {
				if u.Role == "admin" && !u.Disabled {
					adminCount++
				}
			}
			if adminCount <= 1 {
				http.Error(w, `{"error":"cannot demote or disable the last admin"}`, http.StatusBadRequest)
				return
			}
		}
	}
	if body.Role != nil {
		if !user.IsValidRole(*body.Role) {
			http.Error(w, `{"error":"invalid role"}`, http.StatusBadRequest)
			return
		}
		if err := h.Store.UpdateRole(id, user.Role(*body.Role)); err != nil {
			http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
			return
		}
	}
	if body.Password != nil && *body.Password != "" {
		if err := h.Store.UpdatePassword(id, *body.Password); err != nil {
			http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
			return
		}
	}
	// For display_name and disabled, direct DB update — disabled transitions
	// go through the store so outstanding sessions are revoked.
	if body.DisplayName != nil {
		if err := h.Store.UpdateDisplayName(id, *body.DisplayName); err != nil {
			http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
			return
		}
	}
	if body.Disabled != nil {
		if err := h.Store.SetDisabled(id, *body.Disabled); err != nil {
			http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
			return
		}
	}
	u, err := h.Store.GetByID(id)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(u)
	if body.Disabled != nil && *body.Disabled {
		emitWebhook("user.disabled", map[string]any{
			"username": u.Username,
			"actor":    auth.GetSubject(r),
		})
	} else {
		emitWebhook("user.updated", map[string]any{
			"username": u.Username,
			"actor":    auth.GetSubject(r),
		})
	}
}

func (h *UsersHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	if !requirePermFor(r, "users:write") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	id := chi.URLParam(r, "id")
	// Prevent deleting self and last admin
	sub := auth.GetSubject(r)
	if u, _, err := h.Store.GetByUsername(sub); err == nil && u.ID == id {
		http.Error(w, `{"error":"cannot delete self"}`, http.StatusBadRequest)
		return
	}
	// Check if it's last admin
	users, _ := h.Store.List()
	adminCount := 0
	for _, u := range users {
		if u.Role == "admin" && !u.Disabled {
			adminCount++
		}
	}
	if target, err := h.Store.GetByID(id); err == nil && target.Role == "admin" && adminCount <= 1 {
		http.Error(w, `{"error":"cannot delete last admin"}`, http.StatusBadRequest)
		return
	}
	if err := h.Store.Delete(id); err != nil {
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *UsersHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	if !requirePermFor(r, "users:write") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	id := chi.URLParam(r, "id")
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == "" {
		http.Error(w, `{"error":"password required"}`, http.StatusBadRequest)
		return
	}
	if err := h.Store.UpdatePassword(id, body.Password); err != nil {
		http.Error(w, `{"error":"failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
