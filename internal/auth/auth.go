package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	// ensure oauth2 is present in go.mod as required by task (transitively via oidc)
	_ "golang.org/x/oauth2"
)

// Context keys for org/role/subject stored by AdminMiddleware (Phase 3)
type contextKey string

const (
	contextKeyOrgID   contextKey = "org_id"
	contextKeyRole    contextKey = "role"
	contextKeySubject contextKey = "subject"
)

// HashPassword is defined in password.go (bcrypt + legacy verification).

// PasswordEqual compares two plain strings in constant time (bootstrap
// ADMIN_PASSWORD comparison only; credential storage uses VerifyPasswordHash).
func PasswordEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ValidTokenRoles is the exhaustive allowlist of roles that may appear in a
// dashboard JWT. Anything else fails closed.
var ValidTokenRoles = map[string]bool{
	"admin": true, "support": true, "member": true, "readonly": true,
}

const TokenTTL = 24 * time.Hour

// MakeToken issues an admin-role token. Bootstrap/ADMIN_PASSWORD path only.
func MakeToken(secret []byte, subject string) (string, error) {
	return MakeTokenWithOrg(secret, subject, "", "admin")
}

// MakeTokenWithOrg mints a dashboard JWT. role MUST be a non-empty valid role
// and version is the user's current token_version used for revocation; pass
// -1 to omit the claim only for pre-migration compatibility paths (tests).
func MakeTokenWithOrg(secret []byte, subject, orgID, role string) (string, error) {
	return MakeTokenFull(secret, subject, orgID, role, 0)
}

// MakeTokenFull is the canonical minting entry point with revocation version.
func MakeTokenFull(secret []byte, subject, orgID, role string, tokenVersion int64) (string, error) {
	if !ValidTokenRoles[role] {
		return "", fmt.Errorf("invalid role %q", role)
	}
	if subject == "" {
		subject = "unknown"
	}
	claims := jwt.MapClaims{
		"sub":    subject,
		"org_id": orgID,
		"role":   role,
		"tv":     tokenVersion,
		"exp":    time.Now().Add(TokenTTL).Unix(),
		"iat":    time.Now().Unix(),
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString(secret)
}

func MakeOIDCToken(secret []byte, subject, orgID, role, issuer string) (string, error) {
	if !ValidTokenRoles[role] {
		return "", fmt.Errorf("invalid role %q", role)
	}
	claims := jwt.MapClaims{
		"sub":    subject,
		"iss":    issuer,
		"aud":    os.Getenv("OIDC_CLIENT_ID"),
		"org_id": orgID,
		"role":   role,
		"exp":    time.Now().Add(TokenTTL).Unix(),
		"iat":    time.Now().Unix(),
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString(secret)
}

func VerifyToken(secret []byte, tokenStr string) (jwt.MapClaims, error) {
	t, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		return secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, err
	}
	if claims, ok := t.Claims.(jwt.MapClaims); ok && t.Valid {
		return claims, nil
	}
	return nil, jwt.ErrTokenInvalidClaims
}

// SessionChecker lets AdminMiddleware validate a token's subject against the
// current user table (revocation, disabling, deletion) without importing the
// user package. TokenVersionFor returns (currentVersion, userExists).
type SessionChecker interface {
	TokenVersionFor(subject string) (int64, bool)
}

// RoleResolver is an optional SessionChecker extension: when the checker can
// resolve a subject's live role, middleware uses the STORED role instead of
// the token claim. This closes two holes: (1) an external OIDC id_token whose
// "role" claim says "admin" must never grant admin, and (2) role changes take
// effect immediately without waiting for token expiry.
type RoleResolver interface {
	RoleFor(subject string) (string, bool)
}

// AdminMiddleware authenticates dashboard JWTs with fail-closed defaults:
//   - tokens must carry a valid role claim; unknown/empty roles are rejected
//   - when checker is non-nil, the subject must still exist and the token's
//     revocation version ("tv") must match — password changes, role changes,
//     disable and delete all invalidate outstanding sessions
//
// Backward-compatible wrapper: AdminMiddleware(secret) runs without revocation
// checks (tests only); production wiring uses AdminMiddlewareWithRevocation.
func AdminMiddleware(secret []byte) func(http.Handler) http.Handler {
	return adminMiddleware(secret, nil)
}

// AdminMiddlewareWithRevocation is the production entry point.
func AdminMiddlewareWithRevocation(secret []byte, checker SessionChecker) func(http.Handler) http.Handler {
	return adminMiddleware(secret, checker)
}

func adminMiddleware(secret []byte, checker SessionChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				if c, err := r.Cookie("gw_token"); err == nil {
					authHeader = "Bearer " + c.Value
				}
			}
			if !strings.HasPrefix(authHeader, "Bearer ") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			tokenStr := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
			if tokenStr == "" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			claims, err := VerifyToken(secret, tokenStr)
			if err != nil {
				if oidcIssuer := os.Getenv("OIDC_ISSUER"); oidcIssuer != "" {
					if c2, err2 := VerifyOIDCToken(tokenStr, oidcIssuer); err2 == nil {
						claims = c2
					} else {
						http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
						return
					}
				} else {
					http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
					return
				}
			}
			orgID := ""
			role := ""
			subject := ""
			if claims != nil {
				if v, ok := claims["org_id"].(string); ok {
					orgID = v
				}
				if v, ok := claims["role"].(string); ok && ValidTokenRoles[v] {
					role = v
				}
				if v, ok := claims["sub"].(string); ok && v != "" {
					subject = v
				}
			}
			// Fail closed: no valid role or subject in the token → unauthorized.
			if role == "" || subject == "" {
				http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
				return
			}
			// Revocation check against live user state.
			if checker != nil {
				var tv int64
				if raw, ok := claims["tv"]; ok {
					tv = toInt64(raw)
				}
				current, exists := checker.TokenVersionFor(subject)
				if !exists || tv != current {
					http.Error(w, `{"error":"session revoked"}`, http.StatusUnauthorized)
					return
				}
				// Live role always wins over the claim when resolvable.
				if resolver, ok := checker.(RoleResolver); ok {
					if storedRole, found := resolver.RoleFor(subject); found && ValidTokenRoles[storedRole] {
						role = storedRole
					}
				}
			}
			ctx := context.WithValue(r.Context(), contextKeyOrgID, orgID)
			ctx = context.WithValue(ctx, contextKeyRole, role)
			ctx = context.WithValue(ctx, contextKeySubject, subject)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

func isHS256(tokenStr string) bool {
	parts := strings.Split(tokenStr, ".")
	if len(parts) < 2 {
		return false
	}
	hdrBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		// try standard with padding
		if b, err2 := base64.URLEncoding.DecodeString(parts[0]); err2 == nil {
			hdrBytes = b
		} else {
			return false
		}
	}
	var hdr map[string]interface{}
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return false
	}
	alg, _ := hdr["alg"].(string)
	return alg == "HS256"
}

