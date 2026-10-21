package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Protocol identifies the inbound LLM API dialect whose body we rewrite.
type Protocol string

const (
	ProtocolChat      Protocol = "chat"
	ProtocolResponses Protocol = "responses"
	ProtocolAnthropic Protocol = "anthropic"
)

// Reference is one LiteLLM-compatible MCP reference block. server_url is the
// literal sentinel "litellm_proxy" (optionally with /mcp/<servers>), never a
// network URL.
type Reference struct {
	ServerURL       string          `json:"server_url"`
	ServerLabel     string          `json:"server_label"`
	RequireApproval json.RawMessage `json:"require_approval"`
	AllowedTools    []string        `json:"allowed_tools"`
}

// Binding maps a client-facing (namespaced) tool name to its upstream tool.
type Binding struct {
	ServerID   string
	ServerSlug string
	ToolName   string
	Namespaced string
	Auto       bool
}

// Plan is the resolved outcome of expanding MCP references for one request.
type Plan struct {
	Bindings    map[string]Binding
	AutoNames   map[string]bool
	Servers     []Server
	AutoExecute bool
}

// ReferencesFromBody extracts MCP reference blocks from a tools array. It is
// protocol-agnostic: all three supported dialects carry mcp entries in
// "tools" with type "mcp".
func ReferencesFromBody(body []byte) ([]Reference, bool) {
	var probe struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, false
	}
	var refs []Reference
	for _, t := range probe.Tools {
		var typ string
		if raw, ok := t["type"]; ok {
			_ = json.Unmarshal(raw, &typ)
		}
		if typ != "mcp" {
			continue
		}
		var ref Reference
		b, _ := json.Marshal(t)
		if json.Unmarshal(b, &ref) == nil {
			refs = append(refs, ref)
		}
	}
	return refs, len(refs) > 0
}

type selection int

const (
	selectionAll selection = iota
	selectionNamed
)

// parseServerSelection interprets the litellm_proxy server_url grammar:
//
//	litellm_proxy                -> all authorized servers
//	litellm_proxy/mcp            -> all authorized servers
//	litellm_proxy/mcp/github     -> one server
//	litellm_proxy/mcp/github,fs  -> multiple servers
func parseServerSelection(serverURL string) (selection, []string, error) {
	s := strings.TrimSpace(serverURL)
	if s == "" {
		return selectionAll, nil, nil
	}
	if !strings.HasPrefix(s, "litellm_proxy") {
		return 0, nil, fmt.Errorf("invalid mcp server_url %q: expected the literal sentinel \"litellm_proxy\" (or litellm_proxy/mcp/<servers>)", serverURL)
	}
	rest := strings.TrimPrefix(s, "litellm_proxy")
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		return selectionAll, nil, nil
	}
	rest = strings.TrimPrefix(rest, "mcp")
	rest = strings.TrimPrefix(rest, "/")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return selectionAll, nil, nil
	}
	var names []string
	for _, part := range strings.Split(rest, ",") {
		if p := strings.TrimSpace(part); p != "" {
			names = append(names, p)
		}
	}
	if len(names) == 0 {
		return selectionAll, nil, nil
	}
	return selectionNamed, names, nil
}

// approvalNever reports whether a require_approval value means "execute
// automatically". Anything else preserves caller-controlled approval.
func approvalNever(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.EqualFold(strings.TrimSpace(s), "never")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		_, hasNever := obj["never"]
		return hasNever
	}
	return false
}

