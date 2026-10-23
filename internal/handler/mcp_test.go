package handler

// Tests for the centralized MCP administration surface: CRUD, secret
// redaction, enable/disable, discovery, and the per-key grant editor.

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
	"ai-gateway/internal/mcp"
	"ai-gateway/internal/mcp/mcptest"
	"ai-gateway/internal/middleware"

	"github.com/go-chi/chi/v5"
)

func newMCPHandler(t *testing.T) (*MCPHandler, *mcp.Store, *apikey.Store) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 11)
	}
	store := mcp.NewStore(database, master)
	manager := mcp.NewManager(store, 15*time.Second)
	t.Cleanup(manager.Close)
	keys := apikey.NewStore(database)
	return &MCPHandler{Store: store, Manager: manager}, store, keys
}

func mcpRequest(method, path, body string, admin bool) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	role := "member"
	if admin {
		role = "admin"
	}
	// Apply the role context to THIS request: rebuilding it would drop the body.
	ctx := auth.WithRole(r.Context(), role)
	ctx = auth.WithSubject(ctx, role+"-user")
	return r.WithContext(ctx)
}

func chiURL(r *http.Request, id string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

func TestMCPCreateListAndSecretRedaction(t *testing.T) {
	h, store, _ := newMCPHandler(t)
	up := mcptest.Start(t)

	body, _ := json.Marshal(map[string]any{
		"name":       "Secret Server",
		"transport":  "http",
		"url":        up.Server.URL + "/mcp",
		"auth_type":  "bearer",
		"auth_value": "super-secret-token",
		"headers":    map[string]string{"X-Org": "acme"},
	})
	rec := httptest.NewRecorder()
	h.CreateServer(rec, mcpRequest(http.MethodPost, "/api/mcp/servers", string(body), true))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var created mcp.Server
	decodeJSON(t, rec, &created)
	if created.Slug == "" {
		t.Fatal("slug was not derived")
	}
	if !created.HasAuth || !created.HasHeaders {
		t.Fatalf("secret indicators missing: %+v", created)
	}
	if strings.Contains(rec.Body.String(), "super-secret-token") {
		t.Fatal("the credential was returned to the dashboard")
	}
	if strings.Contains(rec.Body.String(), "acme") {
		t.Fatal("static header values were returned to the dashboard")
	}

	// The secret must be recoverable by the runtime path only.
	runtime, err := store.RuntimeServer(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.AuthValue != "super-secret-token" {
		t.Fatalf("auth value = %q", runtime.AuthValue)
	}
	if runtime.Headers["X-Org"] != "acme" {
		t.Fatalf("headers = %v", runtime.Headers)
	}

	// Discovery runs in the background; wait for the observable result.
	if !waitForCond(15*time.Second, func() bool {
		srv, err := store.GetServer(created.ID)
		return err == nil && srv.ToolCount == 3
	}) {
		t.Fatal("background discovery never completed")
	}

	rec = httptest.NewRecorder()
	h.ListServers(rec, mcpRequest(http.MethodGet, "/api/mcp/servers", "", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "super-secret-token") {
		t.Fatal("list leaked a credential")
	}
	var list []mcp.Server
	decodeJSON(t, rec, &list)
	if len(list) != 1 || list[0].ToolCount != 3 {
		t.Fatalf("list = %+v", list)
	}
}

func TestMCPCreateValidation(t *testing.T) {
	h, _, _ := newMCPHandler(t)
	cases := []struct{ name, body string }{
		{"missing name", `{"transport":"http","url":"https://x/mcp"}`},
		{"missing url", `{"name":"x","transport":"http"}`},
		{"bad transport", `{"name":"x","transport":"carrier-pigeon","url":"https://x/mcp"}`},
		{"stdio without command", `{"name":"x","transport":"stdio"}`},
		{"bearer without value", `{"name":"x","transport":"http","url":"https://x/mcp","auth_type":"bearer"}`},
		{"header auth without name", `{"name":"x","transport":"http","url":"https://x/mcp","auth_type":"header","auth_value":"v"}`},
		{"relative url", `{"name":"x","transport":"http","url":"/mcp"}`},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.CreateServer(rec, mcpRequest(http.MethodPost, "/api/mcp/servers", tc.body, true))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, rec.Code, rec.Body.String())
		}
	}
}

func TestMCPUpdateKeepsStoredCredentialWhenBlank(t *testing.T) {
	h, store, _ := newMCPHandler(t)
	up := mcptest.Start(t)
	created, err := store.Create(mcp.ServerInput{
		Name: "Keep", Transport: "http", URL: up.Server.URL + "/mcp",
		AuthType: "bearer", AuthValue: "original-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Blank auth_value on update must NOT wipe the credential.
	body := `{"name":"Renamed","transport":"http","url":"` + up.Server.URL + `/mcp","auth_type":"bearer"}`
	rec := httptest.NewRecorder()
	h.UpdateServer(rec, chiURL(mcpRequest(http.MethodPut, "/api/mcp/servers/x", body, true), created.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	runtime, err := store.RuntimeServer(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.AuthValue != "original-secret" {
		t.Fatalf("credential lost on update: %q", runtime.AuthValue)
	}
	if runtime.Name != "Renamed" {
		t.Fatalf("name = %q", runtime.Name)
	}
}

func TestMCPRotateCredential(t *testing.T) {
	h, store, _ := newMCPHandler(t)
	up := mcptest.Start(t)
	created, _ := store.Create(mcp.ServerInput{
		Name: "Rotate", Transport: "http", URL: up.Server.URL + "/mcp",
		AuthType: "bearer", AuthValue: "old",
	})
	body := `{"name":"Rotate","transport":"http","url":"` + up.Server.URL + `/mcp","auth_type":"bearer","auth_value":"new-secret"}`
	rec := httptest.NewRecorder()
	h.UpdateServer(rec, chiURL(mcpRequest(http.MethodPut, "/api/mcp/servers/x", body, true), created.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	runtime, _ := store.RuntimeServer(created.ID)
	if runtime.AuthValue != "new-secret" {
		t.Fatalf("credential not rotated: %q", runtime.AuthValue)
	}
}

func TestMCPEnableDisableAndDelete(t *testing.T) {
	h, store, _ := newMCPHandler(t)
	up := mcptest.Start(t)
	created, _ := store.Create(mcp.ServerInput{Name: "Toggle", Transport: "http", URL: up.Server.URL + "/mcp"})

	rec := httptest.NewRecorder()
	h.DisableServer(rec, chiURL(mcpRequest(http.MethodPost, "/x/disable", "", true), created.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d", rec.Code)
	}
	got, _ := store.GetServer(created.ID)
	if got.Enabled {
		t.Fatal("server still enabled")
	}

	rec = httptest.NewRecorder()
	h.EnableServer(rec, chiURL(mcpRequest(http.MethodPost, "/x/enable", "", true), created.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status = %d", rec.Code)
	}
	got, _ = store.GetServer(created.ID)
	if !got.Enabled {
		t.Fatal("server still disabled")
	}

	rec = httptest.NewRecorder()
	h.DeleteServer(rec, chiURL(mcpRequest(http.MethodDelete, "/x", "", true), created.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if _, err := store.GetServer(created.ID); err == nil {
		t.Fatal("server still present after delete")
	}
	// Tools are cleaned up with the server.
	tools, _ := store.ListTools(created.ID)
	if len(tools) != 0 {
		t.Fatalf("orphaned tools: %+v", tools)
	}
}

func TestMCPTestAndDiscoverEndpoints(t *testing.T) {
	h, store, _ := newMCPHandler(t)
	up := mcptest.Start(t)
	created, _ := store.Create(mcp.ServerInput{Name: "Probe", Transport: "http", URL: up.Server.URL + "/mcp"})

	rec := httptest.NewRecorder()
	h.TestServer(rec, chiURL(mcpRequest(http.MethodPost, "/x/test", "", true), created.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("test status = %d body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK        bool       `json:"ok"`
		ToolCount int        `json:"tool_count"`
		LatencyMs int64      `json:"latency_ms"`
		Tools     []mcp.Tool `json:"tools"`
	}
	decodeJSON(t, rec, &out)
	if !out.OK || out.ToolCount != 3 || len(out.Tools) != 3 {
		t.Fatalf("test result = %+v", out)
	}
	if out.Tools[0].NamespacedName == "" {
		t.Fatal("tools must be namespaced in the test response")
	}

	rec = httptest.NewRecorder()
	h.DiscoverServer(rec, chiURL(mcpRequest(http.MethodPost, "/x/discover", "", true), created.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("discover status = %d body = %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ListServerTools(rec, chiURL(mcpRequest(http.MethodGet, "/x/tools", "", false), created.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("tools status = %d", rec.Code)
	}
	var tools []mcp.Tool
	decodeJSON(t, rec, &tools)
	if len(tools) != 3 {
		t.Fatalf("tools = %d", len(tools))
	}
	for _, tool := range tools {
		if len(tool.InputSchema) == 0 {
			t.Errorf("tool %s has no schema", tool.Name)
		}
	}
}

func TestMCPTestEndpointReportsUpstreamFailure(t *testing.T) {
	h, store, _ := newMCPHandler(t)
	created, _ := store.Create(mcp.ServerInput{Name: "Dead", Transport: "http", URL: "http://127.0.0.1:1/mcp"})
	rec := httptest.NewRecorder()
	h.TestServer(rec, chiURL(mcpRequest(http.MethodPost, "/x/test", "", true), created.ID))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	decodeJSON(t, rec, &out)
	if out.OK || out.Error == "" {
		t.Fatalf("unexpected: %+v", out)
	}
	got, _ := store.GetServer(created.ID)
	if got.HealthStatus != "down" {
		t.Fatalf("health = %q", got.HealthStatus)
	}
}

func TestMCPKeyPermissionEditor(t *testing.T) {
	h, store, keys := newMCPHandler(t)
	up := mcptest.Start(t)
	created, _ := store.Create(mcp.ServerInput{Name: "Granted", Transport: "http", URL: up.Server.URL + "/mcp"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.Manager.Discover(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	key, err := keys.Create("key-with-mcp")
	if err != nil {
		t.Fatal(err)
	}

	// GET renders every server with its grant state.
	rec := httptest.NewRecorder()
	h.GetKeyPermissions(rec, chiURL(mcpRequest(http.MethodGet, "/api/keys/x/mcp", "", true), key.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var view struct {
		Explicit bool `json:"explicit"`
		Servers  []struct {
			ServerID string `json:"server_id"`
			Granted  bool   `json:"granted"`
		} `json:"servers"`
	}
	decodeJSON(t, rec, &view)
	if view.Explicit {
		t.Fatal("a fresh key must not be explicit")
	}
	if len(view.Servers) != 1 || view.Servers[0].Granted {
		t.Fatalf("view = %+v", view)
	}

	// PUT grants the server with a tool subset.
	body := `{"servers":[{"server_id":"` + created.ID + `","granted":true,"allowed_tools":["echo"]}]}`
	rec = httptest.NewRecorder()
	h.SetKeyPermissions(rec, chiURL(mcpRequest(http.MethodPut, "/api/keys/x/mcp", body, true), key.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", rec.Code, rec.Body.String())
	}
	perms, err := store.ListKeyPermissions(key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(perms) != 1 || !perms[0].Enabled || len(perms[0].AllowedTools) != 1 || perms[0].AllowedTools[0] != "echo" {
		t.Fatalf("perms = %+v", perms)
	}

	// The effective access now exposes exactly one tool.
	verified, ok := keys.Verify(key.Key)
	if !ok {
		t.Fatal("key did not verify")
	}
	access, err := store.AccessForKey(verified)
	if err != nil {
		t.Fatal(err)
	}
	if len(access.Tools) != 1 || access.Tools[0].Name != "echo" {
		t.Fatalf("access = %+v", access.Tools)
	}

	// Revoking every server makes the key explicit with zero access.
	rec = httptest.NewRecorder()
	h.SetKeyPermissions(rec, chiURL(mcpRequest(http.MethodPut, "/api/keys/x/mcp", `{"servers":[]}`, true), key.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d", rec.Code)
	}
	perms, _ = store.ListKeyPermissions(key.ID)
	if len(perms) != 0 {
		t.Fatalf("grants not cleared: %+v", perms)
	}
	access, _ = store.AccessForKey(verified)
	// CORRECTED: this used to assert 3 tools, i.e. that "revoke every server"
	// restored the allow-everything default. That is the defect, not the
	// contract — an admin who removes all MCP access from a key must end up with
	// a key that can use nothing. See mcp/revoke_all_test.go.
	if !access.Explicit || len(access.Tools) != 0 || len(access.Servers) != 0 {
		t.Fatalf("revoking every grant must deny everything, got explicit=%v servers=%d tools=%d",
			access.Explicit, len(access.Servers), len(access.Tools))
	}
}

func TestMCPStatusAndCalls(t *testing.T) {
	h, store, _ := newMCPHandler(t)
	up := mcptest.Start(t)
	created, _ := store.Create(mcp.ServerInput{Name: "Stats", Transport: "http", URL: up.Server.URL + "/mcp"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.Manager.Discover(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Manager.CallTool(ctx, created.ID, "echo", map[string]any{"text": "hi"}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.Status(rec, mcpRequest(http.MethodGet, "/api/mcp/status", "", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var status struct {
		Servers int `json:"servers"`
		Up      int `json:"up"`
		Tools   int `json:"tools"`
	}
	decodeJSON(t, rec, &status)
	if status.Servers != 1 || status.Up != 1 || status.Tools != 3 {
		t.Fatalf("status = %+v", status)
	}

	rec = httptest.NewRecorder()
	h.ListCalls(rec, mcpRequest(http.MethodGet, "/api/mcp/calls", "", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("calls status = %d", rec.Code)
	}
	var calls []mcp.CallRecord
	decodeJSON(t, rec, &calls)
	if len(calls) != 1 || calls[0].Status != "ok" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestMCPRoutesRequirePermissions(t *testing.T) {
	// The route table must gate reads on mcp:read and writes on mcp:write, so a
	// readonly key-holder cannot mutate the registry.
	h, _, _ := newMCPHandler(t)
	r := chi.NewRouter()
	// RequirePerm resolves permissions live from a resolver; register a stub
	// that echoes the permission set the test already placed in context.
	r.Use(middleware.WithPermResolver(stubResolver{perUser: map[string]map[string]bool{
		"member":   {"mcp:read": true, "mcp:write": true},
		"readonly": {"keys:read_own": true},
	}}))
	h.Routes(r)

	// A readonly caller (no mcp perms) is refused a write.
	req := requestAs("readonly", "ro", map[string]bool{"keys:read_own": true})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, reqWithMethod(req, http.MethodPost, "/mcp/servers"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("readonly write status = %d, want 403", rec.Code)
	}
	// A caller holding mcp:write gets past the gate (whatever the handler
	// then answers for an empty body, it must not be 403).
	req = httptest.NewRequest(http.MethodPost, "/mcp/servers", nil)
	ctx := auth.WithRole(req.Context(), "member")
	ctx = auth.WithSubject(ctx, "member")
	req = req.WithContext(ctx)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, reqWithMethod(req, http.MethodPost, "/mcp/servers"))
	if rec.Code == http.StatusForbidden {
		t.Fatal("mcp:write holder was refused")
	}
	// A caller holding mcp:read may list.
	rec = httptest.NewRecorder()
	listReq := httptest.NewRequest(http.MethodGet, "/mcp/servers", nil)
	ctx = auth.WithRole(listReq.Context(), "member")
	ctx = auth.WithSubject(ctx, "member")
	listReq = listReq.WithContext(ctx)
	r.ServeHTTP(rec, listReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("mcp:read list status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func reqWithMethod(base *http.Request, method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header = base.Header
	return r.WithContext(base.Context())
}

func waitForCond(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// stubResolver returns a fixed permission set per user.
type stubResolver struct{ perUser map[string]map[string]bool }

func (s stubResolver) EffectivePermissionsByUsername(username, _ string) (map[string]bool, error) {
	if p, ok := s.perUser[username]; ok {
		return p, nil
	}
	return map[string]bool{}, nil
}

func TestMCPCallAuditJSONShape(t *testing.T) {
	// The dashboard reads snake_case fields; Go's default PascalCase marshaling
	// silently rendered an empty audit table once already.
	h, store, _ := newMCPHandler(t)
	up := mcptest.Start(t)
	created, _ := store.Create(mcp.ServerInput{Name: "Shape", Transport: "http", URL: up.Server.URL + "/mcp"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.Manager.Discover(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Manager.CallToolMeta(ctx, created.ID, "echo", map[string]any{"text": "x"}, "abcd1234", "/mcp"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ListCalls(rec, mcpRequest(http.MethodGet, "/api/mcp/calls", "", false))
	var rows []map[string]any
	decodeJSON(t, rec, &rows)
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	for _, key := range []string{"id", "server_id", "server_slug", "tool_name", "namespaced_name", "key_prefix", "endpoint", "status", "duration_ms", "created_at"} {
		if _, ok := rows[0][key]; !ok {
			t.Errorf("audit row is missing %q (got keys %v)", key, keysOf(rows[0]))
		}
	}
	if rows[0]["namespaced_name"] != created.Slug+"-echo" {
		t.Fatalf("namespaced_name = %v", rows[0]["namespaced_name"])
	}
	if rows[0]["key_prefix"] != "abcd1234" || rows[0]["endpoint"] != "/mcp" {
		t.Fatalf("attribution lost: %+v", rows[0])
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestMCPAdminReadsAreOrgScoped(t *testing.T) {
	h, store, _ := newMCPHandler(t)
	up := mcptest.Start(t)

	// One server in another org, one in ours.
	if _, err := store.Create(mcp.ServerInput{Name: "Foreign", Transport: "http", URL: up.Server.URL + "/mcp", OrgID: "org-other"}); err != nil {
		t.Fatal(err)
	}
	mine, err := store.Create(mcp.ServerInput{Name: "Mine", Transport: "http", URL: up.Server.URL + "/mcp", OrgID: "org-me"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, srv := range mustServers(t, store) {
		if _, err := h.Manager.Discover(ctx, srv.ID); err != nil {
			t.Fatal(err)
		}
	}

	// A member of org-me sees only its own server in each admin read.
	memberReq := func(method, path string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		ctx := auth.WithRole(r.Context(), "member")
		ctx = auth.WithOrgID(ctx, "org-me")
		return r.WithContext(ctx)
	}

	rec := httptest.NewRecorder()
	h.ListServers(rec, memberReq(http.MethodGet, "/api/mcp/servers"))
	var servers []mcp.Server
	decodeJSON(t, rec, &servers)
	if len(servers) != 1 || servers[0].ID != mine.ID {
		t.Fatalf("ListServers leaked: %+v", servers)
	}

	rec = httptest.NewRecorder()
	h.ListAllTools(rec, memberReq(http.MethodGet, "/api/mcp/tools"))
	var tools []mcp.Tool
	decodeJSON(t, rec, &tools)
	for _, tool := range tools {
		if tool.ServerID != mine.ID {
			t.Fatalf("ListAllTools leaked %q from %s", tool.NamespacedName, tool.ServerID)
		}
	}

	rec = httptest.NewRecorder()
	h.Status(rec, memberReq(http.MethodGet, "/api/mcp/status"))
	var status struct {
		Servers int `json:"servers"`
	}
	decodeJSON(t, rec, &status)
	if status.Servers != 1 {
		t.Fatalf("Status counted %d servers, want 1", status.Servers)
	}

	// Audit rows are scoped too.
	if _, err := h.Manager.CallTool(ctx, mine.ID, "echo", map[string]any{"text": "x"}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ListCalls(rec, memberReq(http.MethodGet, "/api/mcp/calls"))
	var calls []mcp.CallRecord
	decodeJSON(t, rec, &calls)
	for _, c := range calls {
		if c.ServerID != mine.ID {
			t.Fatalf("ListCalls leaked a row for %s", c.ServerID)
		}
	}

	// An admin still sees both.
	adminReq := httptest.NewRequest(http.MethodGet, "/api/mcp/servers", nil)
	adminReq = adminReq.WithContext(auth.WithRole(adminReq.Context(), "admin"))
	rec = httptest.NewRecorder()
	h.ListServers(rec, adminReq)
	servers = nil
	decodeJSON(t, rec, &servers)
	if len(servers) != 2 {
		t.Fatalf("admin saw %d servers, want 2", len(servers))
	}
}

func mustServers(t *testing.T, store *mcp.Store) []mcp.Server {
	t.Helper()
	list, err := store.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	return list
}
