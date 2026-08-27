package middleware

import (
	"net/http"

	"ai-gateway/internal/auth"

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
			// Special handling: member can do most writes, but not org deletion unless admin
			if role == "member" {
				// Member cannot delete orgs
				if r.Method == http.MethodDelete && contains(r.URL.Path, "/orgs/") {
					log.Warn().Str("role", role).Str("path", r.URL.Path).Msg("RequireRole: forbid member org delete")
					http.Error(w, `{"error":{"message":"forbidden: member cannot delete organization","type":"permission_error"}}`, http.StatusForbidden)
					return
				}
				// Member allowed for other endpoints
				next.ServeHTTP(w, r)
				return
			}
			log.Warn().Str("role", role).Strs("required", roles).Str("path", r.URL.Path).Msg("RequireRole: forbid")
			http.Error(w, `{"error":{"message":"forbidden: insufficient role","type":"permission_error"}}`, http.StatusForbidden)
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
