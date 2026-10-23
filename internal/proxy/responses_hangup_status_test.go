package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/provider"
)

// cancelOnWrite is a ResponseWriter that cancels the request context the
// moment a chosen marker reaches the wire — a stand-in for an SSE client that
// hangs up as soon as it has read the terminal event.
type cancelOnWrite struct {
	mu     sync.Mutex
	buf    strings.Builder
	marker string
	cancel context.CancelFunc
	fired  bool
	header http.Header
	status int
}

func (c *cancelOnWrite) Header() http.Header {
	if c.header == nil {
		c.header = http.Header{}
	}
	return c.header
}

func (c *cancelOnWrite) WriteHeader(code int) { c.status = code }

func (c *cancelOnWrite) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.buf.Write(p)
	hit := !c.fired && strings.Contains(c.buf.String(), c.marker)
	if hit {
		c.fired = true
	}
	c.mu.Unlock()
	if hit {
		c.cancel()
	}
	return len(p), nil
}

func (c *cancelOnWrite) Flush() {}

func (c *cancelOnWrite) body() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// TestNativeResponsesClientHangupAfterTerminalIsSuccess covers the Responses
// (native pass-through) twin of the chat-completions case: the terminal
// `response.completed` event has already been relayed when the client hangs up,
// so the row must be a success, not a 499 mid-stream failure.
func TestNativeResponsesClientHangupAfterTerminalIsSuccess(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	h := New(provider.NewStore(database, master), database)

	// Upstream body: a complete Responses turn, then silence (the socket stays
	// open, exactly like a provider that has not closed its connection yet).
	pr, pw := io.Pipe()
	go func() {
		io.WriteString(pw, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"status\":\"in_progress\"}}\n\n")
		io.WriteString(pw, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		io.WriteString(pw, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12}}}\n\n")
		// never closed: the client hangs up first
	}()
	t.Cleanup(func() { pw.CloseWithError(io.ErrClosedPipe) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
		Request:    req,
	}

	w := &cancelOnWrite{marker: "response.completed", cancel: cancel}
	ttfb := newDisabledTTFBController(time.Now())
	out := h.streamNativeResponses(w, req, resp, "gpt-5", "kp", "prov", time.Now(), ttfb)
	if !out.committed {
		t.Fatal("expected the native stream to report a committed response")
	}
	if !w.fired {
		t.Fatal("client never received response.completed")
	}

	var status int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := database.QueryRow(`SELECT status FROM request_logs ORDER BY rowid DESC LIMIT 1`).Scan(&status); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != http.StatusOK {
		t.Fatalf("client hung up after response.completed; request_logs status = %d, want 200", status)
	}
}
