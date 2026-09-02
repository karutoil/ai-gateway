package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	pathpkg "path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/audit"
	"ai-gateway/internal/auth"
	"ai-gateway/internal/budget"
	"ai-gateway/internal/cache"
	"ai-gateway/internal/catalog"
	"ai-gateway/internal/config"
	"ai-gateway/internal/db"
	"ai-gateway/internal/discovery"
	"ai-gateway/internal/handler"
	"ai-gateway/internal/lb"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/otel"
	"ai-gateway/internal/passkey"
	"ai-gateway/internal/pat"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/proxy"
	"ai-gateway/internal/rbac"
	"ai-gateway/internal/resilience"
	"ai-gateway/internal/user"
	"ai-gateway/internal/webhook"

	"github.com/go-chi/chi/v5"
	cors "github.com/go-chi/cors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

//go:embed all:web
var embeddedWeb embed.FS

//go:embed openapi.yaml
var embeddedOpenAPI []byte

func main() {
	// `gateway version` / `gateway --version`: print version and exit.
	// Used by install.sh to detect the currently installed release.
	if len(os.Args) > 1 {
		arg := os.Args[1]
		if arg == "version" || arg == "--version" || arg == "-v" {
			fmt.Printf("ai-gateway %s (commit %s)\n", handler.GatewayVersion, handler.GatewayCommit)
			return
		}
	}

	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
	}
	// config.Load loads .env files, but webhook.Global was already built in
	// package init() from the (then-empty) process env — rebuild it so
	// WEBHOOK_URL/WEBHOOK_SECRET configured via .env actually take effect.
	webhook.ReinitFromEnv()
	cfgTrustedProxies = cfg.TrustedProxies

	// Phase 3 dialect toggle: postgres:// uses lib/pq; sqlite remains default
	if strings.HasPrefix(cfg.DatabaseURL, "postgres://") || strings.HasPrefix(cfg.DatabaseURL, "postgresql://") {
		log.Info().Str("dialect", db.Dialect()).Msg("postgres dialect active")
	} else {
		log.Info().Str("dialect", db.Dialect()).Msg("sqlite dialect active")
	}

	database, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to open db")
	}
	// One-time backfill of request_logs.key_id from the key prefix for rows
	// written before per-key attribution existed (migration 012). Best-effort
	// and idempotent; ambiguous legacy prefixes are skipped.
	if n, err := db.BackfillKeyIDs(database); err != nil {
		log.Warn().Err(err).Msg("key_id backfill failed")
	} else if n > 0 {
		log.Info().Int64("rows", n).Msg("backfilled per-key analytics attribution")
	}

	providerStore := provider.NewStore(database, cfg.MasterKey)
	apiKeyStore := apikey.NewStore(database)
	catalogStore := catalog.NewStore(database)
	userStore := user.NewStore(database)
	// Bootstrap admin user from ADMIN_PASSWORD if no dashboard users exist.
	// Production requires an explicit strong password (enforced by config.Load);
	// we never seed known credentials there.
	if cnt, _ := userStore.Count(); cnt == 0 {
		if cfg.Production {
			if cfg.AdminPassword != "" && cfg.AdminPassword != "admin123" {
				if _, err := userStore.Create("admin", cfg.AdminPassword, "admin", "Admin"); err != nil {
					log.Warn().Err(err).Msg("failed to bootstrap admin user from ADMIN_PASSWORD")
				} else {
					log.Info().Msg("bootstrapped admin user from ADMIN_PASSWORD")
				}
			}
		} else if cfg.AdminPassword != "" && cfg.AdminPassword != "admin123" {
			if _, err := userStore.Create("admin", cfg.AdminPassword, "admin", "Admin"); err != nil {
				log.Warn().Err(err).Msg("failed to bootstrap admin user from ADMIN_PASSWORD")
			} else {
				log.Info().Msg("bootstrapped admin user from ADMIN_PASSWORD")
			}
		} else if cnt == 0 {
			// Create default admin with admin123 for dev only (allows RBAC UI to work)
			if _, err := userStore.Create("admin", "admin123", "admin", "Admin"); err != nil {
				log.Debug().Err(err).Msg("bootstrap default admin skipped (may already exist)")
			} else {
				log.Warn().Msg("DEV ONLY: default admin/admin123 created — never use in production")
			}
		}
	}
	discoveryService := discovery.New(database, providerStore, catalogStore)
	proxyHandler := proxy.NewWithCatalog(providerStore, catalogStore, database)
	// Hardened transport + timeouts (long streams are bounded by an idle
	// watchdog, not a blanket client/server deadline).
	proxyHandler.Timeouts = proxy.TimeoutsConfig{
		UpstreamHeader:   time.Duration(cfg.UpstreamHeaderTimeoutSecs) * time.Second,
		RequestTotal:     time.Duration(cfg.RequestTotalTimeoutSecs) * time.Second,
		StreamIdle:       time.Duration(cfg.StreamIdleTimeoutSecs) * time.Second,
		WriteHeaderGrace: time.Duration(cfg.WriteHeaderGraceSecs) * time.Second,
	}
	// Client-facing first-byte budget: default 85s (inside Cloudflare's
	// ~100s edge window so the gateway answers before it can 524). Explicit
	// values win, including 0 to disable; -1 (the env default) keeps 85s.
	switch {
	case cfg.TTFBTimeoutSecs >= 0:
		proxyHandler.Timeouts.TTFB = time.Duration(cfg.TTFBTimeoutSecs) * time.Second
		if cfg.TTFBTimeoutSecs == 0 {
			log.Info().Msg("TTFB_TIMEOUT_SECONDS=0: client first-byte budget disabled (edge proxies may 524 slow upstreams)")
		}
	default:
		proxyHandler.Timeouts.TTFB = proxy.DefaultTimeouts().TTFB
	}
	proxyHandler.CacheTTLSeconds = cfg.CacheTTLSeconds
	proxyHandler.LogBodies = cfg.LogBodies
	proxyHandler.BodyLogMaxBytes = cfg.BodyLogMaxBytes
	proxyHandler.StreamUsageInject = cfg.StreamUsageInject
	proxyHandler.LegacyFallback = cfg.RoutingLegacyFallback
	if cfg.RoutingLegacyFallback {
		log.Info().Msg("ROUTING_LEGACY_FALLBACK=true: heuristic resolution enabled for bare model names without routing rules")
	}
	// Phase 2 wiring: cache selection via REDIS_URL, fallback to MemoryCache gracefully
	if cfg.RedisURL != "" {
		if rc, err := cache.NewRedisCache(cfg.RedisURL); err == nil {
			proxyHandler.Cache = rc
			log.Info().Str("redis_url", cfg.RedisURL).Msg("redis cache enabled")
		} else {
			log.Warn().Err(err).Msg("redis cache init failed, falling back to memory")
			proxyHandler.Cache = cache.NewMemoryCache(512)
		}
	} else {
		proxyHandler.Cache = cache.NewMemoryCache(512)
	}
	discoveryService.Cache = proxyHandler.Cache
	proxyHandler.Retry = &resilience.DefaultRetryPolicy{MaxRetries: cfg.RetryMaxRetries, BaseDelay: time.Duration(cfg.RetryBaseDelayMs) * time.Millisecond}
	proxyHandler.Metrics = otel.NewMetrics()

	go func() {
		// Sync now when empty, then re-sync daily: pricing/model metadata
		// drift over time and the "boot-sync only when empty" behavior left
		// deployments with a frozen snapshot forever. Failure surfaces in
		// logs and /health config is unaffected.
		syncCatalog := func(reason string) {
			n, err := catalogStore.FetchAndSync()
			if err != nil {
				log.Error().Err(err).Str("trigger", reason).Msg("catalog sync failed")
			} else {
				log.Info().Int("models", n).Str("trigger", reason).Msg("catalog synced")
			}
		}
		count, _ := catalogStore.Count()
		if count == 0 {
			log.Info().Msg("catalog empty, syncing from models.dev...")
			syncCatalog("boot")
		}
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			syncCatalog("daily")
		}
	}()

	provider.StartHealthChecker(database, providerStore, 5*time.Minute)

	// singletons
	_ = webhook.Global // rebuilt post-env-load via webhook.ReinitFromEnv() above
	// Nightly retention purge for request_logs when LOG_RETENTION_DAYS is set.
	if cfg.LogRetentionDays > 0 {
		go runLogRetention(database, cfg.LogRetentionDays)
	}
	rec := audit.NewDBRecorder(database)
	limiter := budget.NewDBLimiter(database)
	lbStore := lb.NewStore(database)
	proxyHandler.LB = lbStore
	proxyHandler.Usage = &budget.UsageSink{Limiter: limiter}

	var rl middleware.Limiter
	if cfg.RedisURL != "" {
		if rrl, err := middleware.NewRedisRateLimiter(cfg.RedisURL); err == nil {
			rl = rrl
			log.Info().Str("redis_url", cfg.RedisURL).Msg("redis rate limiter enabled")
		} else {
			log.Warn().Err(err).Msg("redis rate limiter init failed, falling back to memory")
			rl = middleware.NewRateLimiter()
		}
	} else {
		rl = middleware.NewRateLimiter()
	}
	proxyHandler.RateLimiter = rl
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recovery)
	r.Use(middleware.Logger)
	r.Use(audit.Middleware(rec))
	// Cloudflare Tunnel / reverse proxy: trust X-Forwarded-* and CF-Connecting-IP
	// This makes RemoteAddr, scheme and host correct behind cloudflared.
	r.Use(forwardedHeaders)
	r.Use(securityHeaders)
	// CORS: permissive by default ("*") so tunnel public URL works out-of-the-box.
	// Set PUBLIC_URL or CORS_ALLOWED_ORIGINS to lock down. See .env.example.
	allowedOrigins := cfg.AllowedOrigins()
	allowCreds := false
	if len(allowedOrigins) == 1 && allowedOrigins[0] == "*" {
		allowCreds = false
	} else {
		// When specific origins are set, allow credentials for cookie auth (gw_token)
		allowCreds = true
	}
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   allowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH", "HEAD"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-Requested-With", "x-api-key", "X-Provider", "anthropic-version", "X-Gateway-Key-Prefix", "X-Gateway-Org", "X-Request-ID"},
		ExposedHeaders:   []string{"X-Request-ID", "Content-Type", "Authorization"},
		AllowCredentials: allowCreds,
		MaxAge:           300,
	}))

	// liveness: always 200 when process is up (no DB check)
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		dbStatus := "up"
		if err := database.Ping(); err != nil {
			dbStatus = "down"
		}
		resp := map[string]any{
			"status":    "ok",
			"version":   handler.GatewayVersion,
			"config_ok": cfg.ConfigOK(),
			"db":        dbStatus,
		}
		// Configuration/deployment detail (cors origins, public URL) is only
		// disclosed to loopback callers — orchestrator probes and the launch
		// gate run locally; unauthenticated remote callers get the minimal set.
		if isLoopbackRequest(r) {
			if cfg.PublicURL != "" {
				resp["public_url"] = cfg.PublicURL
			}
			// expose cors for tunnel debugging (not sensitive locally)
			resp["cors"] = cfg.AllowedOrigins()
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	// readiness: checks db.Ping, returns 503 when DB down
	r.Get("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := database.Ping(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"ready":false,"db":"down"}`))
			return
		}
		w.Write([]byte(`{"ready":true,"db":"up"}`))
	})
	// real Prometheus metrics via otel + promhttp. METRICS_PROTECT=true gates
	// the endpoint behind dashboard auth for exposure to hostile networks.
	if cfg.MetricsRequireAuth {
		r.Group(func(r chi.Router) {
			r.Use(auth.AdminMiddleware(cfg.JWTSecret))
			r.Handle("/metrics", promhttp.Handler())
		})
	} else {
		r.Handle("/metrics", promhttp.Handler())
	}

	admin := &handler.AdminHandler{
		ProviderStore: providerStore,
		APIKeyStore:   apiKeyStore,
		Config:        cfg,
		DB:            database,
		Discovery:     discoveryService,
		UserStore:     userStore,
		Recorder:      rec,
		AuthLimiter:   middleware.NewAuthRateLimiter(),
	}
	patStore := pat.NewStore(database)
	auth.SetPATAuthenticator(
		func(raw string) (string, string, bool) {
			uid, scopes, ok := patStore.Authenticate(raw)
			if !ok || !patStore.UserIDValid(uid) {
				return "", "", false
			}
			return uid, scopes, true
		},
		func(userID string) (string, string, error) {
			u, err := userStore.GetByID(userID)
			if err != nil || u.Disabled {
				return "", "", fmt.Errorf("user disabled or missing")
			}
			return u.Username, string(u.Role), nil
		},
	)

	usersHandler := &handler.UsersHandler{
		Store:       userStore,
		Config:      cfg,
		DB:          database,
		Recorder:    rec,
		APIKeyStore: apiKeyStore,
	}

	// DB-backed webhooks: every enabled webhook row receives matching events.
	webhookDispatch := webhook.NewDBDispatch(database)
	webhook.SetGlobal(webhookDispatch)
	webhookHandler := &handler.WebhookHandler{DB: database, Dispatcher: webhookDispatch, Recorder: rec}
	profileHandler := &handler.ProfileHandler{
		Store:    userStore,
		DB:       database,
		Recorder: rec,
		PATs:     patStore,
	}
	passkeyHandler, err := passkey.NewHandler(database, userStore, cfg)
	if err != nil {
		log.Warn().Err(err).Msg("passkey handler init failed, passkey disabled")
		passkeyHandler = nil
	}
	catalogHandler := &handler.CatalogHandler{
		Store: catalogStore,
		DB:    database,
	}
	discoveryHandler := &handler.DiscoveryHandler{
		Service: discoveryService,
	}
	orgHandler := &handler.OrgHandler{
		DB:       database,
		Recorder: rec,
	}
	r.Route("/api", func(r chi.Router) {
		// CSRF origin-integrity for cookie-authenticated mutations (bearer-token
		// API clients are unaffected).
		r.Use(middleware.CSRFProtection)
		// Fine-grained RBAC: RequirePerm middleware resolves effective
		// permissions through the user store (live per request).
		r.Use(middleware.WithPermResolver(userStore))
		admin.Routes(r)
		// Passkey public endpoints (no auth) — for login via passkey + recovery.
		// Each credential-verification path gets brute-force limiting.
		authLimiter := middleware.NewAuthRateLimiter()
		acctOf := middleware.AccountFromLoginBody
		if passkeyHandler != nil {
			r.With(authLimiter.Middleware(acctOf)).Post("/auth/passkey/login/begin", passkeyHandler.BeginLogin)
			r.With(authLimiter.Middleware(func(req *http.Request) string { return "" })).Post("/auth/passkey/login/finish", passkeyHandler.FinishLogin)
			r.With(authLimiter.Middleware(acctOf)).Post("/auth/recovery/verify", passkeyHandler.VerifyRecovery)
		}
		r.Group(func(r chi.Router) {
			r.Use(auth.AdminMiddlewareWithRevocation(cfg.JWTSecret, userStore))
			profileHandler.Routes(r)
			webhookHandler.Routes(r)
			usersHandler.Routes(r)
			if passkeyHandler != nil {
				r.Post("/auth/passkey/register/begin", passkeyHandler.BeginRegistration)
				r.Post("/auth/passkey/register/finish", passkeyHandler.FinishRegistration)
				r.Get("/auth/passkey/credentials", passkeyHandler.ListCredentials)
				r.Post("/auth/passkey/recovery/generate", passkeyHandler.GenerateRecovery)
				r.Post("/auth/passkey/disable", passkeyHandler.DisablePasskey)
			}
			r.With(middleware.RequirePerm(rbac.PermAuditRead)).Get("/audit", admin.ListAudit)
			r.Route("/models", catalogHandler.Routes)
			// Load-balancer rule management (admin-only).
			routingHandler := handler.NewRoutingHandler(lbStore)
			routingHandler.Routes(r)
			// Mutating discovery/catalog/config routes are admin-only; readonly
			// and member roles retain read access where exposed.
			r.With(middleware.RequirePerm(rbac.PermCatalogWrite)).Post("/provider-models", discoveryHandler.AddManual)
			r.Get("/provider-models", discoveryHandler.List)
			r.With(middleware.RequirePerm(rbac.PermCatalogWrite)).Put("/provider-models/{id}", discoveryHandler.Update)
			r.With(middleware.RequirePerm(rbac.PermCatalogWrite)).Post("/provider-models/{id}/enrich", discoveryHandler.Enrich)
			r.With(middleware.RequirePerm(rbac.PermCatalogWrite)).Delete("/provider-models/{id}", discoveryHandler.Delete)
			r.With(middleware.RequirePerm(rbac.PermProvidersWrite)).Post("/providers/{id}/discover", discoveryHandler.DiscoverProvider)
			r.With(middleware.RequirePerm(rbac.PermProvidersWrite)).Post("/discover-all", discoveryHandler.DiscoverAll)
			// org scaffold — admin-only, RBAC enforced
			orgHandler.Routes(r)
		})
	})

	r.Group(func(r chi.Router) {
		r.Use(middleware.GatewayAuthWithJWTRevocation(apiKeyStore, cfg.JWTSecret, userStore))
		r.Use(budget.Middleware(limiter))
		r.Use(middleware.GatewayRateLimitWithLimits(rl, func(req *http.Request) middleware.RateLimits {
			if k, ok := middleware.GatewayKeyFromContext(req.Context()); ok && k != nil {
				rpm := k.RateLimitRPM
				if rpm == 0 {
					rpm = 60
				}
				return middleware.RateLimits{RPM: rpm, RPH: k.RateLimitRPH, RPD: k.RateLimitRPD}
			}
			return middleware.RateLimits{RPM: 60}
		}))
		r.Post("/v1/chat/completions", proxyHandler.ChatCompletions)
		r.Post("/chat/completions", proxyHandler.ChatCompletions)
		r.Post("/v1/completions", proxyHandler.Completions)
		r.Post("/completions", proxyHandler.Completions)
		r.Post("/v1/embeddings", proxyHandler.Embeddings)
		r.Post("/embeddings", proxyHandler.Embeddings)
		r.Get("/v1/models", proxyHandler.Models)
		r.Get("/v1/models/{id}", proxyHandler.GetModel)
		r.Get("/models", proxyHandler.Models)
		// Anthropic compat — handle both /v1/messages and /messages for SDK baseURL flexibility
		r.Post("/v1/messages", proxyHandler.AnthropicMessages)
		r.Post("/messages", proxyHandler.AnthropicMessages)
		// Responses API — both with and without /v1
		r.Post("/v1/responses", proxyHandler.Responses)
		r.Post("/responses", proxyHandler.Responses)
	})

	// openapi: served from the embedded spec so the binary is self-contained
	// outside a source checkout (the old filesystem fallbacks included the
	// original developer's absolute path).
	r.Get("/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/yaml")
		w.Write(embeddedOpenAPI)
	})
	// SPA fallback - must be last and not intercept /api or /v1
	r.NotFound(serveWeb(database))
	r.Get("/*", serveWeb(database))

	addr := ":" + cfg.Port
	srv := &http.Server{
		Addr:        addr,
		Handler:     r,
		ReadTimeout: 30 * time.Second,
		// WriteTimeout is intentionally 0: streaming LLM responses are managed
		// per-request via ResponseController deadlines in the proxy handlers.
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	// Socket activation (systemd): when launched with LISTEN_FDS, serve on the
	// inherited listener instead of binding our own. The socket then belongs to
	// systemd, not the process, so a gateway restart stops accepting only for
	// microseconds — connections that arrive during the restart are queued in
	// the kernel backlog and served by the new process. Without this, every
	// restart drops connections from the moment the old process closes its
	// listener until the new one binds (SDKs retry, but tail latency spikes).
	ln, listenFDs := inheritListener()
	if ln != nil {
		log.Info().Int("fds", listenFDs).Msg("socket activation: serving on inherited listener (LISTEN_FDS)")
	} else {
		log.Info().Str("addr", addr).Str("public_url", cfg.PublicURL).Strs("cors", cfg.AllowedOrigins()).Msg("starting AI Gateway " + handler.GatewayVersion + " (tunnel-ready)")
		if cfg.PublicURL != "" {
			log.Info().Str("public_url", cfg.PublicURL).Msg("Cloudflare Tunnel: ensure cloudflared points to http://localhost:" + cfg.Port + " and PUBLIC_URL matches tunnel hostname")
		}
		log.Info().Msg("Admin login: POST /api/auth/login {password: $ADMIN_PASSWORD} -> token")
		log.Info().Msg("Gateway auth: Authorization: Bearer sk-gw-... for /v1/*")
		log.Info().Msg("Models catalog: GET /api/models/catalog?q=gpt&provider=openai (auth required)")
	}
	go func() {
		var err error
		if ln != nil {
			err = srv.Serve(ln)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("server failed")
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info().Msg("shutting down...")
	// Graceful drain: stop accepting, then let in-flight requests (long LLM
	// streams included) finish within SHUTDOWN_GRACE_SECONDS. After the grace
	// window, Close() force-terminates whatever remains so the process always
	// exits — a stuck stream must not turn SIGTERM into a SIGKILL from the
	// supervisor (which would lose the graceful handoff entirely).
	drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Duration(cfg.ShutdownGraceSecs)*time.Second)
	defer drainCancel()
	if err := srv.Shutdown(drainCtx); err != nil {
		log.Warn().Err(err).Msg("drain window expired; closing remaining connections")
		_ = srv.Close()
	}
	// Flush SQLite WAL so the on-disk database is consistent for the next
	// process even if the machine loses power right after exit.
	if err := database.Close(); err != nil {
		log.Warn().Err(err).Msg("db close error")
	}
	log.Info().Msg("exited")
}

// inheritListener implements the systemd socket-activation protocol
// (sd_listen_fds): when LISTEN_FDS=1 and LISTEN_PID equals this process's
// pid, fd 3 is an already-bound, already-listening socket passed by the
// supervisor. Returning it lets restarts hand off the listen socket with no
// connection loss. Any malformed activation environment is ignored — the
// gateway then binds normally.
func inheritListener() (net.Listener, int) {
	pid := os.Getenv("LISTEN_PID")
	fds := os.Getenv("LISTEN_FDS")
	if pid == "" || fds == "" {
		return nil, 0
	}
	if n, err := strconv.Atoi(pid); err != nil || n != os.Getpid() {
		return nil, 0
	}
	n, err := strconv.Atoi(fds)
	if err != nil || n < 1 {
		return nil, 0
	}
	// Unset the env vars so child processes (none today, but discovery or
	// future plugins) don't inherit a stale activation context.
	defer os.Unsetenv("LISTEN_PID")
	defer os.Unsetenv("LISTEN_FDS")
	// sd_listen_fds spec: fds 0/1/2 are stdin/stdout/stderr, so the first
	// listening socket is fd 3.
	file := os.NewFile(3, "systemd-listen-fd")
	if file == nil {
		return nil, 0
	}
	ln, err := net.FileListener(file)
	if err != nil {
		log.Warn().Err(err).Msg("socket activation: inherited fd is not a usable listener; falling back to bind")
		return nil, 0
	}
	return ln, n
}

func forwardedHeaders(next http.Handler) http.Handler {
	trusted := parseTrustedProxies(cfgTrustedProxies)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerIP := peerHost(r)
		// Spoof-resistant defaults: forwarded headers are honored only from
		// loopback peers (cloudflared tunnel default) or configured proxies.
		if !ipInRanges(peerIP, trusted) {
			next.ServeHTTP(w, r)
			return
		}
		// Cloudflare Tunnel sets CF-Connecting-IP to real client IP.
		// Also respect X-Forwarded-For (first entry) and X-Real-IP.
		realIP := ""
		if cf := r.Header.Get("CF-Connecting-IP"); cf != "" {
			realIP = strings.TrimSpace(cf)
		} else if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// X-Forwarded-For may be "client, proxy1, proxy2"
			if idx := strings.Index(xff, ","); idx != -1 {
				realIP = strings.TrimSpace(xff[:idx])
			} else {
				realIP = strings.TrimSpace(xff)
			}
		} else if xri := r.Header.Get("X-Real-IP"); xri != "" {
			realIP = strings.TrimSpace(xri)
		}
		if realIP != "" {
			r.Header.Set("X-Real-IP", realIP)
			// Override RemoteAddr for rate limiting / logging (keep port dummy)
			if !strings.Contains(realIP, ":") {
				r.RemoteAddr = realIP + ":0"
			} else {
				r.RemoteAddr = realIP
			}
		}
		// Scheme from X-Forwarded-Proto (cloudflared sets to https for public URL)
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			r.URL.Scheme = proto
		}
		// Host from X-Forwarded-Host or original Host
		if fwdHost := r.Header.Get("X-Forwarded-Host"); fwdHost != "" {
			r.Host = fwdHost
		}
		next.ServeHTTP(w, r)
	})
}

