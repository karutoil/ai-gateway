package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AuthRateLimiter provides brute-force protection for authentication
// endpoints. It tracks fixed-window counters per IP and per account:
//
//	per-IP:       AuthLimitPerIP attempts / minute
//	per-account:  AuthLimitPerAccount attempts / minute (across all IPs)
//
// Successful logins do NOT reset counters — the limiter only bounds the rate
// of guessing; it is deliberately cheap and worst-case safe.
type AuthRateLimiter struct {
	mu        sync.Mutex
	ipHits    map[string]*authWindow
	acctHits  map[string]*authWindow
	limitIP   int
	limitAcct int
	lastSweep time.Time
}

type authWindow struct {
	count int
	reset time.Time
}

func NewAuthRateLimiter() *AuthRateLimiter {
	return &AuthRateLimiter{
		ipHits:    map[string]*authWindow{},
		acctHits:  map[string]*authWindow{},
		limitIP:   AuthAttemptsPerIPPerMinute,
		limitAcct: AuthAttemptsPerAccountPerMinute,
	}
}

// Tunables exported for tests/config.
var (
	AuthAttemptsPerIPPerMinute      = 10
	AuthAttemptsPerAccountPerMinute = 5
	authWindowLen                   = time.Minute
)

// Middleware returns an http middleware enforcing the limits. accountField is
// extracted lazily via extractor so the same limiter serves login/passkey/
// recovery endpoints with different JSON shapes.
func (l *AuthRateLimiter) Middleware(accountOf func(r *http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if l.allow(clientIP(r), accountOf(r)) {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Retry-After", "60")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"too many attempts, retry later","type":"rate_limit_error"}}`))
		})
	}
}

func (l *AuthRateLimiter) allow(ip, account string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)
	if !bump(l.ipHits, "ip:"+ip, l.limitIP, now) {
		return false
	}
	if account != "" && !bump(l.acctHits, "acct:"+account, l.limitAcct, now) {
		return false
	}
	return true
}

func bump(m map[string]*authWindow, key string, limit int, now time.Time) bool {
	w, ok := m[key]
	if !ok || now.After(w.reset) {
		m[key] = &authWindow{count: 1, reset: now.Add(authWindowLen)}
		return true
	}
	if w.count >= limit {
		return false
	}
	w.count++
	return true
}

// sweepLocked periodically clears stale windows to keep memory bounded even
// under distributed scanning from many source addresses.
func (l *AuthRateLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < 10*time.Minute {
		return
	}
	l.lastSweep = now
	for k, w := range l.ipHits {
		if now.After(w.reset.Add(time.Minute)) {
			delete(l.ipHits, k)
		}
	}
	for k, w := range l.acctHits {
		if now.After(w.reset.Add(time.Minute)) {
			delete(l.acctHits, k)
		}
	}
}

// clientIP returns the direct peer address only. Auth decisions must never use
// spoofable forwarded headers before TRUSTED_PROXIES processing has run.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host
}

// AccountFromLoginBody extracts the username from a JSON login-shaped body so
// the same limiter can protect login/passkey/recovery endpoints.
func AccountFromLoginBody(r *http.Request) string {
	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.Contains(ct, "json") {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var b struct {
		Username string `json:"username"`
	}
	if json.Unmarshal(body, &b) != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(b.Username))
}

// CSRFProtection enforces origin integrity for state-changing requests that
// rely on cookie authentication. Browsers always send Origin on cross-site
// POSTs; API clients using Authorization headers are unaffected.
func CSRFProtection(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		// If the caller authenticated with a bearer token there is no ambient
		// cookie authority to abuse; cookie-authenticated requests fall here.
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			// No Origin header (non-browser client or very old browser): allow,
			// since SameSite=Lax already blocks the classic CSRF vector and we
			// cannot distinguish a curl client from a legacy browser here.
			next.ServeHTTP(w, r)
			return
		}
		target := requestHost(r)
		oHost := hostOf(origin)
		if oHost != "" && strings.EqualFold(oHost, target) {
			next.ServeHTTP(w, r)
			return
		}
		if sfSite := r.Header.Get("Sec-Fetch-Site"); sfSite == "same-origin" || sfSite == "none" {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, `{"error":"cross-origin request rejected"}`, http.StatusForbidden)
	})
}

func requestHost(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		return hostOnly(fwd)
	}
	return hostOnly(r.Host)
}

func hostOf(originURL string) string {
	s := strings.TrimPrefix(originURL, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return hostOnly(s)
}

// hostOnly strips a trailing :port for comparison robustness behind proxies.
func hostOnly(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if before, _, found := strings.Cut(h, ":"); found && !strings.Contains(before, "]") {
		return before
	}
	return h
}
