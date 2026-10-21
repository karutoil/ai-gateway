package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"

	"ai-gateway/internal/mcp/mcptest"

	"github.com/go-chi/chi/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// testFixture wires a real SQLite database, the MCP store/manager/endpoint and
// a gateway key — the same construction path main.go uses.
type testFixture struct {
	DB      *Store
	Manager *Manager
	Keys    *apikey.Store
	Key     string
	KeyObj  interface{ GetPrefix() string }
	store   interface{}
}

func newFixture(t *testing.T) (*Store, *Manager, *apikey.Store, string) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 7)
	}
	store := NewStore(database, master)
	manager := NewManager(store, 20*time.Second)
	t.Cleanup(manager.Close)
	keys := apikey.NewStore(database)
	key, err := keys.Create("test-key")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return store, manager, keys, key.Key
}

func registerUpstream(t *testing.T, store *Store, manager *Manager, up *mcptest.Upstream, name string) *Server {
	t.Helper()
	srv, err := store.Create(ServerInput{
		Name:      name,
		Transport: "http",
		URL:       up.Server.URL + "/mcp",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tools, err := manager.Discover(ctx, srv.ID)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(tools) < 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools))
	}
	got, err := store.GetServer(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

/* ------------------------------------------------------------------ */
/* Discovery                                                          */
/* ------------------------------------------------------------------ */

func TestDiscoverPersistsNamespacedTools(t *testing.T) {
	store, manager, _, _ := newFixture(t)
	up := mcptest.Start(t)

	srv := registerUpstream(t, store, manager, up, "Test Upstream")
	if srv.ToolCount != 3 {
		t.Fatalf("tool_count = %d, want 3", srv.ToolCount)
	}
	if srv.HealthStatus != "up" {
		t.Fatalf("health = %q, want up", srv.HealthStatus)
	}

	tools, err := store.ListTools(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.NamespacedName] = true
		if !strings.HasPrefix(tool.NamespacedName, srv.Slug+"-") {
			t.Errorf("tool %q is not namespaced with %q", tool.NamespacedName, srv.Slug)
		}
		if len(tool.InputSchema) == 0 {
			t.Errorf("tool %q has no input schema", tool.Name)
		}
	}
	for _, want := range []string{srv.Slug + "-echo", srv.Slug + "-add", srv.Slug + "-fail"} {
		if !names[want] {
			t.Errorf("missing tool %q (got %v)", want, names)
		}
	}
	// description survived the round trip
	for _, tool := range tools {
		if tool.Name == "echo" && tool.Description == "" {
			t.Error("echo description was dropped")
		}
	}
}

