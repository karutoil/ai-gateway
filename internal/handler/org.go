package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"ai-gateway/internal/audit"
	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/rbac"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// OrgHandler provides Phase 2.5 pre-enterprise scaffolding.
// All routes are admin-only (behind auth.AdminMiddleware). RBAC enforced via middleware.RequireRole.
type OrgHandler struct {
	DB       *sql.DB
	Recorder audit.Recorder
}

func (h *OrgHandler) ensureTables() {
	if h.DB == nil {
		return
	}
	db.ExecSchemaIdempotent(h.DB, `CREATE TABLE IF NOT EXISTS organizations(id TEXT PRIMARY KEY, name TEXT UNIQUE NOT NULL, created_at DATETIME NOT NULL)`)
	db.ExecSchemaIdempotent(h.DB, `CREATE TABLE IF NOT EXISTS memberships(id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE, user_id TEXT NOT NULL, role TEXT NOT NULL, created_at DATETIME NOT NULL)`)
	db.ExecSchemaIdempotent(h.DB, `CREATE INDEX IF NOT EXISTS idx_memberships_org ON memberships(org_id)`)
	db.ExecAlterIdempotent(h.DB, `ALTER TABLE providers ADD COLUMN org_id TEXT REFERENCES organizations(id)`)
	db.ExecAlterIdempotent(h.DB, `ALTER TABLE gateway_keys ADD COLUMN org_id TEXT REFERENCES organizations(id)`)
	db.ExecSchemaIdempotent(h.DB, `CREATE INDEX IF NOT EXISTS idx_providers_org ON providers(org_id)`)
	db.ExecSchemaIdempotent(h.DB, `CREATE INDEX IF NOT EXISTS idx_gateway_keys_org ON gateway_keys(org_id)`)
}

func (h *OrgHandler) Routes(r chi.Router) {
	// Reads are gated by orgs:read — the write routes below already require
	// orgs:write, but the two GETs previously had NO permission check, so any
	// authenticated user (and any orgs:read revoke, which was silently
	// ignored) could enumerate every org and membership (user ids + roles).
	r.With(middleware.RequirePerm(rbac.PermOrgsRead)).Get("/orgs", h.ListOrgs)
	r.With(middleware.RequirePerm(rbac.PermOrgsWrite)).Post("/orgs", h.CreateOrg)
	r.With(middleware.RequirePerm(rbac.PermOrgsWrite)).Delete("/orgs/{id}", h.DeleteOrg)
	r.With(middleware.RequirePerm(rbac.PermOrgsWrite)).Post("/orgs/{id}/members", h.AddMember)
	r.With(middleware.RequirePerm(rbac.PermOrgsRead)).Get("/orgs/{id}/members", h.ListMembers)
}

