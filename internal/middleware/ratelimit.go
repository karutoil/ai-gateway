package middleware

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

// Limiter is the interface for rate limiting; both in-memory and Redis implementations satisfy it.
type Limiter interface {
	Allow(key string, rpm int) bool
	AllowWithLimits(key string, limits RateLimits) bool
	AllowTokens(key string, tokens int, tpm int) bool
}

// RateLimits captures per-key extended limits. Zero means disabled.
type RateLimits struct {
	RPM int // requests per minute (0 = unlimited)
	RPH int // requests per hour
	RPD int // requests per day
	TPM int // tokens per minute (checked via AllowTokens, not AllowWithLimits)
}

type bucket struct {
	count int
	reset time.Time
}

type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{buckets: make(map[string]*bucket)}
	go rl.cleanupLoop()
	return rl
}

func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		now := time.Now()
		for k, b := range rl.buckets {
			if now.After(b.reset.Add(2 * time.Minute)) {
				delete(rl.buckets, k)
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *RateLimiter) Allow(key string, rpm int) bool {
	return rl.AllowWithLimits(key, RateLimits{RPM: rpm})
}

// AllowWithLimits atomically evaluates every configured window and commits
// increments ONLY if all of them admit the request. The previous version
// committed earlier windows before later ones could deny, silently burning an
// RPM slot whenever an RPH/RPD check failed.
func (rl *RateLimiter) AllowWithLimits(key string, lim RateLimits) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()

	type windowOp struct {
		key      string // full bucket key
		isNew    bool   // true → (re)create with count=1 at now+window
		incNewAt time.Time
	}

	var ops []windowOp

	admit := func(suffix string, limit int, window time.Duration) bool {
		if limit <= 0 {
			return true
		}
		if limit > 10000000 {
			limit = 10000000
		}
		wk := key + suffix
		b, ok := rl.buckets[wk]
		if !ok || now.After(b.reset) {
			if limit < 1 {
				return false
			}
			ops = append(ops, windowOp{key: wk, isNew: true, incNewAt: now.Add(window)})
			return true
		}
		if b.count+1 > limit {
			return false
		}
		ops = append(ops, windowOp{key: wk})
		return true
	}

	if lim.RPM > 0 && !admit(":m", lim.RPM, time.Minute) {
		return false
	}
	if lim.RPH > 0 && !admit(":h", lim.RPH, time.Hour) {
		return false
	}
	if lim.RPD > 0 && !admit(":d", lim.RPD, 24*time.Hour) {
		return false
	}

	// Every window passed: apply mutations in one atomic step. Denial paths
	// above left zero residue by construction.
	for _, op := range ops {
		if op.isNew {
			rl.buckets[op.key] = &bucket{count: 1, reset: op.incNewAt}
		} else {
			rl.buckets[op.key].count++
		}
	}
	return true
}

func (rl *RateLimiter) AllowTokens(key string, tokens int, tpm int) bool {
	if tpm <= 0 || tokens <= 0 {
		return true
	}
	if tpm > 10000000 {
		tpm = 10000000
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	bk := key + ":tpm"
	b, ok := rl.buckets[bk]
	if !ok || now.After(b.reset) {
		if tokens > tpm {
			return false
		}
		rl.buckets[bk] = &bucket{count: tokens, reset: now.Add(time.Minute)}
		return true
	}
	if b.count+tokens > tpm {
		return false
	}
	b.count += tokens
	return true
}

// RedisRateLimiter uses Redis INCR with TTL for sliding window, fallback to in-memory on error.
type RedisRateLimiter struct {
	client   *redis.Client
	fallback *RateLimiter
}

var _ Limiter = (*RateLimiter)(nil)
var _ Limiter = (*RedisRateLimiter)(nil)

func NewRedisRateLimiter(redisURL string) (*RedisRateLimiter, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		log.Warn().Err(err).Str("redis_url", redisURL).Msg("redis ping failed for rate limiter, will fallback to memory")
	}
	return &RedisRateLimiter{client: client, fallback: NewRateLimiter()}, nil
}

func NewRedisRateLimiterWithClient(client *redis.Client) *RedisRateLimiter {
	return &RedisRateLimiter{client: client, fallback: NewRateLimiter()}
}

func (r *RedisRateLimiter) Allow(key string, rpm int) bool {
	return r.AllowWithLimits(key, RateLimits{RPM: rpm})
}

