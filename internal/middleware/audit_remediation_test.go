package middleware

import (
	"net/http/httptest"
	"testing"
)

// ACCT-014: unauthenticated rate-limit buckets must key on host-only IP,
// not RemoteAddr with ephemeral port.
func TestAuditRemoteHostStripsPort(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	if got := remoteHost(r); got != "10.0.0.5" {
		t.Fatalf("remoteHost = %q, want 10.0.0.5", got)
	}
	r.RemoteAddr = "10.0.0.5"
	if got := remoteHost(r); got != "10.0.0.5" {
		t.Fatalf("remoteHost bare = %q", got)
	}
}
