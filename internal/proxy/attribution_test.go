package proxy

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAttributionFromBodyUserField(t *testing.T) {
	// The OpenAI `user` field is the zero-plumbing route: a client that sends
	// nothing but a normal OpenAI request is still attributed per end user.
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	a := attributionFromRequest(r, []byte(`{"model":"m","user":"user-123"}`))
	if a.EndUser != "user-123" {
		t.Fatalf("end_user = %q, want user-123", a.EndUser)
	}
	if a.App != "" {
		t.Fatalf("app = %q, want empty", a.App)
	}
}

func TestAttributionFromAnthropicMetadata(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	a := attributionFromRequest(r, []byte(`{"model":"m","metadata":{"user_id":"anth-9"}}`))
	if a.EndUser != "anth-9" {
		t.Fatalf("end_user = %q, want anth-9", a.EndUser)
	}
}

func TestAttributionPrecedence(t *testing.T) {
	// Explicit header beats the body field.
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set(HeaderEndUser, "header-user")
	a := attributionFromRequest(r, []byte(`{"user":"body-user"}`))
	if a.EndUser != "header-user" {
		t.Fatalf("end_user = %q, want header-user (header wins)", a.EndUser)
	}

	// App: X-App-Id beats X-Title beats the referer origin.
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r2.Header.Set(HeaderReferer, "https://app.example.com/chat/page")
	if got := attributionFromRequest(r2, nil).App; got != "https://app.example.com" {
		t.Fatalf("app from referer = %q, want origin only", got)
	}
	r2.Header.Set(HeaderTitle, "My App")
	if got := attributionFromRequest(r2, nil).App; got != "My App" {
		t.Fatalf("app with title = %q, want My App", got)
	}
	r2.Header.Set(HeaderAppID, "app-42")
	if got := attributionFromRequest(r2, nil).App; got != "app-42" {
		t.Fatalf("app with explicit id = %q, want app-42", got)
	}

	// OpenRouter's header spelling works unchanged.
	r3 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r3.Header.Set(HeaderTitleAlt, "OR App")
	if got := attributionFromRequest(r3, nil).App; got != "OR App" {
		t.Fatalf("X-OpenRouter-Title not honoured: %q", got)
	}
}

func TestAttributionSanitization(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set(HeaderEndUser, "bad\x00user\x1bname\n")
	if got := attributionFromRequest(r, nil).EndUser; got != "badusername" {
		t.Fatalf("control characters not stripped: %q", got)
	}

	// Length is capped: these values are attacker-influenced and indexed.
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r2.Header.Set(HeaderEndUser, strings.Repeat("x", 5000))
	got := attributionFromRequest(r2, nil).EndUser
	if len(got) != maxAttributionLen {
		t.Fatalf("end_user length = %d, want cap %d", len(got), maxAttributionLen)
	}
}

func TestAttributionContextRoundTrip(t *testing.T) {
	if a := AttributionFrom(context.Background()); a.Any() {
		t.Fatalf("bare context produced attribution: %+v", a)
	}
	ctx := WithAttribution(context.Background(), Attribution{EndUser: "u", App: "a"})
	a := AttributionFrom(ctx)
	if a.EndUser != "u" || a.App != "a" {
		t.Fatalf("round trip = %+v", a)
	}
	// An empty attribution is not stored, so Any() stays false downstream.
	if a := AttributionFrom(WithAttribution(context.Background(), Attribution{})); a.Any() {
		t.Fatal("empty attribution was stored")
	}
}

// TestAttributionReachesLogRow drives the full path: context -> logging funnel
// -> request_logs columns. This is the wiring that the mechanical ctx
// plumbing could most easily have broken.
func TestAttributionReachesLogRow(t *testing.T) {
	h, _ := newCostHandler(t)
	ctx := WithAttribution(context.Background(), Attribution{EndUser: "user-abc", App: "app-xyz"})
	h.logRequestExtendedBodies(ctx, "", "prov-1", "m", "chat.completions", 200, 10, 5, 3, 0.01, false, nil, nil, &logMeta{})

	var endUser, app string
	if err := h.DB.QueryRow(`SELECT COALESCE(end_user,''), COALESCE(app,'') FROM request_logs ORDER BY created_at DESC LIMIT 1`).Scan(&endUser, &app); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if endUser != "user-abc" || app != "app-xyz" {
		t.Fatalf("persisted attribution = %q/%q, want user-abc/app-xyz", endUser, app)
	}
}

// TestAttributionAbsentStaysNull keeps "unattributed" distinguishable from
// "attributed to an empty string".
func TestAttributionAbsentStaysNull(t *testing.T) {
	h, _ := newCostHandler(t)
	h.logRequestExtendedBodies(context.Background(), "", "prov-1", "m", "chat.completions", 200, 10, 5, 3, 0.01, false, nil, nil, &logMeta{})

	var endUser, app *string
	if err := h.DB.QueryRow(`SELECT end_user, app FROM request_logs ORDER BY created_at DESC LIMIT 1`).Scan(&endUser, &app); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if endUser != nil || app != nil {
		t.Fatalf("unattributed row stored non-NULL: %v/%v", endUser, app)
	}
}
