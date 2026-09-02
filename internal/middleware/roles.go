package middleware

import (
	"context"
	"net/http"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/rbac"

	"github.com/rs/zerolog/log"
)

// RequireRole enforces RBAC for Phase 3. It checks the role stored by auth.AdminMiddleware.
// - "admin" can do everything
// - "member" can read/write providers/keys but cannot delete orgs or manage members with admin role
// - "readonly" can only GET (read) — any POST/PUT/DELETE returns 403
//
// Enforces RBAC: admin > member > readonly
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(roles))
	for _, r := range roles {
		allowed[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role := auth.GetRole(r)
			if role == "" {
				http.Error(w, `{"error":{"message":"forbidden: missing role","type":"permission_error"}}`, http.StatusForbidden)
				return
			}
			// If caller passed no specific roles, just log (used as audit marker)
			if len(allowed) == 0 {
				log.Info().Str("role", role).Str("path", r.URL.Path).Msg("RequireRole: allow (no roles specified)")
				next.ServeHTTP(w, r)
				return
			}
			// Admin always allowed
			if role == "admin" {
				next.ServeHTTP(w, r)
				return
			}
			// Check explicit allowed roles
			if allowed[role] {
				// Additional readonly guard: readonly cannot mutate even if explicitly allowed via roles containing readonly
				if role == "readonly" && (r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete || r.Method == http.MethodPatch) {
					// If endpoint is read-only safe (GET), allow; otherwise forbid
					// For Phase 3 gate: readonly cannot POST /api/providers
					log.Warn().Str("role", role).Str("method", r.Method).Str("path", r.URL.Path).Msg("RequireRole: forbid readonly mutation")
					http.Error(w, `{"error":{"message":"forbidden: readonly role cannot mutate","type":"permission_error"}}`, http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			// SECURITY: no implicit fall-through. A role passes only when it
			// is explicitly listed (admin is always allowed). The previous
			// "member can do most writes" special case let members through
			// every admin-only gate (routing rules, discovery, org writes,
			// settings), which was a privilege-escalation hole.
			log.Warn().Str("role", role).Strs("required", roles).Str("path", r.URL.Path).Msg("RequireRole: forbid")
			http.Error(w, `{"error":{"message":"forbidden: insufficient role","type":"permission_error"}}`, http.StatusForbidden)
		})
	}
}

// PermResolver reports a user's effective permission set (role defaults
// composed with stored overrides). Implemented by user.Store; resolved live
// per request so permission edits apply without re-login — same contract as
// auth.RoleResolver.
type PermResolver interface {
	EffectivePermissionsByUsername(username, role string) (map[string]bool, error)
}

// permResolverKey carries the handler's PermResolver so RequirePerm can also
// expose the resolved set to handlers via auth.GetPerms.
type permResolverKey struct{}

// WithPermResolver registers the PermResolver on the router (call once next
// to other router-level wiring). Without it RequirePerm fails closed.
func WithPermResolver(res PermResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(withPermResolver(r.Context(), res)))
		})
	}
}

type permResolverCtx struct{}

func withPermResolver(ctx context.Context, res PermResolver) context.Context {
	return context.WithValue(ctx, permResolverCtx{}, res)
}

// permResolverFrom extracts the registered resolver, if any.
func permResolverFrom(r *http.Request) PermResolver {
	if v := r.Context().Value(permResolverCtx{}); v != nil {
		if res, ok := v.(PermResolver); ok {
			return res
		}
	}
	return nil
}

// RequireAnyPerm enforces that the caller holds AT LEAST ONE of the listed
// permissions and caches the effective set in context (auth.GetPerms) for
// the handler — used for read routes that branch on ownership (e.g. GET
// /keys passes with keys:read (all) or keys:read_own (own only); the handler
// narrows the list when only the latter holds).
func RequireAnyPerm(permissions ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role := auth.GetRole(r)
			if role == "" {
				http.Error(w, `{"error":{"message":"forbidden: missing role","type":"permission_error"}}`, http.StatusForbidden)
				return
			}
			if role == rbac.RoleAdmin {
				next.ServeHTTP(w, r)
				return
			}
			res := permResolverFrom(r)
			if res == nil {
				log.Error().Strs("perms", permissions).Str("path", r.URL.Path).Msg("RequireAnyPerm: no PermResolver registered — failing closed")
				http.Error(w, `{"error":{"message":"forbidden: permission system unavailable","type":"permission_error"}}`, http.StatusForbidden)
				return
			}
			perms, err := res.EffectivePermissionsByUsername(auth.GetSubject(r), role)
			if err != nil {
				log.Error().Err(err).Str("path", r.URL.Path).Msg("RequireAnyPerm: resolution failed")
				http.Error(w, `{"error":{"message":"forbidden: permission resolution failed","type":"permission_error"}}`, http.StatusForbidden)
				return
			}
			r = r.WithContext(auth.WithPerms(r.Context(), perms))
			for _, p := range permissions {
				if rbac.Has(perms, p) {
					next.ServeHTTP(w, r)
					return
				}
			}
			log.Warn().Strs("perms", permissions).Str("role", role).Str("path", r.URL.Path).Msg("RequireAnyPerm: forbid")
			http.Error(w, `{"error":{"message":"forbidden: missing permission","type":"permission_error"}}`, http.StatusForbidden)
		})
	}
}

// RequirePerm enforces a single fine-grained permission. Resolution order:
//
//  1. admin role → allow immediately (admins are all-powerful by design)
//  2. PermResolver registered via WithPermResolver → resolve live from the
//     user store, cache the set in the request context for handlers
//     (auth.GetPerms), then check membership
//  3. no resolver registered or resolution failure → 403 (fail closed)
//
// Note RequirePerm composes WITH the auth middleware (which provides the
// subject/role context); it never replaces it.
func RequirePerm(permission string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role := auth.GetRole(r)
			if role == "" {
				http.Error(w, `{"error":{"message":"forbidden: missing role","type":"permission_error"}}`, http.StatusForbidden)
				return
			}
			if role == rbac.RoleAdmin {
				next.ServeHTTP(w, r)
				return
			}
			res := permResolverFrom(r)
			if res == nil {
				log.Error().Str("perm", permission).Str("path", r.URL.Path).Msg("RequirePerm: no PermResolver registered — failing closed")
				http.Error(w, `{"error":{"message":"forbidden: permission system unavailable","type":"permission_error"}}`, http.StatusForbidden)
				return
			}
			perms, err := res.EffectivePermissionsByUsername(auth.GetSubject(r), role)
			if err != nil {
				log.Error().Err(err).Str("perm", permission).Str("path", r.URL.Path).Msg("RequirePerm: resolution failed")
				http.Error(w, `{"error":{"message":"forbidden: permission resolution failed","type":"permission_error"}}`, http.StatusForbidden)
				return
			}
			r = r.WithContext(auth.WithPerms(r.Context(), perms))
			if !rbac.Has(perms, permission) {
				log.Warn().Str("perm", permission).Str("role", role).Str("path", r.URL.Path).Msg("RequirePerm: forbid")
				http.Error(w, `{"error":{"message":"forbidden: missing permission `+permission+`","type":"permission_error"}}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
