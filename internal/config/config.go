package config

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"
)

// ENV validation table:
// | ENV var              | Required | Default             | Validation / Notes                                                                 |
// | PORT                 | no       | 8080                | numeric 1-65535                                                                    |
// | DATABASE_URL         | no       | ./data/gateway.db   | file path or :memory: or file: URI; postgres:// DSN switches to the (beta) Postgres dialect |
// | ADMIN_PASSWORD       | no       | admin123 (dev)      | if ENV==production and ==admin123 → ERROR log + health config_ok:false            |
// | MASTER_KEY           | no*      | derived/persistent  | *required in prod; must be 64 hex chars (32 bytes). hex.Decode + len==32 check    |
// | JWT_SECRET           | no*      | derived/persistent  | *required in prod; ≥32 chars (hex 64). Derived from MASTER_KEY/admin password     |
// | ENV                  | no       | "" (dev)            | if ==production triggers strength checks (ADMIN_PASSWORD, MASTER_KEY, JWT_SECRET)   |
// | MASTER_KEY_FILE      | no       | <db-dir>/.master_key| override persistent key file path                                                |
// | JWT_SECRET_FILE      | no       | <db-dir>/.jwt_secret| override JWT seed file path                                                     |
// | REDIS_URL            | no       | ""                  | redis:// URL for cache + rate limiting; when set uses Redis, fallback to memory if unavailable |
// | PUBLIC_URL           | no       | ""                  | https://gateway.example.com — public URL for Cloudflare Tunnel / reverse proxy. Used for CORS + logs |
// | CORS_ALLOWED_ORIGINS | no       | "" (= *)            | comma-separated origins, e.g. https://app.example.com,https://ai.example.com. Overrides PUBLIC_URL. Supports "*" |
// | TRUSTED_PROXIES      | no       | ""                  | comma-separated IPs/CIDRs trusted for X-Forwarded-* / CF-Connecting-IP (empty = loopback only; "*" trusts all) |
// | IMAGE_TIMEOUT_SECONDS| no       | 300                 | client first-byte budget for /v1/images/*: -1/unset = 300s default, 0 = disabled, N = N seconds |
// | GLOBAL_DAILY_TOKEN_LIMIT | no   | 0 (disabled)        | in-memory per-key daily token cap enforced before the DB budget ledger (per-replica) |