func TestDiscoverMarksDownServerAndRecordsError(t *testing.T) {
	store, manager, _, _ := newFixture(t)
	srv, err := store.Create(ServerInput{
		Name:      "Dead",
		Transport: "http",
		URL:       "http://127.0.0.1:1/mcp", // nothing listens here
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.Discover(ctx, srv.ID); err == nil {
		t.Fatal("expected discovery to fail against a closed port")
	}
	got, err := store.GetServer(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HealthStatus != "down" {
		t.Fatalf("health = %q, want down", got.HealthStatus)
	}
	if got.LastError == "" {
		t.Fatal("expected the connection error to be recorded")
	}
}

/* ------------------------------------------------------------------ */
/* Execution                                                          */
/* ------------------------------------------------------------------ */

func TestCallToolRoutesToCorrectServer(t *testing.T) {
	store, manager, _, _ := newFixture(t)
	upA := mcptest.Start(t)
	upB := mcptest.Start(t)

	a := registerUpstream(t, store, manager, upA, "Alpha")
	b := registerUpstream(t, store, manager, upB, "Beta")
	if a.Slug == b.Slug {
		t.Fatalf("slug collision: %q", a.Slug)
	}

	ctx := context.Background()
	res, err := manager.CallTool(ctx, b.ID, "echo", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Text != "echo: hello" {
		t.Fatalf("text = %q", res.Text)
	}
	if res.ServerSlug != b.Slug {
		t.Fatalf("routed to %q, want %q", res.ServerSlug, b.Slug)
	}
	// Only the targeted upstream received the call.
	if got := upB.Calls.Total(); got != 1 {
		t.Fatalf("target upstream calls = %d, want 1", got)
	}
	if got := upA.Calls.Total(); got != 0 {
		t.Fatalf("sibling upstream calls = %d, want 0", got)
	}
	if upB.Calls.Log()["echo"]["text"] != "hello" {
		t.Fatalf("arguments did not reach the upstream: %v", upB.Calls.Log()["echo"])
	}
}

func TestCallToolPropagatesToolError(t *testing.T) {
	store, manager, _, _ := newFixture(t)
	up := mcptest.Start(t)
	srv := registerUpstream(t, store, manager, up, "Errors")

	res, err := manager.CallTool(context.Background(), srv.ID, "fail", nil)
	if err != nil {
		t.Fatalf("a tool-level error must not be a transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("IsError should be true")
	}
	if !strings.Contains(res.Text, "upstream tool failure") {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestCallToolRecordsAuditRow(t *testing.T) {
	store, manager, _, _ := newFixture(t)
	up := mcptest.Start(t)
	srv := registerUpstream(t, store, manager, up, "Audited")

	if _, err := manager.CallToolMeta(context.Background(), srv.ID, "add", map[string]any{"a": 2, "b": 3}, "abcd1234", "/mcp"); err != nil {
		t.Fatal(err)
	}
	calls, err := store.RecentCalls(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("call rows = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.NamespacedName != srv.Slug+"-add" || got.Status != "ok" || got.KeyPrefix != "abcd1234" || got.Endpoint != "/mcp" {
		t.Fatalf("unexpected audit row: %+v", got)
	}
}

func TestDisabledServerRejectsCalls(t *testing.T) {
	store, manager, _, _ := newFixture(t)
	up := mcptest.Start(t)
	srv := registerUpstream(t, store, manager, up, "Toggled")

	if err := store.SetEnabled(srv.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CallTool(context.Background(), srv.ID, "echo", map[string]any{"text": "x"}); err == nil {
		t.Fatal("a disabled server must not execute tools")
	}
	if up.Calls.Total() != 0 {
		t.Fatal("upstream was contacted for a disabled server")
	}
}

func TestReconnectAfterUpstreamRestart(t *testing.T) {
	store, manager, _, _ := newFixture(t)
	up := mcptest.Start(t)
	srv := registerUpstream(t, store, manager, up, "Restarted")

	if _, err := manager.CallTool(context.Background(), srv.ID, "echo", map[string]any{"text": "one"}); err != nil {
		t.Fatal(err)
	}
	up.Close()
	// A call against the dead server fails and trips the backoff.
	if _, err := manager.CallTool(context.Background(), srv.ID, "echo", map[string]any{"text": "two"}); err == nil {
		t.Fatal("expected failure after upstream shutdown")
	}
	// Point the row at a fresh upstream on a new port and force a reconnect.
	fresh := mcptest.Start(t)
	if _, err := store.db.Exec(`UPDATE mcp_servers SET url=? WHERE id=?`, fresh.Server.URL+"/mcp", srv.ID); err != nil {
		t.Fatal(err)
	}
	manager.CloseServer(srv.ID)
	manager.mu.Lock()
	delete(manager.conns, srv.ID) // clear the backoff window
	manager.mu.Unlock()

	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		res, err := manager.CallTool(context.Background(), srv.ID, "echo", map[string]any{"text": "three"})
		if err == nil && res.Text == "echo: three" {
			return
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("did not reconnect to the restarted upstream: %v", lastErr)
}

/* ------------------------------------------------------------------ */
/* Permissions                                                        */
/* ------------------------------------------------------------------ */

func TestAccessDefaultsToAllEnabledServers(t *testing.T) {
	store, manager, keys, rawKey := newFixture(t)
	up := mcptest.Start(t)
	registerUpstream(t, store, manager, up, "Open")
	key, ok := keys.Verify(rawKey)
	if !ok {
		t.Fatal("key did not verify")
	}
	access, err := store.AccessForKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if access.Explicit {
		t.Fatal("a key with no grants must not be explicit")
	}
	if len(access.Servers) != 1 || len(access.Tools) != 3 {
		t.Fatalf("servers=%d tools=%d, want 1/3", len(access.Servers), len(access.Tools))
	}
}

func TestKeyAllowlistHidesUnauthorizedServers(t *testing.T) {
	store, manager, keys, rawKey := newFixture(t)
	upA := mcptest.Start(t)
	upB := mcptest.Start(t)
	a := registerUpstream(t, store, manager, upA, "Allowed")
	_ = registerUpstream(t, store, manager, upB, "Denied")

	key, _ := keys.Verify(rawKey)
	if err := store.SetKeyPermissions(key.ID, []KeyPermission{{KeyID: key.ID, ServerID: a.ID, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	access, err := store.AccessForKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if !access.Explicit {
		t.Fatal("a key with a grant row must be explicit")
	}
	if len(access.Servers) != 1 || access.Servers[0].ID != a.ID {
		t.Fatalf("unexpected servers: %+v", access.Servers)
	}
	for _, tool := range access.Tools {
		if tool.ServerID != a.ID {
			t.Fatalf("unauthorized tool leaked into access: %s", tool.NamespacedName)
		}
	}
}

func TestKeyToolAllowlistFiltersTools(t *testing.T) {
	store, manager, keys, rawKey := newFixture(t)
	up := mcptest.Start(t)
	a := registerUpstream(t, store, manager, up, "Partial")

	key, _ := keys.Verify(rawKey)
	if err := store.SetKeyPermissions(key.ID, []KeyPermission{{
		KeyID: key.ID, ServerID: a.ID, Enabled: true, AllowedTools: []string{"echo"},
	}}); err != nil {
		t.Fatal(err)
	}
	access, err := store.AccessForKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(access.Tools) != 1 || access.Tools[0].Name != "echo" {
		t.Fatalf("unexpected tools: %+v", access.Tools)
	}
}

func TestDisabledServerExcludedFromAccess(t *testing.T) {
	store, manager, keys, rawKey := newFixture(t)
	up := mcptest.Start(t)
	srv := registerUpstream(t, store, manager, up, "Off")
	if err := store.SetEnabled(srv.ID, false); err != nil {
		t.Fatal(err)
	}
	key, _ := keys.Verify(rawKey)
	access, err := store.AccessForKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(access.Servers) != 0 || len(access.Tools) != 0 {
		t.Fatalf("disabled server leaked: %d servers %d tools", len(access.Servers), len(access.Tools))
	}
}

/* ------------------------------------------------------------------ */
/* Unified /mcp endpoint                                              */
/* ------------------------------------------------------------------ */

func TestEndpointMCPClientEndToEnd(t *testing.T) {
	store, manager, keys, rawKey := newFixture(t)
	up := mcptest.Start(t)
	srv := registerUpstream(t, store, manager, up, "Endpoint")

	endpoint := NewEndpoint(store, manager, keys)
	router := httptest.NewServer(endpoint.Auth(endpoint))
	defer router.Close()

	// A real MCP client connects to the gateway's /mcp endpoint — exactly what
	// Pi / OpenCode / Cursor do.
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint:             router.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: headerInjector{"x-litellm-api-key": "Bearer " + rawKey}},
		MaxRetries:           0,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(tools.Tools))
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	if !names[srv.Slug+"-echo"] {
		t.Fatalf("namespaced tool missing: %v", names)
	}

	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      srv.Slug + "-echo",
		Arguments: map[string]any{"text": "through-gateway"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("empty tool result")
	}
	text, _ := res.Content[0].(*sdk.TextContent)
	if text == nil || text.Text != "echo: through-gateway" {
		t.Fatalf("unexpected result: %+v", res.Content[0])
	}
	if got := up.Calls.Log()["echo"]["text"]; got != "through-gateway" {
		t.Fatalf("upstream saw %v", got)
	}
}

func TestEndpointRejectsMissingAndBadKeys(t *testing.T) {
	store, manager, keys, _ := newFixture(t)
	endpoint := NewEndpoint(store, manager, keys)
	router := httptest.NewServer(endpoint.Auth(endpoint))
	defer router.Close()

	for _, tc := range []struct{ name, header, value string }{
		{"missing", "", ""},
		{"garbage", "Authorization", "Bearer not-a-key"},
		{"bad body", "x-api-key", "sk-gw-" + strings.Repeat("0", 64)},
	} {
		req, _ := http.NewRequest("POST", router.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if tc.header != "" {
			req.Header.Set(tc.header, tc.value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", tc.name, resp.StatusCode)
		}
	}
}

func TestEndpointRESTHelpers(t *testing.T) {
	store, manager, keys, rawKey := newFixture(t)
	up := mcptest.Start(t)
	srv := registerUpstream(t, store, manager, up, "Rest")

	endpoint := NewEndpoint(store, manager, keys)
	router := httptest.NewServer(endpoint.Auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/mcp/server":
			endpoint.ListServersREST(w, r)
		case "/mcp-rest/tools/list":
			endpoint.ListToolsREST(w, r)
		case "/mcp-rest/tools/call":
			endpoint.CallToolREST(w, r)
		default:
			http.NotFound(w, r)
		}
	})))
	defer router.Close()

	get := func(path string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", router.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+rawKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		return resp, body
	}

	resp, body := get("/v1/mcp/server")
	if resp.StatusCode != 200 {
		t.Fatalf("servers status %d", resp.StatusCode)
	}
	var servers []map[string]any
	if err := json.Unmarshal(body, &servers); err != nil {
		t.Fatalf("servers json: %v (%s)", err, body)
	}
	if len(servers) != 1 || servers[0]["server_name"] != srv.Slug {
		t.Fatalf("unexpected servers: %s", body)
	}

	resp, body = get("/mcp-rest/tools/list")
	if resp.StatusCode != 200 {
		t.Fatalf("tools status %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), srv.Slug+"-echo") {
		t.Fatalf("tools body: %s", body)
	}

	// Scripted call.
	payload, _ := json.Marshal(map[string]any{"server_id": srv.ID, "name": "add", "arguments": map[string]any{"a": 4, "b": 5}})
	req, _ := http.NewRequest("POST", router.URL+"/mcp-rest/tools/call", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Content-Type", "application/json")
	cresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer cresp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(cresp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["text"] != "9" {
		t.Fatalf("call result: %v", out)
	}

	// Unauthorized tool name on a valid server is refused.
	payload, _ = json.Marshal(map[string]any{"server_id": srv.ID, "name": "nonexistent"})
	req, _ = http.NewRequest("POST", router.URL+"/mcp-rest/tools/call", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Content-Type", "application/json")
	cresp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cresp2.Body.Close()
	if cresp2.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown tool status = %d, want 403", cresp2.StatusCode)
	}
}

// headerInjector attaches fixed headers to every request (MCP client auth).
type headerInjector map[string]string

func (h headerInjector) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

/* ------------------------------------------------------------------ */
/* Path-scoped selection (LiteLLM grammar)                            */
/* ------------------------------------------------------------------ */

// serveMCPTree mounts the endpoint the way cmd/gateway/main.go does.
func serveMCPTree(endpoint *Endpoint) http.Handler {
	r := chi.NewRouter()
	r.Use(endpoint.Auth)
	r.Handle("/mcp", endpoint)
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { endpoint.ServeHTTP(w, req) })
	r.Handle("/{server}/mcp", dispatch)
	r.Handle("/mcp/{server}", dispatch)
	return r
}

func listToolNames(t *testing.T, base, path, key string) (int, []string) {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil)
	hc := &http.Client{Transport: headerInjector{"x-litellm-api-key": "Bearer " + key}}
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint:             base + path,
		HTTPClient:           hc,
		MaxRetries:           0,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return 0, nil
	}
	defer session.Close()
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		return 0, nil
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return len(names), names
}

func TestPathSelectionNarrowsTools(t *testing.T) {
	store, manager, keys, rawKey := newFixture(t)
	upA := mcptest.Start(t)
	upB := mcptest.Start(t)
	a := registerUpstream(t, store, manager, upA, "Path A")
	b := registerUpstream(t, store, manager, upB, "Path B")

	router := httptest.NewServer(serveMCPTree(NewEndpoint(store, manager, keys)))
	defer router.Close()

	if n, _ := listToolNames(t, router.URL, "/mcp", rawKey); n != 6 {
		t.Fatalf("unscoped tool count = %d, want 6", n)
	}
	// /<server>/mcp
	n, names := listToolNames(t, router.URL, "/"+a.Slug+"/mcp", rawKey)
	if n != 3 {
		t.Fatalf("/%s/mcp tool count = %d, want 3", a.Slug, n)
	}
	for _, name := range names {
		if !strings.HasPrefix(name, a.Slug+"-") {
			t.Fatalf("/%s/mcp exposed %q", a.Slug, name)
		}
	}
	// LiteLLM's own grammar: /mcp/<server>
	n, names = listToolNames(t, router.URL, "/mcp/"+b.Slug, rawKey)
	if n != 3 {
		t.Fatalf("/mcp/%s tool count = %d, want 3", b.Slug, n)
	}
	for _, name := range names {
		if !strings.HasPrefix(name, b.Slug+"-") {
			t.Fatalf("/mcp/%s exposed %q", b.Slug, name)
		}
	}
}

func TestPathSelectionRefusesUnknownAndUnauthorized(t *testing.T) {
	store, manager, keys, rawKey := newFixture(t)
	allowed := mcptest.Start(t)
	denied := mcptest.Start(t)
	a := registerUpstream(t, store, manager, allowed, "Allowed")
	deniedSrv := registerUpstream(t, store, manager, denied, "Denied")

	// Restrict the key to the allowed server.
	key, _ := keys.Verify(rawKey)
	if err := store.SetKeyPermissions(key.ID, []KeyPermission{{KeyID: key.ID, ServerID: a.ID, Enabled: true}}); err != nil {
		t.Fatal(err)
	}

	router := httptest.NewServer(serveMCPTree(NewEndpoint(store, manager, keys)))
	defer router.Close()

	post := func(path string) (int, string) {
		req, _ := http.NewRequest("POST", router.URL+path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("x-litellm-api-key", "Bearer "+rawKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		defer resp.Body.Close()
		body := make([]byte, 0, 512)
		buf := make([]byte, 512)
		for {
			n, err := resp.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		return resp.StatusCode, string(body)
	}

	// The denied server must be refused by BOTH path forms.
	for _, path := range []string{"/" + deniedSrv.Slug + "/mcp", "/mcp/" + deniedSrv.Slug} {
		status, body := post(path)
		if status != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404 (%s)", path, status, body)
		}
		if !strings.Contains(body, "not registered or not authorized") {
			t.Fatalf("%s body = %s", path, body)
		}
	}
	// A server that does not exist at all likewise.
	status, _ := post("/does-not-exist/mcp")
	if status != http.StatusNotFound {
		t.Fatalf("unknown server status = %d, want 404", status)
	}
	// The authorized one still works.
	status, body := post("/" + a.Slug + "/mcp")
	if status != http.StatusOK {
		t.Fatalf("authorized server status = %d (%s)", status, body)
	}
}

/* ------------------------------------------------------------------ */
/* Legacy SSE transport                                               */
/* ------------------------------------------------------------------ */

func TestSSETransportDiscoveryAndCall(t *testing.T) {
	store, manager, _, _ := newFixture(t)
	up := mcptest.NewSSE()
	// The SSE session deliberately OUTLIVES the request context that
	// established it (see Manager.connect): close the manager before the
	// test server, or httptest.Server.Close blocks forever on the session's
	// still-open event stream. manager.Close is idempotent, so the fixture's
	// own cleanup is harmless.
	defer func() { manager.Close(); up.Close() }()

	srv, err := store.Create(ServerInput{Name: "SSE Server", Transport: "sse", URL: up.Server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tools, err := manager.Discover(ctx, srv.ID)
	if err != nil {
		t.Fatalf("SSE discovery failed: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(tools))
	}
	res, err := manager.CallTool(ctx, srv.ID, "echo", map[string]any{"text": "over-sse"})
	if err != nil {
		t.Fatalf("SSE call failed: %v", err)
	}
	if res.Text != "echo: over-sse" {
		t.Fatalf("result = %q", res.Text)
	}
	got, _ := store.GetServer(srv.ID)
	if got.HealthStatus != "up" {
		t.Fatalf("health = %q", got.HealthStatus)
	}
}

/* ------------------------------------------------------------------ */
/* Connection retry and cancellation                                  */
/* ------------------------------------------------------------------ */

func TestCallToolRetriesOnceAfterConnectionLoss(t *testing.T) {
	store, manager, _, _ := newFixture(t)
	up := mcptest.Start(t)
	srv := registerUpstream(t, store, manager, up, "Flaky")

	// Establish the session, then make the cached session stale by closing the
	// upstream's idle connections out from under the client.
	if _, err := manager.CallTool(context.Background(), srv.ID, "echo", map[string]any{"text": "first"}); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	if c := manager.conns[srv.ID]; c != nil && c.session != nil {
		_ = c.session.Close() // simulate a dropped connection
	}
	manager.mu.Unlock()

	// The call must transparently reconnect and succeed, not surface an error.
	res, err := manager.CallTool(context.Background(), srv.ID, "echo", map[string]any{"text": "second"})
	if err != nil {
		t.Fatalf("expected a transparent reconnect, got %v", err)
	}
	if res.Text != "echo: second" {
		t.Fatalf("result = %q", res.Text)
	}
}

func TestCallToolHonorsCancellation(t *testing.T) {
	store, manager, _, _ := newFixture(t)
	up := mcptest.Start(t)
	srv := registerUpstream(t, store, manager, up, "Cancel")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call
	if _, err := manager.CallTool(ctx, srv.ID, "echo", map[string]any{"text": "x"}); err == nil {
		t.Fatal("a cancelled context must fail the call")
	}
}

func TestToolLoopHonorsCancellation(t *testing.T) {
	// A cancelled client context must abort the loop promptly rather than
	// running every round.
	store, manager, _, _ := newFixture(t)
	up := mcptest.Start(t)
	srv := registerUpstream(t, store, manager, up, "LoopCancel")

	plan := &Plan{
		Bindings:    map[string]Binding{srv.Slug + "-echo": {ServerID: srv.ID, ServerSlug: srv.Slug, ToolName: "echo", Namespaced: srv.Slug + "-echo", Auto: true}},
		AutoNames:   map[string]bool{srv.Slug + "-echo": true},
		Servers:     []Server{*srv},
		AutoExecute: true,
	}
	bridge := NewBridge(store, manager, 25, 30*time.Second)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)

	rounds := 0
	exec := func(ctx context.Context, body []byte) RoundResponse {
		rounds++
		// Model always asks for a tool call.
		payload := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"` + srv.Slug + `-echo","arguments":"{\"text\":\"x\"}"}}]}}]}`
		return RoundResponse{Status: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(payload)}
	}
	bridge.RunAuto(rec, req, []byte(`{"model":"m","messages":[]}`), plan, ProtocolChat, "/v1/chat/completions", "test", false, exec)
	if rounds > 1 {
		t.Fatalf("cancelled loop ran %d rounds; it must stop at the first check", rounds)
	}
}

func TestManagerCloseReleasesSessions(t *testing.T) {
	// Both transports must be torn down by Close()/CloseServer: a retained
	// connect context (needed for the SSE stream) must not leak.
	store, manager, _, _ := newFixture(t)
	upHTTP := mcptest.Start(t)
	upSSE := mcptest.NewSSE()
	defer upSSE.Close()

	a := registerUpstream(t, store, manager, upHTTP, "HTTP")
	sse, err := store.Create(ServerInput{Name: "SSE", Transport: "sse", URL: upSSE.Server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.Discover(ctx, sse.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, sse.ID} {
		if _, err := manager.CallTool(ctx, id, "echo", map[string]any{"text": "x"}); err != nil {
			t.Fatalf("call on %s: %v", id, err)
		}
	}
	manager.mu.Lock()
	n := len(manager.conns)
	manager.mu.Unlock()
	if n != 2 {
		t.Fatalf("cached sessions = %d, want 2", n)
	}
	manager.Close()
	manager.mu.Lock()
	n = len(manager.conns)
	manager.mu.Unlock()
	if n != 0 {
		t.Fatalf("sessions left after Close: %d", n)
	}
}

func TestAccessForKeyIsOrgScoped(t *testing.T) {
	// A key belonging to one org must not reach another org's MCP servers.
	store, manager, keys, rawKey := newFixture(t)
	upA := mcptest.Start(t)
	upB := mcptest.Start(t)
	global := mcptest.Start(t)

	other := "org-b"
	mine := "org-a"
	if _, err := store.Create(ServerInput{Name: "Other Org", Transport: "http", URL: upA.Server.URL + "/mcp", OrgID: other}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ServerInput{Name: "My Org", Transport: "http", URL: upB.Server.URL + "/mcp", OrgID: mine}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ServerInput{Name: "Shared", Transport: "http", URL: global.Server.URL + "/mcp"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, srv := range mustServers(t, store) {
		if _, err := manager.Discover(ctx, srv.ID); err != nil {
			t.Fatalf("discover %s: %v", srv.Name, err)
		}
	}

	key, ok := keys.Verify(rawKey)
	if !ok {
		t.Fatal("key did not verify")
	}
	key.OrgID = &mine
	access, err := store.AccessForKey(key)
	if err != nil {
		t.Fatal(err)
	}
	slugs := map[string]bool{}
	for _, srv := range access.Servers {
		slugs[srv.Slug] = true
	}
	if !slugs["my-org"] {
		t.Fatalf("own-org server missing: %v", slugs)
	}
	if !slugs["shared"] {
		t.Fatalf("global server must be visible: %v", slugs)
	}
	if slugs["other-org"] {
		t.Fatalf("another org's server leaked: %v", slugs)
	}
}

func mustServers(t *testing.T, store *Store) []Server {
	t.Helper()
	list, err := store.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestAuditRecordsActualEndpointPath(t *testing.T) {
	store, manager, keys, rawKey := newFixture(t)
	up := mcptest.Start(t)
	srv := registerUpstream(t, store, manager, up, "Path Audit")

	router := httptest.NewServer(serveMCPTree(NewEndpoint(store, manager, keys)))
	defer router.Close()

	client := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint:             router.URL + "/" + srv.Slug + "/mcp",
		HTTPClient:           &http.Client{Transport: headerInjector{"x-litellm-api-key": "Bearer " + rawKey}},
		MaxRetries:           0,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: srv.Slug + "-echo", Arguments: map[string]any{"text": "x"},
	}); err != nil {
		t.Fatal(err)
	}
	calls, err := store.RecentCalls(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) == 0 {
		t.Fatal("no audit row")
	}
	if want := "/" + srv.Slug + "/mcp"; calls[0].Endpoint != want {
		t.Fatalf("endpoint = %q, want %q", calls[0].Endpoint, want)
	}
}

func TestServerInfoReportsGatewayVersion(t *testing.T) {
	// The version reported to MCP clients must come from the single source of
	// truth (handler.GatewayVersion), not a hardcoded literal in this package.
	store, manager, keys, rawKey := newFixture(t)
	up := mcptest.Start(t)
	registerUpstream(t, store, manager, up, "Versioned")

	endpoint := NewEndpoint(store, manager, keys)
	endpoint.Version = "9.9.9-test"
	router := httptest.NewServer(endpoint.Auth(endpoint))
	defer router.Close()

	req, _ := http.NewRequest("POST", router.URL+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("x-litellm-api-key", "Bearer "+rawKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, `"version":"9.9.9-test"`) {
		t.Fatalf("serverInfo did not report the injected version: %s", body)
	}
}

func TestServerInfoDefaultsToDevWhenUnset(t *testing.T) {
	e := &Endpoint{}
	if got := e.VersionOrDefault(); got != "dev" {
		t.Fatalf("VersionOrDefault() = %q, want dev", got)
	}
}
