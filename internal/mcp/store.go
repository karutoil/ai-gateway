package mcp

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/google/uuid"
)

// ErrNotFound is returned when a server does not exist.
var ErrNotFound = errors.New("mcp server not found")

// ErrDuplicateSlug is returned when a slug is already taken.
var ErrDuplicateSlug = errors.New("mcp server slug already exists")

// Store persists MCP server configuration, discovered tools and key grants.
// Credentials are encrypted with the same AES-256-GCM master key used for
// provider API keys, so there is exactly one at-rest secret-management story.
type Store struct {
	db        *sql.DB
	masterKey []byte
}

func NewStore(database *sql.DB, masterKey []byte) *Store {
	return &Store{db: database, masterKey: masterKey}
}

var slugDisallowed = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify converts a display name into a stable tool-namespace prefix.
func Slugify(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := slugDisallowed.ReplaceAllString(b.String(), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "mcp"
	}
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

// normalizeSlug keeps user-provided slugs safe for namespaced tool names.
func normalizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return ""
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func (s *Store) uniqueSlug(base, excludeID string) (string, error) {
	if base == "" {
		base = "mcp"
	}
	for i := 0; i < 100; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", base, i+1)
		}
		var existing string
		err := s.db.QueryRow(db.Q(`SELECT id FROM mcp_servers WHERE slug=?`), candidate).Scan(&existing)
		if err == sql.ErrNoRows {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
		if existing == excludeID {
			return candidate, nil
		}
	}
	return "", ErrDuplicateSlug
}

// ServerInput is the validated write payload from the admin API.
//
// Env and Headers are POINTERS so the payload can express three states:
//   - nil (key absent from the JSON)  -> keep the stored blob
//   - {}   (explicitly empty object)  -> clear it
//   - any other map                   -> replace it
//
// They must be tri-state because the secret values are never returned by the
// API (`json:"-"`), so the dashboard form cannot repopulate them. With plain
// maps, the form posted `{}` on every save and `Update` wrote NULL over both
// blobs — renaming a server destroyed its GITHUB_TOKEN and static auth headers
// irrecoverably. auth_value was already protected against exactly this; env and
// headers were not.
type ServerInput struct {
	Name       string             `json:"name"`
	Slug       string             `json:"slug"`
	Transport  string             `json:"transport"`
	URL        string             `json:"url"`
	Command    string             `json:"command"`
	Args       []string           `json:"args"`
	Env        *map[string]string `json:"env,omitempty"`
	Headers    *map[string]string `json:"headers,omitempty"`
	AuthType   string             `json:"auth_type"`
	AuthHeader string             `json:"auth_header"`
	AuthValue  string             `json:"auth_value"`
	Enabled    *bool              `json:"enabled"`
	OrgID      string             `json:"org_id"`
}

// validate checks the payload shape. keepExistingAuth relaxes the
// "auth_value required" rule for updates, where a blank value means "keep the
// stored credential" rather than "no credential" — otherwise editing an
// unrelated field (the name, a header) would silently wipe a working token.
func (in *ServerInput) validate(keepExistingAuth bool) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return errors.New("name is required")
	}
	if len(in.Name) > 128 {
		return errors.New("name is too long (max 128)")
	}
	switch Transport(in.Transport) {
	case TransportHTTP, TransportSSE:
		if strings.TrimSpace(in.URL) == "" {
			return errors.New("url is required for http/sse transport")
		}
		if len(in.URL) > 2048 {
			return errors.New("url is too long")
		}
		if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
			return errors.New("url must start with http:// or https://")
		}
	case TransportStdio:
		if strings.TrimSpace(in.Command) == "" {
			return errors.New("command is required for stdio transport")
		}
	default:
		return fmt.Errorf("invalid transport %q (want http, sse or stdio)", in.Transport)
	}
	switch AuthType(in.AuthType) {
	case "", AuthNone:
		in.AuthType = string(AuthNone)
	case AuthBearer, AuthAPIKey:
		if len(strings.TrimSpace(in.AuthValue)) == 0 && !keepExistingAuth {
			return fmt.Errorf("auth_value is required for %s auth", in.AuthType)
		}
	case AuthHeader:
		if strings.TrimSpace(in.AuthHeader) == "" {
			return errors.New("auth_header is required for header auth")
		}
		if strings.TrimSpace(in.AuthValue) == "" && !keepExistingAuth {
			return errors.New("auth_value is required for header auth")
		}
	default:
		return fmt.Errorf("invalid auth_type %q", in.AuthType)
	}
	return nil
}

