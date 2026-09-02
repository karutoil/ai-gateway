package proxy

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// The gateway keeps the client connection SILENT until a usable upstream
// response exists (pre-commit retry/failover contract in proxyWithMetrics).
// Behind an edge proxy with a first-byte timeout — Cloudflare and
// Cloudflare Tunnel enforce ~100s — that silence is fatal: the edge
// synthesizes a 524, the client disconnects, and the gateway later logs a
// 499 for a request that was still healthy. Production showed exactly that
// signature: 499s clustered at ~124.9s (client gone after the 524) while the
// upstream was still within its 120s header budget.
//
// ttfbController defends against it:
//   - Each upstream attempt waits at most the client's REMAINING first-byte
//     budget, so the gateway regains control before the edge does.
//   - Buffered requests that run out of budget get an honest 504 (which
//     edges pass through) instead of a synthesized 524.
//   - Streaming requests commit SSE headers plus keepalive comment frames
//     (": keepalive"), which keep the edge's first-byte/idle clocks fed; the
//     upstream may then take as long as it needs while the client stays
//     connected.
type ttfbController struct {
	budget    time.Duration // 0 disables every check
	start     time.Time     // client request start (shared across candidates)
	mu        sync.Mutex    // serializes client writes once committed
	stopCh    chan struct{} // closed by stop() to end the heartbeat goroutine
	stopOnce  sync.Once
	committed atomic.Bool // commitKeepalive entered (exactly once)
	sent      atomic.Bool // keepalive status+first frame are on the wire
}

// keepaliveFrame is an SSE comment: invisible to event parsers but a real
// byte on the wire, which resets edge-proxy and client idle timers.
const keepaliveFrame = ": keepalive\n\n"

// keepaliveInterval must sit well below any plausible edge first-byte/idle
// timeout (Cloudflare: 100s connect / 524, ~30-60s typical idle windows).
var keepaliveInterval = 15 * time.Second

func newTTFBController(budget time.Duration, start time.Time) *ttfbController {
	return &ttfbController{budget: budget, start: start, stopCh: make(chan struct{})}
}

// age is time elapsed since the CLIENT request started — shared across the
// whole candidate/fallback chain, not per upstream attempt.
func (c *ttfbController) age() time.Duration { return time.Since(c.start) }

// expired reports whether the first-byte budget is spent and SSE headers
// have not been committed yet.
func (c *ttfbController) expired() bool {
	return c != nil && c.budget > 0 && !c.committed.Load() && c.age() >= c.budget
}

// remaining bounds how long the next upstream attempt may wait for response
// headers. Zero means "no additional bound" (disabled, already committed, or
// budget spent — callers branch on expired()/headersCommitted() for those).
func (c *ttfbController) remaining() time.Duration {
	if c == nil || c.budget <= 0 || c.committed.Load() {
		return 0
	}
	left := c.budget - c.age()
	if left <= 0 {
		return 0
	}
	return left
}

// headersCommitted reports whether keepalive SSE headers are already on the
// wire — after which a second WriteHeader is impossible and every further
// failure must be delivered in-band.
func (c *ttfbController) headersCommitted() bool {
	return c != nil && c.committed.Load()
}

func (c *ttfbController) stopped() bool {
	select {
	case <-c.stopCh:
		return true
	default:
		return false
	}
}

// stop ends the heartbeat goroutine. Synchronized on mu so it never races a
// heartbeat write that is already in flight.
func (c *ttfbController) stop() {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		close(c.stopCh)
		c.mu.Unlock()
	})
}