// runLogRetention deletes request_logs older than the configured retention
// window once per day. Runs forever until process exit; best-effort.
func runLogRetention(database *sql.DB, days int) {
	if database == nil || days <= 0 {
		return
	}
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	purge := func() {
		cutoff := time.Now().UTC().AddDate(0, 0, -days)
		var bound string
		if db.Dialect() == "postgres" {
			bound = cutoff.Format(time.RFC3339Nano)
		} else {
			bound = cutoff.Format("2006-01-02 15:04:05.999999999+00:00")
		}
		res, err := database.Exec(`DELETE FROM request_logs WHERE created_at < ?`, bound)
		if err != nil {
			log.Warn().Err(err).Msg("log retention purge failed")
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Info().Int64("deleted", n).Int("days", days).Msg("request log retention purge")
		}
	}
	purge()
	for range ticker.C {
		purge()
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "0")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		// Baseline CSP: same-origin resources; inline kept because the Vite
		// build injects inline styles/bootstrap script. Google Fonts (loaded
		// by web/index.html) is allowed for stylesheet + font fetches; its CSS
		// is fetched with CORS so style-src-elem needs the explicit origin.
		// Still blocks external script/object/iframe injection paths an XSS
		// payload relies on.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; style-src-elem 'self' 'unsafe-inline' https://fonts.googleapis.com; img-src 'self' data:; font-src 'self' data: https://fonts.gstatic.com; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// isLoopbackRequest reports whether the request originates from a loopback
// address (directly or via a TRUSTED proxy header resolution already applied).
func isLoopbackRequest(r *http.Request) bool {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// cfgTrustedProxies is set in main() from config before the router starts.
var cfgTrustedProxies string

// parseTrustedProxies builds a trusted-peer set: loopback always, plus any
// CIDRs/IPs from TRUSTED_PROXIES ("*" disables the check entirely).
func parseTrustedProxies(spec string) []*net.IPNet {
	nets := []*net.IPNet{
		mustCIDR("127.0.0.0/8"),
		mustCIDR("::1/128"),
	}
	if strings.TrimSpace(spec) == "*" {
		return []*net.IPNet{} // empty list handled as trust-all below
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n := mustCIDR(part)
		if n != nil {
			nets = append(nets, n)
		}
	}
	return nets
}

func mustCIDR(s string) *net.IPNet {
	if !strings.Contains(s, "/") {
		if ip := net.ParseIP(s); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			s = fmt.Sprintf("%s/%d", ip.String(), bits)
		}
	}
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		return nil
	}
	return n
}

