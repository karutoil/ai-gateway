package middleware

import (
	"context"
	"net/http"
	"runtime/debug"
	"strings"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/auth"
	"ai-gateway/internal/models"

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
// authenticate the gateway hop.
func GatewayAuthWithJWT(store *apikey.Store, jwtSecret []byte) func(http.Handler) http.Handler {
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
