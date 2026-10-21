package mcp

import (
	"context"
	"testing"
	"time"

	"ai-gateway/internal/db"

	"ai-gateway/internal/mcp/mcptest"
)

// TestSSESessionSurvivesConnectTimeout is the regression test for the
// connect-context bug: the legacy SSE transport ties its long-lived GET
// stream to the context passed to Connect, so deriving that context with a
// deadline (the old context.WithTimeout(ctx, callTimeout)) silently killed
// every SSE session callTimeout seconds after it was established. The next
// tools/list then died with EOF and the outage was blamed on a healthy
// upstream. The session context must carry no deadline and no caller
// cancellation; the handshake stays bounded by the watchdog instead.
func TestSSESessionSurvivesConnectTimeout(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 7)
	}
	store := NewStore(database, master)
	// callTimeout far SHORTER than the session's expected life: with the old
	// deadline-derived context the session dies ~200ms after connect.
	manager := NewManager(store, 200*time.Millisecond)
	t.Cleanup(manager.Close)

	up := mcptest.NewSSE()
	// Teardown order matters: the session outlives this test's contexts by
	// design, so the manager (which cancels the session context and ends the
	// event-stream GET) must close before the httptest server, or
	// httptest.Server.Close blocks on the open stream.
	defer func() { manager.Close(); up.Close() }()

	srv, err := store.Create(ServerInput{Name: "sse-longlived", Transport: "sse", URL: up.Server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.Discover(ctx, srv.ID); err != nil {
		t.Fatalf("SSE discovery failed: %v", err)
	}

	// Capture the cached session.
	manager.mu.Lock()
	before := manager.conns[srv.ID]
	manager.mu.Unlock()
	if before == nil || before.session == nil {
		t.Fatal("no cached session after discovery")
	}

	// Wait well past callTimeout. Under the bug, the GET stream's context
	// deadline fired here and the session died in place.
	time.Sleep(800 * time.Millisecond)

	manager.mu.Lock()
	after := manager.conns[srv.ID]
	manager.mu.Unlock()
	if after == nil || after.session == nil {
		t.Fatal("cached session vanished without a failure being recorded")
	}
	if after.session != before.session {
		t.Fatal("session was silently re-established — the cached one died")
	}

	// The session must still be USABLE, not just cached: a torn-down GET
	// stream fails ListTools (EOF / connection closed).
	res, err := after.session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("session unusable %s after connect (callTimeout=%s): %v",
			800*time.Millisecond, 200*time.Millisecond, err)
	}
	if len(res.Tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(res.Tools))
	}
}