type Config struct {
	Port               string
	DatabaseURL        string
	MasterKey          []byte // 32 bytes for AES-GCM
	AdminPassword      string
	JWTSecret          []byte
	RedisURL           string
	PublicURL          string
	CORSAllowedOrigins string
	TrustedProxies     string

	// Production posture
	Production    bool // ENV == "production"
	AllowInsecure bool // ALLOW_INSECURE=true overrides production strength checks

	// Timeouts / resilience (seconds; 0 means disabled where sensible).
	UpstreamHeaderTimeoutSecs int // dial+TLS+response-header deadline per attempt
	RequestTotalTimeoutSecs   int // overall request budget incl. retries (0 = none)
	StreamIdleTimeoutSecs     int // max gap between stream chunks (0 = none)
	WriteHeaderGraceSecs      int // non-streaming responses write deadline
	ShutdownGraceSecs         int // graceful drain window for in-flight streams
	TTFBTimeoutSecs           int // client-facing first-byte budget; -1 = default, 0 = disabled
	MaxProxyBodyMB            int // proxy request-body cap in MiB (0 = unlimited)

	// SessionMonthlyBudgetUSD caps what a dashboard SESSION may spend on the
	// proxy plane. A session JWT is accepted on /v1/*, and a session has no
	// gateway key of its own, so without this a logged-in user could spend
	// without limit while every API key stayed under its own cap. 0 = uncapped
	// (the historical behaviour).
	SessionMonthlyBudgetUSD float64

	CacheTTLSeconds int
	// CacheStreams extends the exact-match response cache to STREAMING
	// requests (opt-in): a clean stream's exact SSE bytes are stored and
	// replayed verbatim for identical requests. Most production traffic is
	// streaming, which the classic non-stream cache can never serve.
	CacheStreams bool

	RetryMaxRetries  int
	RetryBaseDelayMs int

	// Request-body logging & retention (opt-in).
	LogBodies        bool
	BodyLogMaxBytes  int
	LogRetentionDays int

	// Stream usage accounting: inject stream_options.include_usage for OpenAI-
	// compatible upstreams when client didn't opt in (improves billing accuracy;
	// some upstreams reject unknown params so it stays off by default).
	StreamUsageInject bool

	// RoutingLegacyFallback restores pre-strategy heuristic model resolution
	// (provider_models ownership round-robin, name heuristics, default
	// provider) for bare model names with no routing rule. Default off:
	// unrouted bare models are rejected with 404 model_not_routed.
	RoutingLegacyFallback bool

	MetricsRequireAuth bool

	// GlobalDailyTokenLimit is an optional deployment-wide per-key daily
	// token cap enforced IN MEMORY (budget.MemoryLimiter) in front of the
	// DB-backed ledger — an emergency cost brake that applies even to keys
	// with no DB-configured limits. GLOBAL_DAILY_TOKEN_LIMIT=0 (default)
	// disables it. NOTE: state is per-process; each gateway replica enforces
	// the cap against its own traffic only.
	GlobalDailyTokenLimit int

	// ImageTimeoutSecs is the client-facing first-byte budget for
	// /v1/images/generations and /v1/images/edits. Image generation is
	// legitimately slow (10–60s+), so it must NOT inherit the 85s chat TTFB
	// default.
	//
	// Tri-state, mirrored by proxy.Handler.ImageTTFB:
	//
	//	< 0  (unset, or IMAGE_TIMEOUT_SECONDS=-1) -> built-in default (300s)
	//	== 0                                    -> budget DISABLED
	//	> 0                                     -> that many seconds
	//
	// Parsing uses getIntSigned rather than getInt: the generic getInt helper
	// rejects any non-digit character, which silently made the -1 sentinel
	// unreachable from the environment and collapsed "disabled" onto
	// "default".
	ImageTimeoutSecs int

	// Centralized MCP (Model Context Protocol) registry + unified /mcp
	// endpoint.
	//
	// MCPMaxRounds caps the gateway-side tool-execution loop for requests that
	// opt in with require_approval:"never". It exists to make an unbounded
	// model/tool ping-pong impossible: a model that keeps calling tools would
	// otherwise loop forever inside one client request.
	MCPMaxRounds int
	// MCPCallTimeoutSecs bounds one MCP operation (connect / list / call) and
	// one LLM round inside the loop.
	MCPCallTimeoutSecs int
	// MCPLoopTimeoutSecs bounds the whole tool loop for one client request.
	MCPLoopTimeoutSecs int
	// MCPHealthIntervalSecs is how often enabled MCP servers are probed and
	// reconnected by the background health loop.
	MCPHealthIntervalSecs int
}

// getInt reads an integer env var with default fallback. Digits only: it is
// used by knobs where a negative value has no meaning.
// getFloat reads a non-negative decimal setting. A malformed or negative value
// falls back to def so a typo cannot silently disable a cap.
func getFloat(name string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return def
	}
	return f
}

func getInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// getIntSigned reads an integer env var that may legitimately be negative, with
// a default fallback for unset/malformed values. Used by tri-state knobs where
// -1 is a meaningful sentinel (e.g. IMAGE_TIMEOUT_SECONDS).
func getIntSigned(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	neg := false
	if strings.HasPrefix(v, "-") {
		neg = true
		v = v[1:]
	}
	if v == "" {
		return def
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		return -n
	}
	return n
}

func getBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// streamUsageInjectDefault: STREAM_USAGE_INJECT defaults to true (usage
// injection on streams the client did not ask for include_usage) because
// silent zero-usage streams break budgets, quotas and cost analytics.
// Operators can opt out with STREAM_USAGE_INJECT=0/false/off.
func streamUsageInjectDefault() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("STREAM_USAGE_INJECT"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// logBodiesDefault: LOG_BODIES defaults to true — the usage log stores
// request/response bodies (including assembled stream text) truncated to
// BODY_LOG_MAX_BYTES and credential-scrubbed, which is what makes the Logs
// page actually useful. Privacy-sensitive deployments can opt out with
// LOG_BODIES=0/false/off; the gateway then logs metadata only.
func logBodiesDefault() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_BODIES"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// IsWeakProductionPassword reports whether the config uses the insecure default
// ADMIN_PASSWORD while ENV==production. Health handlers should expose config_ok accordingly.
func (c *Config) IsWeakProductionPassword() bool {
	env := os.Getenv("ENV")
	if env == "" {
		env = os.Getenv("GO_ENV")
	}
	return env == "production" && c.AdminPassword == "admin123"
}

// ConfigOK returns true when production hardening checks pass (used by /health).
func (c *Config) ConfigOK() bool { return !c.IsWeakProductionPassword() }

// AllowedOrigins returns parsed CORS origins for middleware.
// If CORS_ALLOWED_ORIGINS is set, split on comma. Else if PUBLIC_URL is set, use its origin.
// Else returns ["*"] (permissive for tunnel/dev).
func (c *Config) AllowedOrigins() []string {
	if c.CORSAllowedOrigins != "" {
		raw := c.CORSAllowedOrigins
		// Support "*" as single value
		if strings.TrimSpace(raw) == "*" {
			return []string{"*"}
		}
		parts := strings.Split(raw, ",")
		var out []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if c.PublicURL != "" {
		// Extract origin (scheme + host) from PUBLIC_URL
		p := c.PublicURL
		// strip trailing slash and path
		if idx := strings.Index(p, "://"); idx != -1 {
			rest := p[idx+3:]
			if slash := strings.Index(rest, "/"); slash != -1 {
				rest = rest[:slash]
			}
			origin := p[:idx+3] + rest
			return []string{origin}
		}
		return []string{p}
	}
	return []string{"*"}
}

// tryLoadDotEnv loads .env from repo root or cwd if present, without overriding existing env.
// It makes a single root .env.example work for both gateway and web (Vite uses envDir: '../').
func tryLoadDotEnv() {
	candidates := []string{
		".env",
		"../.env",
		"../../.env",
	}
	// Also try executable's directory (for embedded binary)
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), ".env"))
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", ".env"))
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", "..", ".env"))
	}
	for _, p := range candidates {
		if f, err := os.Open(p); err == nil {
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if strings.HasPrefix(line, "export ") {
					line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
				}
				eq := strings.Index(line, "=")
				if eq < 0 {
					continue
				}
				k := strings.TrimSpace(line[:eq])
				v := strings.TrimSpace(line[eq+1:])
				// strip surrounding quotes
				if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
					v = v[1 : len(v)-1]
				}
				// don't override already-set env
				if _, exists := os.LookupEnv(k); !exists && k != "" {
					_ = os.Setenv(k, v)
				}
			}
			_ = f.Close()
			if len(candidates) > 0 {
				log.Debug().Str("path", p).Msg("loaded .env")
			}
			return
		}
	}
}

