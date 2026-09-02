package handler

// Tests for the fine-grained RBAC surface: permission resolution helpers,
// the permissions GET/PUT endpoints, and keys:read_own ownership scoping.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
	"ai-gateway/internal/rbac"
	"ai-gateway/internal/user"
	"time"

	"github.com/go-chi/chi/v5"
)

// requestAs builds a handler-level request carrying role/subject context
// (and optionally a cached permission set, as RequirePerm injects).
func requestAs(role, username string, perms map[string]bool) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := auth.WithRole(r.Context(), role)
	ctx = auth.WithSubject(ctx, username)
	if perms != nil {
		ctx = auth.WithPerms(ctx, perms)
	}
	return r.WithContext(ctx)
}

func requestWithURL(base *http.Request, path string) *http.Request {
	r := httptest.NewRequest(base.Method, path, nil)
	r.Header = base.Header
	return r.WithContext(base.Context())
}

func putWithURL(base *http.Request, path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r.WithContext(base.Context())
}

func TestRequirePermForResolution(t *testing.T) {
	// Admin bypass.
	if !requirePermFor(requestAs("admin", "admin", nil), rbac.PermUsersWrite) {
		t.Fatal("admin must pass any requirePermFor")
	}
	// Default member: users:read admin-only (legacy behavior), users:write no.
	req := requestAs("member", "m1", nil)
	if requirePermFor(req, rbac.PermUsersRead) {
		t.Error("member must NOT have users:read by default (legacy admin-only route)")
	}
	if requirePermFor(req, rbac.PermUsersWrite) {
		t.Error("member must not have users:write by default")
	}
	// Cached override set honored (as RequirePerm injects).
	if !requirePermFor(requestAs("member", "m1", map[string]bool{rbac.PermUsersWrite: true}), rbac.PermUsersWrite) {
		t.Error("granted override must pass")
	}
	if requirePermFor(requestAs("member", "m1", map[string]bool{rbac.PermUsersRead: false}), rbac.PermUsersRead) {
		t.Error("revoked override must deny")
	}
	// Unknown role: no defaults, fails closed.
	if requirePermFor(requestAs("weird", "x", nil), rbac.PermUsersRead) {
		t.Error("unknown role must fail closed")
	}
}

type permEnv struct {
	users  *UsersHandler
	admin  *AdminHandler
	store  *user.Store
	keys   *apikey.Store
	closer func()
}

func newPermEnv(t *testing.T) *permEnv {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := user.NewStore(database)
	ks := apikey.NewStore(database)
	return &permEnv{
		users:  &UsersHandler{Store: store, DB: database},
		admin:  &AdminHandler{DB: database, APIKeyStore: ks, UserStore: store},
		store:  store,
		keys:   ks,
		closer: func() { database.Close() },
	}
}

