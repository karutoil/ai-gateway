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
	"ai-gateway/internal/lb"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

func TestRoutingRulesAPI(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	ps := provider.NewStore(database, master)
	pa, err := ps.Create("prov-a", "openai", "https://a.example.com/v1", "sk-a")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := ps.Create("prov-b", "openai", "https://b.example.com/v1", "sk-b")
	if err != nil {
		t.Fatal(err)
	}

	store := lb.NewStore(database)
	rh := NewRoutingHandler(store)
	cfg := &config.Config{AdminPassword: "unused", JWTSecret: []byte("test-jwt-secret-which-is-long-enough")}
	h := &AdminHandler{Config: cfg, DB: database}

	r := chi.NewRouter()
	r.Post("/api/auth/login", h.Login)
	r.Group(func(r chi.Router) {
		r.Use(auth.AdminMiddlewareWithRevocation(cfg.JWTSecret, nil))
		rh.Routes(r)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	// login
	lb_, _ := json.Marshal(map[string]string{"username": "admin", "password": ""})
	_ = lb_
	// bootstrap path: no dashboard users exist, ADMIN_PASSWORD check
	resp, err := http.Post(srv.URL+"/api/auth/login", "application/json", bytes.NewReader([]byte(`{"password":"unused"}`)))
	if err != nil {
		t.Fatal(err)
	}
	var sess struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&sess)
	resp.Body.Close()
	if sess.Token == "" {
		t.Fatal("no token")
	}
	authz := func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+sess.Token) }

	// PUT rule with both providers
	body, _ := json.Marshal(map[string]interface{}{"providers": []string{pa.ID, pb.ID}})
	req, _ := http.NewRequest("PUT", srv.URL+"/lb/rules/gpt-4o-mini", bytes.NewReader(body))
	authz(req)
	putResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if putResp.StatusCode != 201 {
		t.Fatalf("PUT /lb/rules → %d", putResp.StatusCode)
	}
	putResp.Body.Close()

	// GET lists it
	getReq, _ := http.NewRequest("GET", srv.URL+"/lb/rules", nil)
	authz(getReq)
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	var rules []struct {
		Model     string `json:"model"`
		Providers []struct {
			ProviderID string `json:"provider_id"`
		} `json:"providers"`
	}
	json.NewDecoder(getResp.Body).Decode(&rules)
	getResp.Body.Close()
	if len(rules) != 1 || rules[0].Model != "gpt-4o-mini" || len(rules[0].Providers) != 2 ||
		rules[0].Providers[0].ProviderID != pa.ID || rules[0].Providers[1].ProviderID != pb.ID {
		t.Fatalf("unexpected rules %+v", rules)
	}

	// Unknown provider rejected
	badBody, _ := json.Marshal(map[string]interface{}{"providers": []string{"does-not-exist"}})
	badReq, _ := http.NewRequest("PUT", srv.URL+"/lb/rules/gpt-4o", bytes.NewReader(badBody))
	authz(badReq)
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	ioCopyClose(badResp)
	if badResp.StatusCode != 400 {
		t.Fatalf("unknown provider should 400, got %d", badResp.StatusCode)
	}

	// DELETE removes it
	delReq, _ := http.NewRequest("DELETE", srv.URL+"/lb/rules/gpt-4o-mini", nil)
	authz(delReq)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != 204 {
		t.Fatalf("DELETE → %d", delResp.StatusCode)
	}
	getReq2, _ := http.NewRequest("GET", srv.URL+"/lb/rules", nil)
	authz(getReq2)
	g2, _ := http.DefaultClient.Do(getReq2)
	var after []map[string]interface{}
	json.NewDecoder(g2.Body).Decode(&after)
	g2.Body.Close()
	if len(after) != 0 {
		t.Fatalf("rule not deleted: %+v", after)
	}
}

func ioCopyClose(resp *http.Response) {
	buf := make([]byte, 1024)
	for {
		if n, _ := resp.Body.Read(buf); n == 0 {
			break
		}
	}
	resp.Body.Close()
}