func Load() (*Config, error) {
	tryLoadDotEnv()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	redisURL := os.Getenv("REDIS_URL")
	publicURL := strings.TrimSpace(os.Getenv("PUBLIC_URL"))
	corsOrigins := strings.TrimSpace(os.Getenv("CORS_ALLOWED_ORIGINS"))
	trustedProxies := strings.TrimSpace(os.Getenv("TRUSTED_PROXIES"))
	// Validate PUBLIC_URL if set
	if publicURL != "" {
		if !strings.HasPrefix(publicURL, "http://") && !strings.HasPrefix(publicURL, "https://") {
			log.Warn().Str("public_url", publicURL).Msg("PUBLIC_URL should start with http:// or https://")
		}
	}
	db := os.Getenv("DATABASE_URL")
	if db == "" {
		db = "./data/gateway.db"
	}
	adminPw := os.Getenv("ADMIN_PASSWORD")
	if adminPw == "" {
		adminPw = "admin123" // dev default
	}
	// Production hardening: fail closed instead of booting insecure.
	envCheck := os.Getenv("ENV")
	if envCheck == "" {
		envCheck = os.Getenv("GO_ENV")
	}
	production := envCheck == "production"
	allowInsecure := getBool("ALLOW_INSECURE")
	if production {
		if adminPw == "admin123" || adminPw == "" {
			if !allowInsecure {
				return nil, fmt.Errorf("ENV=production requires a strong ADMIN_PASSWORD (got default/empty). Set ADMIN_PASSWORD, or ALLOW_INSECURE=true to override at your own risk")
			}
			log.Error().Msg("INSECURE: production booted with default/empty ADMIN_PASSWORD under ALLOW_INSECURE=true")
		}
		if len(adminPw) < 12 && !allowInsecure {
			return nil, fmt.Errorf("ENV=production requires ADMIN_PASSWORD of at least 12 characters")
		}
	}
	mkHex := os.Getenv("MASTER_KEY")
	var mk []byte
	if mkHex != "" {
		b, err := hex.DecodeString(mkHex)
		if err != nil {
			return nil, fmt.Errorf("MASTER_KEY must be hex: %w", err)
		}
		if len(b) != 32 {
			return nil, fmt.Errorf("MASTER_KEY must be 32 bytes (64 hex chars), got %d", len(b))
		}
		mk = b
	} else if production && !allowInsecure {
		return nil, fmt.Errorf("ENV=production requires an explicit MASTER_KEY (64 hex chars). Generate one with: openssl rand -hex 32")
	} else {
		if adminPw != "admin123" {
			// deterministic derivation from admin password (stable across restarts when MASTER_KEY unset)
			h := sha256.Sum256([]byte("gateway-master-key:" + adminPw))
			mk = h[:]
			log.Warn().Msg("MASTER_KEY not set: derived deterministic key from ADMIN_PASSWORD (set MASTER_KEY in prod for stronger secrecy)")
		} else {
			// Try to load/generate a persistent key file so provider keys survive restarts even with default password
			// This fixes the bug where every restart used an ephemeral random key and broke saved provider creds.
			persisted, err := loadOrCreateMasterKey(db)
			if err != nil {
				return nil, fmt.Errorf("failed to load/create persistent master key: %w", err)
			}
			mk = persisted
			log.Warn().Msg("MASTER_KEY not set and ADMIN_PASSWORD is default: using persistent key file (set MASTER_KEY and ADMIN_PASSWORD in production for stronger secrecy)")
		}
	}
	jwtSecret := os.Getenv("JWT_SECRET")
	// JWT_SECRET length check: if explicitly set but too short, keep it but warn (existing MASTER_KEY length is hard-fail above).
	if jwtSecret != "" && len(jwtSecret) < 32 {
		if production && !allowInsecure {
			return nil, fmt.Errorf("ENV=production requires JWT_SECRET of at least 32 characters")
		}
		log.Error().Msg("JWT_SECRET is too short (<32 chars) — set a strong random value (≥32 chars) for production")
	}
	if jwtSecret == "" {
		if production && !allowInsecure {
			return nil, fmt.Errorf("ENV=production requires an explicit JWT_SECRET (≥32 chars). Generate one with: openssl rand -hex 32")
		}
		if adminPw != "admin123" {
			h := sha256.Sum256([]byte("gateway-jwt:" + adminPw))
			jwtSecret = hex.EncodeToString(h[:])
			log.Warn().Msg("JWT_SECRET not set: derived from ADMIN_PASSWORD (set JWT_SECRET in prod)")
		} else {
			// Derive JWT secret from the persistent master key so it also survives restarts
			// If master key is persistent, this will be stable; otherwise fallback to file
			if mk != nil && len(mk) == 32 {
				h := sha256.Sum256([]byte("gateway-jwt:" + hex.EncodeToString(mk)))
				jwtSecret = hex.EncodeToString(h[:])
			} else {
				persisted, err := loadOrCreateJWTSeed(db)
				if err == nil {
					h := sha256.Sum256(persisted)
					jwtSecret = hex.EncodeToString(h[:])
				} else {
					b := make([]byte, 32)
					rand.Read(b)
					jwtSecret = hex.EncodeToString(b)
					log.Warn().Msg("JWT_SECRET not set and ADMIN_PASSWORD is default: using ephemeral random secret — admin sessions will expire on restart (set JWT_SECRET in prod)")
				}
			}
		}
	}
	cfg := &Config{
		Port:               port,
		DatabaseURL:        db,
		MasterKey:          mk,
		AdminPassword:      adminPw,
		JWTSecret:          []byte(jwtSecret),
		RedisURL:           redisURL,
		PublicURL:          publicURL,
		CORSAllowedOrigins: corsOrigins,
		TrustedProxies:     trustedProxies,

		Production:    production,
		AllowInsecure: allowInsecure,

		UpstreamHeaderTimeoutSecs: getInt("UPSTREAM_HEADER_TIMEOUT_SECONDS", 120),
		RequestTotalTimeoutSecs:   getInt("REQUEST_TOTAL_TIMEOUT_SECONDS", 0),
		StreamIdleTimeoutSecs:     getInt("STREAM_IDLE_TIMEOUT_SECONDS", 300),
		WriteHeaderGraceSecs:      getInt("WRITE_HEADER_GRACE_SECONDS", 60),
		ShutdownGraceSecs:         getInt("SHUTDOWN_GRACE_SECONDS", 90),
		// Time-to-first-byte budget surfaced to clients (see proxy.TTFB):
		// buffered requests get an honest 504, streams get SSE keepalive
		// commits — both before a Cloudflare edge can synthesize a 524.
		// 0 disables; -1 uses the built-in default.
		TTFBTimeoutSecs: getInt("TTFB_TIMEOUT_SECONDS", -1),

		// Proxy request-body cap in MiB (413 beyond). Large agentic CLI
		// sessions legitimately exceed the old fixed 64 MiB; 0 disables.
		MaxProxyBodyMB:          getInt("MAX_PROXY_BODY_MB", 64),
		SessionMonthlyBudgetUSD: getFloat("SESSION_MONTHLY_BUDGET_USD", 0),

		CacheTTLSeconds: getInt("CACHE_TTL_SECONDS", 10),
		CacheStreams:    getBool("CACHE_STREAMS"),

		RetryMaxRetries:  getInt("RETRY_MAX_RETRIES", 2),
		RetryBaseDelayMs: getInt("RETRY_BASE_DELAY_MS", 200),

		LogBodies:        logBodiesDefault(),
		BodyLogMaxBytes:  getInt("BODY_LOG_MAX_BYTES", 8192),
		LogRetentionDays: getInt("LOG_RETENTION_DAYS", 0),

		// Billing-correct default: STREAM_USAGE_INJECT defaults to TRUE so streamed
		// requests are actually metered (OpenAI streams carry no usage unless the
		// client asked for include_usage). Set STREAM_USAGE_INJECT=0 to restore
		// byte-pure pass-through at the cost of zero usage on such streams.
		StreamUsageInject: streamUsageInjectDefault(),

		// ROUTING_LEGACY_FALLBACK=true restores heuristic resolution for
		// bare model names with no routing rule (migration escape hatch).
		RoutingLegacyFallback: getBool("ROUTING_LEGACY_FALLBACK"),

		MetricsRequireAuth: getBool("METRICS_PROTECT"),

		// Optional in-memory deployment-wide per-key daily token cap.
		GlobalDailyTokenLimit: getInt("GLOBAL_DAILY_TOKEN_LIMIT", 0),

		// Image endpoints get their own first-byte budget: image generation
		// routinely exceeds the 85s chat budget. -1 = built-in default (300s).
		ImageTimeoutSecs: getIntSigned("IMAGE_TIMEOUT_SECONDS", -1),

		// Centralized MCP. 25 rounds matches the LiteLLM-compatible default
		// documented for gateway-side tool execution.
		MCPMaxRounds:          getInt("MCP_MAX_ROUNDS", 25),
		MCPCallTimeoutSecs:    getInt("MCP_CALL_TIMEOUT_SECONDS", 90),
		MCPLoopTimeoutSecs:    getInt("MCP_LOOP_TIMEOUT_SECONDS", 600),
		MCPHealthIntervalSecs: getInt("MCP_HEALTH_INTERVAL_SECONDS", 60),
	}
	// Wildcard CORS is incompatible with a hardened control plane.
	if cfg.Production && !allowInsecure {
		origins := cfg.AllowedOrigins()
		wild := len(origins) == 0 || (len(origins) == 1 && origins[0] == "*")
		if wild {
			return nil, fmt.Errorf("ENV=production requires explicit CORS_ALLOWED_ORIGINS or PUBLIC_URL (wildcard CORS is unsafe). Set ALLOW_INSECURE=true to override")
		}
	}
	return cfg, nil
}

