package proxy

// Per-key analytics attribution: a request proxied with a real sk-gw-* key
// must stamp request_logs.key_id with that key's gateway_keys.id (the column
// the /api/keys/{id}/analytics endpoint aggregates on).

import (
	"net/http"
	"strings"
	"testing"
)

func TestRequestLogAttributesKeyID(t *testing.T) {
	if testing.Short() {
		t.Skip("uses the full throughput harness; skipped under -short like the load tests")
	}
	th := newThroughputHarness(t)
	defer th.close()

	req, err := http.NewRequest(http.MethodPost, th.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+th.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// The proxy must have written exactly one log row, stamped with the
	// authenticating key's id.
	var keyID, prefix string
	if err := th.database.QueryRow(`SELECT key_prefix, key_id FROM request_logs`).Scan(&prefix, &keyID); err != nil {
		t.Fatalf("read request_logs: %v", err)
	}
	if keyID == "" {
		t.Fatalf("request_logs.key_id is empty — per-key analytics would see no traffic")
	}
	if keyID != th.keyID {
		t.Fatalf("key_id = %s, want %s (prefix %s)", keyID, th.keyID, prefix)
	}
}

func TestGatewayKeyIDCachesMisses(t *testing.T) {
	h := &Handler{DB: nil}
	// No DB at all: must not panic and must return the cached/empty miss.
	if got := h.gatewayKeyID("deadbeef"); got != "" {
		t.Fatalf("gatewayKeyID with nil DB = %q, want empty", got)
	}
	if got := h.gatewayKeyID(""); got != "" {
		t.Fatalf("gatewayKeyID(\"\") = %q, want empty", got)
	}
}