func (r *RedisRateLimiter) AllowWithLimits(key string, lim RateLimits) bool {
	if lim.RPM <= 0 && lim.RPH <= 0 && lim.RPD <= 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Single atomic script: read all windows, deny without consuming if any
	// limit is exceeded, otherwise increment all and refresh TTLs. This
	// replaces the previous three sequential INCRs which let a request denied
	// by RPD/RPH still consume RPM quota, and made each window an independent
	// check-then-act.
	script := `
local m = redis.call("GET", KEYS[1])
local h = redis.call("GET", KEYS[2])
local d = redis.call("GET", KEYS[3])
local rm, rh, rd = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])
if rm > 0 and m ~= false and tonumber(m) + 1 > rm then return 1 end
if rh > 0 and h ~= false and tonumber(h) + 1 > rh then return 2 end
if rd > 0 and d ~= false and tonumber(d) + 1 > rd then return 3 end
if rm > 0 then
  local c = redis.call("INCR", KEYS[1]); if c == 1 then redis.call("EXPIRE", KEYS[1], 60) end
end
if rh > 0 then
  local c = redis.call("INCR", KEYS[2]); if c == 1 then redis.call("EXPIRE", KEYS[2], 3600) end
end
if rd > 0 then
  local c = redis.call("INCR", KEYS[3]); if c == 1 then redis.call("EXPIRE", KEYS[3], 86400) end
end
return 0`
	keys := []string{"ratelimit:" + key + ":m", "ratelimit:" + key + ":h", "ratelimit:" + key + ":d"}
	res, err := r.client.Eval(ctx, script, keys, lim.RPM, lim.RPH, lim.RPD).Result()
	if err != nil {
		log.Debug().Err(err).Str("key", key).Msg("redis multi-window INCR failed, fallback to memory")
		return r.fallbackAllow(key, lim)
	}
	code := -1
	switch v := res.(type) {
	case int64:
		code = int(v)
	case int:
		code = v
	}
	return code == 0
}

// fallbackAllow routes a single Redis failure onto the in-memory limiter via
// its PUBLIC (mutex-taking) entry point. The previous code called the
// unexported allowWindowLocked directly — a method whose contract requires the
// caller to hold rl.mu — producing unsynchronized map access under concurrent
// load: torn counts at best, a runtime crash at worst.
func (r *RedisRateLimiter) fallbackAllow(key string, lim RateLimits) bool {
	if r.fallback == nil {
		return true
	}
	return r.fallback.AllowWithLimits(key, lim)
}

func (r *RedisRateLimiter) AllowTokens(key string, tokens int, tpm int) bool {
	if tpm <= 0 || tokens <= 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	redisKey := "ratelimit:" + key + ":tpm"
	script := `local c = redis.call("INCRBY", KEYS[1], ARGV[2]); if c == tonumber(ARGV[2]) then redis.call("EXPIRE", KEYS[1], 60) end; return c`
	res, err := r.client.Eval(ctx, script, []string{redisKey}, 60, tokens).Result()
	if err != nil {
		if r.fallback != nil {
			return r.fallback.AllowTokens(key, tokens, tpm)
		}
		return true
	}
	var count int64
	switch v := res.(type) {
	case int64:
		count = v
	case int:
		count = int64(v)
	default:
		if r.fallback != nil {
			return r.fallback.AllowTokens(key, tokens, tpm)
		}
		return true
	}
	return int(count) <= tpm
}

func (r *RedisRateLimiter) Close() error {
	if r.client != nil {
		return r.client.Close()
	}
	return nil
}

func GatewayRateLimit(rl Limiter, getRPM func(r *http.Request) int) func(http.Handler) http.Handler {
	return GatewayRateLimitWithLimits(rl, func(r *http.Request) RateLimits {
		return RateLimits{RPM: getRPM(r)}
	})
}

func GatewayRateLimitWithLimits(rl Limiter, getLimits func(r *http.Request) RateLimits) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Prefer verified key from context (set by GatewayAuth) — no DB hit.
			if k, ok := GatewayKeyFromContext(r.Context()); ok && k != nil {
				limits := getLimits(r)
				// If caller didn't supply limits, derive from verified key.
				if limits.RPM == 0 && limits.RPH == 0 && limits.RPD == 0 {
					limits = RateLimits{RPM: k.RateLimitRPM, RPH: k.RateLimitRPH, RPD: k.RateLimitRPD}
				}
				prefix := k.Prefix
				if prefix == "" {
					prefix = r.Header.Get("X-Gateway-Key-Prefix")
				}
				if prefix == "" {
					prefix = r.RemoteAddr
				}
				if !rl.AllowWithLimits(prefix, limits) {
					retry := "60"
					if limits.RPH > 0 && limits.RPM == 0 {
						retry = "3600"
					} else if limits.RPD > 0 && limits.RPM == 0 && limits.RPH == 0 {
						retry = "86400"
					}
					w.Header().Set("Retry-After", retry)
					http.Error(w, `{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`, http.StatusTooManyRequests)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			// Fallback: no verified key in context (e.g. unauthenticated probe) — use header/IP.
			prefix := r.Header.Get("X-Gateway-Key-Prefix")
			if prefix == "" {
				auth := r.Header.Get("Authorization")
				if strings.HasPrefix(auth, "Bearer ") {
					token := strings.TrimPrefix(auth, "Bearer ")
					if len(token) > 8 {
						prefix = token[:8]
					}
				}
			}
			if prefix == "" {
				prefix = r.RemoteAddr
			}
			limits := getLimits(r)
			if !rl.AllowWithLimits(prefix, limits) {
				retry := "60"
				if limits.RPH > 0 && limits.RPM == 0 {
					retry = "3600"
				} else if limits.RPD > 0 && limits.RPM == 0 && limits.RPH == 0 {
					retry = "86400"
				}
				w.Header().Set("Retry-After", retry)
				http.Error(w, `{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
