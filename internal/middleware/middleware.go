package middleware

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/auth"
	"ai-gateway/internal/models"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

type gatewayKeyCtxKey struct{}

// GatewayKeyFromContext returns the verified GatewayKey stored by GatewayAuth.
func GatewayKeyFromContext(ctx context.Context) (*models.GatewayKey, bool) {
	v := ctx.Value(gatewayKeyCtxKey{})
	if v == nil {
		return nil, false
	}
	k, ok := v.(*models.GatewayKey)
	return k, ok
}

// RedactHeaders returns a copy of h with Authorization and x-api-key redacted.
func RedactHeaders(h http.Header) http.Header {
	out := h.Clone()
	if out.Get("Authorization") != "" {
		out.Set("Authorization", "[REDACTED]")
	}
	if out.Get("authorization") != "" {
		out.Set("authorization", "[REDACTED]")
	}
	if out.Get("x-api-key") != "" {
		out.Set("x-api-key", "[REDACTED]")
	}
	if out.Get("X-Api-Key") != "" {
		out.Set("X-Api-Key", "[REDACTED]")
	}
	if out.Get("X-API-Key") != "" {
		out.Set("X-API-Key", "[REDACTED]")
	}
	return out
}

// RedactValue returns "[REDACTED]" for non-empty sensitive values, empty otherwise.
func RedactValue(v string) string {
	if v == "" {
		return ""
	}
	return "[REDACTED]"
}

// SafePrefix returns the gateway key prefix for logging (never the full key).
func SafePrefix(r *http.Request) string {
	if k, ok := GatewayKeyFromContext(r.Context()); ok && k != nil {
		return k.Prefix
	}
	if p := r.Header.Get("X-Gateway-Key-Prefix"); p != "" {
		return p
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		auth = r.Header.Get("x-api-key")
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if strings.HasPrefix(token, "sk-gw-") && len(token) >= len("sk-gw-")+8 {
		return token[len("sk-gw-") : len("sk-gw-")+8]
	}
	return ""
}

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		r.Header.Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = 200
		}
		evt := log.Info()
		if status >= 500 {
			evt = log.Error()
		} else if status >= 400 {
			evt = log.Warn()
		}
		prefix := SafePrefix(r)
		_ = RedactHeaders(r.Header)
		evt.Str("method", r.Method).Str("path", r.URL.Path).Int("status", status).Str("req_id", r.Header.Get("X-Request-ID")).Str("key_prefix", prefix).Int64("bytes", rec.written).Msg("request")
	})
}

