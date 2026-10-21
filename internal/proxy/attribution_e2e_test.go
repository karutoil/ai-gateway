package proxy

// Attribution over the REAL handler chain.
//
// The unit tests in attribution_test.go cover extraction in isolation; this
// file drives POST /v1/chat/completions through chi + GatewayAuth + the proxy
// handler into request_logs, because the bug being fixed was not in the
// extraction helper — it was that nothing a harness actually sends ever
// reached it. A captured production request carried only
// "User-Agent: pi (linux …; x64)" and no attribution header, so every row
// landed with end_user and app NULL and the Attribution page was empty.

import (
	"net/http"
	"testing"

	"ai-gateway/internal/models"
)

const piUserAgent = "pi (linux 6.12.111+deb13-amd64; x64)"

// protoChatUpstream is the smallest [OI]-shaped upstream that satisfies the
// proxy: one non-streaming completion with usage.
func protoChatUpstream(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m1",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
}

// TestAttributionClientEndToEnd is the regression gate for "attribution always
// shows undefined": a harness request with nothing but a User-Agent must still
// be attributable, and must not be recorded as an app.
func TestAttributionClientEndToEnd(t *testing.T) {
	env := protoNewEnv(t, protoChatUpstream, models.ProviderOpenAI, "/v1", "chat")
	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`

	code, _, raw := env.PostRaw("/v1/chat/completions", body, map[string]string{
		"User-Agent": piUserAgent,
	})
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, raw)
	}

	var client, endUser, app *string
	if err := env.DB.QueryRow(
		`SELECT client, end_user, app FROM request_logs ORDER BY rowid DESC LIMIT 1`).
		Scan(&client, &endUser, &app); err != nil {
		t.Fatalf("read back request_logs: %v", err)
	}
	if client == nil || *client != "pi" {
		t.Fatalf("client = %v, want pi (the User-Agent product token)", client)
	}
	// The two opt-in dimensions stay honest: nothing declared them.
	if endUser != nil {
		t.Fatalf("end_user = %v, want NULL", *endUser)
	}
	if app != nil {
		t.Fatalf("app = %v, want NULL — a client must not be promoted to an app", *app)
	}
}

// TestAttributionDeclaredHeadersEndToEnd keeps the explicit path working and
// checks that client is recorded alongside it rather than replacing it.
func TestAttributionDeclaredHeadersEndToEnd(t *testing.T) {
	env := protoNewEnv(t, protoChatUpstream, models.ProviderOpenAI, "/v1", "chat")
	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}],"user":"alice"}`

	code, _, raw := env.PostRaw("/v1/chat/completions", body, map[string]string{
		"User-Agent": piUserAgent,
		"X-App-Id":   "app-42",
		"X-End-User": "override-user",
	})
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, raw)
	}

	var client, endUser, app string
	if err := env.DB.QueryRow(
		`SELECT COALESCE(client,''), COALESCE(end_user,''), COALESCE(app,'') FROM request_logs ORDER BY rowid DESC LIMIT 1`).
		Scan(&client, &endUser, &app); err != nil {
		t.Fatalf("read back request_logs: %v", err)
	}
	if client != "pi" {
		t.Fatalf("client = %q, want pi", client)
	}
	if app != "app-42" {
		t.Fatalf("app = %q, want app-42", app)
	}
	// X-End-User beats the body field.
	if endUser != "override-user" {
		t.Fatalf("end_user = %q, want override-user", endUser)
	}
}

// TestAttributionSentinelEndToEnd pins the literal "undefined" symptom at the
// HTTP boundary: a client that interpolates an unset variable must land as
// NULL, not as an identity called "undefined".
func TestAttributionSentinelEndToEnd(t *testing.T) {
	env := protoNewEnv(t, protoChatUpstream, models.ProviderOpenAI, "/v1", "chat")
	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`

	code, _, raw := env.PostRaw("/v1/chat/completions", body, map[string]string{
		"User-Agent": piUserAgent,
		"X-App-Id":   "undefined",
		"X-End-User": "null",
	})
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, raw)
	}

	var endUser, app *string
	if err := env.DB.QueryRow(
		`SELECT end_user, app FROM request_logs ORDER BY rowid DESC LIMIT 1`).
		Scan(&endUser, &app); err != nil {
		t.Fatalf("read back request_logs: %v", err)
	}
	if endUser != nil || app != nil {
		t.Fatalf("sentinel stored as identity: end_user=%v app=%v", endUser, app)
	}
}
