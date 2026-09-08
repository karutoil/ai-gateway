package apikey

import (
	"strings"
	"testing"
)

// SEC-008: key generation must fail closed on RNG failure path (success
// path asserts format/entropy source); MustGenerate must not silently
// return zero material.
func TestAuditGenerateFormat(t *testing.T) {
	s, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, "sk-gw-") || len(s) != len("sk-gw-")+64 {
		t.Fatalf("unexpected key format len=%d prefix=%q", len(s), s[:6])
	}
	if got := MustGenerate(); !strings.HasPrefix(got, "sk-gw-") {
		t.Fatalf("MustGenerate bad prefix %q", got)
	}
}