func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error().Interface("panic", rec).Bytes("stack", debug.Stack()).Str("path", r.URL.Path).Msg("panic recovered")
				http.Error(w, `{"error":{"message":"internal server error"}}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func GatewayAuth(store *apikey.Store) func(http.Handler) http.Handler {
	return GatewayAuthWithJWT(store, nil)
}

// GatewayAuthWithJWT accepts sk-gw-* keys and, when jwtSecret is set, admin session JWTs
// (Playground logged-in user). Session tokens never bypass provider routing; they only
// authenticate the gateway hop. No revocation checking (tests).
func GatewayAuthWithJWT(store *apikey.Store, jwtSecret []byte) func(http.Handler) http.Handler {
	return GatewayAuthWithJWTRevocation(store, jwtSecret, nil)
}

// GatewayAuthWithJWTRevocation is the production entry point: JWT-shaped
// tokens are additionally validated against the live user table, so revoked
// (password-changed, disabled, deleted) dashboard users lose proxy access
// immediately instead of at token expiry.
func GatewayAuthWithJWTRevocation(store *apikey.Store, jwtSecret []byte, checker auth.SessionChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			authz := r.Header.Get("Authorization")
			if authz == "" {
				if c, err := r.Cookie("gw_token"); err == nil && c.Value != "" {
					authz = "Bearer " + c.Value
				}
			}
			if authz == "" {
				authz = r.Header.Get("x-api-key")
				if authz != "" && !strings.HasPrefix(authz, "Bearer ") {
					authz = "Bearer " + authz
				}
			}
			if authz == "" {
				http.Error(w, `{"error":{"message":"missing gateway api key","type":"authentication_error"}}`, http.StatusUnauthorized)
				return
			}
			token := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
			if token == "" {
				http.Error(w, `{"error":{"message":"invalid api key","type":"authentication_error"}}`, http.StatusUnauthorized)
				return
			}
			if strings.HasPrefix(token, "sk-gw-") {
				if len(token) != len("sk-gw-")+64 {
					http.Error(w, `{"error":{"message":"invalid gateway api key","type":"authentication_error"}}`, http.StatusUnauthorized)
					return
				}
				key, ok := store.Verify(token)
				if !ok {
					http.Error(w, `{"error":{"message":"invalid gateway api key","type":"authentication_error"}}`, http.StatusUnauthorized)
					return
				}
				// Per-key IP allowlist: empty allows any client; the client IP must
				// match an entry (exact IP or CIDR range) otherwise.
				if key.IPAllowlist != "" && !clientIPAllowed(clientIP(r), key.IPAllowlist) {
					log.Warn().Str("key_prefix", key.Prefix).Str("client_ip", clientIP(r)).Msg("gateway key rejected: IP not allowed")
					http.Error(w, `{"error":{"message":"client IP not allowed for this key","type":"permission_error"}}`, http.StatusForbidden)
					return
				}
				// Monthly spend cap: calendar-month spend for this key must stay
				// under the configured budget before admitting another request.
				if key.MonthlyBudgetUSD > 0 {
					if spent := store.MonthSpendUSD(key.ID); spent >= key.MonthlyBudgetUSD {
						log.Warn().Str("key_prefix", key.Prefix).Float64("spent", spent).Float64("budget", key.MonthlyBudgetUSD).Msg("gateway key: monthly budget exceeded")
						w.Header().Set("Retry-After", secondsUntilNextMonthUTC())
						http.Error(w, `{"error":{"message":"monthly spend budget exceeded for this key","type":"budget_exceeded"}}`, http.StatusTooManyRequests)
						return
					}
				}
				attachGatewayKey(next, w, r, key)
				return
			}
			if len(jwtSecret) > 0 {
				claims, err := auth.VerifyToken(jwtSecret, token)
				if err == nil && claims != nil {
					subject := "admin"
					orgID := ""
					if v, ok := claims["sub"].(string); ok && v != "" {
						subject = v
					}
					if v, ok := claims["org_id"].(string); ok {
						orgID = v
					}
					// Revocation: mirror the dashboard middleware — a session
					// whose token_version no longer matches (password change,
					// role change, disable, delete) must not keep spending
					// provider quota until expiry.
					if checker != nil {
						current, exists := checker.TokenVersionFor(subject)
						if !exists || claimsTV(claims) != current {
							http.Error(w, `{"error":{"message":"session revoked","type":"authentication_error"}}`, http.StatusUnauthorized)
							return
						}
					}
					key := sessionGatewayKey(subject, orgID)
					attachGatewayKey(next, w, r, key)
					return
				}
			}
			http.Error(w, `{"error":{"message":"invalid api key","type":"authentication_error"}}`, http.StatusUnauthorized)
		})
	}
}

func attachGatewayKey(next http.Handler, w http.ResponseWriter, r *http.Request, key *models.GatewayKey) {
	ctx := context.WithValue(r.Context(), gatewayKeyCtxKey{}, key)
	r = r.WithContext(ctx)
	r.Header.Set("X-Gateway-Key-Prefix", key.Prefix)
	if key.OrgID != nil && *key.OrgID != "" {
		r.Header.Set("X-Gateway-Org", *key.OrgID)
	}
	next.ServeHTTP(w, r)
}

func sessionGatewayKey(subject, orgID string) *models.GatewayKey {
	prefix := sessionPrefix(subject)
	key := &models.GatewayKey{
		ID:           "session:" + subject,
		Name:         "session",
		Prefix:       prefix,
		RateLimitRPM: 120,
	}
	if orgID != "" {
		key.OrgID = &orgID
	}
	return key
}

func sessionPrefix(subject string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(subject) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	out := b.String()
	if len(out) > 8 {
		out = out[:8]
	}
	if out == "" {
		out = "admin"
	}
	return "sess" + out
}

// claimsTV extracts the token_version claim ("tv") from a dashboard JWT.
func claimsTV(claims jwt.MapClaims) int64 {
	if raw, ok := claims["tv"]; ok {
		switch n := raw.(type) {
		case float64:
			return int64(n)
		case int64:
			return n
		case int:
			return int64(n)
		case json.Number:
			i, _ := n.Int64()
			return i
		}
	}
	return 0
}

// clientIPAllowed reports whether ip matches the allowlist of exact IPs
// and/or CIDR ranges (comma or whitespace separated).
func clientIPAllowed(ip, allowlist string) bool {
	if strings.TrimSpace(allowlist) == "" {
		return true
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, entry := range strings.FieldsFunc(allowlist, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if prefix, err := netip.ParsePrefix(entry); err == nil {
				addr, ok := netip.AddrFromSlice(parsed)
				if ok && prefix.Contains(addr.Unmap()) {
					return true
				}
			}
			continue
		}
		if e := net.ParseIP(entry); e != nil && e.Equal(parsed) {
			return true
		}
	}
	return false
}

// secondsUntilNextMonthUTC is the Retry-After hint when a monthly budget is
// exhausted (the counter resets at the UTC month boundary).
func secondsUntilNextMonthUTC() string {
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	return strconv.Itoa(int(next.Sub(now).Seconds()) + 1)
}