func (s *Store) encrypt(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 || string(b) == "{}" || string(b) == "null" {
		return nil, nil
	}
	return provider.Encrypt(b, s.masterKey)
}

// encryptPtr is the tri-state variant of encrypt used by Update: a nil pointer
// means "the caller did not send this field, keep what is stored", which is
// distinguishable from an explicit empty map ("clear it").
func (s *Store) encryptPtr(v *map[string]string) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return s.encrypt(*v)
}

func (s *Store) decryptMap(blob []byte) map[string]string {
	if len(blob) == 0 {
		return nil
	}
	plain, err := provider.Decrypt(blob, s.masterKey)
	if err != nil {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(plain, &m) != nil {
		return nil
	}
	return m
}

func (s *Store) decryptString(blob []byte) string {
	if len(blob) == 0 {
		return ""
	}
	plain, err := provider.Decrypt(blob, s.masterKey)
	if err != nil {
		return ""
	}
	return string(plain)
}

func (s *Store) Create(in ServerInput) (*Server, error) {
	if err := in.validate(false); err != nil {
		return nil, err
	}
	slugBase := normalizeSlug(in.Slug)
	if slugBase == "" {
		slugBase = Slugify(in.Name)
	}
	slug, err := s.uniqueSlug(slugBase, "")
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	argsJSON, _ := json.Marshal(in.Args)
	var envMap, headersMap map[string]string
	if in.Env != nil {
		envMap = *in.Env
	}
	if in.Headers != nil {
		headersMap = *in.Headers
	}
	envEnc, err := s.encrypt(envMap)
	if err != nil {
		return nil, err
	}
	headersEnc, err := s.encrypt(headersMap)
	if err != nil {
		return nil, err
	}
	var authEnc []byte
	if in.AuthValue != "" {
		authEnc, err = provider.Encrypt([]byte(in.AuthValue), s.masterKey)
		if err != nil {
			return nil, err
		}
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	var org any
	if in.OrgID != "" {
		org = in.OrgID
	}
	_, err = s.db.Exec(db.Q(`INSERT INTO mcp_servers
		(id,name,slug,transport,url,command,args,env_enc,headers_enc,auth_type,auth_header,auth_value_enc,enabled,health_status,tool_count,org_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		id, in.Name, slug, in.Transport, nullStr(in.URL), nullStr(in.Command), string(argsJSON),
		envEnc, headersEnc, in.AuthType, nullStr(in.AuthHeader), authEnc, db.BoolInt(enabled), "unknown", 0, org, now, now)
	if err != nil {
		return nil, err
	}
	return s.GetServer(id)
}

func (s *Store) Update(id string, in ServerInput) (*Server, error) {
	existing, err := s.GetServer(id)
	if err != nil {
		return nil, err
	}
	// A blank auth_value keeps the credential already on the row (see
	// validate); pass the flag so validation agrees with the write below.
	if err := in.validate(strings.TrimSpace(in.AuthValue) == "" && existing.HasAuth); err != nil {
		return nil, err
	}
	slugBase := normalizeSlug(in.Slug)
	if slugBase == "" {
		slugBase = existing.Slug
	}
	slug, err := s.uniqueSlug(slugBase, existing.ID)
	if err != nil {
		return nil, err
	}
	argsJSON, _ := json.Marshal(in.Args)
	// env_enc / headers_enc are only rewritten when the caller actually sent the
	// field (nil = keep). See ServerInput's doc comment: a plain map cannot
	// distinguish "the form left it blank" from "clear it", and the blank case
	// is what silently destroyed stored credentials on an unrelated edit.
	envEnc, err := s.encryptPtr(in.Env)
	if err != nil {
		return nil, err
	}
	headersEnc, err := s.encryptPtr(in.Headers)
	if err != nil {
		return nil, err
	}
	// Credential rotation: an empty auth_value keeps the stored secret (so the
	// UI can edit unrelated fields without re-typing it).
	var authEnc any
	if in.AuthValue != "" {
		b, err := provider.Encrypt([]byte(in.AuthValue), s.masterKey)
		if err != nil {
			return nil, err
		}
		authEnc = b
	} else {
		authEnc = nil
	}
	enabled := existing.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	now := time.Now().UTC()
	query := `UPDATE mcp_servers SET name=?,slug=?,transport=?,url=?,command=?,args=?,auth_type=?,auth_header=?,enabled=?,updated_at=?`
	params := []any{in.Name, slug, in.Transport, nullStr(in.URL), nullStr(in.Command), string(argsJSON), in.AuthType, nullStr(in.AuthHeader), db.BoolInt(enabled), now}
	if in.Env != nil {
		query += `,env_enc=?`
		params = append(params, envEnc)
	}
	if in.Headers != nil {
		query += `,headers_enc=?`
		params = append(params, headersEnc)
	}
	// org_id was previously accepted in the payload, validated, and then
	// silently dropped: an admin could not move a server between orgs and PUT
	// answered 200 with the old org, which reads as success.
	if strings.TrimSpace(in.OrgID) != "" {
		query += `,org_id=?`
		params = append(params, in.OrgID)
	}
	if authEnc != nil || in.AuthType == string(AuthNone) {
		query += `,auth_value_enc=?`
		if in.AuthType == string(AuthNone) {
			params = append(params, nil)
		} else {
			params = append(params, authEnc)
		}
	}
	query += ` WHERE id=?`
	params = append(params, id)
	if _, err := s.db.Exec(db.Q(query), params...); err != nil {
		return nil, err
	}
	return s.GetServer(id)
}

func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func (s *Store) GetServer(idOrSlug string) (*Server, error) {
	row := s.db.QueryRow(db.Q(`SELECT id,name,slug,transport,COALESCE(url,''),COALESCE(command,''),COALESCE(args,'[]'),env_enc,headers_enc,auth_type,COALESCE(auth_header,''),auth_value_enc,enabled,health_status,COALESCE(last_error,''),last_health_at,tool_count,org_id,created_at,updated_at FROM mcp_servers WHERE id=? OR slug=?`), idOrSlug, idOrSlug)
	srv, err := scanServer(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return srv, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanServer(row rowScanner) (*Server, error) {
	var srv Server
	var argsJSON string
	var envEnc, headersEnc, authEnc []byte
	var enabled int
	var lastHealth sql.NullTime
	var org sql.NullString
	if err := row.Scan(&srv.ID, &srv.Name, &srv.Slug, &srv.Transport, &srv.URL, &srv.Command, &argsJSON,
		&envEnc, &headersEnc, &srv.AuthType, &srv.AuthHeader, &authEnc, &enabled, &srv.HealthStatus,
		&srv.LastError, &lastHealth, &srv.ToolCount, &org, &srv.CreatedAt, &srv.UpdatedAt); err != nil {
		return nil, err
	}
	srv.Enabled = enabled != 0
	if lastHealth.Valid {
		t := lastHealth.Time
		srv.LastHealthAt = &t
	}
	if org.Valid {
		srv.OrgID = &org.String
	}
	_ = json.Unmarshal([]byte(argsJSON), &srv.Args)
	srv.HasEnv = len(envEnc) > 0
	srv.HasHeaders = len(headersEnc) > 0
	srv.HasAuth = len(authEnc) > 0
	srv.Env = nil
	srv.Headers = nil
	srv.AuthValue = ""
	return &srv, nil
}

// RuntimeServer returns a copy with decrypted secrets, for connection use only.
func (s *Store) RuntimeServer(idOrSlug string) (*Server, error) {
	srv, err := s.GetServer(idOrSlug)
	if err != nil {
		return nil, err
	}
	// Re-read encrypted blobs (scanServer deliberately drops them).
	var envEnc, headersEnc, authEnc []byte
	if err := s.db.QueryRow(db.Q(`SELECT env_enc,headers_enc,auth_value_enc FROM mcp_servers WHERE id=?`), srv.ID).Scan(&envEnc, &headersEnc, &authEnc); err != nil {
		return nil, err
	}
	srv.Env = s.decryptMap(envEnc)
	srv.Headers = s.decryptMap(headersEnc)
	srv.AuthValue = s.decryptString(authEnc)
	return srv, nil
}

func (s *Store) ListServers() ([]Server, error) {
	rows, err := s.db.Query(db.Q(`SELECT id,name,slug,transport,COALESCE(url,''),COALESCE(command,''),COALESCE(args,'[]'),env_enc,headers_enc,auth_type,COALESCE(auth_header,''),auth_value_enc,enabled,health_status,COALESCE(last_error,''),last_health_at,tool_count,org_id,created_at,updated_at FROM mcp_servers ORDER BY name`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Server
	for rows.Next() {
		srv, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *srv)
	}
	if out == nil {
		out = []Server{}
	}
	return out, rows.Err()
}

func (s *Store) Delete(id string) error {
	res, err := s.db.Exec(db.Q(`DELETE FROM mcp_servers WHERE id=?`), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.db.Exec(db.Q(`DELETE FROM mcp_server_tools WHERE server_id=?`), id)
	s.db.Exec(db.Q(`DELETE FROM mcp_key_permissions WHERE server_id=?`), id)
	return nil
}

func (s *Store) SetEnabled(id string, enabled bool) error {
	res, err := s.db.Exec(db.Q(`UPDATE mcp_servers SET enabled=?, updated_at=? WHERE id=?`), db.BoolInt(enabled), time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetHealth(id, status, lastErr string) error {
	_, err := s.db.Exec(db.Q(`UPDATE mcp_servers SET health_status=?, last_error=?, last_health_at=? WHERE id=?`), status, lastErr, time.Now().UTC(), id)
	return err
}

// SaveTools replaces the cached tool list for a server atomically.
func (s *Store) SaveTools(serverID string, tools []Tool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(db.Q(`DELETE FROM mcp_server_tools WHERE server_id=?`), serverID); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, t := range tools {
		schema := string(t.InputSchema)
		if schema == "" {
			schema = `{"type":"object"}`
		}
		if _, err := tx.Exec(db.Q(`INSERT INTO mcp_server_tools(server_id,name,description,input_schema,discovered_at) VALUES(?,?,?,?,?)`),
			serverID, t.Name, t.Description, schema, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(db.Q(`UPDATE mcp_servers SET tool_count=?, updated_at=? WHERE id=?`), len(tools), now, serverID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListTools(serverID string) ([]Tool, error) {
	srv, err := s.GetServer(serverID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(db.Q(`SELECT server_id,name,COALESCE(description,''),COALESCE(input_schema,'{"type":"object"}'),discovered_at FROM mcp_server_tools WHERE server_id=? ORDER BY name`), srv.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tool
	for rows.Next() {
		var t Tool
		var schema string
		if err := rows.Scan(&t.ServerID, &t.Name, &t.Description, &schema, &t.DiscoveredAt); err != nil {
			return nil, err
		}
		t.ServerSlug = srv.Slug
		t.ServerName = srv.Name
		t.NamespacedName = Namespaced(srv.Slug, t.Name)
		t.InputSchema = json.RawMessage(schema)
		out = append(out, t)
	}
	if out == nil {
		out = []Tool{}
	}
	return out, rows.Err()
}

// AllTools returns cached tools for every enabled server.
func (s *Store) AllTools() ([]Tool, error) {
	servers, err := s.ListServers()
	if err != nil {
		return nil, err
	}
	var out []Tool
	for _, srv := range servers {
		if !srv.Enabled {
			continue
		}
		tools, err := s.ListTools(srv.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, tools...)
	}
	if out == nil {
		out = []Tool{}
	}
	return out, nil
}

// Namespaced builds the collision-free client-facing tool name.
func Namespaced(slug, tool string) string {
	return slug + "-" + tool
}

// --- per-key grants -------------------------------------------------------

func (s *Store) ListKeyPermissions(keyID string) ([]KeyPermission, error) {
	rows, err := s.db.Query(db.Q(`SELECT key_id,server_id,COALESCE(allowed_tools,''),enabled,created_at,updated_at FROM mcp_key_permissions WHERE key_id=?`), keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyPermission
	for rows.Next() {
		var p KeyPermission
		var allowed string
		var enabled int
		if err := rows.Scan(&p.KeyID, &p.ServerID, &allowed, &enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		if strings.TrimSpace(allowed) != "" {
			_ = json.Unmarshal([]byte(allowed), &p.AllowedTools)
		}
		p.Enabled = enabled != 0
		out = append(out, p)
	}
	if out == nil {
		out = []KeyPermission{}
	}
	return out, rows.Err()
}

func (s *Store) SetKeyPermissions(keyID string, perms []KeyPermission) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(db.Q(`DELETE FROM mcp_key_permissions WHERE key_id=?`), keyID); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, p := range perms {
		allowed, _ := json.Marshal(p.AllowedTools)
		if _, err := tx.Exec(db.Q(`INSERT INTO mcp_key_permissions(key_id,server_id,allowed_tools,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?)`),
			keyID, p.ServerID, string(allowed), db.BoolInt(p.Enabled), now, now); err != nil {
			return err
		}
	}
	// "Revoke all" must actually revoke. Deleting every grant row made the key
	// indistinguishable from one that was never configured, and an unconfigured
	// key is allowed every enabled tool — so the most common admin action
	// (remove all MCP access) silently granted the widest possible access. The
	// marker records that this key has been explicitly configured, which makes
	// AccessForKey fail closed on an empty row set.
	if _, err := tx.Exec(db.Q(`INSERT INTO mcp_key_configured(key_id,configured_at) VALUES(?,?)
		ON CONFLICT(key_id) DO UPDATE SET configured_at=excluded.configured_at`), keyID, now); err != nil {
		return err
	}
	return tx.Commit()
}

// keyConfigured reports whether the operator has ever set this key's grants.
func (s *Store) keyConfigured(keyID string) (bool, error) {
	var n int
	err := s.db.QueryRow(db.Q(`SELECT COUNT(*) FROM mcp_key_configured WHERE key_id=?`), keyID).Scan(&n)
	if err != nil {
		// Table absent on a deployment that predates this migration: treat as
		// unconfigured (the historical permissive default) rather than denying.
		return false, nil
	}
	return n > 0, nil
}

// Access describes the effective tools a key may use.
type Access struct {
	Servers []Server
	Tools   []Tool
	// Explicit is true when the key has at least one grant row (which turns
	// the default allow-all into an allowlist).
	Explicit bool
}

// AccessForKey resolves the effective server/tool set for a gateway key.
// A key with no grant rows sees every enabled server and tool (the default
// remains permissive, matching a self-hosted single-tenant deployment);
// adding a row switches that key to an explicit allowlist.
func (s *Store) AccessForKey(key *models.GatewayKey) (*Access, error) {
	servers, err := s.ListServers()
	if err != nil {
		return nil, err
	}
	access := &Access{}
	perms, err := s.ListKeyPermissions(key.ID)
	if err != nil {
		return nil, err
	}
	access.Explicit = len(perms) > 0
	if !access.Explicit {
		// An explicit "no grants" state (see SetKeyPermissions) is a DENY, not
		// the historical allow-all default.
		if configured, cerr := s.keyConfigured(key.ID); cerr == nil && configured {
			access.Explicit = true
		}
	}
	permByServer := make(map[string]KeyPermission, len(perms))
	for _, p := range perms {
		permByServer[p.ServerID] = p
	}
	for _, srv := range servers {
		if !srv.Enabled {
			continue
		}
		// Tenant isolation: a server owned by another org is invisible to this
		// key, exactly like the admin API's org scoping. A server with no org
		// is global (shared), which is the historical single-tenant default.
		if srv.OrgID != nil && *srv.OrgID != "" && (key.OrgID == nil || *key.OrgID != *srv.OrgID) {
			continue
		}
		allowedTools := []string(nil)
		if access.Explicit {
			p, ok := permByServer[srv.ID]
			if !ok || !p.Enabled {
				continue
			}
			allowedTools = p.AllowedTools
		}
		tools, err := s.ListTools(srv.ID)
		if err != nil {
			return nil, err
		}
		var kept []Tool
		for _, t := range tools {
			if len(allowedTools) == 0 || toolListContains(allowedTools, t.Name, t.NamespacedName) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 && len(allowedTools) > 0 {
			// Server grants no usable tools; omit it.
			continue
		}
		access.Servers = append(access.Servers, srv)
		access.Tools = append(access.Tools, kept...)
	}
	return access, nil
}

func toolListContains(list []string, names ...string) bool {
	for _, want := range list {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		for _, n := range names {
			if want == n {
				return true
			}
		}
	}
	return false
}

func (s *Store) RecordCall(rec CallRecord) {
	if rec.ID == "" {
		rec.ID = uuid.NewString()
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	_, _ = s.db.Exec(db.Q(`INSERT INTO mcp_tool_calls(id,server_id,server_slug,tool_name,namespaced_name,key_prefix,endpoint,status,error,duration_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`),
		rec.ID, rec.ServerID, rec.ServerSlug, rec.ToolName, rec.NamespacedName, rec.KeyPrefix, rec.Endpoint, rec.Status, rec.Error, rec.DurationMs, rec.CreatedAt)
}

// RecentCalls returns the most recent tool-call audit rows.
func (s *Store) RecentCalls(limit int) ([]CallRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(db.Q(`SELECT id,server_id,server_slug,tool_name,namespaced_name,COALESCE(key_prefix,''),COALESCE(endpoint,''),status,COALESCE(error,''),duration_ms,created_at FROM mcp_tool_calls ORDER BY created_at DESC LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CallRecord
	for rows.Next() {
		var r CallRecord
		if err := rows.Scan(&r.ID, &r.ServerID, &r.ServerSlug, &r.ToolName, &r.NamespacedName, &r.KeyPrefix, &r.Endpoint, &r.Status, &r.Error, &r.DurationMs, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
