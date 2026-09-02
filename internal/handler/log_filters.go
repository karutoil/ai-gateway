package handler

// Shared filter construction for the request-logs read surface:
//
//	GET /api/logs        (admin.go — paginated rows)
//	GET /api/logs/group  (log_group.go — grouped rollups)
//	GET /api/logs/export (admin.go — CSV of the filtered set)
//
// One builder keeps validation, org scoping and search semantics identical
// across all three, so a view filtered in the UI aggregates and exports
// exactly what it shows.

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
	"ai-gateway/internal/rbac"
)

// logFilterWhere builds the WHERE clause shared by the logs read endpoints.
//
// orgID scopes every query to the caller's organization ("" = unscoped /
// global admin). The providers LEFT JOIN is required whenever org scoping is
// active; callers must compose the FROM clause accordingly.
//
// Accepted params (all optional, unknown params ignored):
//
//	model, key (prefix), endpoint, status ("failed" or code), since (RFC3339 or Go duration),
//	key_id, provider_id, stream (true/false), min_latency_ms, max_latency_ms,
//	has_error (true), q (search: model, endpoint, error; bodies only with search_bodies=true)
func logFilterWhere(orgID string, q url.Values) (whereSQL string, args []any) {
	var where []string
	if orgID != "" {
		where = append(where, "p.org_id=?")
		args = append(args, orgID)
	}
	if m := strings.TrimSpace(q.Get("model")); m != "" {
		where = append(where, "(rl.model LIKE ? OR rl.model = ?)")
		args = append(args, "%"+m+"%", m)
	}
	if k := strings.TrimSpace(q.Get("key")); k != "" {
		where = append(where, "rl.key_prefix LIKE ?")
		args = append(args, k+"%")
	}
	if ep := strings.TrimSpace(q.Get("endpoint")); ep != "" {
		where = append(where, "rl.endpoint LIKE ?")
		args = append(args, "%"+ep+"%")
	}
	if s := strings.TrimSpace(q.Get("status")); s != "" {
		if s == "failed" {
			where = append(where, "rl.status >= 400")
		} else if code, err := strconv.Atoi(s); err == nil {
			where = append(where, "rl.status=?")
			args = append(args, code)
		}
	}
	if sv := strings.TrimSpace(q.Get("since")); sv != "" {
		if t, err := time.Parse(time.RFC3339, sv); err == nil {
			where = append(where, "rl.created_at >= ?")
			args = append(args, t.UTC())
		} else if d, err := time.ParseDuration(sv); err == nil && d > 0 {
			where = append(where, "rl.created_at >= ?")
			args = append(args, time.Now().UTC().Add(-d))
		}
	}

	// --- extended filters ---

	// Exact key attribution (the analytics column). Legacy rows have NULL.
	if kid := strings.TrimSpace(q.Get("key_id")); kid != "" {
		where = append(where, "rl.key_id = ?")
		args = append(args, kid)
	}
	// Exact provider.
	if pid := strings.TrimSpace(q.Get("provider_id")); pid != "" {
		where = append(where, "rl.provider_id = ?")
		args = append(args, pid)
	}
	// Stream / non-stream. db.BoolLit renders 1/0 vs TRUE/FALSE per dialect.
	if s := strings.TrimSpace(q.Get("stream")); s == "true" || s == "false" {
		where = append(where, "rl.is_stream = "+db.BoolLit(s == "true"))
	}
	if v, err := strconv.Atoi(q.Get("min_latency_ms")); err == nil && v >= 0 {
		where = append(where, "rl.latency_ms >= ?")
		args = append(args, v)
	}
	if v, err := strconv.Atoi(q.Get("max_latency_ms")); err == nil && v > 0 {
		where = append(where, "rl.latency_ms <= ?")
		args = append(args, v)
	}
	if strings.EqualFold(strings.TrimSpace(q.Get("has_error")), "true") {
		where = append(where, "rl.error IS NOT NULL")
	}
	// Server-side search. Default covers model/endpoint/error (cheap,
	// error has its own index); request/response bodies are opt-in because
	// LIKE over stored bodies is the expensive path.
	if search := strings.TrimSpace(q.Get("q")); search != "" {
		like := logSearchLIKE(search)
		parts := []string{
			"lower(rl.model) LIKE ? ESCAPE '\\'",
			"lower(rl.endpoint) LIKE ? ESCAPE '\\'",
			"lower(COALESCE(rl.error,'')) LIKE ? ESCAPE '\\'",
		}
		if strings.EqualFold(strings.TrimSpace(q.Get("search_bodies")), "true") {
			parts = append(parts,
				"lower(COALESCE(rl.request_body,'')) LIKE ? ESCAPE '\\'",
				"lower(COALESCE(rl.response_body,'')) LIKE ? ESCAPE '\\'")
		}
		clause := "(" + strings.Join(parts, " OR ") + ")"
		where = append(where, clause)
		for range parts {
			args = append(args, like)
		}
	}

	if len(where) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// logSearchLIKE escapes a user search term for a SQL LIKE with backslash
// escape semantics and lowercases it (case-insensitive match on SQLite's
// case-sensitive LIKE, consistent on Postgres which already lowercases LIKE).
func logSearchLIKE(term string) string {
	term = strings.ToLower(strings.TrimSpace(term))
	if len(term) > 200 {
		term = term[:200]
	}
	term = strings.ReplaceAll(term, `\`, `\\`)
	term = strings.ReplaceAll(term, `%`, `\%`)
	term = strings.ReplaceAll(term, `_`, `\_`)
	return "%" + term + "%"
}

// logScopeOwnOnly returns the caller's log-visibility scope:
//
//	hasAll=true  → sees everything the org scope allows (logs:read held)
//	hasAll=false → keys:read_own only: rows must be attributed to keys the
//	               caller created (request_logs.key_id IN (their keys))
//
// alias prefixes the column refs ("" for none — Stats uses unaliased
// request_logs; the list endpoints alias as rl). The returned WHERE fragment
// composes with logFilterWhere output; extraArgs append AFTER shared args.
// userID "" with hasAll=false yields an always-false clause (see nothing,
// fail closed).
func logScopeOwnOnly(r *http.Request, h *AdminHandler, alias string) (hasAll bool, extraWhere string, extraArgs []any) {
	col := func(name string) string {
		if alias == "" {
			return name
		}
		return alias + "." + name
	}
	role := auth.GetRole(r)
	if role == rbac.RoleAdmin {
		return true, "", nil
	}
	perms := auth.GetPerms(r)
	if perms == nil {
		perms = rbac.Defaults(role)
	}
	if rbac.Has(perms, rbac.PermLogsRead) {
		return true, "", nil
	}
	if !rbac.Has(perms, rbac.PermKeysReadOwn) {
		// Neither perm: RequireAnyPerm should have blocked; fail closed.
		return false, "1=0", nil
	}
	// Scope to keys the caller created. Dashboard-session traffic has
	// key_id NULL and is never attributable to a real key — excluded.
	uid := ""
	if h != nil && h.UserStore != nil {
		if u, _, err := h.UserStore.GetByUsername(auth.GetSubject(r)); err == nil {
			uid = u.ID
		}
	}
	if uid == "" {
		return false, "1=0", nil
	}
	return false, col("key_id") + " IN (SELECT id FROM gateway_keys WHERE created_by = ?)", []any{uid}
}