func (h *OrgHandler) CreateOrg(w http.ResponseWriter, r *http.Request) {
	h.ensureTables()
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, `{"error":{"message":"name required","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}
	if len(body.Name) > 64 {
		http.Error(w, `{"error":{"message":"name too long","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	if _, err := h.DB.Exec(db.Q(`INSERT INTO organizations(id,name,created_at) VALUES(?,?,?)`), id, body.Name, now); err != nil {
		http.Error(w, `{"error":{"message":"create failed","type":"proxy_error"}}`, http.StatusBadRequest)
		return
	}
	if h.Recorder != nil {
		// Record the ACTING subject, not a hardcoded "admin" — the audit
		// trail previously attributed every org write to "admin" regardless
		// of who performed it.
		_ = h.Recorder.Log(auth.GetSubject(r), "create", "organization", id, body.Name)
	}
	org := models.Organization{ID: id, Name: body.Name, CreatedAt: now}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(org)
	emitWebhook("org.created", map[string]any{
		"name":  body.Name,
		"actor": auth.GetSubject(r),
	})
}

// orgVisibleTo reports whether the caller may see the given organization.
// Global admins see everything; an org-scoped caller sees only its own org;
// an unscoped non-admin sees only while the deployment has no orgs at all
// (single-tenant install). Without this, orgs:read — a default for member,
// support and readonly — exposed the full tenant list.
func (h *OrgHandler) orgVisibleTo(r *http.Request, orgID string) bool {
	if auth.GetRole(r) == rbac.RoleAdmin {
		return true
	}
	callerOrg := auth.GetOrgID(r)
	if callerOrg != "" {
		return callerOrg == orgID
	}
	var cnt int
	if err := h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM organizations`)).Scan(&cnt); err != nil {
		return true // table missing → no orgs → single-tenant
	}
	return cnt == 0
}

func (h *OrgHandler) ListOrgs(w http.ResponseWriter, r *http.Request) {
	h.ensureTables()
	// Read every row BEFORE applying the visibility filter: orgVisibleTo issues
	// its own query, and nesting a query inside an open rows iteration on the
	// single-connection SQLite pool deadlocks (the pool has no second
	// connection to hand out).
	rows, err := h.DB.Query(`SELECT id, name, created_at FROM organizations ORDER BY created_at DESC`)
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "query failed", httperr.TypeProxy)
		return
	}
	var all []models.Organization
	for rows.Next() {
		var o models.Organization
		if err := rows.Scan(&o.ID, &o.Name, &o.CreatedAt); err == nil {
			all = append(all, o)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "query failed", httperr.TypeProxy)
		return
	}
	out := make([]models.Organization, 0, len(all))
	for _, o := range all {
		if h.orgVisibleTo(r, o.ID) {
			out = append(out, o)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *OrgHandler) DeleteOrg(w http.ResponseWriter, r *http.Request) {
	h.ensureTables()
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, `{"error":{"message":"id required","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}
	// Atomic multi-table delete: clearing references and removing the org in
	// one transaction prevents partial states (e.g. memberships gone but org
	// alive) if a statement fails midway.
	tx, err := h.DB.Begin()
	if err != nil {
		http.Error(w, `{"error":{"message":"transaction failed","type":"proxy_error"}}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	// SECURITY: the org's KEYS are REVOKED, not promoted to global. NULL org_id
	// is this codebase's encoding for "global/shared", so the previous
	// `UPDATE gateway_keys SET org_id=NULL` did not decommission anything — it
	// turned every key of the deleted org into a shared key that still
	// authenticated, appeared in global dashboards and could be rotated by any
	// admin. Deleting a tenant must end its access, not widen it.
	if _, err := tx.Exec(db.Q(`UPDATE gateway_keys SET revoked_at=? WHERE org_id=? AND revoked_at IS NULL`), time.Now().UTC(), id); err != nil {
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	// Providers are DISABLED (health_status=disabled + credential kept) rather
	// than re-scoped: leaving them global would let any tenant route traffic
	// through the deleted tenant's credentials.
	if _, err := tx.Exec(db.Q(`UPDATE providers SET health_status='disabled' WHERE org_id=?`), id); err != nil {
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	// Membership rows are the only place the org claim comes from; bump every
	// affected user's token_version so the org_id already baked into their live
	// JWTs stops being honoured for the rest of its 24h TTL.
	if _, err := tx.Exec(db.Q(`UPDATE dashboard_users SET token_version = COALESCE(token_version,0)+1 WHERE id IN (SELECT user_id FROM memberships WHERE org_id=?)`), id); err != nil {
		log.Warn().Err(err).Str("org", id).Msg("delete org: could not revoke member sessions")
	}
	if _, err := tx.Exec(db.Q(`DELETE FROM memberships WHERE org_id=?`), id); err != nil {
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(db.Q(`DELETE FROM organizations WHERE id=?`), id); err != nil {
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	if h.Recorder != nil {
		_ = h.Recorder.Log(auth.GetSubject(r), "delete", "organization", id, r.URL.Path)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *OrgHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	h.ensureTables()
	orgID := chi.URLParam(r, "id")
	if orgID == "" {
		http.Error(w, `{"error":{"message":"org id required","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}
	var exists int
	if err := h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM organizations WHERE id=?`), orgID).Scan(&exists); err != nil || exists == 0 {
		http.Error(w, `{"error":{"message":"organization not found","type":"not_found_error"}}`, http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserID == "" {
		http.Error(w, `{"error":{"message":"user_id required","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}
	if body.Role == "" {
		body.Role = "member"
	}
	if body.Role != "admin" && body.Role != "member" && body.Role != "support" && body.Role != "readonly" {
		http.Error(w, `{"error":{"message":"role must be admin|member|support|readonly","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}
	// Validate the target user exists (prevents ghost memberships that can
	// never resolve to a JWT org claim) and reject duplicates.
	var ucnt int
	if err := h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM dashboard_users WHERE id=?`), body.UserID).Scan(&ucnt); err != nil || ucnt == 0 {
		http.Error(w, `{"error":{"message":"user not found","type":"not_found_error"}}`, http.StatusNotFound)
		return
	}
	var dup int
	if err := h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM memberships WHERE org_id=? AND user_id=?`), orgID, body.UserID).Scan(&dup); err == nil && dup > 0 {
		http.Error(w, `{"error":{"message":"user is already a member of this organization","type":"invalid_request_error"}}`, http.StatusConflict)
		return
	}
	if h.Recorder != nil {
		_ = h.Recorder.Log(auth.GetSubject(r), "add_member", "membership", orgID, body.UserID+":"+body.Role)
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	if _, err := h.DB.Exec(db.Q(`INSERT INTO memberships(id,org_id,user_id,role,created_at) VALUES(?,?,?,?,?)`), id, orgID, body.UserID, body.Role, now); err != nil {
		http.Error(w, `{"error":"insert failed"}`, http.StatusInternalServerError)
		return
	}
	// The org claim is minted into the JWT at login, so a user added to an org
	// saw nothing in it until they logged out and back in ("the dashboard is
	// broken"). Bumping token_version forces a re-mint; the middleware already
	// treats a stale version as "log in again".
	if _, err := h.DB.Exec(db.Q(`UPDATE dashboard_users SET token_version = COALESCE(token_version,0)+1 WHERE id=?`), body.UserID); err != nil {
		log.Warn().Err(err).Str("user", body.UserID).Msg("add member: could not force session refresh")
	}
	m := models.Membership{ID: id, OrgID: orgID, UserID: body.UserID, Role: body.Role, CreatedAt: now}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(m)
}

func (h *OrgHandler) ListMembers(w http.ResponseWriter, r *http.Request) {
	h.ensureTables()
	orgID := chi.URLParam(r, "id")
	// Same tenancy rule as ListOrgs: a membership row exposes user ids and
	// roles, so an out-of-scope org id must answer 404 rather than list.
	if !h.orgVisibleTo(r, orgID) {
		httperr.NotFound(w, "organization not found")
		return
	}
	rows, err := h.DB.Query(db.Q(`SELECT id, org_id, user_id, role, created_at FROM memberships WHERE org_id=? ORDER BY created_at DESC`), orgID)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var out []models.Membership
	for rows.Next() {
		var m models.Membership
		if err := rows.Scan(&m.ID, &m.OrgID, &m.UserID, &m.Role, &m.CreatedAt); err == nil {
			out = append(out, m)
		}
	}
	if out == nil {
		out = []models.Membership{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