// BuildPlan resolves references against a key's effective access and returns
// the concrete tool definitions needed to expand the request body.
func BuildPlan(access *Access, refs []Reference) (*Plan, []Tool, error) {
	plan := &Plan{Bindings: map[string]Binding{}, AutoNames: map[string]bool{}}
	serverBySlug := map[string]Server{}
	serverByName := map[string]Server{}
	toolsByServer := map[string][]Tool{}
	for _, srv := range access.Servers {
		serverBySlug[strings.ToLower(srv.Slug)] = srv
		serverByName[strings.ToLower(srv.Name)] = srv
	}
	for _, t := range access.Tools {
		toolsByServer[t.ServerID] = append(toolsByServer[t.ServerID], t)
	}
	var selectedTools []Tool
	for _, ref := range refs {
		sel, names, err := parseServerSelection(ref.ServerURL)
		if err != nil {
			return nil, nil, err
		}
		var selected []Server
		if sel == selectionAll {
			selected = access.Servers
		} else {
			seen := map[string]bool{}
			for _, name := range names {
				key := strings.ToLower(strings.TrimSpace(name))
				srv, ok := serverBySlug[key]
				if !ok {
					srv, ok = serverByName[key]
				}
				if !ok {
					return nil, nil, fmt.Errorf("mcp server %q is not registered or not authorized for this key", name)
				}
				if seen[srv.ID] {
					continue
				}
				seen[srv.ID] = true
				selected = append(selected, srv)
			}
		}
		auto := approvalNever(ref.RequireApproval)
		allowed := normalizeToolList(ref.AllowedTools)
		for _, srv := range selected {
			for _, t := range toolsByServer[srv.ID] {
				if len(allowed) > 0 && !toolListContains(allowed, t.Name, t.NamespacedName) {
					continue
				}
				if _, exists := plan.Bindings[t.NamespacedName]; exists {
					continue
				}
				b := Binding{ServerID: srv.ID, ServerSlug: srv.Slug, ToolName: t.Name, Namespaced: t.NamespacedName, Auto: auto}
				plan.Bindings[t.NamespacedName] = b
				if auto {
					plan.AutoNames[t.NamespacedName] = true
					plan.AutoExecute = true
				}
				selectedTools = append(selectedTools, t)
				plan.Servers = appendServerUnique(plan.Servers, srv)
			}
		}
	}
	if len(plan.Bindings) == 0 {
		return nil, nil, errors.New("mcp_no_tools_available: no authorized tools matched the request's mcp references")
	}
	return plan, selectedTools, nil
}

func normalizeToolList(list []string) []string {
	var out []string
	for _, s := range list {
		if v := strings.TrimSpace(s); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func appendServerUnique(list []Server, srv Server) []Server {
	for _, s := range list {
		if s.ID == srv.ID {
			return list
		}
	}
	return append(list, srv)
}

// ExpandBody replaces mcp reference entries with ordinary function tools for
// the inbound protocol, filtered to the plan.
func ExpandBody(body []byte, proto Protocol, plan *Plan, tools []Tool) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}
	var existing []map[string]json.RawMessage
	if raw, ok := root["tools"]; ok && len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, &existing)
	}
	var kept []map[string]json.RawMessage
	for _, t := range existing {
		var typ string
		if raw, ok := t["type"]; ok {
			_ = json.Unmarshal(raw, &typ)
		}
		if typ == "mcp" {
			continue
		}
		kept = append(kept, t)
	}
	toolByName := map[string]Tool{}
	for _, t := range tools {
		toolByName[t.NamespacedName] = t
	}
	for _, name := range sortedBindingNames(plan.Bindings) {
		b := plan.Bindings[name]
		t, ok := toolByName[name]
		if !ok {
			t = Tool{Name: b.ToolName, NamespacedName: b.Namespaced, InputSchema: json.RawMessage(`{"type":"object"}`)}
		}
		kept = append(kept, functionTool(proto, b, t))
	}
	if len(kept) == 0 {
		delete(root, "tools")
	} else {
		raw, err := json.Marshal(kept)
		if err != nil {
			return nil, err
		}
		root["tools"] = raw
	}
	return json.Marshal(root)
}

func functionTool(proto Protocol, b Binding, tool Tool) map[string]json.RawMessage {
	schema := tool.InputSchema
	if len(schema) == 0 || string(schema) == "null" {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	desc, _ := json.Marshal(tool.Description)
	name, _ := json.Marshal(b.Namespaced)
	switch proto {
	case ProtocolAnthropic:
		return map[string]json.RawMessage{
			"name":         name,
			"description":  desc,
			"input_schema": schema,
		}
	case ProtocolResponses:
		return map[string]json.RawMessage{
			"type":        json.RawMessage(`"function"`),
			"name":        name,
			"description": desc,
			"parameters":  schema,
		}
	default:
		fn, _ := json.Marshal(map[string]json.RawMessage{
			"name":        name,
			"description": desc,
			"parameters":  schema,
		})
		return map[string]json.RawMessage{
			"type":     json.RawMessage(`"function"`),
			"function": fn,
		}
	}
}

func sortedBindingNames(m map[string]Binding) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
