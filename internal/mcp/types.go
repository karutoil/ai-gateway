// Package mcp implements the gateway's centralized Model Context Protocol
// management plane: a registry of upstream MCP servers, connection management,
// tool discovery, a unified /mcp endpoint, per-key grants, and the
// LiteLLM-compatible MCP reference expansion used by the LLM APIs.
package mcp

import (
	"encoding/json"
	"time"
)

// Transport is how the gateway reaches an upstream MCP server. Clients always
// reach the gateway itself over Streamable HTTP.
type Transport string

const (
	TransportHTTP  Transport = "http"  // Streamable HTTP (modern default)
	TransportSSE   Transport = "sse"   // legacy HTTP+SSE transport
	TransportStdio Transport = "stdio" // locally launched subprocess
)

// AuthType selects how the gateway authenticates to an upstream MCP server.
type AuthType string

const (
	AuthNone   AuthType = "none"
	AuthBearer AuthType = "bearer"  // Authorization: Bearer <value>
	AuthAPIKey AuthType = "api_key" // X-API-Key: <value> (or auth_header)
	AuthHeader AuthType = "header"  // <auth_header>: <value> verbatim
)

// Server is the persisted registry row plus derived, secret-free metadata.
// Secret material never appears in JSON: the dashboard only learns whether a
// secret is configured (HasAuth / HasEnv / HasHeaders).
type Server struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Slug         string     `json:"slug"`
	Transport    Transport  `json:"transport"`
	URL          string     `json:"url,omitempty"`
	Command      string     `json:"command,omitempty"`
	Args         []string   `json:"args,omitempty"`
	AuthType     AuthType   `json:"auth_type"`
	AuthHeader   string     `json:"auth_header,omitempty"`
	Enabled      bool       `json:"enabled"`
	HealthStatus string     `json:"health_status"`
	LastError    string     `json:"last_error,omitempty"`
	LastHealthAt *time.Time `json:"last_health_at,omitempty"`
	ToolCount    int        `json:"tool_count"`
	OrgID        *string    `json:"org_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`

	// Secret-free indicators for the UI.
	HasAuth    bool `json:"has_auth"`
	HasEnv     bool `json:"has_env"`
	HasHeaders bool `json:"has_headers"`

	// Secrets — only populated on write paths / runtime resolution. Never
	// marshaled (json:"-").
	Env       map[string]string `json:"-"`
	Headers   map[string]string `json:"-"`
	AuthValue string            `json:"-"`
}

// Tool is a discovered MCP tool. NamespacedName is what clients see and what
// LLM function tools are called; it is "<server-slug>-<tool-name>".
type Tool struct {
	ServerID       string          `json:"server_id"`
	ServerSlug     string          `json:"server_slug"`
	ServerName     string          `json:"server_name"`
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	InputSchema    json.RawMessage `json:"input_schema,omitempty"`
	NamespacedName string          `json:"namespaced_name"`
	DiscoveredAt   time.Time       `json:"discovered_at"`
}

// KeyPermission is one explicit per-key grant for a server. AllowedTools empty
// means every tool on the server.
type KeyPermission struct {
	KeyID        string    `json:"key_id"`
	ServerID     string    `json:"server_id"`
	AllowedTools []string  `json:"allowed_tools"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// CallRecord is the audit row written after a tool call.
type CallRecord struct {
	ID             string    `json:"id"`
	ServerID       string    `json:"server_id"`
	ServerSlug     string    `json:"server_slug"`
	ToolName       string    `json:"tool_name"`
	NamespacedName string    `json:"namespaced_name"`
	KeyPrefix      string    `json:"key_prefix"`
	Endpoint       string    `json:"endpoint"`
	Status         string    `json:"status"` // ok | error | denied
	Error          string    `json:"error,omitempty"`
	DurationMs     int64     `json:"duration_ms"`
	CreatedAt      time.Time `json:"created_at"`
}

// ToolResult is the normalized result of a tool call, protocol-agnostic.
type ToolResult struct {
	ServerSlug string          `json:"server_slug"`
	ServerID   string          `json:"server_id"`
	Tool       string          `json:"tool"`
	Namespaced string          `json:"name"`
	IsError    bool            `json:"is_error"`
	Text       string          `json:"text,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"`
	DurationMs int64           `json:"duration_ms"`
}