func createTargetUser(t *testing.T, env *permEnv) string {
	t.Helper()
	body := `{"username":"target","password":"password123","role":"member"}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(auth.WithRole(r.Context(), "admin"))
	r = r.WithContext(auth.WithSubject(r.Context(), "admin"))
	w := httptest.NewRecorder()
	env.users.CreateUser(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create user: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.ID == "" {
		t.Fatalf("no id in create response: %s", w.Body.String())
	}
	return created.ID
}

func TestPermissionsEndpointsRoundTrip(t *testing.T) {
	env := newPermEnv(t)
	defer env.closer()
	target := createTargetUser(t, env)

	r := chi.NewRouter()
	r.Get("/api/admin/users/{id}/permissions", env.users.GetUserPermissions)
	r.Put("/api/admin/users/{id}/permissions", env.users.PutUserPermissions)

	// GET: catalog + defaults, no overrides.
	req := requestWithURL(requestAs("admin", "admin", nil), "/api/admin/users/"+target+"/permissions")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("get permissions: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Effective []string        `json:"effective"`
		Overrides map[string]bool `json:"overrides"`
		Catalog   []string        `json:"catalog"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Catalog) != len(rbac.All) {
		t.Fatalf("catalog size = %d, want %d", len(out.Catalog), len(rbac.All))
	}
	if len(out.Overrides) != 0 {
		t.Fatalf("fresh user should have no overrides: %v", out.Overrides)
	}

	// PUT: grant audit:read (admin-only by default), revoke keys:delete.
	body := `{"` + rbac.PermAuditRead + `": true, "` + rbac.PermKeysDelete + `": false}`
	w = httptest.NewRecorder()
	r.ServeHTTP(w, putWithURL(requestAs("admin", "admin", nil), "/api/admin/users/"+target+"/permissions", body))
	if w.Code != 200 {
		t.Fatalf("put permissions: %d %s", w.Code, w.Body.String())
	}

	u, _ := env.store.GetByID(target)
	effective, err := env.store.EffectivePermissions(u.ID, "member")
	if err != nil {
		t.Fatal(err)
	}
	if !rbac.Has(effective, rbac.PermAuditRead) {
		t.Error("granted audit:read missing from effective set")
	}
	if rbac.Has(effective, rbac.PermKeysDelete) {
		t.Error("revoked keys:delete still present")
	}
	if !rbac.Has(effective, rbac.PermKeysRead) {
		t.Error("untouched keys:read should remain (role default)")
	}

	// GET now reports the overrides.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, requestWithURL(requestAs("admin", "admin", nil), "/api/admin/users/"+target+"/permissions"))
	out.Overrides = nil
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Overrides) != 2 || !out.Overrides[rbac.PermAuditRead] || out.Overrides[rbac.PermKeysDelete] {
		t.Fatalf("overrides = %v, want audit:read=true keys:delete=false", out.Overrides)
	}

	// Unknown permission → 400.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, putWithURL(requestAs("admin", "admin", nil), "/api/admin/users/"+target+"/permissions", `{"bogus:perm": true}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown permission: %d, want 400", w.Code)
	}

	// null clears the override (role default applies again).
	w = httptest.NewRecorder()
	r.ServeHTTP(w, putWithURL(requestAs("admin", "admin", nil), "/api/admin/users/"+target+"/permissions", `{"`+rbac.PermAuditRead+`": null}`))
	if w.Code != 200 {
		t.Fatalf("null clear: %d %s", w.Code, w.Body.String())
	}
	effective, _ = env.store.EffectivePermissions(u.ID, "member")
	if rbac.Has(effective, rbac.PermAuditRead) {
		t.Error("cleared override should fall back to role default (deny)")
	}

	// users:write guard: a member cannot edit others' permissions.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, putWithURL(requestAs("member", "m1", nil), "/api/admin/users/"+target+"/permissions", `{"`+rbac.PermAuditRead+`": true}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("member put permissions: %d, want 403", w.Code)
	}
}

func TestOwnKeysScoping(t *testing.T) {
	env := newPermEnv(t)
	defer env.closer()

	ownerA, _ := env.store.Create("owner-a", "password123", "member", "A")
	ownerB, _ := env.store.Create("owner-b", "password123", "member", "B")

	// A creates two keys; B one; plus one legacy unowned key.
	_, _ = env.keys.CreateWithOrg("a1", "", ownerA.ID)
	k2, _ := env.keys.CreateWithOrg("a2", "", ownerA.ID)
	_, _ = env.keys.CreateWithOrg("b1", "", ownerB.ID)
	_, _ = env.keys.Create("legacy")

	// read_own-only caller A sees exactly their two keys.
	w := httptest.NewRecorder()
	env.admin.ListKeys(w, requestAs("member", "owner-a", map[string]bool{rbac.PermKeysReadOwn: true}))
	if w.Code != 200 {
		t.Fatalf("own-only list keys: %d %s", w.Code, w.Body.String())
	}
	var keys []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &keys)
	if len(keys) != 2 {
		t.Fatalf("own-only view has %d keys, want 2 (A's own): %v", len(keys), keys)
	}
	for _, k := range keys {
		name, _ := k["name"].(string)
		if name == "legacy" || name == "b1" {
			t.Errorf("own-only view leaked key %q", name)
		}
	}

	// keys:read caller (support default) sees everything including legacy.
	w = httptest.NewRecorder()
	env.admin.ListKeys(w, requestAs("support", "sup1", nil))
	if w.Code != 200 {
		t.Fatalf("support list keys: %d", w.Code)
	}
	keys = nil
	_ = json.Unmarshal(w.Body.Bytes(), &keys)
	if len(keys) != 4 {
		t.Fatalf("full view has %d keys, want 4", len(keys))
	}

	// Neither read perm → the route middleware (RequireAnyPerm) denies with
	// 403 before the handler runs. At the handler level (defense in depth),
	// an explicit empty permission set yields an EMPTY list, never a leak.
	w = httptest.NewRecorder()
	env.admin.ListKeys(w, requestAs("member", "nokeys", map[string]bool{}))
	if w.Code != 200 {
		t.Fatalf("empty-set list keys: %d", w.Code)
	}
	keys = nil
	_ = json.Unmarshal(w.Body.Bytes(), &keys)
	if len(keys) != 0 {
		t.Fatalf("no-perm handler leak: %v, want []", keys)
	}

	// created_by stamped and readable.
	k, err := env.keys.GetByID(k2.ID)
	if err != nil || k.CreatedBy == nil || *k.CreatedBy != ownerA.ID {
		t.Fatalf("created_by not stamped: %+v err=%v", k, err)
	}

	// Ownership scoping also applies to KeyAnalytics (can't read others' key
	// analytics by guessing ids when own-only).
	w = httptest.NewRecorder()
	r := chi.NewRouter()
	r.Get("/api/keys/{id}/analytics", env.admin.KeyAnalytics)
	r.ServeHTTP(w, requestWithURL(requestAs("member", "owner-b", map[string]bool{rbac.PermKeysReadOwn: true}), "/api/keys/"+k2.ID+"/analytics"))
	if w.Code != http.StatusNotFound && w.Code != http.StatusForbidden {
		t.Fatalf("own-only analytics on A's key: %d, want 404/403", w.Code)
	}
}