// keyFileDir returns the directory to store persistent key files.
// It uses MASTER_KEY_FILE / JWT_SECRET_FILE env if set, otherwise the DB directory or ./data.
func keyFileDir(dbPath string) string {
	if f := os.Getenv("MASTER_KEY_FILE"); f != "" {
		return filepath.Dir(f)
	}
	if dbPath == ":memory:" || strings.HasPrefix(dbPath, "file:") {
		// For in-memory DB, fallback to ./data (or current dir if data not writable)
		return "./data"
	}
	// Handle DSN with query params like "./data/gateway.db?_journal_mode=WAL"
	clean := dbPath
	if idx := strings.Index(clean, "?"); idx != -1 {
		clean = clean[:idx]
	}
	dir := filepath.Dir(clean)
	if dir == "." || dir == "" {
		return "./data"
	}
	return dir
}

func masterKeyFilePath(dbPath string) string {
	if f := os.Getenv("MASTER_KEY_FILE"); f != "" {
		return f
	}
	dir := keyFileDir(dbPath)
	return filepath.Join(dir, ".master_key")
}

func jwtSeedFilePath(dbPath string) string {
	if f := os.Getenv("JWT_SECRET_FILE"); f != "" {
		return f
	}
	dir := keyFileDir(dbPath)
	return filepath.Join(dir, ".jwt_secret")
}

