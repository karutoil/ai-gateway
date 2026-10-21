package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/middleware"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog/log"
)

// Endpoint serves the unified MCP surface: /mcp (Streamable HTTP), the
// LiteLLM-compatible REST helpers, and the per-server path form.
type Endpoint struct {
	Store   *Store
	Manager *Manager
	APIKeys *apikey.Store

	// Version is reported to MCP clients in serverInfo. It is assigned from
	// handler.GatewayVersion by main so there is exactly ONE version string in
	// the binary (this package cannot import handler — handler imports mcp).
	// Left empty in tests, where VersionOrDefault supplies a placeholder.
	Version string

	handler *sdk.StreamableHTTPHandler
}

// VersionOrDefault is the version reported to MCP clients.
func (e *Endpoint) VersionOrDefault() string {
	if e.Version != "" {
		return e.Version
	}
	return "dev"
}

func NewEndpoint(store *Store, manager *Manager, keys *apikey.Store) *Endpoint {
	e := &Endpoint{Store: store, Manager: manager, APIKeys: keys}
	e.handler = sdk.NewStreamableHTTPHandler(e.serverForRequest, &sdk.StreamableHTTPOptions{
		// Stateful sessions: MCP clients (Pi, OpenCode, Claude Code) negotiate
		// once and reuse the session id; the SDK keeps the session map.
		JSONResponse: false,
	})
	return e
}

// Auth authenticates MCP clients with a gateway key. MCP traffic prefers
// x-litellm-api-key (LiteLLM convention) so Authorization stays free for
// upstream OAuth, but Authorization/x-api-key are accepted too.
func (e *Endpoint) Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeMCPAuthError(w, http.StatusUnauthorized, "missing gateway api key")
			return
		}
		if !strings.HasPrefix(token, "sk-gw-") {
			writeMCPAuthError(w, http.StatusUnauthorized, "invalid gateway api key")
			return
		}
		key, ok := e.APIKeys.Verify(token)
		if !ok {
			writeMCPAuthError(w, http.StatusUnauthorized, "invalid gateway api key")
			return
		}
		r = middleware.SetGatewayKey(r, key)
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	for _, h := range []string{"x-litellm-api-key", "x-api-key", "Authorization"} {
		v := strings.TrimSpace(r.Header.Get(h))
		if v == "" {
			continue
		}
		v = strings.TrimPrefix(v, "Bearer ")
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	if c, err := r.Cookie("gw_token"); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

func writeMCPAuthError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"error":   map[string]any{"code": -32001, "message": msg},
		"id":      nil,
	})
}

// ServeHTTP handles /mcp. The optional x-mcp-servers header (or ?servers=
// query) narrows the session to a comma-separated server list, matching
// LiteLLM's selection behavior.
func (e *Endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A path-selected server that is unknown or disabled must answer 404, not
	// serve an empty tool list: silently returning zero tools reads like a
	// broken server at the client.
	// Path-selection forms: /<server>/mcp and LiteLLM's /mcp/<server>.
	if r.URL.Query().Get("servers") == "" && r.URL.Path != "/mcp" {
		path := strings.Trim(r.URL.Path, "/")
		var server string
		switch {
		case strings.HasSuffix(path, "/mcp"):
			server = strings.TrimSuffix(path, "/mcp")
		case strings.HasPrefix(path, "mcp/"):
			server = strings.TrimPrefix(path, "mcp/")
		}
		if server != "" && !strings.Contains(server, "mcp/") {
			q := r.URL.Query()
			q.Set("servers", server)
			r.URL.RawQuery = q.Encode()
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("servers")); raw != "" {
		key, _ := middleware.GatewayKeyFromContext(r.Context())
		if key != nil {
			access, err := e.Store.AccessForKey(key)
			if err == nil {
				known := map[string]bool{}
				for _, srv := range access.Servers {
					known[strings.ToLower(srv.Slug)] = true
					known[strings.ToLower(srv.Name)] = true
				}
				for _, p := range strings.Split(raw, ",") {
					name := strings.ToLower(strings.TrimSpace(p))
					if name != "" && !known[name] {
						e.NotFound(w, "mcp server "+name+" is not registered or not authorized for this key")
						return
					}
				}
			}
		}
	}
	e.handler.ServeHTTP(w, r)
}

// ServeServerPath handles /{server}/mcp (and /{a,b}/mcp), selecting exactly
// the named server(s). Selection narrows existing access; it never grants.
func (e *Endpoint) ServeServerPath(w http.ResponseWriter, r *http.Request) {
	server := strings.Trim(strings.TrimSuffix(r.URL.Path, "/mcp"), "/")
	if server != "" {
		q := r.URL.Query()
		q.Set("servers", server)
		r.URL.RawQuery = q.Encode()
	}
	e.handler.ServeHTTP(w, r)
}

// NotFound answers unknown MCP paths (e.g. a disabled server selected in the
// path) with a JSON-RPC error rather than the SPA.
func (e *Endpoint) NotFound(w http.ResponseWriter, msg string) {
	writeMCPAuthError(w, http.StatusNotFound, msg)
}

func (e *Endpoint) serverForRequest(r *http.Request) *sdk.Server {
	key, ok := middleware.GatewayKeyFromContext(r.Context())
	if !ok || key == nil {
		return nil
	}
	access, err := e.Store.AccessForKey(key)
	if err != nil {
		log.Error().Err(err).Str("key", key.Prefix).Msg("mcp: resolve key access failed")
		return nil
	}
	allowed := requestedServers(r)
	// Audit attribution: record the path the client actually used
	// (/mcp, /<server>/mcp, /mcp/<server>) rather than a generic "/mcp".
	endpointPath := r.URL.Path
	server := sdk.NewServer(&sdk.Implementation{Name: "ai-gateway", Version: e.VersionOrDefault()}, nil)
	for _, tool := range access.Tools {
		if len(allowed) > 0 && !allowed[strings.ToLower(tool.ServerSlug)] && !allowed[strings.ToLower(tool.ServerName)] {
			continue
		}
		schema := inputSchemaMap(tool.InputSchema)
		binding := tool
		server.AddTool(&sdk.Tool{
			Name:        binding.NamespacedName,
			Description: binding.Description,
			InputSchema: schema,
		}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			args := map[string]any{}
			if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
				_ = json.Unmarshal(req.Params.Arguments, &args)
			}
			res, err := e.Manager.CallToolMeta(ctx, binding.ServerID, binding.Name, args, key.Prefix, endpointPath)
			if err != nil {
				return &sdk.CallToolResult{
					IsError: true,
					Content: []sdk.Content{&sdk.TextContent{Text: "tool execution failed: " + err.Error()}},
				}, nil
			}
			return &sdk.CallToolResult{
				IsError: res.IsError,
				Content: []sdk.Content{&sdk.TextContent{Text: res.Text}},
			}, nil
		})
	}
	return server
}

