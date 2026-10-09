package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Regression test for the 524 saga: Logger's statusRecorder wrapped the
// ResponseWriter WITHOUT forwarding http.Flusher.
//
// Go promotes only the methods declared on the embedded interface itself:
// http.ResponseWriter = {Header, Write, WriteHeader}. Flush() lives on the
// separate http.Flusher interface and is therefore NOT promoted by embedding.
// Every w.(http.Flusher) assertion below the Logger silently failed, so the
// TTFB keepalive machinery (internal/proxy/ttfb.go) wrote its SSE frames
// into net/http's ~2KB write buffer and never pushed them to the socket.
// Behind Cloudflare the origin then stayed byteless for 100s -> 524.
//
// This test drives a REAL httptest server whose handler blocks after writing
// one small frame: net/http only auto-flushes when its buffer fills (2KB) or
// the handler returns, so the frame can reach the client while the handler
// is still running ONLY if Flush survives the middleware wrapper.
func TestLoggerPreservesFlusher(t *testing.T) {
	release := make(chan struct{})
	h := Logger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
			t.Errorf("write keepalive frame: %v", err)
		}
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("http.Flusher hidden by Logger's statusRecorder — keepalive frames cannot reach the socket")
		}
		f.Flush()
		<-release // hold the handler open: bytes can only be on the wire if Flush worked
	}))

	srv := httptest.NewServer(h)
	// Unblock the handler BEFORE waiting on the server's active handlers,
	// otherwise shutdown deadlocks (Close waits for the handler, the handler
	// waits for release).
	defer func() {
		close(release)
		srv.Close()
	}()

	// client.Timeout bounds the whole exchange (headers + body read), so a
	// blocked handler with a broken Flush surfaces as a deadline error here.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("no response headers while handler still running — Flush is broken through Logger: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 64)
	n, rerr := resp.Body.Read(buf)
	if n == 0 || rerr != nil {
		t.Fatalf("keepalive frame never reached the client (n=%d, err=%v) — Flush is broken through Logger", n, rerr)
	}
	if got := string(buf[:n]); got != ": keepalive\n\n" {
		t.Fatalf("unexpected frame on the wire: %q", got)
	}
}
