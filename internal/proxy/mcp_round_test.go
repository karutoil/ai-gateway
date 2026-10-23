package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The MCP-round marker must be unreachable from the wire.
func TestMCPRoundMarkerIsNotHeaderControlled(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Gateway-MCP-Round", "1")
	if mcpRound(r) {
		t.Fatal("a client-supplied X-Gateway-MCP-Round header still marks the request as an internal MCP round: retries are re-enabled on billable creates and streaming is forced down the buffered, unmetered path")
	}
	// The context marker is the only way in.
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r2 = r2.WithContext(withMCPRound(context.Background()))
	if !mcpRound(r2) {
		t.Fatal("the context marker must mark a round")
	}
}
