package config

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
)

// ENV validation table:
// | ENV var              | Required | Default             | Validation / Notes                                                                 |
// | PORT                 | no       | 8080                | numeric 1-65535                                                                    |
// | DATABASE_URL         | no       | ./data/gateway.db   | file path or :memory: or file: URI; sqlite only in 1.6 (postgres in Phase 3)      |
// | ADMIN_PASSWORD       | no       | admin123 (dev)      | if ENV==production and ==admin123 → ERROR log + health config_ok:false            |
// | MASTER_KEY           | no*      | derived/persistent  | *required in prod; must be 64 hex chars (32 bytes). hex.Decode + len==32 check    |
// | JWT_SECRET           | no*      | derived/persistent  | *required in prod; ≥32 chars (hex 64). Derived from MASTER_KEY/admin password     |
// | ENV                  | no       | "" (dev)            | if ==production triggers strength checks (ADMIN_PASSWORD, MASTER_KEY, JWT_SECRET)   |
// | MASTER_KEY_FILE      | no       | <db-dir>/.master_key| override persistent key file path                                                |
// | JWT_SECRET_FILE      | no       | <db-dir>/.jwt_secret| override JWT seed file path                                                     |
// | REDIS_URL            | no       | ""                  | redis:// URL for cache + rate limiting; when set uses Redis, fallback to memory if unavailable |
// | PUBLIC_URL           | no       | ""                  | https://gateway.example.com — public URL for Cloudflare Tunnel / reverse proxy. Used for CORS + logs |
// | CORS_ALLOWED_ORIGINS | no       | "" (= *)            | comma-separated origins, e.g. https://app.example.com,https://ai.example.com. Overrides PUBLIC_URL. Supports "*" |
// | TRUSTED_PROXIES      | no       | ""                  | comma-separated IPs/CIDRs trusted for X-Forwarded-* / CF-Connecting-IP (empty = trust all for tunnel) |

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

	CacheTTLSeconds int

	RetryMaxRetries  int
	RetryBaseDelayMs int

	BreakerAllowedFails      int
	BreakerCooldownSeconds   int
	BreakerHalfOpenSuccesses int

	// Request-body logging & retention (opt-in).
	LogBodies        bool
	BodyLogMaxBytes  int
	LogRetentionDays int

	// Stream usage accounting: inject stream_options.include_usage for OpenAI-
	// compatible upstreams when client didn't opt in (improves billing accuracy;
	// some upstreams reject unknown params so it stays off by default).
	StreamUsageInject bool

	MetricsRequireAuth bool
}

// getInt reads an integer env var with default fallback.
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

func getBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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

		CacheTTLSeconds: getInt("CACHE_TTL_SECONDS", 10),

		RetryMaxRetries:  getInt("RETRY_MAX_RETRIES", 2),
		RetryBaseDelayMs: getInt("RETRY_BASE_DELAY_MS", 200),

		BreakerAllowedFails:      getInt("BREAKER_ALLOWED_FAILS", 5),
		BreakerCooldownSeconds:   getInt("BREAKER_COOLDOWN_SECONDS", 30),
		BreakerHalfOpenSuccesses: getInt("BREAKER_HALF_OPEN_SUCCESSES", 2),

		LogBodies:        getBool("LOG_BODIES"),
		BodyLogMaxBytes:  getInt("BODY_LOG_MAX_BYTES", 8192),
		LogRetentionDays: getInt("LOG_RETENTION_DAYS", 0),

		StreamUsageInject: getBool("STREAM_USAGE_INJECT"),

		MetricsRequireAuth: getBool("METRICS_PROTECT"),
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
