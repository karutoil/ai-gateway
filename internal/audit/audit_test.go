package audit

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
)

// SECURITY: LogFromRequest must attribute actors from VERIFIED request
// context only. The previous version trusted the client-settable X-Actor
// header and defaulted to "admin" — any caller could forge the audit trail.
func TestLogFromRequestIgnoresClientActorHeader(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rec := NewDBRecorder(database)

	// Unauthenticated caller tries to forge attribution via X-Actor.
	req := httptest.NewRequest(http.MethodPost, "/api/keys", nil)
	req.Header.Set("X-Actor", "evil-admin")
	LogFromRequest(rec, req, "key-1", "POST /api/keys")

	var actor string
	if err := database.QueryRow(`SELECT actor FROM audit_logs ORDER BY rowid DESC LIMIT 1`).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if actor != "anonymous" {
		t.Fatalf("unauthenticated write must be attributed to anonymous, got %q (X-Actor forged?)", actor)
	}

	// Authenticated caller: actor is the verified JWT subject, not the header.
	req2 := httptest.NewRequest(http.MethodPost, "/api/keys", nil)
	req2.Header.Set("X-Actor", "someone-else")
	ctx := auth.WithSubject(auth.WithRole(req2.Context(), "member"), "alice")
	req2 = req2.WithContext(ctx)
	LogFromRequest(rec, req2, "key-2", "POST /api/keys")

	if err := database.QueryRow(`SELECT actor FROM audit_logs ORDER BY rowid DESC LIMIT 1`).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if actor != "alice" {
		t.Fatalf("authenticated write must be attributed to the verified subject, got %q", actor)
	}
}
