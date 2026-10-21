package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/audit"
	"ai-gateway/internal/auth"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/mcp"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/rbac"

	"github.com/go-chi/chi/v5"
)

// MCPHandler is the administration API for the centralized MCP registry.
type MCPHandler struct {
	Store    *mcp.Store
	Manager  *mcp.Manager
	Recorder audit.Recorder
}

// Routes registers the MCP management surface. Reads are available to every
// role that can view providers; writes follow the provider-writer policy.
func (h *MCPHandler) Routes(r chi.Router) {
	r.With(middleware.RequirePerm(rbac.PermMCPRead)).Get("/mcp/servers", h.ListServers)
	r.With(middleware.RequirePerm(rbac.PermMCPRead)).Get("/mcp/servers/{id}", h.GetServer)
	r.With(middleware.RequirePerm(rbac.PermMCPWrite)).Post("/mcp/servers", h.CreateServer)
	r.With(middleware.RequirePerm(rbac.PermMCPWrite)).Put("/mcp/servers/{id}", h.UpdateServer)
	r.With(middleware.RequirePerm(rbac.PermMCPWrite)).Delete("/mcp/servers/{id}", h.DeleteServer)
	r.With(middleware.RequirePerm(rbac.PermMCPWrite)).Post("/mcp/servers/{id}/enable", h.EnableServer)
	r.With(middleware.RequirePerm(rbac.PermMCPWrite)).Post("/mcp/servers/{id}/disable", h.DisableServer)
	r.With(middleware.RequirePerm(rbac.PermMCPWrite)).Post("/mcp/servers/{id}/test", h.TestServer)
	r.With(middleware.RequirePerm(rbac.PermMCPWrite)).Post("/mcp/servers/{id}/discover", h.DiscoverServer)
	r.With(middleware.RequirePerm(rbac.PermMCPRead)).Get("/mcp/servers/{id}/tools", h.ListServerTools)
	r.With(middleware.RequirePerm(rbac.PermMCPRead)).Get("/mcp/tools", h.ListAllTools)
	r.With(middleware.RequirePerm(rbac.PermMCPRead)).Get("/mcp/status", h.Status)
	r.With(middleware.RequirePerm(rbac.PermMCPRead)).Get("/mcp/calls", h.ListCalls)
	r.With(middleware.RequirePerm(rbac.PermMCPRead)).Get("/keys/{id}/mcp", h.GetKeyPermissions)
	r.With(middleware.RequirePerm(rbac.PermKeysLimits)).Put("/keys/{id}/mcp", h.SetKeyPermissions)
}

func (h *MCPHandler) audit(r *http.Request, action, target string, meta any) {
	if h.Recorder == nil {
		return
	}
	actor := auth.GetSubject(r)
	if actor == "" {
		actor = auth.GetRole(r)
	}
	b, _ := json.Marshal(meta)
	_ = h.Recorder.Log(actor, action, target, target, string(b))
}

// visibleServerIDs returns the set of server IDs the caller may see: global
// servers plus those owned by the caller's org. Admins see everything.
func (h *MCPHandler) visibleServerIDs(r *http.Request) (map[string]bool, bool) {
	role := auth.GetRole(r)
	org := auth.GetOrgID(r)
	servers, err := h.Store.ListServers()
	if err != nil {
		return nil, false
	}
	out := make(map[string]bool, len(servers))
	for _, srv := range servers {
		if role == "admin" || srv.OrgID == nil || *srv.OrgID == "" || *srv.OrgID == org {
			out[srv.ID] = true
		}
	}
	return out, true
}