func TestSetKeyOwner(t *testing.T) {
	env := newPermEnv(t)
	defer env.closer()

	owner, _ := env.store.Create("keyowner", "password123", "member", "KO")
	other, _ := env.store.Create("otheruser", "password123", "member", "OU")
	k, _ := env.keys.CreateWithOrg("k1", "", owner.ID)

	r := chi.NewRouter()
	r.Put("/api/keys/{id}/owner", env.admin.SetKeyOwner)

	admin := requestAs("admin", "admin", nil)

	// Reassign to another user.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, putWithURL(admin, "/api/keys/"+k.ID+"/owner", `{"user_id":"`+other.ID+`"}`))
	if w.Code != 200 {
		t.Fatalf("reassign: %d %s", w.Code, w.Body.String())
	}
	got, _ := env.keys.GetByID(k.ID)
	if got.CreatedBy == nil || *got.CreatedBy != other.ID {
		t.Fatalf("created_by = %v, want %s", got.CreatedBy, other.ID)
	}

	// Clear ownership (null).
	w = httptest.NewRecorder()
	r.ServeHTTP(w, putWithURL(admin, "/api/keys/"+k.ID+"/owner", `{"user_id": null}`))
	if w.Code != 200 {
		t.Fatalf("clear owner: %d", w.Code)
	}
	got, _ = env.keys.GetByID(k.ID)
	if got.CreatedBy != nil {
		t.Fatalf("created_by = %v, want nil", got.CreatedBy)
	}

	// Unknown user → 404.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, putWithURL(admin, "/api/keys/"+k.ID+"/owner", `{"user_id":"no-such-user"}`))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown owner: %d, want 404", w.Code)
	}

	// CountByOwner reflects assignments.
	env.keys.SetKeyOwner(k.ID, owner.ID)
	counts, _ := env.keys.CountByOwner()
	if counts[owner.ID] != 1 {
		t.Fatalf("CountByOwner[%s] = %d, want 1", owner.ID, counts[owner.ID])
	}
}

