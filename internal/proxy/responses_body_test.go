package proxy

import (
	"strings"
	"testing"
)

func TestErrSnippetStripsControlCharsAndCaps(t *testing.T) {
	in := []byte("line1\nline2\trx\x00tail")
	got := errSnippet(in)
	if strings.ContainsAny(got, "\n\x00") {
		t.Fatalf("control chars not stripped: %q", got)
	}
	if !strings.Contains(got, "line2\trx") {
		t.Fatalf("content lost: %q", got)
	}
	long := strings.Repeat("a", 500)
	if len(errSnippet([]byte(long))) != 200 {
		t.Fatalf("snippet not capped at 200 bytes")
	}
}

func TestDeleteResponsesKey(t *testing.T) {
	body := []byte(`{"model":"m","stream":true,"stream_tool_calls":true,"input":"hi"}`)
	out := deleteResponsesKey(body, "stream_tool_calls")
	if out == nil {
		t.Fatal("expected key to be removed, got nil")
	}
	if strings.Contains(string(out), "stream_tool_calls") {
		t.Fatalf("key still present: %s", out)
	}
	if !strings.Contains(string(out), `"stream":true`) {
		t.Fatalf("other keys lost: %s", out)
	}
	// Absent key: nil (no re-marshal).
	if deleteResponsesKey(body, "not_there") != nil {
		t.Fatal("absent key must return nil")
	}
	// Invalid JSON: nil.
	if deleteResponsesKey([]byte("not-json"), "x") != nil {
		t.Fatal("invalid json must return nil")
	}
}
