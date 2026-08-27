package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
)

func TestGatewayAuthRejectsMissing(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ks := apikey.NewStore(database)
	h := GatewayAuth(ks)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("expected 401 got %d", rec.Code)
	}
}

func TestGatewayAuthJWTSession(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ks := apikey.NewStore(database)
	secret := []byte("test-jwt-secret-which-is-long-enough")
	token, err := auth.MakeToken(secret, "alice")
	if err != nil {
		t.Fatal(err)
	}
	h := GatewayAuthWithJWT(ks, secret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := GatewayKeyFromContext(r.Context())
		if !ok || key == nil {
			t.Fatal("expected session key in context")
		}
		if key.Prefix != "sessalice" {
			t.Fatalf("prefix %s", key.Prefix)
		}
		if r.Header.Get("X-Gateway-Key-Prefix") != "sessalice" {
			t.Fatalf("header prefix %s", r.Header.Get("X-Gateway-Key-Prefix"))
		}
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		b, _ := io.ReadAll(rec.Body)
		t.Fatalf("expected 200 got %d %s", rec.Code, string(b))
	}
}

func TestGatewayAuthJWTRejectedWithoutSecret(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ks := apikey.NewStore(database)
	secret := []byte("test-jwt-secret-which-is-long-enough")
	token, err := auth.MakeToken(secret, "alice")
	if err != nil {
		t.Fatal(err)
	}
	h := GatewayAuth(ks)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("expected 401 without jwt secret, got %d", rec.Code)
	}
}