func TestLogsOwnershipScoping(t *testing.T) {
	env := newPermEnv(t)
	defer env.closer()

	owner, _ := env.store.Create("logowner", "password123", "readonly", "LO")
	_, _ = env.store.Create("logother", "password123", "readonly", "LO2")

	// Two keys: one owned, one unowned; traffic attributed to each via key_id.
	kOwned, _ := env.keys.CreateWithOrg("owned-key", "", owner.ID)
	kOther, _ := env.keys.Create("unowned-key")
	seedLogRows(t, env.admin, []logSeed{
		{model: "m1", endpoint: "chat", status: 200, latency: 100, tokens: 10, keyPrefix: kOwned.Prefix, keyID: kOwned.ID, providerID: "p1", at: time.Now().UTC().Add(-time.Hour)},
		{model: "m2", endpoint: "chat", status: 200, latency: 100, tokens: 10, keyPrefix: kOther.Prefix, keyID: kOther.ID, providerID: "p1", at: time.Now().UTC().Add(-2 * time.Hour)},
	})

	r := chi.NewRouter()
	r.Get("/api/logs", env.admin.Logs)
	r.Get("/api/stats", env.admin.Stats)
	r.Get("/api/logs/export", env.admin.LogsExport)

	// keys:read_own-only caller: sees ONLY their key's rows.
	req := requestAs("readonly", "logowner", map[string]bool{rbac.PermKeysReadOwn: true, rbac.PermLogsRead: false})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, requestWithURL(req, "/api/logs"))
	if w.Code != 200 {
		t.Fatalf("scoped logs: %d", w.Code)
	}
	var rows []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &rows)
	if len(rows) != 1 {
		t.Fatalf("own-scoped logs has %d rows, want 1", len(rows))
	}
	if rows[0]["model"] != "m1" {
		t.Fatalf("scoped row = %v, want m1 (owned key's traffic)", rows[0]["model"])
	}

	// Stats aggregates narrowed: requests badge = 1, not 2.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, requestWithURL(req, "/api/stats?range=24h"))
	if w.Code != 200 {
		t.Fatalf("scoped stats: %d", w.Code)
	}
	var stats map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &stats)
	if got, _ := stats["requests"].(float64); got != 1 {
		t.Fatalf("scoped stats requests = %v, want 1", stats["requests"])
	}

	// CSV export narrowed: header + 1 row.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, requestWithURL(req, "/api/logs/export"))
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("scoped export lines=%d, want 2", len(lines))
	}
	if !strings.Contains(lines[1], "m1") {
		t.Fatalf("scoped export row = %q", lines[1])
	}

	// A different read_own caller sees none of owner's rows.
	req = requestAs("readonly", "logother", map[string]bool{rbac.PermKeysReadOwn: true})
	w = httptest.NewRecorder()
	r.ServeHTTP(w, requestWithURL(req, "/api/logs"))
	rows = nil
	_ = json.Unmarshal(w.Body.Bytes(), &rows)
	if len(rows) != 0 {
		t.Fatalf("other read_own caller sees %d rows, want 0", len(rows))
	}

	// logs:read holder (support default) sees everything.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, requestWithURL(requestAs("support", "sup", nil), "/api/logs"))
	rows = nil
	_ = json.Unmarshal(w.Body.Bytes(), &rows)
	if len(rows) != 2 {
		t.Fatalf("support sees %d rows, want 2", len(rows))
	}
}

func TestGetLogBodyPermission(t *testing.T) {
	env := newPermEnv(t)
	defer env.closer()

	// Seed one row with bodies.
	seedLogRows(t, env.admin, []logSeed{
		{model: "m", endpoint: "chat", status: 200, latency: 100, tokens: 10, providerID: "p1", at: time.Now().UTC()},
	})
	var rowID string
	_ = env.admin.DB.QueryRow(`SELECT id FROM request_logs LIMIT 1`).Scan(&rowID)

	r := chi.NewRouter()
	r.Get("/api/logs/{id}", env.admin.GetLog)

	// logs:read WITHOUT logs:read_bodies → metadata ok, bodies stripped.
	req := requestAs("readonly", "viewer", map[string]bool{rbac.PermLogsRead: true})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, requestWithURL(req, "/api/logs/"+rowID))
	if w.Code != 200 {
		t.Fatalf("metadata-only GetLog: %d %s", w.Code, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if rb := out["request_body"]; rb != nil && rb != "" {
		t.Fatalf("request_body leaked without logs:read_bodies: %v", rb)
	}
	if _, ok := out["log"]; !ok {
		// metadata rides under "log" (envelope) or top-level fields; the
		// critical assertion is that bodies are stripped, which is above.
		t.Log("no log envelope in response — checking fields directly")
	}

	// logs:read_bodies granted → bodies present.
	req = requestAs("readonly", "viewer", map[string]bool{rbac.PermLogsRead: true, rbac.PermLogsReadBodies: true})
	w = httptest.NewRecorder()
	r.ServeHTTP(w, requestWithURL(req, "/api/logs/"+rowID))
	out = nil
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if rb, _ := out["request_body"].(string); !strings.Contains(rb, "findme-needle") {
		t.Fatalf("request_body missing with logs:read_bodies: %v", out["request_body"])
	}

	// Neither perm → 403.
	req = requestAs("readonly", "viewer", map[string]bool{rbac.PermKeysReadOwn: true})
	w = httptest.NewRecorder()
	r.ServeHTTP(w, requestWithURL(req, "/api/logs/"+rowID))
	if w.Code != http.StatusForbidden {
		t.Fatalf("no-perm GetLog: %d, want 403", w.Code)
	}
}
