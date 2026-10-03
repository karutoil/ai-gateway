package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// captureLogs redirects the global logger into buf for the duration of the
// test and restores it afterwards.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = prev })
	return &buf
}

// TestLoggerReportsCacheHit covers the cache section on the accessor log
// line: a response served from the gateway cache must be visible as
// cache=hit cache_hit=true without correlating against request_logs.
func TestLoggerReportsCacheHit(t *testing.T) {
	buf := captureLogs(t)
	h := Logger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(200)
		w.Write([]byte("{}"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	out := buf.String()
	// Lowercase, to match the cache_status vocabulary stored in request_logs.
	if !bytes.Contains([]byte(out), []byte(`"cache":"hit"`)) {
		t.Fatalf("expected cache=hit in log line, got: %s", out)
	}
	if !bytes.Contains([]byte(out), []byte(`"cache_hit":true`)) {
		t.Fatalf("expected cache_hit=true, got: %s", out)
	}
}

// TestLoggerReportsCacheMissAndBypass: a real upstream call after a cache
// miss logs cache=miss; a non-eligible request logs cache=bypass. Both must
// be distinguishable so an all-bypass run is explainable from logs alone.
func TestLoggerReportsCacheMissAndBypass(t *testing.T) {
	for _, tc := range []struct {
		header  string
		wantSub string
	}{
		{"MISS", `"cache":"miss"`},
		{"BYPASS", `"cache":"bypass"`},
	} {
		buf := captureLogs(t)
		h := Logger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Cache", tc.header)
			w.WriteHeader(200)
			w.Write([]byte("{}"))
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
		out := buf.String()
		if !bytes.Contains([]byte(out), []byte(tc.wantSub)) {
			t.Fatalf("X-Cache=%s: expected %s in log line, got: %s", tc.header, tc.wantSub, out)
		}
		if !bytes.Contains([]byte(out), []byte(`"cache_hit":false`)) {
			t.Fatalf("X-Cache=%s: expected cache_hit=false, got: %s", tc.header, out)
		}
	}
}

// TestLoggerCacheAbsentIsNone: handlers outside the cache path (dashboard,
// health, static assets) set no X-Cache header. The field must still be
// present and explicitly "none" rather than empty or missing.
func TestLoggerCacheAbsentIsNone(t *testing.T) {
	buf := captureLogs(t)
	h := Logger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	out := buf.String()
	if !bytes.Contains([]byte(out), []byte(`"cache":"none"`)) {
		t.Fatalf("expected cache=none for uncached handler, got: %s", out)
	}
	if !bytes.Contains([]byte(out), []byte(`"cache_hit":false`)) {
		t.Fatalf("expected cache_hit=false, got: %s", out)
	}
}

// TestLoggerCacheSetWithoutWriteHeader: handlers that write a body without an
// explicit WriteHeader (the common proxy path) must still have their
// X-Cache disposition captured — the header map is already final when Write
// runs, so the recorder must sample it there too.
func TestLoggerCacheSetWithoutWriteHeader(t *testing.T) {
	buf := captureLogs(t)
	h := Logger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cache", "HIT")
		w.Write([]byte("{}"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if !bytes.Contains([]byte(buf.String()), []byte(`"cache_hit":true`)) {
		t.Fatalf("expected cache_hit=true when WriteHeader is skipped, got: %s", buf.String())
	}
}