func (h *MCPHandler) ListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := h.Store.ListServers()
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "failed to list mcp servers", httperr.TypeProxy)
		return
	}
	org := auth.GetOrgID(r)
	role := auth.GetRole(r)
	out := make([]mcp.Server, 0, len(servers))
	for _, srv := range servers {
		if role != "admin" && org != "" && srv.OrgID != nil && *srv.OrgID != "" && *srv.OrgID != org {
			continue
		}
		out = append(out, srv)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *MCPHandler) GetServer(w http.ResponseWriter, r *http.Request) {
	srv, err := h.Store.GetServer(chi.URLParam(r, "id"))
	if err != nil {
		httperr.NotFound(w, "mcp server not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(srv)
}

func (h *MCPHandler) CreateServer(w http.ResponseWriter, r *http.Request) {
	var in mcp.ServerInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		httperr.Invalid(w, "invalid json")
		return
	}
	// Non-admins can only create servers inside their verified org.
	if auth.GetRole(r) != "admin" {
		in.OrgID = auth.GetOrgID(r)
		if in.OrgID == "" {
			httperr.Forbidden(w, "org scope required")
			return
		}
	}
	srv, err := h.Store.Create(in)
	if err != nil {
		httperr.Invalid(w, err.Error())
		return
	}
	h.audit(r, "mcp_server_create", srv.ID, map[string]any{"name": srv.Name, "transport": srv.Transport, "slug": srv.Slug})
	// First discovery runs in the background so the create call is fast; the
	// dashboard polls /status or calls /discover explicitly.
	go func(id string) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if _, err := h.Manager.Discover(ctx, id); err != nil {
			h.Store.SetHealth(id, "down", err.Error())
		}
	}(srv.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(srv)
}

func (h *MCPHandler) UpdateServer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := h.Store.GetServer(id)
	if err != nil {
		httperr.NotFound(w, "mcp server not found")
		return
	}
	if auth.GetRole(r) != "admin" {
		org := auth.GetOrgID(r)
		if org == "" || existing.OrgID == nil || *existing.OrgID != org {
			httperr.NotFound(w, "mcp server not found")
			return
		}
	}
	var in mcp.ServerInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		httperr.Invalid(w, "invalid json")
		return
	}
	srv, err := h.Store.Update(existing.ID, in)
	if err != nil {
		httperr.Invalid(w, err.Error())
		return
	}
	h.audit(r, "mcp_server_update", srv.ID, map[string]any{"name": srv.Name, "transport": srv.Transport})
	// Reconnect with the new configuration and refresh the tool cache.
	h.Manager.CloseServer(srv.ID)
	go func(id string) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if _, err := h.Manager.Discover(ctx, id); err != nil {
			h.Store.SetHealth(id, "down", err.Error())
		}
	}(srv.ID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(srv)
}

func (h *MCPHandler) DeleteServer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := h.Store.GetServer(id)
	if err != nil {
		httperr.NotFound(w, "mcp server not found")
		return
	}
	if auth.GetRole(r) != "admin" {
		org := auth.GetOrgID(r)
		if org == "" || existing.OrgID == nil || *existing.OrgID != org {
			httperr.NotFound(w, "mcp server not found")
			return
		}
	}
	if err := h.Store.Delete(existing.ID); err != nil {
		httperr.Write(w, http.StatusInternalServerError, "delete failed", httperr.TypeProxy)
		return
	}
	h.Manager.CloseServer(existing.ID)
	h.audit(r, "mcp_server_delete", existing.ID, map[string]any{"name": existing.Name})
	w.WriteHeader(http.StatusNoContent)
}

func (h *MCPHandler) EnableServer(w http.ResponseWriter, r *http.Request) { h.setEnabled(w, r, true) }
func (h *MCPHandler) DisableServer(w http.ResponseWriter, r *http.Request) {
	h.setEnabled(w, r, false)
}

func (h *MCPHandler) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	id := chi.URLParam(r, "id")
	if err := h.Store.SetEnabled(id, enabled); err != nil {
		httperr.NotFound(w, "mcp server not found")
		return
	}
	if !enabled {
		h.Manager.CloseServer(id)
	} else {
		go func(id string) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			_, _ = h.Manager.Discover(ctx, id)
		}(id)
	}
	action := "mcp_server_disable"
	if enabled {
		action = "mcp_server_enable"
	}
	h.audit(r, action, id, nil)
	srv, _ := h.Store.GetServer(id)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(srv)
}

func (h *MCPHandler) TestServer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	tools, err := h.Manager.Discover(ctx, id)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		h.Store.SetHealth(id, "down", err.Error())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": false, "error": err.Error(), "latency_ms": latency,
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "tool_count": len(tools), "latency_ms": latency, "tools": tools,
	})
}

func (h *MCPHandler) DiscoverServer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	tools, err := h.Manager.Discover(ctx, id)
	if err != nil {
		httperr.Proxy(w, http.StatusBadGateway, "discovery failed: "+err.Error())
		return
	}
	h.audit(r, "mcp_server_discover", id, map[string]any{"tools": len(tools)})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "tools": tools})
}

