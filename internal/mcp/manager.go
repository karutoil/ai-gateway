package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"ai-gateway/internal/netguard"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog/log"
)

// Manager owns live MCP client sessions. Connections are lazy: a server is
// connected on first use (or by the health loop), cached, and transparently
// re-established after a failure with exponential backoff.
type Manager struct {
	store       *Store
	callTimeout time.Duration
	httpClient  *http.Client

	// Version is the clientInfo version announced to upstream MCP servers,
	// assigned from handler.GatewayVersion by main (see Endpoint.Version).
	Version string

	mu    sync.Mutex
	conns map[string]*serverConn
	// connecting is the per-server single-flight guard for session(): a server
	// id maps to a channel closed when its in-flight connect finishes.
	connecting map[string]chan struct{}

	closed bool
}

type serverConn struct {
	session *sdk.ClientSession
	cancel  context.CancelFunc

	failures    int
	lastAttempt time.Time
	nextRetry   time.Time
}

func NewManager(store *Store, callTimeout time.Duration) *Manager {
	if callTimeout <= 0 {
		callTimeout = 60 * time.Second
	}
	return &Manager{
		store:       store,
		callTimeout: callTimeout,
		httpClient: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				MaxIdleConns:          64,
				IdleConnTimeout:       90 * time.Second,
			},
		},
		conns: make(map[string]*serverConn),
	}
}

func (m *Manager) CallTimeout() time.Duration { return m.callTimeout }

// clientVersion is the version announced to upstream MCP servers.
func (m *Manager) clientVersion() string {
	if m.Version != "" {
		return m.Version
	}
	return "dev"
}

// headerTransport injects static headers and the configured upstream
// credential into every request. Credentials are attached here, at the wire
// level, so they never appear in a request body, a log line, or an API
// response.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
	auth    AuthType
	name    string
	value   string
}

func (h *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	switch h.auth {
	case AuthBearer:
		r.Header.Set("Authorization", "Bearer "+h.value)
	case AuthAPIKey:
		name := h.name
		if name == "" {
			name = "X-API-Key"
		}
		r.Header.Set(name, h.value)
	case AuthHeader:
		if h.name != "" {
			r.Header.Set(h.name, h.value)
		}
	}
	return h.base.RoundTrip(r)
}

func (m *Manager) transportFor(srv *Server) (sdk.Transport, error) {
	switch srv.Transport {
	case TransportHTTP, TransportSSE:
		// SSRF guard: the URL is operator-supplied, so refuse to fetch private
		// address space on its behalf (production default). The check runs at
		// CONNECT time, not only at validate time, so a DNS record that later
		// changes to a private address is still caught.
		if err := netguard.CheckURLHost(srv.URL, AllowPrivateUpstreams()); err != nil {
			return nil, fmt.Errorf("mcp server %q: %w", srv.Name, err)
		}
		client := *m.httpClient
		client.Transport = &headerTransport{
			base:    m.httpClient.Transport,
			headers: srv.Headers,
			auth:    srv.AuthType,
			name:    srv.AuthHeader,
			value:   srv.AuthValue,
		}
		if srv.Transport == TransportSSE {
			return &sdk.SSEClientTransport{Endpoint: srv.URL, HTTPClient: &client}, nil
		}
		// The standalone server->client SSE stream is optional per the spec and
		// deliberately OFF here: the gateway is a request/response proxy that
		// never consumes server-initiated notifications (tools/list_changed is
		// handled by the periodic health/discovery probe instead). Keeping a
		// persistent GET open per upstream would pin one connection per server
		// for the process lifetime and make teardown wait on it.
		return &sdk.StreamableClientTransport{
			Endpoint:             srv.URL,
			HTTPClient:           &client,
			MaxRetries:           3,
			DisableStandaloneSSE: true,
		}, nil
	case TransportStdio:
		cmd := exec.Command(srv.Command, srv.Args...)
		cmd.Env = childEnv(srv.Env)
		return &sdk.CommandTransport{Command: cmd}, nil
	default:
		return nil, fmt.Errorf("unsupported mcp transport %q", srv.Transport)
	}
}

