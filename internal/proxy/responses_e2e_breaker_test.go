package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/resilience"
)

// E2E: a /v1/responses request against an OpenAI-compat upstream must return
// a NON-EMPTY, well-formed body to the client. (Live traffic showed 200s with
// empty bodies — the client got nothing.)
func TestResponsesNonStreamReturnsBody(t *testing.T) {
	upCalled := 0
	up := func(w http.ResponseWriter, r *http.Request) {
		upCalled++
		if strings.HasSuffix(r.URL.Path, "/responses") {
			// ckff-style upstream: no native /responses support.
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"message":"not found"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	}
	srv, key := newHygieneEnv(t, up, func(hh *Handler) http.HandlerFunc { return hh.Responses }, "/v1/responses")
	resp := postHygiene(t, srv.URL+"/v1/responses", key, `{"model":"gpt-4o-mini","input":"hi"}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		t.Fatal("client received an EMPTY 200 body on /v1/responses")
	}
	var probe map[string]interface{}
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, body)
	}
	if probe["error"] != nil {
		t.Fatalf("error relayed: %s", body)
	}
}

// The responses path must not bypass the breaker: with the circuit open, the
// native probe must be skipped (no upstream call at all).
func TestResponsesNativeProbeRespectsBreaker(t *testing.T) {
	nativeHits := 0
	up := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/responses") {
			nativeHits++
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{}`)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}
	srv, key := newHygieneEnv(t, up, func(hh *Handler) http.HandlerFunc {
		// Wire a real breaker — New() leaves it nil (noop) for tests. The
		// mount callback runs before the server accepts traffic.
		hh.Breaker = resilience.NewMemoryCircuitBreakerFull(5, time.Minute, 30*time.Second, 2)
		return hh.Responses
	}, "/v1/responses")

	// Trip the breaker: threshold is 5 failures by default.
	for i := 0; i < 6; i++ {
		resp := postHygiene(t, srv.URL+"/v1/responses", key, `{"model":"gpt-4o-mini","input":"q`+string(rune('a'+i))+`"}`)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// Force-open state must deny without any upstream call. Confirm via the
	// handler: after 6 hard failures the next request should short-circuit.
	hitsBefore := nativeHits
	resp := postHygiene(t, srv.URL+"/v1/responses", key, `{"model":"gpt-4o-mini","input":"blocked"}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if nativeHits > hitsBefore {
		t.Fatalf("native probe hit upstream despite open circuit (hits %d -> %d)", hitsBefore, nativeHits)
	}
}

// An upstream that answers 200 with an EMPTY body (new-api channel-death
// signature) must not be relayed as a silent success — nor cached. The
// gateway treats it as a transport failure: retry, then 502.
func TestEmptyUpstreamBodyIsNotASuccess(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/responses") {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{}`)
			return
		}
		// The lie: HTTP 200, zero bytes.
		w.WriteHeader(http.StatusOK)
	}
	srv, key := newHygieneEnv(t, up, func(hh *Handler) http.HandlerFunc { return hh.Responses }, "/v1/responses")
	resp := postHygiene(t, srv.URL+"/v1/responses", key, `{"model":"gpt-4o-mini","input":"hi"}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 200 {
		t.Fatalf("empty upstream 200 relayed to client as success (status 200, body %q)", body)
	}
	if resp.StatusCode < 500 {
		t.Fatalf("status = %d, want a 5xx signaling the upstream failure", resp.StatusCode)
	}

	// And it must not have been cached: a second identical request must also
	// fail closed, not serve a cached empty 200.
	resp2 := postHygiene(t, srv.URL+"/v1/responses", key, `{"model":"gpt-4o-mini","input":"hi"}`)
	defer resp2.Body.Close()
	io.Copy(io.Discard, resp2.Body)
	if resp2.StatusCode == 200 {
		t.Fatal("empty 200 was cached and replayed as success")
	}
}