func requestedServers(r *http.Request) map[string]bool {
	raw := strings.TrimSpace(r.Header.Get("x-mcp-servers"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("servers"))
	}
	if raw == "" {
		return nil
	}
	out := map[string]bool{}
	for _, p := range strings.Split(raw, ",") {
		if v := strings.ToLower(strings.TrimSpace(p)); v != "" {
			out[v] = true
		}
	}
	return out
}

func inputSchemaMap(raw json.RawMessage) map[string]any {
	if len(raw) > 0 {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil && m != nil {
			if t, _ := m["type"].(string); t == "object" {
				return m
			}
			// MCP requires an object-rooted input schema; preserve properties
			// where possible instead of discarding the server's definition.
			if _, ok := m["properties"]; ok {
				m["type"] = "object"
				return m
			}
		}
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// --- LiteLLM-compatible REST helpers -------------------------------------

// ListServersREST handles GET /v1/mcp/server.
func (e *Endpoint) ListServersREST(w http.ResponseWriter, r *http.Request) {
	key, _ := middleware.GatewayKeyFromContext(r.Context())
	if key == nil {
		writeMCPAuthError(w, http.StatusUnauthorized, "missing gateway api key")
		return
	}
	access, err := e.Store.AccessForKey(key)
	if err != nil {
		http.Error(w, `{"error":"failed to resolve access"}`, http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(access.Servers))
	for _, srv := range access.Servers {
		out = append(out, map[string]any{
			"server_id":     srv.ID,
			"server_name":   srv.Slug,
			"name":          srv.Name,
			"transport":     srv.Transport,
			"health_status": srv.HealthStatus,
			"tool_count":    srv.ToolCount,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ListToolsREST handles GET /mcp-rest/tools/list.
func (e *Endpoint) ListToolsREST(w http.ResponseWriter, r *http.Request) {
	key, _ := middleware.GatewayKeyFromContext(r.Context())
	if key == nil {
		writeMCPAuthError(w, http.StatusUnauthorized, "missing gateway api key")
		return
	}
	access, err := e.Store.AccessForKey(key)
	if err != nil {
		http.Error(w, `{"error":"failed to resolve access"}`, http.StatusInternalServerError)
		return
	}
	want := strings.TrimSpace(r.URL.Query().Get("server_id"))
	tools := make([]map[string]any, 0, len(access.Tools))
	for _, t := range access.Tools {
		if want != "" && t.ServerID != want && t.ServerSlug != want {
			continue
		}
		tools = append(tools, map[string]any{
			"name":            t.Name,
			"namespaced_name": t.NamespacedName,
			"server_id":       t.ServerID,
			"server_name":     t.ServerSlug,
			"description":     t.Description,
			"input_schema":    json.RawMessage(t.InputSchema),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"tools": tools, "error": nil, "message": "ok"})
}

// CallToolREST handles POST /mcp-rest/tools/call.
func (e *Endpoint) CallToolREST(w http.ResponseWriter, r *http.Request) {
	key, _ := middleware.GatewayKeyFromContext(r.Context())
	if key == nil {
		writeMCPAuthError(w, http.StatusUnauthorized, "missing gateway api key")
		return
	}
	var body struct {
		ServerID string         `json:"server_id"`
		Name     string         `json:"name"`
		Args     map[string]any `json:"arguments"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.ServerID == "" {
		http.Error(w, `{"error":"missing_parameter","detail":"server_id is required"}`, http.StatusBadRequest)
		return
	}
	access, err := e.Store.AccessForKey(key)
	if err != nil {
		http.Error(w, `{"error":"failed to resolve access"}`, http.StatusInternalServerError)
		return
	}
	var found *Tool
	for i := range access.Tools {
		t := access.Tools[i]
		if t.ServerID != body.ServerID && t.ServerSlug != body.ServerID {
			continue
		}
		if t.Name == body.Name || t.NamespacedName == body.Name {
			found = &t
			break
		}
	}
	if found == nil {
		http.Error(w, `{"error":"tool_not_authorized","detail":"the named tool is not available to this key on that server"}`, http.StatusForbidden)
		return
	}
	res, err := e.Manager.CallToolMeta(r.Context(), found.ServerID, found.Name, body.Args, key.Prefix, "/mcp-rest/tools/call")
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"server_id":   found.ServerID,
		"server_name": found.ServerSlug,
		"tool":        found.Name,
		"is_error":    res.IsError,
		"content":     json.RawMessage(res.Content),
		"text":        res.Text,
		"duration_ms": res.DurationMs,
	})
}
