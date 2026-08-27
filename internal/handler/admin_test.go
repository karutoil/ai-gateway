package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/user"

	"github.com/go-chi/chi/v5"
)

func TestLoginAndReadonlyCannotCreateProvider(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	us := user.NewStore(database)
	if _, err := us.Create("admin", "adminpass", "admin", "Admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := us.Create("viewer", "viewpass", "readonly", "Viewer"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AdminPassword: "unused", JWTSecret: []byte("test-jwt-secret-which-is-long-enough")}
	master := make([]byte, 32)
	h := &AdminHandler{
		ProviderStore: provider.NewStore(database, master),
		Config:        cfg,
		DB:            database,
		UserStore:     us,
	}
	r := chi.NewRouter()
	r.Post("/api/auth/login", h.Login)
	r.Group(func(r chi.Router) {
		r.Use(auth.AdminMiddleware(cfg.JWTSecret))
		r.Get("/api/providers", h.ListProviders)
		r.With(middleware.RequireRole("admin", "member")).Post("/api/providers", h.CreateProvider)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	login := func(user, pass string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"username": user, "password": pass})
		resp, err := http.Post(srv.URL+"/api/auth/login", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("login %s %d", user, resp.StatusCode)
		}
		var out map[string]string
		json.NewDecoder(resp.Body).Decode(&out)
		if out["token"] == "" {
			t.Fatal("no token")
		}
		return out["token"]
	}
	adminTok := login("admin", "adminpass")
	viewTok := login("viewer", "viewpass")

	body := []byte(`{"name":"openai","type":"openai","api_key":"sk-test","base_url":"https://api.openai.com/v1"}`)
	req, _ := http.NewRequest("POST", srv.URL+"/api/providers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+viewTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("readonly POST expected 403 got %d", resp.StatusCode)
	}

	req, _ = http.NewRequest("POST", srv.URL+"/api/providers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("admin POST expected 201 got %d", resp.StatusCode)
	}
}

func TestLoginRejectsBadPassword(t *testing.T) {
	cfg := &config.Config{AdminPassword: "secret", JWTSecret: []byte("test-jwt-secret-which-is-long-enough")}
	h := &AdminHandler{Config: cfg}
	r := chi.NewRouter()
	r.Post("/api/auth/login", h.Login)
	srv := httptest.NewServer(r)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/auth/login", "application/json", bytes.NewReader([]byte(`{"password":"nope"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["error"]["type"] != "authentication_error" {
		t.Fatalf("%v", body)
	}
}