// peerHost extracts the bare IP of the direct connection peer.
func peerHost(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.Trim(host, "[]")
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host
}

func ipInRanges(ipStr string, ranges []*net.IPNet) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	// Empty range set = trust all (explicit TRUSTED_PROXIES="*").
	if len(ranges) == 0 {
		return true
	}
	for _, n := range ranges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func serveWeb(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/health") || strings.HasPrefix(r.URL.Path, "/ready") || strings.HasPrefix(r.URL.Path, "/metrics") || strings.HasPrefix(r.URL.Path, "/openapi.yaml") {
			http.NotFound(w, r)
			return
		}
		sub, err := fs.Sub(embeddedWeb, "web")
		if err != nil {
			serveFallback(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		// Cache policy: hashed /assets/* files are immutable (content hash in
		// the filename) — cache for a year. index.html and SPA fallbacks must
		// never be cached: they reference the hashed filenames, and a stale
		// shell leaves users stuck on an old UI version after every deploy.
		if strings.HasPrefix(path, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		}
		// SPA fallback: unknown paths (client-side routes like /keys) get
		// index.html. Serve it explicitly — http.FileServer re-resolves
		// r.URL.Path itself and would 404 the original path. Asset paths are
		// exempt: a missing /assets/*.js is a real 404 (stale HTML reference),
		// not a client-side route.
		if _, err := fs.Stat(sub, path); err != nil {
			if strings.HasPrefix(path, "assets/") {
				http.NotFound(w, r)
				return
			}
			// Guard against path traversal ("/../"): only serve the app shell
			// when the request path is clean.
			if r.URL.Path != pathpkg.Clean(r.URL.Path) {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			f, err := sub.Open("index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer f.Close()
			_, _ = io.Copy(w, f)
			return
		}
		http.FileServer(http.FS(sub)).ServeHTTP(w, r)
	}
}

func serveFallback(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/v1/") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>AI Gateway</title><style>body{font-family:system-ui, sans-serif; max-width:800px; margin:40px auto; padding:20px; background:#0F1311; color:#F8F6F1} a{color:#2CF6B3} code{background:#1a1f1d; padding:2px 6px; border-radius:4px} .card{border:1px solid #2a2f2d; padding:20px; border-radius:12px; margin:20px 0} h1{color:#FFB84D}</style></head><body><h1>⚡ AI Gateway — 1.5</h1><p>Go gateway is running. UI not yet built — use API.</p></body></html>`))
}