// commitKeepalive writes SSE headers plus the first keepalive comment frame,
// then starts the background heartbeat. The first call commits; later calls
// are no-ops. Called only for streaming requests whose first-byte budget is
// exhausted — buffered requests have no in-band keepalive and get an honest
// 504 instead.
func (c *ttfbController) commitKeepalive(w http.ResponseWriter, fallbackFrom string) {
	if c == nil || c.budget <= 0 || !c.committed.CompareAndSwap(false, true) {
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Cache", "MISS")
	if fallbackFrom != "" {
		hdr.Set("X-Fallback-Used", fallbackFrom)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, keepaliveFrame)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	c.sent.Store(true)
	go c.heartbeat(w)
}

// heartbeat emits keepalive frames until stop(). Every write holds mu —
// shared with keepaliveSafeWriter — so pump/relay writes never interleave
// with heartbeat frames.
func (c *ttfbController) heartbeat(w http.ResponseWriter) {
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	flusher, _ := w.(http.Flusher)
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.mu.Lock()
			if c.stopped() {
				c.mu.Unlock()
				return
			}
			_, _ = io.WriteString(w, keepaliveFrame)
			if flusher != nil {
				flusher.Flush()
			}
			c.mu.Unlock()
		}
	}
}

// keepaliveSafeWriter serializes ResponseWriter writes between the heartbeat
// goroutine and the stream pump once keepalive headers are committed. It is
// installed around the handler's ResponseWriter for the WHOLE candidate chain
// (before any upstream attempt), so pumpStream, terminal-error writers, and
// heartbeat frames all share c.mu. With c == nil (TTFB disabled) or the
// keepalive not yet sent it is a transparent passthrough; after the keepalive
// commit, WriteHeader becomes a no-op so late status writes can never corrupt
// the committed SSE stream.
type keepaliveSafeWriter struct {
	http.ResponseWriter
	c *ttfbController
}

func (k *keepaliveSafeWriter) Write(p []byte) (int, error) {
	if k.c == nil {
		return k.ResponseWriter.Write(p)
	}
	k.c.mu.Lock()
	defer k.c.mu.Unlock()
	return k.ResponseWriter.Write(p)
}

func (k *keepaliveSafeWriter) WriteHeader(code int) {
	if k.c != nil && k.c.sent.Load() {
		return // keepalive status already on the wire; a second is impossible
	}
	k.ResponseWriter.WriteHeader(code)
}

func (k *keepaliveSafeWriter) Flush() {
	if f, ok := k.ResponseWriter.(http.Flusher); ok {
		if k.c != nil {
			k.c.mu.Lock()
			defer k.c.mu.Unlock()
		}
		f.Flush()
	}
}

// sseClientDialect reports whether the SSE frames streamed to the client must
// speak anthropic's dialect (/v1/messages) rather than OpenAI's.
func sseClientDialect(endpoint string, isAnthropicUpstream bool) bool {
	return isAnthropicUpstream && endpoint == "messages"
}

// writeSSEUpstreamError terminates an already-committed SSE exchange with a
// protocol-correct in-band error frame: OpenAI clients receive a
// `data: {"error":...}` frame followed by [DONE]; anthropic-dialect clients
// (/v1/messages) receive an `event: error` frame. Used wherever a second
// HTTP status is impossible because keepalive headers already flowed.
func writeSSEUpstreamError(w http.ResponseWriter, anthropicDialect bool, msg string) {
	if len(msg) > 512 {
		msg = msg[:512] + "…"
	}
	flusher, _ := w.(http.Flusher)
	writeStreamTerminator(w, flusher, anthropicDialect, msg)
}

// armTTFBWatchdog cancels attemptCancel if the upstream has not produced
// response headers within the client's remaining first-byte budget. It uses
// time.AfterFunc rather than context.WithTimeout so a SUCCESSFUL header
// arrival disarms the deadline — the response BODY (a long stream) must
// never be bounded by the first-byte budget. Returns the timer to stop as
// soon as Client.Do returns.
func armTTFBWatchdog(c *ttfbController, attemptCancel context.CancelFunc) *time.Timer {
	left := c.remaining()
	if left <= 0 {
		return nil
	}
	return time.AfterFunc(left, attemptCancel)
}