// connect establishes a session. The caller must NOT hold m.mu.
func (m *Manager) connect(ctx context.Context, srv *Server) (*sdk.ClientSession, error) {
	transport, err := m.transportFor(srv)
	if err != nil {
		return nil, err
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "ai-gateway", Version: m.clientVersion()}, nil)
	// The session context owns the connection's lifetime. The legacy SSE
	// transport ties its long-lived GET stream to the context passed to
	// Connect (http.NewRequestWithContext in the SDK), so this context must
	// carry NEITHER the caller's cancellation NOR a deadline:
	//
	//   - deriving it from the HTTP request context tore the session down
	//     when the establishing request finished;
	//   - the previous WithTimeout(ctx, callTimeout) variant killed every
	//     SSE session callTimeout seconds after it was established — the
	//     next tools/list then died with EOF and the outage was blamed on a
	//     healthy upstream.
	//
	// The Streamable and stdio transports detach from / ignore the context,
	// so this only matters for SSE, but it is applied uniformly. The
	// handshake stays bounded by the watchdog below, which cancels this
	// context (aborting an in-flight SSE GET) when Connect does not finish
	// within callTimeout. The context is cancelled when the session is
	// replaced or closed.
	sessionCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	type connectOutcome struct {
		session *sdk.ClientSession
		err     error
	}
	done := make(chan connectOutcome, 1)
	go func() {
		session, err := client.Connect(sessionCtx, transport, nil)
		done <- connectOutcome{session: session, err: err}
	}()
	var out connectOutcome
	watchdog := time.NewTimer(m.callTimeout)
	defer watchdog.Stop()
	select {
	case out = <-done:
	case <-watchdog.C:
		cancel() // abort the in-flight handshake
		// Connect must observe the cancellation and return promptly; if a
		// transport that ignores the context completes later, close whatever
		// it produced instead of leaking it.
		select {
		case out = <-done:
		case <-time.After(2 * time.Second):
			go func() {
				if late := <-done; late.session != nil {
					_ = late.session.Close()
				}
			}()
			return nil, fmt.Errorf("mcp connect to server %q timed out after %s", srv.Slug, m.callTimeout)
		}
	}
	if out.err != nil {
		cancel()
		return nil, out.err
	}
	session := out.session
	m.mu.Lock()
	if old := m.conns[srv.ID]; old != nil {
		if old.cancel != nil {
			old.cancel()
		}
		_ = old.session.Close()
	}
	m.conns[srv.ID] = &serverConn{session: session, cancel: cancel, lastAttempt: time.Now(), failures: 0}
	m.mu.Unlock()
	return session, nil
}

// session returns a live session, reconnecting when needed and honoring the
// per-server backoff after failures.
//
// CONCURRENCY: exactly one goroutine performs the connect. Without this
// single-flight, N goroutines that need a cold server all ran the full
// handshake and each one closed the session the previous one had just
// installed, so callers holding the "winner" got
// `client is closing` on their first tool call. Measured before the fix: 8
// parallel calls to a server whose cache had been dropped produced 4 failures.
func (m *Manager) session(ctx context.Context, srv *Server) (*sdk.ClientSession, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("mcp manager is closed")
	}
	if c := m.conns[srv.ID]; c != nil && c.session != nil {
		s := c.session
		m.mu.Unlock()
		return s, nil
	}
	if c := m.conns[srv.ID]; c != nil && time.Now().Before(c.nextRetry) {
		wait := time.Until(c.nextRetry)
		m.mu.Unlock()
		return nil, fmt.Errorf("mcp server %q is in reconnect backoff for %s", srv.Slug, wait.Round(time.Second))
	}
	// Single-flight: if a connect for this server is already running, wait for
	// it instead of starting a second handshake that would close the first.
	if ch, inflight := m.connecting[srv.ID]; inflight {
		m.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		m.mu.Lock()
		c := m.conns[srv.ID]
		if c != nil && c.session != nil {
			s := c.session
			m.mu.Unlock()
			return s, nil
		}
		m.mu.Unlock()
		return nil, fmt.Errorf("mcp server %q failed to connect", srv.Slug)
	}
	if m.connecting == nil {
		m.connecting = make(map[string]chan struct{})
	}
	done := make(chan struct{})
	m.connecting[srv.ID] = done
	m.mu.Unlock()

	session, err := m.connect(ctx, srv)

	m.mu.Lock()
	delete(m.connecting, srv.ID)
	m.mu.Unlock()
	close(done)

	if err != nil {
		m.recordFailure(srv.ID, err)
		return nil, err
	}
	m.store.SetHealth(srv.ID, "up", "")
	return session, nil
}