func loadOrCreateMasterKey(dbPath string) ([]byte, error) {
	path := masterKeyFilePath(dbPath)
	// Try to read existing
	if data, err := os.ReadFile(path); err == nil {
		hexStr := strings.TrimSpace(string(data))
		// Support both hex and raw
		if b, err := hex.DecodeString(hexStr); err == nil && len(b) == 32 {
			return b, nil
		}
		// If file contains raw 32 bytes, use directly
		if len(data) == 32 {
			return data, nil
		}
		// Try trimming newlines and re-decode
		log.Warn().Str("path", path).Msg("existing master key file has invalid format, regenerating")
	}
	// Generate new
	mk := make([]byte, 32)
	if _, err := rand.Read(mk); err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}
	hexStr := hex.EncodeToString(mk)
	// Write with 0600
	if err := os.WriteFile(path, []byte(hexStr+"\n"), 0600); err != nil {
		return nil, err
	}
	log.Info().Str("path", path).Msg("generated persistent master key file")
	return mk, nil
}

func loadOrCreateJWTSeed(dbPath string) ([]byte, error) {
	path := jwtSeedFilePath(dbPath)
	if data, err := os.ReadFile(path); err == nil {
		seed := strings.TrimSpace(string(data))
		if seed != "" {
			return []byte(seed), nil
		}
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(seed)+"\n"), 0600); err != nil {
		return nil, err
	}
	log.Info().Str("path", path).Msg("generated persistent jwt seed file")
	return seed, nil
}