func (h *MCPHandler) ListServerTools(w http.ResponseWriter, r *http.Request) {
	tools, err := h.Store.ListTools(chi.URLParam(r, "id"))
	if err != nil {
		httperr.NotFound(w, "mcp server not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tools)
}

func (h *MCPHandler) ListAllTools(w http.ResponseWriter, r *http.Request) {
	visible, ok := h.visibleServerIDs(r)
	if !ok {
		httperr.Write(w, http.StatusInternalServerError, "failed to list tools", httperr.TypeProxy)
		return
	}
	tools, err := h.Store.AllTools()
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "failed to list tools", httperr.TypeProxy)
		return
	}
	out := make([]mcp.Tool, 0, len(tools))
	for _, t := range tools {
		if visible[t.ServerID] {
			out = append(out, t)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *MCPHandler) Status(w http.ResponseWriter, r *http.Request) {
	servers, err := h.Store.ListServers()
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "failed to read status", httperr.TypeProxy)
		return
	}
	visible, ok := h.visibleServerIDs(r)
	if !ok {
		httperr.Write(w, http.StatusInternalServerError, "failed to read status", httperr.TypeProxy)
		return
	}
	up, down, disabled, tools, total := 0, 0, 0, 0, 0
	for _, srv := range servers {
		if !visible[srv.ID] {
			continue
		}
		total++
		if !srv.Enabled {
			disabled++
			continue
		}
		tools += srv.ToolCount
		switch srv.HealthStatus {
		case "up":
			up++
		case "down":
			down++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"servers":      total,
		"up":           up,
		"down":         down,
		"disabled":     disabled,
		"tools":        tools,
		"endpoint":     "/mcp",
		"max_rounds":   h.Manager.CallTimeout().String(),
		"generated_at": time.Now().UTC(),
	})
}

func (h *MCPHandler) ListCalls(w http.ResponseWriter, r *http.Request) {
	calls, err := h.Store.RecentCalls(200)
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "failed to list calls", httperr.TypeProxy)
		return
	}
	visible, ok := h.visibleServerIDs(r)
	if !ok {
		httperr.Write(w, http.StatusInternalServerError, "failed to list calls", httperr.TypeProxy)
		return
	}
	out := make([]mcp.CallRecord, 0, len(calls))
	for _, c := range calls {
		if visible[c.ServerID] {
			out = append(out, c)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *MCPHandler) GetKeyPermissions(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "id")
	perms, err := h.Store.ListKeyPermissions(keyID)
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "failed to read permissions", httperr.TypeProxy)
		return
	}
	servers, err := h.Store.ListServers()
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "failed to read servers", httperr.TypeProxy)
		return
	}
	// Return the effective view: every server with its grant state, so the UI
	// can render an allowlist editor without a second call.
	type row struct {
		ServerID     string   `json:"server_id"`
		ServerName   string   `json:"server_name"`
		ServerSlug   string   `json:"server_slug"`
		ToolCount    int      `json:"tool_count"`
		Granted      bool     `json:"granted"`
		AllowedTools []string `json:"allowed_tools"`
	}
	byServer := map[string]mcp.KeyPermission{}
	for _, p := range perms {
		byServer[p.ServerID] = p
	}
	rows := make([]row, 0, len(servers))
	for _, srv := range servers {
		p, ok := byServer[srv.ID]
		rows = append(rows, row{
			ServerID:     srv.ID,
			ServerName:   srv.Name,
			ServerSlug:   srv.Slug,
			ToolCount:    srv.ToolCount,
			Granted:      ok && p.Enabled,
			AllowedTools: p.AllowedTools,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"key_id":   keyID,
		"explicit": len(perms) > 0,
		"servers":  rows,
	})
}

func (h *MCPHandler) SetKeyPermissions(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "id")
	var body struct {
		Servers []struct {
			ServerID     string   `json:"server_id"`
			Granted      bool     `json:"granted"`
			AllowedTools []string `json:"allowed_tools"`
		} `json:"servers"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		httperr.Invalid(w, "invalid json")
		return
	}
	var perms []mcp.KeyPermission
	for _, s := range body.Servers {
		if !s.Granted {
			continue
		}
		var tools []string
		for _, t := range s.AllowedTools {
			if v := strings.TrimSpace(t); v != "" {
				tools = append(tools, v)
			}
		}
		perms = append(perms, mcp.KeyPermission{KeyID: keyID, ServerID: s.ServerID, AllowedTools: tools, Enabled: true})
	}
	if err := h.Store.SetKeyPermissions(keyID, perms); err != nil {
		httperr.Write(w, http.StatusInternalServerError, "failed to save permissions", httperr.TypeProxy)
		return
	}
	h.audit(r, "mcp_key_permissions", keyID, map[string]any{"servers": len(perms)})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "servers": len(perms)})
}