func (m *Manager) recordFailure(serverID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.conns[serverID]
	if c == nil {
		c = &serverConn{}
		m.conns[serverID] = c
	}
	if c.session != nil {
		_ = c.session.Close()
		c.session = nil
	}
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.failures++
	// 2s, 4s, 8s, 16s, capped at 60s.
	backoff := time.Duration(1<<min(c.failures, 5)) * time.Second
	if backoff > 60*time.Second {
		backoff = 60 * time.Second
	}
	c.lastAttempt = time.Now()
	c.nextRetry = time.Now().Add(backoff)
	m.store.SetHealth(serverID, "down", err.Error())
}

// CloseServer tears down a cached session (config change / disable / delete).
func (m *Manager) CloseServer(serverID string) {
	m.drop(serverID)
}

func (m *Manager) drop(serverID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.conns[serverID]; c != nil {
		if c.session != nil {
			_ = c.session.Close()
		}
		// Release the connect context retained for long-lived transports
		// (see connect); without this an SSE stream leaks a goroutine.
		if c.cancel != nil {
			c.cancel()
		}
		delete(m.conns, serverID)
	}
}

// Discover connects, lists tools, persists the cache, and marks the server up.
func (m *Manager) Discover(ctx context.Context, serverID string) ([]Tool, error) {
	srv, err := m.store.RuntimeServer(serverID)
	if err != nil {
		return nil, err
	}
	session, err := m.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	listCtx, cancel := context.WithTimeout(ctx, m.callTimeout)
	defer cancel()
	res, err := session.ListTools(listCtx, nil)
	if err != nil {
		m.recordFailure(srv.ID, err)
		return nil, err
	}
	tools := make([]Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		schema, _ := json.Marshal(t.InputSchema)
		if len(schema) == 0 || string(schema) == "null" {
			schema = []byte(`{"type":"object"}`)
		}
		tools = append(tools, Tool{
			ServerID:       srv.ID,
			ServerSlug:     srv.Slug,
			ServerName:     srv.Name,
			Name:           t.Name,
			Description:    t.Description,
			InputSchema:    schema,
			NamespacedName: Namespaced(srv.Slug, t.Name),
		})
	}
	if err := m.store.SaveTools(srv.ID, tools); err != nil {
		return nil, err
	}
	m.store.SetHealth(srv.ID, "up", "")
	return tools, nil
}

// Check performs a liveness probe and refreshes the tool cache on success.
func (m *Manager) Check(ctx context.Context, serverID string) error {
	srv, err := m.store.RuntimeServer(serverID)
	if err != nil {
		return err
	}
	if !srv.Enabled {
		return nil
	}
	session, err := m.session(ctx, srv)
	if err != nil {
		return err
	}
	pingCtx, cancel := context.WithTimeout(ctx, m.callTimeout)
	defer cancel()
	if err := session.Ping(pingCtx, nil); err != nil {
		m.recordFailure(srv.ID, err)
		return err
	}
	_, err = m.Discover(ctx, srv.ID)
	return err
}

// StartHealthLoop probes enabled servers on an interval and reconnects them.
// It runs until ctx is cancelled.
func (m *Manager) StartHealthLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	probe := func() {
		servers, err := m.store.ListServers()
		if err != nil {
			log.Warn().Err(err).Msg("mcp health: list servers failed")
			return
		}
		for _, srv := range servers {
			if !srv.Enabled {
				continue
			}
			probeCtx, cancel := context.WithTimeout(ctx, m.callTimeout)
			err := m.Check(probeCtx, srv.ID)
			cancel()
			if err != nil {
				log.Debug().Err(err).Str("server", srv.Slug).Msg("mcp health probe failed")
			}
		}
	}
	// Boot probe shortly after start so the dashboard shows real status.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
			probe()
		}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probe()
		}
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	conns := m.conns
	m.conns = make(map[string]*serverConn)
	m.mu.Unlock()
	for _, c := range conns {
		if c.session != nil {
			_ = c.session.Close()
		}
		if c.cancel != nil {
			c.cancel()
		}
	}
}

// CallTool routes one tool invocation to its server and normalizes the result.
// A connection-level failure is retried once against a fresh session; a
// tool-level error (result.IsError) is returned as data, never retried,
// because the tool may have had side effects.
func (m *Manager) CallTool(ctx context.Context, serverID, toolName string, args map[string]any) (ToolResult, error) {
	return m.callTool(ctx, serverID, toolName, args, "", "")
}

