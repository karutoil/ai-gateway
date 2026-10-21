package pgsmoke

// Postgres coverage for the centralized MCP store.
//
// SQLite cannot catch the dialect bugs this suite exists for, and production
// runs Postgres. Specifically this exercises:
//
//   - migration 032 DDL (BLOB -> BYTEA, DATETIME -> TIMESTAMPTZ),
//   - BYTEA round-tripping of the AES-GCM credential blobs,
//   - scanning a row whose nullable columns (url/command/org_id/
//     last_health_at) are all NULL,
//   - db.BoolInt on the INTEGER `enabled` column (lib/pq rejects a Go bool),
//   - "LIMIT ?" rebinding to "LIMIT $1",
//   - transactions in SaveTools.

import (
	"testing"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/mcp"
)

func mcpMasterKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 5)
	}
	return k
}

// TestMCPSchemaOnPostgres asserts the migration produced the expected column
// types. A BLOB left untranslated would surface as a missing column here.
func TestMCPSchemaOnPostgres(t *testing.T) {
	database := openPG(t)

	cases := []struct {
		table  string
		column string
		want   string
	}{
		{"mcp_servers", "env_enc", "bytea"},
		{"mcp_servers", "headers_enc", "bytea"},
		{"mcp_servers", "auth_value_enc", "bytea"},
		{"mcp_servers", "enabled", "integer"},
		{"mcp_servers", "last_health_at", "timestamp with time zone"},
		{"mcp_servers", "created_at", "timestamp with time zone"},
		{"mcp_servers", "tool_count", "integer"},
		{"mcp_server_tools", "input_schema", "text"},
		{"mcp_key_permissions", "allowed_tools", "text"},
		{"mcp_key_permissions", "enabled", "integer"},
		{"mcp_tool_calls", "duration_ms", "integer"},
		{"mcp_tool_calls", "created_at", "timestamp with time zone"},
	}
	for _, tc := range cases {
		var got string
		err := database.QueryRow(`
			SELECT data_type FROM information_schema.columns
			WHERE table_name=$1 AND column_name=$2`, tc.table, tc.column).Scan(&got)
		if err != nil {
			t.Errorf("%s.%s: %v", tc.table, tc.column, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s.%s type = %q, want %q", tc.table, tc.column, got, tc.want)
		}
	}
}

// TestMCPStoreCRUDAgainstPostgres drives every write/read path in the MCP
// store on Postgres, including the encrypted-blob round trip.
func TestMCPStoreCRUDAgainstPostgres(t *testing.T) {
	database := openPG(t)
	store := mcp.NewStore(database, mcpMasterKey())

	// --- create with secrets, a URL and static headers ---
	created, err := store.Create(mcp.ServerInput{
		Name:      "pg-mcp-server",
		Transport: "http",
		URL:       "https://mcp.example.com/mcp",
		AuthType:  "bearer",
		AuthValue: "pg-secret-token",
		Headers:   map[string]string{"X-Org": "acme"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Slug != "pg-mcp-server" {
		t.Fatalf("slug = %q", created.Slug)
	}
	if !created.HasAuth || !created.HasHeaders {
		t.Fatalf("secret indicators not set: %+v", created)
	}

	// --- BYTEA round trip ---
	runtime, err := store.RuntimeServer(created.ID)
	if err != nil {
		t.Fatalf("RuntimeServer: %v", err)
	}
	if runtime.AuthValue != "pg-secret-token" {
		t.Errorf("auth value round trip = %q", runtime.AuthValue)
	}
	if runtime.Headers["X-Org"] != "acme" {
		t.Errorf("headers round trip = %v", runtime.Headers)
	}

	// --- a stdio row leaves url NULL and command set ---
	stdio, err := store.Create(mcp.ServerInput{
		Name: "pg-stdio", Transport: "stdio", Command: "/bin/true",
		Args: []string{"--flag", "value"}, Env: map[string]string{"TOKEN": "abc"},
	})
	if err != nil {
		t.Fatalf("Create stdio: %v", err)
	}
	if stdio.URL != "" {
		t.Errorf("stdio URL should scan back empty, got %q", stdio.URL)
	}
	if len(stdio.Args) != 2 || stdio.Args[0] != "--flag" {
		t.Errorf("args round trip = %v", stdio.Args)
	}
	stdioRuntime, err := store.RuntimeServer(stdio.ID)
	if err != nil {
		t.Fatalf("RuntimeServer stdio: %v", err)
	}
	if stdioRuntime.Env["TOKEN"] != "abc" {
		t.Errorf("env round trip = %v", stdioRuntime.Env)
	}

	// --- ListServers must scan a NULL-URL, NULL-org, NULL-last_health row ---
	list, err := store.ListServers()
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListServers = %d rows, want 2", len(list))
	}
	var sawNullHealth bool
	for _, s := range list {
		if s.LastHealthAt == nil && s.OrgID == nil {
			sawNullHealth = true
		}
	}
	if !sawNullHealth {
		t.Error("expected at least one row with NULL last_health_at/org_id")
	}

	// --- BoolInt on the INTEGER enabled column ---
	if err := store.SetEnabled(created.ID, false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	got, err := store.GetServer(created.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.Enabled {
		t.Error("server still enabled after SetEnabled(false)")
	}
	if err := store.SetEnabled(created.ID, true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}

	// --- SetHealth writes a TIMESTAMPTZ ---
	if err := store.SetHealth(created.ID, "up", ""); err != nil {
		t.Fatalf("SetHealth: %v", err)
	}
	got, err = store.GetServer(created.ID)
	if err != nil {
		t.Fatalf("GetServer after health: %v", err)
	}
	if got.HealthStatus != "up" {
		t.Errorf("health = %q", got.HealthStatus)
	}
	if got.LastHealthAt == nil {
		t.Error("last_health_at not persisted")
	}

	// --- SaveTools transaction + ListTools ---
	tools := []mcp.Tool{
		{ServerID: created.ID, Name: "echo", Description: "Echo",
			InputSchema: []byte(`{"type":"object","properties":{"text":{"type":"string"}}}`)},
		{ServerID: created.ID, Name: "add", Description: "Add",
			InputSchema: []byte(`{"type":"object"}`)},
	}
	if err := store.SaveTools(created.ID, tools); err != nil {
		t.Fatalf("SaveTools: %v", err)
	}
	saved, err := store.ListTools(created.ID)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("ListTools = %d, want 2", len(saved))
	}
	if saved[0].NamespacedName != created.Slug+"-add" && saved[0].NamespacedName != created.Slug+"-echo" {
		t.Errorf("namespaced name = %q", saved[0].NamespacedName)
	}
	// Re-saving replaces, not appends.
	if err := store.SaveTools(created.ID, tools[:1]); err != nil {
		t.Fatalf("SaveTools (replace): %v", err)
	}
	saved, _ = store.ListTools(created.ID)
	if len(saved) != 1 {
		t.Fatalf("after replace ListTools = %d, want 1", len(saved))
	}
	// tool_count is kept in sync.
	got, _ = store.GetServer(created.ID)
	if got.ToolCount != 1 {
		t.Errorf("tool_count = %d, want 1", got.ToolCount)
	}

	// --- per-key grants: JSON allowed_tools round trip ---
	if err := store.SetKeyPermissions("pg-key-1", []mcp.KeyPermission{
		{KeyID: "pg-key-1", ServerID: created.ID, AllowedTools: []string{"echo"}, Enabled: true},
	}); err != nil {
		t.Fatalf("SetKeyPermissions: %v", err)
	}
	perms, err := store.ListKeyPermissions("pg-key-1")
	if err != nil {
		t.Fatalf("ListKeyPermissions: %v", err)
	}
	if len(perms) != 1 || len(perms[0].AllowedTools) != 1 || perms[0].AllowedTools[0] != "echo" {
		t.Fatalf("perms round trip = %+v", perms)
	}
	if !perms[0].Enabled {
		t.Error("permission enabled flag lost")
	}

	// --- audit rows: "LIMIT ?" must rebind to "LIMIT $1" ---
	store.RecordCall(mcp.CallRecord{
		ServerID: created.ID, ServerSlug: created.Slug, ToolName: "echo",
		NamespacedName: created.Slug + "-echo", KeyPrefix: "abcdef12",
		Endpoint: "/mcp", Status: "ok", DurationMs: 7, CreatedAt: time.Now().UTC(),
	})
	calls, err := store.RecentCalls(10)
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("RecentCalls = %d, want 1", len(calls))
	}
	if calls[0].KeyPrefix != "abcdef12" || calls[0].Endpoint != "/mcp" {
		t.Errorf("audit row = %+v", calls[0])
	}

	// --- delete cascades to tools and grants ---
	if err := store.Delete(created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.GetServer(created.ID); err == nil {
		t.Error("server still present after delete")
	}
	if remaining, _ := store.ListTools(created.ID); len(remaining) != 0 {
		t.Errorf("orphaned tools after delete: %+v", remaining)
	}
	if remaining, _ := store.ListKeyPermissions("pg-key-1"); len(remaining) != 0 {
		t.Errorf("orphaned grants after delete: %+v", remaining)
	}
}

// TestMCPUniqueSlugAgainstPostgres covers the slug-collision path, which relies
// on a UNIQUE index and the ErrNoRows branch.
func TestMCPUniqueSlugAgainstPostgres(t *testing.T) {
	database := openPG(t)
	store := mcp.NewStore(database, mcpMasterKey())

	first, err := store.Create(mcp.ServerInput{Name: "dup-name", Transport: "http", URL: "https://a/mcp"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := store.Create(mcp.ServerInput{Name: "dup-name", Transport: "http", URL: "https://b/mcp"})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	if second.Slug == first.Slug {
		t.Fatalf("slug collision not resolved: both %q", first.Slug)
	}
	if second.Slug != "dup-name-2" {
		t.Errorf("second slug = %q, want dup-name-2", second.Slug)
	}
	if _, err := database.Exec(db.Q(`DELETE FROM mcp_servers WHERE id IN (?,?)`), first.ID, second.ID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}
