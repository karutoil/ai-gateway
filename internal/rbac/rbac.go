// Package rbac implements the gateway's fine-grained permission system.
//
// Model:
//
//	effective(user) = (role defaults ∪ explicit grants) − explicit revokes
//
// A permission is a "resource:action" string from the fixed catalog below.
// Every dashboard user has a base role (admin|support|member|readonly) whose
// default set preserves the pre-RBAC behavior exactly; user_permissions rows
// then grant or revoke individual permissions on top. Admins always pass
// every check (by design they cannot be locked out).
//
// Resolution is intentionally dependency-free (pure functions over sets) so
// middleware, handlers and the user store all share one source of truth.
package rbac

import "sort"

// Permission names. Adding a new one requires no migration — the catalog is
// the single source of truth, and unknown strings fail closed everywhere.
const (
	// Keys (gateway API credentials)
	PermKeysRead    = "keys:read"     // view all keys
	PermKeysReadOwn = "keys:read_own" // view only keys created by the caller
	PermKeysCreate  = "keys:create"
	PermKeysUpdate  = "keys:update" // rename
	PermKeysRotate  = "keys:rotate" // regenerate the secret (grace keeps old working)
	PermKeysDelete  = "keys:delete" // revoke
	PermKeysLimits  = "keys:limits" // rate limits / model allowlists

	// Providers (upstream credentials)
	PermProvidersRead   = "providers:read"
	PermProvidersWrite  = "providers:write" // create + update
	PermProvidersTest   = "providers:test"
	PermProvidersDelete = "providers:delete"

	// Request logs & analytics
	PermLogsRead       = "logs:read"        // metadata (status, latency, tokens, cost)
	PermLogsReadBodies = "logs:read_bodies" // stored request/response payloads
	PermAnalyticsRead  = "analytics:read"

	// Routing / load-balancer rules
	PermRoutingRead  = "routing:read"
	PermRoutingWrite = "routing:write"

	// Model catalog
	PermCatalogRead  = "catalog:read"
	PermCatalogWrite = "catalog:write" // sync, aliases, settings

	// Organizations / teams
	PermOrgsRead  = "orgs:read"
	PermOrgsWrite = "orgs:write"

	// Dashboard users
	PermUsersRead  = "users:read"
	PermUsersWrite = "users:write" // create/update/disable/delete users

	// Audit trail
	PermAuditRead = "audit:read"

	// Gateway-wide settings
	PermSettingsWrite = "settings:write"
)

// All is the full catalog. Order is presentation order for the UI.
var All = []string{
	PermKeysRead, PermKeysReadOwn, PermKeysCreate, PermKeysUpdate, PermKeysRotate, PermKeysDelete, PermKeysLimits,
	PermProvidersRead, PermProvidersWrite, PermProvidersTest, PermProvidersDelete,
	PermLogsRead, PermLogsReadBodies, PermAnalyticsRead,
	PermRoutingRead, PermRoutingWrite,
	PermCatalogRead, PermCatalogWrite,
	PermOrgsRead, PermOrgsWrite,
	PermUsersRead, PermUsersWrite,
	PermAuditRead,
	PermSettingsWrite,
}

// Valid reports whether p is a known catalog entry. Unknown permissions must
// fail closed: IsValid guards every write path so typos can't grant nothing
// (or worse, be stored and later misinterpreted).
func Valid(p string) bool {
	for _, known := range All {
		if known == p {
			return true
		}
	}
	return false
}

// Role names mirrored from internal/user (stringly to avoid an import cycle;
// user.Role normalizes into exactly these).
const (
	RoleAdmin    = "admin"
	RoleSupport  = "support"
	RoleMember   = "member"
	RoleReadonly = "readonly"
)

// roleDefaults maps each role to its default permission set. These sets are
// chosen to reproduce the pre-RBAC RequireRole call sites exactly:
//
//   - routes guarded RequireRole("admin","member","support") become writable
//     by admin+support+member → those three roles carry the write perm.
//   - routes guarded RequireRole("admin") only (routing, catalog, orgs,
//     audit, settings, users) give the write perm to admin alone.
//   - readonly keeps read perms; the middleware readonly-mutation backstop
//     remains as defense in depth.
var roleDefaults = map[string]map[string]bool{
	RoleAdmin: allSet(), // admin holds everything (and checks bypass anyway)

	RoleSupport: set(
		PermKeysRead, PermKeysReadOwn, PermKeysCreate, PermKeysUpdate, PermKeysDelete, PermKeysLimits,
		PermProvidersRead, PermProvidersWrite, PermProvidersTest, PermProvidersDelete,
		PermLogsRead, PermLogsReadBodies, PermAnalyticsRead,
		PermCatalogRead, PermOrgsRead,
	),

	RoleMember: set(
		PermKeysRead, PermKeysReadOwn, PermKeysCreate, PermKeysUpdate, PermKeysRotate, PermKeysDelete, PermKeysLimits,
		PermProvidersRead, PermProvidersWrite, PermProvidersTest,
		PermLogsRead, PermLogsReadBodies, PermAnalyticsRead,
		PermCatalogRead, PermOrgsRead,
	),

	RoleReadonly: set(
		PermKeysReadOwn, // own keys only (includes rotating them) — keys:read (all keys) is NOT default
		PermProvidersRead, PermProvidersTest,
		PermCatalogRead, PermOrgsRead,
		// logs:read / analytics:read are deliberately NOT defaults: readonly
		// users see only their own keys' traffic. Grant these explicitly (or
		// via the Perms editor) for ops-style read-only auditors.
	),
}

// Defaults returns a copy of the default permission set for a role. Unknown
// roles yield the empty set (fail closed); callers normalize roles upstream.
func Defaults(role string) map[string]bool {
	s, ok := roleDefaults[role]
	if !ok {
		return map[string]bool{}
	}
	out := make(map[string]bool, len(s))
	for p := range s {
		out[p] = true
	}
	return out
}

// Effective composes the caller's permission set: role defaults plus grants,
// minus revokes. The admin role short-circuits to the full set — admins keep
// every permission regardless of revokes (they cannot be locked out).
//
//	grants/revokes: maps permission → true (grant) / false (revoke), matching
//	the user_permissions.granted column (1/0 → true/false).
func Effective(role string, grants map[string]bool) map[string]bool {
	if role == RoleAdmin {
		return allSet()
	}
	base := Defaults(role)
	for p, granted := range grants {
		if !Valid(p) {
			continue // unknown entries never influence the outcome
		}
		if granted {
			base[p] = true
		} else {
			delete(base, p)
		}
	}
	return base
}

// Has reports whether perms grants p. Nil-safe for convenience.
func Has(perms map[string]bool, p string) bool {
	return perms != nil && perms[p]
}

// Sorted returns the permission set as a sorted slice (stable JSON).
func Sorted(perms map[string]bool) []string {
	out := make([]string, 0, len(perms))
	for p := range perms {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func allSet() map[string]bool {
	s := make(map[string]bool, len(All))
	for _, p := range All {
		s[p] = true
	}
	return s
}

func set(perms ...string) map[string]bool {
	s := make(map[string]bool, len(perms))
	for _, p := range perms {
		s[p] = true
	}
	return s
}
