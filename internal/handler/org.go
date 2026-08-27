package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"ai-gateway/internal/audit"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
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
	_, _ = h.DB.Exec(`CREATE TABLE IF NOT EXISTS organizations(id TEXT PRIMARY KEY, name TEXT UNIQUE NOT NULL, created_at DATETIME NOT NULL)`)
	_, _ = h.DB.Exec(`CREATE TABLE IF NOT EXISTS memberships(id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE, user_id TEXT NOT NULL, role TEXT NOT NULL, created_at DATETIME NOT NULL)`)
	_, _ = h.DB.Exec(`CREATE INDEX IF NOT EXISTS idx_memberships_org ON memberships(org_id)`)
	_, _ = h.DB.Exec(`ALTER TABLE providers ADD COLUMN org_id TEXT REFERENCES organizations(id)`)
	_, _ = h.DB.Exec(`ALTER TABLE gateway_keys ADD COLUMN org_id TEXT REFERENCES organizations(id)`)
	_, _ = h.DB.Exec(`CREATE INDEX IF NOT EXISTS idx_providers_org ON providers(org_id)`)
	_, _ = h.DB.Exec(`CREATE INDEX IF NOT EXISTS idx_gateway_keys_org ON gateway_keys(org_id)`)
}

func (h *OrgHandler) Routes(r chi.Router) {
	r.Get("/orgs", h.ListOrgs)
	r.With(middleware.RequireRole("admin")).Post("/orgs", h.CreateOrg)
	r.With(middleware.RequireRole("admin")).Delete("/orgs/{id}", h.DeleteOrg)
	r.With(middleware.RequireRole("admin")).Post("/orgs/{id}/members", h.AddMember)
	r.Get("/orgs/{id}/members", h.ListMembers)
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
		http.Error(w, `{"error":{"message":"create failed: `+err.Error()+`","type":"proxy_error"}}`, http.StatusBadRequest)
		return
	}
	if h.Recorder != nil {
		_ = h.Recorder.Log("admin", "create", "organization", id, body.Name)
	}
	org := models.Organization{ID: id, Name: body.Name, CreatedAt: now}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(org)
}

func (h *OrgHandler) ListOrgs(w http.ResponseWriter, r *http.Request) {
	h.ensureTables()
	rows, err := h.DB.Query(`SELECT id, name, created_at FROM organizations ORDER BY created_at DESC`)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var out []models.Organization
	for rows.Next() {
		var o models.Organization
		if err := rows.Scan(&o.ID, &o.Name, &o.CreatedAt); err == nil {
			out = append(out, o)
		}
	}
	if out == nil {
		out = []models.Organization{}
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
	// Clear org references first to avoid FK constraint (providers/keys/memberships)
	// Use UPDATE to NULL so global providers remain but are no longer scoped
	if _, err := tx.Exec(db.Q(`UPDATE providers SET org_id=NULL WHERE org_id=?`), id); err != nil {
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(db.Q(`UPDATE gateway_keys SET org_id=NULL WHERE org_id=?`), id); err != nil {
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
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
		_ = h.Recorder.Log("admin", "delete", "organization", id, r.URL.Path)
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
		_ = h.Recorder.Log("admin", "add_member", "membership", orgID, body.UserID+":"+body.Role)
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	if _, err := h.DB.Exec(db.Q(`INSERT INTO memberships(id,org_id,user_id,role,created_at) VALUES(?,?,?,?,?)`), id, orgID, body.UserID, body.Role, now); err != nil {
		http.Error(w, `{"error":"insert failed"}`, http.StatusInternalServerError)
		return
	}
	m := models.Membership{ID: id, OrgID: orgID, UserID: body.UserID, Role: body.Role, CreatedAt: now}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(m)
}

func (h *OrgHandler) ListMembers(w http.ResponseWriter, r *http.Request) {
	h.ensureTables()
	orgID := chi.URLParam(r, "id")
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