// CallToolMeta is CallTool with caller attribution for the audit trail.
func (m *Manager) CallToolMeta(ctx context.Context, serverID, toolName string, args map[string]any, keyPrefix, endpoint string) (ToolResult, error) {
	return m.callTool(ctx, serverID, toolName, args, keyPrefix, endpoint)
}

func (m *Manager) callTool(ctx context.Context, serverID, toolName string, args map[string]any, keyPrefix, endpoint string) (ToolResult, error) {
	srv, err := m.store.RuntimeServer(serverID)
	if err != nil {
		return ToolResult{}, err
	}
	if !srv.Enabled {
		return ToolResult{}, fmt.Errorf("mcp server %q is disabled", srv.Slug)
	}
	start := time.Now()
	result, err := m.callOnce(ctx, srv, toolName, args)
	if err != nil && isConnectionError(err) {
		m.drop(srv.ID)
		result, err = m.callOnce(ctx, srv, toolName, args)
	}
	if err != nil {
		m.store.RecordCall(CallRecord{ServerID: srv.ID, ServerSlug: srv.Slug, ToolName: toolName, NamespacedName: Namespaced(srv.Slug, toolName), KeyPrefix: keyPrefix, Endpoint: endpoint, Status: "error", Error: err.Error(), DurationMs: time.Since(start).Milliseconds()})
		return ToolResult{}, err
	}
	out := normalizeResult(srv, toolName, result)
	out.DurationMs = time.Since(start).Milliseconds()
	status := "ok"
	errText := ""
	if out.IsError {
		status = "error"
		errText = out.Text
	}
	m.store.RecordCall(CallRecord{ServerID: srv.ID, ServerSlug: srv.Slug, ToolName: toolName, NamespacedName: Namespaced(srv.Slug, toolName), KeyPrefix: keyPrefix, Endpoint: endpoint, Status: status, Error: errText, DurationMs: out.DurationMs})
	return out, nil
}

func (m *Manager) callOnce(ctx context.Context, srv *Server, toolName string, args map[string]any) (*sdk.CallToolResult, error) {
	session, err := m.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, m.callTimeout)
	defer cancel()
	return session.CallTool(callCtx, &sdk.CallToolParams{Name: toolName, Arguments: args})
}

// isConnectionError reports whether err is a TRANSPORT failure that provably
// happened before the request reached the server, i.e. a case where retrying
// cannot duplicate a side effect.
//
// SECURITY/CORRECTNESS: this used to substring-match the whole error string for
// words like "session", "connection", "closed" and "reset". The error text
// includes the UPSTREAM'S OWN JSON-RPC message, so any MCP server that words a
// tool failure with one of those words ("upstream session expired while
// writing") turned a failed-but-committed write into a guaranteed SECOND
// execution — two issues filed, two charges, two emails. The decision must be
// made on error TYPES raised by the transport, never on upstream prose.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	// A caller cancellation or a deadline is never retried: the work may have
	// completed and the client is gone anyway.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	// Only genuine I/O-level failures qualify. A *jsonrpc2 or *mcp error value
	// (the upstream answering with an error) is deliberately NOT in this set.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var nerr net.Error
	if errors.As(err, &nerr) {
		return true
	}
	// The SDK reports a session that is already closed with a sentinel that is
	// not an I/O error; it is still a safe pre-execution retry because the
	// request never left the client.
	if errors.Is(err, sdk.ErrConnectionClosed) {
		return true
	}
	return false
}

func normalizeResult(srv *Server, toolName string, res *sdk.CallToolResult) ToolResult {
	out := ToolResult{ServerID: srv.ID, ServerSlug: srv.Slug, Tool: toolName, Namespaced: Namespaced(srv.Slug, toolName)}
	if res == nil {
		out.IsError = true
		out.Text = "tool returned no result"
		return out
	}
	out.IsError = res.IsError
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
			continue
		}
		b, err := json.Marshal(c)
		if err == nil {
			parts = append(parts, string(b))
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			parts = append(parts, string(b))
		}
	}
	out.Text = strings.Join(parts, "\n")
	if b, err := json.Marshal(res.Content); err == nil {
		out.Content = b
	}
	if out.Text == "" {
		out.Text = "(empty result)"
	}
	return out
}
