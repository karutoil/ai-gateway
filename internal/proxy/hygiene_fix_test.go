package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

// Hygiene regressions from the adversarial verification sweep (F1/F2).

func newHygieneEnv(t *testing.T, up http.HandlerFunc, mountFor func(hh *Handler) http.HandlerFunc, path string) (*httptest.Server, string) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	us := httptest.NewServer(up)
	t.Cleanup(us.Close)
	if _, err := ps.Create("hyg-up", models.ProviderOpenAI, us.URL+"/v1", "sk-h"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("hyg-key")
	if err != nil {
		t.Fatal(err)
	}
	hh := New(ps, database)
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post(path, mountFor(hh))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, k.Key
}

func junkHeaders(w http.ResponseWriter) {
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Keep-Alive", "timeout=42")
	w.Header().Set("Te", "trailers")
	w.Header().Set("Upgrade", "h2c")
	w.Header().Set("Trailer", "X-Debug")
}

func postHygiene(t *testing.T, url, key, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func checkNoHopByHop(t *testing.T, resp *http.Response, ctx string) {
	t.Helper()
	for _, hname := range []string{"Connection", "Keep-Alive", "Te", "Upgrade", "Trailer"} {
		if v := resp.Header.Get(hname); v != "" {
			t.Fatalf("[%s] hop-by-hop %s leaked: %q", ctx, hname, v)
		}
	}
}

const chatCompletionOK = `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

// F1a: converted non-stream responses must strip upstream hop-by-hop headers.
func TestHopByHopConvertedResponses(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		junkHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, chatCompletionOK)
	}
	srv, key := newHygieneEnv(t, up, func(hh *Handler) http.HandlerFunc { return hh.Responses }, "/v1/responses")
	resp := postHygiene(t, srv.URL+"/v1/responses", key, `{"model":"m","input":"q"}`)
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	checkNoHopByHop(t, resp, "converted-responses")
}

// F1b: streamed relay commit path must also strip hop-by-hop headers.
func TestHopByHopStreamedRelay(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		junkHeaders(w)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fl.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
	}
	srv, key := newHygieneEnv(t, up, func(hh *Handler) http.HandlerFunc { return hh.ChatCompletions }, "/v1/chat/completions")
	resp := postHygiene(t, srv.URL+"/v1/chat/completions", key,
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	checkNoHopByHop(t, resp, "streamed-relay")
}

// F2: a Location-less 3xx from the native /responses probe (JSON body, no
// transport error) must NOT be replayed to the client as pseudo-success; the
// request falls through to translation instead.
func TestRefusedRedirectWithoutLocationFallsThrough(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/responses") {
			w.WriteHeader(http.StatusFound) // 302 WITHOUT Location
			io.WriteString(w, `{"stealth":"noloc-marker"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, chatCompletionOK)
	}
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	us := httptest.NewServer(http.HandlerFunc(up))
	defer us.Close()
	if _, err := ps.Create("f2-up", models.ProviderOpenAI, us.URL+"/v1", "sk-f2"); err != nil {
		t.Fatal(err)
	}
	k, _ := ks.Create("f2-key")
	hh := New(ps, database)
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/responses", hh.Responses)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp := postHygiene(t, srv.URL+"/v1/responses", k.Key, `{"model":"m","input":"q"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusFound || strings.Contains(string(b), "noloc-marker") {
		t.Fatalf("refused redirect swallowed: status=%d body=%s", resp.StatusCode, string(b))
	}
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"object":"response"`) || !strings.Contains(string(b), `"output_text":"hi"`) {
		t.Fatalf("expected translated Responses-shape fallback success, got %d %s", resp.StatusCode, string(b))
	}
}