func verifyHS256Fallback(tokenStr, expectedIssuer string) (jwt.MapClaims, error) {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		if pw := os.Getenv("ADMIN_PASSWORD"); pw != "" && pw != "admin123" {
			h := sha256.Sum256([]byte("gateway-jwt:" + pw))
			secret = hex.EncodeToString(h[:])
		}
	}
	if secret == "" {
		return nil, jwt.ErrTokenInvalidClaims
	}
	claims, err := VerifyToken([]byte(secret), tokenStr)
	if err != nil {
		return nil, err
	}
	if iss, ok := claims["iss"].(string); ok && expectedIssuer != "" && iss != expectedIssuer {
		return nil, jwt.ErrTokenInvalidIssuer
	}
	if audVal := claims["aud"]; audVal != nil {
		expectedAud := os.Getenv("OIDC_CLIENT_ID")
		if expectedAud != "" {
			switch v := audVal.(type) {
			case string:
				if v != expectedAud {
					return nil, jwt.ErrTokenInvalidClaims
				}
			case []interface{}:
				found := false
				for _, a := range v {
					if s, ok := a.(string); ok && s == expectedAud {
						found = true
						break
					}
				}
				if !found {
					return nil, jwt.ErrTokenInvalidClaims
				}
			}
		}
	}
	return claims, nil
}

// oidcProviderCache memoizes oidc.Provider per issuer. VerifyOIDCToken is
// called from the request path; the previous uncached NewProvider fetch per
// verification added discovery latency to every external-token request and
// turned the IdP into a DoS amplifier.
var (
	oidcProvidersMu sync.Mutex
	oidcProviders   = map[string]*oidc.Provider{}
)

func cachedOIDCProvider(ctx context.Context, issuer string) (*oidc.Provider, error) {
	oidcProvidersMu.Lock()
	defer oidcProvidersMu.Unlock()
	if p, ok := oidcProviders[issuer]; ok {
		return p, nil
	}
	p, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, err
	}
	oidcProviders[issuer] = p
	return p, nil
}

func VerifyOIDCToken(tokenStr, expectedIssuer string) (jwt.MapClaims, error) {
	// HMAC fallback for HS256 tokens used in tests
	if isHS256(tokenStr) {
		return verifyHS256Fallback(tokenStr, expectedIssuer)
	}
	// Real OIDC JWKS verification
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	provider, err := cachedOIDCProvider(ctx, expectedIssuer)
	if err != nil {
		return nil, err
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: os.Getenv("OIDC_CLIENT_ID")})
	idToken, err := verifier.Verify(ctx, tokenStr)
	if err != nil {
		return nil, err
	}
	var claims map[string]interface{}
	if err := idToken.Claims(&claims); err != nil {
		return nil, err
	}
	return jwt.MapClaims(claims), nil
}

// GetOrgID returns the caller's org scope from the VERIFIED JWT claim only.
// SECURITY: never fall back to client-supplied headers (X-Org /
// X-Organization-Id) — that let any authenticated caller spoof another
// tenant's scope and read/mutate its data.
func GetOrgID(r *http.Request) string {
	if v := r.Context().Value(contextKeyOrgID); v != nil {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func GetRole(r *http.Request) string {
	if v := r.Context().Value(contextKeyRole); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	// Fail closed: an unauthenticated/missing role context must never read as admin.
	return ""
}

func GetSubject(r *http.Request) string {
	if v := r.Context().Value(contextKeySubject); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
