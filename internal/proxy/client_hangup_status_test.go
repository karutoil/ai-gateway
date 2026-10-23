package proxy

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

// setupHangupServer builds a gateway whose upstream COMPLETES the assistant
// turn (finish_reason + [DONE]) and then keeps the socket open for a moment,
// like a real provider that does not close the TCP connection the instant it
// writes [DONE]. The returned release channel unblocks that upstream.
func setupHangupServer(t *testing.T) (*httptest.Server, *Handler, string, chan struct{}) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"done"}}]}` + "\n\n"))
		w.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107}}` + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Real providers keep the connection alive for a beat after the
		// terminator; that is exactly the window in which a client that has
		// what it needs hangs up.
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}))

	if _, err := ps.Create("openai", models.ProviderOpenAI, upstream.URL+"/v1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("test-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)

	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	srv := httptest.NewServer(r)
	t.Cleanup(func() {
		close(release)
		upstream.Close()
		srv.Close()
	})
	return srv, h, k.Key, release
}

// lastLogStatus polls request_logs until a row exists (the log write happens
// after the pump returns) and returns its status.
func lastLogStatus(t *testing.T, h *Handler) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var status int
		err := h.DB.QueryRow(`SELECT status FROM request_logs ORDER BY rowid DESC LIMIT 1`).Scan(&status)
		if err == nil {
			return status
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no request_logs row written")
	return 0
}

// TestClientHangupAfterTerminatorIsSuccess pins the disposition of the single
// biggest source of "499" rows in production: SSE clients (every AI SDK /
// harness) close the response body as soon as they have read the terminal
// frame, instead of waiting for the upstream's EOF. The gateway used to see
// r.Context().Done() before it saw EOF and logged 499 + midStreamFailure for a
// response that had been fully delivered and billed.
func TestClientHangupAfterTerminatorIsSuccess(t *testing.T) {
	srv, h, key, _ := setupHangupServer(t)

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 got %d", resp.StatusCode)
	}
	// Read exactly up to the terminator, then hang up — no EOF read.
	br := bufio.NewReader(resp.Body)
	sawDone := false
	for {
		line, err := br.ReadString('\n')
		if strings.TrimSpace(line) == "data: [DONE]" {
			sawDone = true
			break
		}
		if err != nil {
			break
		}
	}
	resp.Body.Close()
	if !sawDone {
		t.Fatal("client never saw the [DONE] terminator")
	}

	if status := lastLogStatus(t, h); status != http.StatusOK {
		t.Fatalf("client hung up after a COMPLETE response; request_logs status = %d, want 200 (499 is reserved for a client that vanished before the terminal frame)", status)
	}
}

// TestClientHangupBeforeFirstByteIsStill499 guards the other side of the rule:
// a client that disappears before any terminal frame must still be reported as
// 499 (client closed request), not laundered into a success.
func TestClientHangupBeforeFirstByteIsStill499(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Headers only, then silence: the client gives up mid-wait.
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}))
	if _, err := ps.Create("openai", models.ProviderOpenAI, upstream.URL+"/v1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("test-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	srv := httptest.NewServer(r)
	t.Cleanup(func() {
		close(release)
		upstream.Close()
		srv.Close()
	})

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+k.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() // no terminal frame was ever delivered

	if status := lastLogStatus(t, h); status == http.StatusOK {
		t.Fatalf("client vanished before any terminal frame; status = %d, must not be reported as a success", status)
	}
}
