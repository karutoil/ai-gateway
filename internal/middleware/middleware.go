package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/auth"
	"ai-gateway/internal/models"
	"ai-gateway/internal/rbac"

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
	if out.Get("api-key") != "" {
		out.Set("api-key", "[REDACTED]")
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
	// NOTE: the X-Gateway-Key-Prefix header is NOT trusted here. It is set by the
	// middleware AFTER a key is verified, so a value present without a key in
	// context came from the client: trusting it let any caller attribute its
	// requests to another key in the access log, poisoning "what did key X do?"
	// forensics. Callers that need the header (budget/ratelimit, which run after
	// SetGatewayKey) read it directly.
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

// Flush forwards http.Flusher through the recorder. Go promotes only the
// methods declared on the embedded http.ResponseWriter INTERFACE — Flush()
// lives on the separate http.Flusher interface — so without this override
// every w.(http.Flusher) assertion below the Logger fails, keepalive/SSE
// frames stay trapped in net/http's write buffer, and edge proxies
// (Cloudflare, ~100s first-byte window) 524 the request. Regression test:
// flusher_regression_test.go.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController and interface upgrades reach the real
// connection through this recorder.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

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

// Recovery converts a panic into a 500 — but only when nothing has been
// written yet.
//
// Every streaming path (proxy commit(), responses_stream, the MCP bridge)
// writes 200 + SSE headers before bytes flow. An unconditional http.Error after
// that is ignored as a superfluous WriteHeader, so the client received a
// valid-looking SSE stream with a bare JSON blob injected into it and no
// terminal frame — the exact failure the SSE paths take pains to avoid.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sr, _ := w.(*statusRecorder)
		defer func() {
			if rec := recover(); rec != nil {
				log.Error().Interface("panic", rec).Bytes("stack", debug.Stack()).Str("path", r.URL.Path).Msg("panic recovered")
				if sr != nil && sr.status != 0 {
					// Response already committed: the status line cannot be
					// changed. Close the stream cleanly instead of injecting a
					// foreign payload into it.
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					return
				}
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
				// Shared per-key admission policy (IP allowlist + monthly spend
				// cap) — see AdmitKey. Alternate authentication surfaces (the
				// unified /mcp endpoint) call the same helper.
				if !AdmitKey(store, w, r, key) {
					return
				}
				attachGatewayKey(next, w, r, key)
				return
			}
			if len(jwtSecret) > 0 {
				claims, err := auth.VerifyToken(jwtSecret, token)
				if err == nil && claims != nil {
					sub, ok := claims["sub"].(string)
					if !ok || sub == "" {
						http.Error(w, `{"error":{"message":"invalid api key","type":"authentication_error"}}`, http.StatusUnauthorized)
						return
					}
					subject := sub
					orgID := ""
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
					// A dashboard session is a PROXY credential: it spends the
					// operator's provider money with no per-user budget. Gate it
					// on proxy:use so a `readonly` account cannot run the
					// Playground (or replay its session cookie against
					// /v1/images/generations) and burn the most expensive model.
					// Role defaults are the check here because per-user grants are
					// not carried in the JWT; readonly does not get proxy:use.
					role, _ := claims["role"].(string)
					if role != "" && !rbac.Has(rbac.Defaults(role), rbac.PermProxyUse) {
						http.Error(w, `{"error":{"message":"this account may not use the proxy API (proxy:use)","type":"permission_error"}}`, http.StatusForbidden)
						return
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

// SetGatewayKey attaches an already-verified gateway key to the request
// context and headers. Alternate authentication surfaces (the unified /mcp
// endpoint accepts x-litellm-api-key in addition to Bearer/x-api-key) verify
// the secret themselves and then hand the key to the shared rate-limit and
// budget middleware through this helper.
func SetGatewayKey(r *http.Request, key *models.GatewayKey) *http.Request {
	if key == nil {
		return r
	}
	ctx := context.WithValue(r.Context(), gatewayKeyCtxKey{}, key)
	r = r.WithContext(ctx)
	r.Header.Set("X-Gateway-Key-Prefix", key.Prefix)
	if key.OrgID != nil && *key.OrgID != "" {
		r.Header.Set("X-Gateway-Org", *key.OrgID)
	}
	return r
}

// AdmitKey applies the per-key admission policy shared by EVERY authenticated
// gateway surface: the per-key IP allowlist and the calendar-month spend cap.
// It writes the rejecting response and returns false when the key must not be
// admitted; it writes nothing and returns true otherwise.
//
// GatewayAuth applies it inline on /v1. Alternate authentication surfaces
// (the unified /mcp endpoint, which verifies sk-gw keys itself and attaches
// them via SetGatewayKey) MUST call it after verification: without it, a key
// restricted by IP or over its monthly budget could shed both controls simply
// by speaking MCP instead of plain HTTP.
// AdmitKeyResult describes an admission denial: HTTP status, a client
// message, the OpenAI error type, and the Retry-After delay (string, set
// only for spend-cap rejections). Status is 0 when the key is admitted.
type AdmitKeyResult struct {
	Status     int
	Message    string
	ErrType    string
	RetryAfter string
}

// CheckAdmission runs the per-key admission policy (IP allowlist, calendar-
// month spend cap) WITHOUT writing a response, so alternate authentication
// surfaces can reject with their own envelope (the unified /mcp endpoint
// answers with the JSON-RPC error object instead of the OpenAI-style body
// AdmitKey writes on /v1). Callers must write the denial themselves when
// Status is non-zero.
func CheckAdmission(store *apikey.Store, r *http.Request, key *models.GatewayKey) AdmitKeyResult {
	// Per-key IP allowlist: empty allows any client; the client IP must
	// match an entry (exact IP or CIDR range) otherwise. clientIP reads the
	// direct peer only, so it is spoof-safe: forwardedHeaders (the only place
	// X-Forwarded-For is honored) runs before routing and rewrites
	// RemoteAddr to the verified real client IP.
	if key.IPAllowlist != "" && !clientIPAllowed(clientIP(r), key.IPAllowlist) {
		log.Warn().Str("key_prefix", key.Prefix).Str("client_ip", clientIP(r)).Msg("gateway key rejected: IP not allowed")
		return AdmitKeyResult{
			Status:  http.StatusForbidden,
			Message: "client IP not allowed for this key",
			ErrType: "permission_error",
		}
	}
	// Monthly spend cap: calendar-month spend for this key must stay
	// under the configured budget before admitting another request.
	// The prefix is passed too so spend that never got a key_id
	// stamped (pre-backfill rows) still counts.
	if key.MonthlyBudgetUSD > 0 {
		if spent := store.MonthSpendUSDForPrefix(key.ID, key.Prefix); spent >= key.MonthlyBudgetUSD {
			log.Warn().Str("key_prefix", key.Prefix).Float64("spent", spent).Float64("budget", key.MonthlyBudgetUSD).Msg("gateway key: monthly budget exceeded")
			return AdmitKeyResult{
				Status:     http.StatusTooManyRequests,
				Message:    "monthly spend budget exceeded for this key",
				ErrType:    "budget_exceeded",
				RetryAfter: secondsUntilNextMonthUTC(),
			}
		}
	}
	return AdmitKeyResult{}
}

func AdmitKey(store *apikey.Store, w http.ResponseWriter, r *http.Request, key *models.GatewayKey) bool {
	res := CheckAdmission(store, r, key)
	if res.Status == 0 {
		return true
	}
	if res.RetryAfter != "" {
		w.Header().Set("Retry-After", res.RetryAfter)
	}
	http.Error(w, fmt.Sprintf(`{"error":{"message":%q,"type":%q}}`, res.Message, res.ErrType), res.Status)
	return false
}

func attachGatewayKey(next http.Handler, w http.ResponseWriter, r *http.Request, key *models.GatewayKey) {
	ctx := context.WithValue(r.Context(), gatewayKeyCtxKey{}, key)
	r = r.WithContext(ctx)
	r.Header.Set("X-Gateway-Key-Prefix", key.Prefix)
	if key.OrgID != nil && *key.OrgID != "" {
		r.Header.Set("X-Gateway-Org", *key.OrgID)
	}
	// Internal-only markers must never arrive from the wire. The MCP tool loop
	// used to signal itself with this header; acting on a client-supplied value
	// re-enabled retries on billable creates and forced streaming requests down
	// the buffered (unmetered) path. The marker now lives in the request
	// context, and this Del keeps a future reader from trusting the header.
	r.Header.Del("X-Gateway-MCP-Round")
	next.ServeHTTP(w, r)
}

// sessionBudgetUSD is the optional monthly spend cap applied to dashboard
// sessions (SESSION_MONTHLY_BUDGET_USD). Zero = uncapped, which is the previous
// behaviour: session spend is attributed (see apikey.MonthSpendUSDForPrefix) but
// nothing enforced it, so one logged-in user could spend without limit.
// sessionBudgetCap prefers the config value (which validates and documents the
// knob) and falls back to a direct env read for tests that build a Handler
// without going through config.Load.
func sessionBudgetCap() float64 {
	if SessionMonthlyBudgetUSD > 0 {
		return SessionMonthlyBudgetUSD
	}
	return sessionBudgetUSD()
}

func sessionBudgetUSD() float64 {
	v := strings.TrimSpace(os.Getenv("SESSION_MONTHLY_BUDGET_USD"))
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// SessionMonthlyBudgetUSD is the config-driven override for sessionBudgetUSD,
// set once from cfg at startup. Kept as a package var so the session key (built
// per request) stays allocation-cheap and free of config plumbing.
var SessionMonthlyBudgetUSD float64

// SetSessionMonthlyBudget installs the operator's session spend cap.
func SetSessionMonthlyBudget(usd float64) { SessionMonthlyBudgetUSD = usd }

func sessionGatewayKey(subject, orgID string) *models.GatewayKey {
	prefix := sessionPrefix(subject)
	key := &models.GatewayKey{
		ID:               "session:" + subject,
		Name:             "session",
		Prefix:           prefix,
		RateLimitRPM:     120,
		MonthlyBudgetUSD: sessionBudgetCap(),
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
