package proxy

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/catalog"
	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

// decisionRowServer wires a gateway with one generic (openai-typed) provider
// that advertises decision models in its own listing, the way a Jev reseller
// does. The upstream records the path of every request so the test can prove
// which decision endpoint the gateway addressed.
func decisionRowServer(t *testing.T) (*httptest.Server, *sql.DB, string, func() []string) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	var mu sync.Mutex
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "decisions"), strings.Contains(r.URL.Path, "systemone"):
			_, _ = w.Write([]byte(`{"model":"jev-1.13","answers":{"q":{"type":"noul","noul":0.92}},"usage":{"input_tokens":272,"output_tokens":20}}`))
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		}
	}))
	t.Cleanup(upstream.Close)

	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i)
	}
	ps := provider.NewStore(database, master)
	if _, err := ps.Create("surplus", models.ProviderOpenAI, upstream.URL+"/v1", "k"); err != nil {
		t.Fatal(err)
	}
	ks := apikey.NewStore(database)
	k, err := ks.Create("test-key")
	if err != nil {
		t.Fatal(err)
	}

	h := newLegacyHandlerWithCatalog(ps, catalog.NewStore(database), database)
	r := chi.NewRouter()
	r.Post("/v1/chat/completions", h.ChatCompletions)
	r.Post("/v1/systemone", h.SystemOne)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return srv, database, k.Key, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(paths))
		copy(out, paths)
		return out
	}
}

// insertDecisionRow records a provider_models row — exactly what GET /v1/models
// advertises and what the capability guards must agree with.
func insertDecisionRow(t *testing.T, database *sql.DB, providerID, modelID string, decision bool, modalities string) {
	t.Helper()
	now := time.Now().UTC()
	flag := 0
	if decision {
		flag = 1
	}
	if _, err := database.Exec(`INSERT INTO provider_models(
		id, provider_id, model_id, display_name, owned_by, decision, modalities, source, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		providerID+"-"+modelID, providerID, modelID, modelID, "surplus", flag, modalities, "manual", now, now); err != nil {
		t.Fatalf("insert provider_models row %s: %v", modelID, err)
	}
}

// TestAdvertisedDecisionRowAcceptedOnSystemOne is the regression guard for the
// reported failure: `POST /v1/systemone {"model":"surplus/jev-1.13"}` returned
//
//	model 'surplus/jev-1.13' is not a decision model; POST /v1/systemone accepts
//	noul/choice/score decision models (for text use /v1/chat/completions)
//
// even though GET /v1/models advertised the same id as `"decision": true`.
// Cause: the guard consulted only the catalog, and models.dev has no notion of
// decision models — the bare slug "jev-1.13" resolved to a *different*
// provider's text row (opencode/jev-1.13), which vetoed both the provider's own
// row and the curated jev family. The provider's row is the listing, so it wins.
func TestAdvertisedDecisionRowAcceptedOnSystemOne(t *testing.T) {
	srv, database, key, paths := decisionRowServer(t)
	pid := providerIDFor(t, database, "surplus")
	insertDecisionRow(t, database, pid, "jev-1.13", true, `{"input":["text"],"output":["text"]}`)
	// The catalog row that used to veto the classification (foreign provider
	// namespace, text modalities).
	upsertCatalogRow(t, database, "opencode/jev-1.13", false, false, false, `{"input":["text"],"output":["text"]}`)

	resp, body := postJSONImage(t, srv.URL+"/v1/systemone", key,
		`{"model":"surplus/jev-1.13","state":"x","questions":{"q":{"type":"noul","instructions":"is this a test?"}}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("advertised decision model must be accepted: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Answers map[string]any `json:"answers"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Answers) == 0 {
		t.Fatalf("unexpected decision response: %s", body)
	}
	// The request must have gone to the Jev dialect path, not llama.cpp's.
	got := paths()
	if len(got) != 1 || got[0] != "/v1/decisions" {
		t.Fatalf("upstream paths = %v, want [/v1/decisions]", got)
	}

	// Listing parity: the same row refuses chat traffic with a pointer to the
	// decision endpoint, so /v1/models and the guards agree about the id.
	resp, body = postJSONImage(t, srv.URL+"/v1/chat/completions", key,
		`{"model":"surplus/jev-1.13","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "/v1/systemone") {
		t.Fatalf("decision row must be refused on chat: %d %s", resp.StatusCode, body)
	}
}

// TestProviderTextRowKeepsChat proves the row is authoritative in both
// directions: a provider that lists a family id as a text model keeps chat
// traffic (the curated `jev-` family rule must not make a reseller's chat mirror
// uncallable), while the decision endpoint still refuses it with the actionable
// message.
func TestProviderTextRowKeepsChat(t *testing.T) {
	srv, database, key, _ := decisionRowServer(t)
	pid := providerIDFor(t, database, "surplus")
	insertDecisionRow(t, database, pid, "jev-1.14", false, `{"input":["text"],"output":["text"]}`)

	resp, body := postJSONImage(t, srv.URL+"/v1/chat/completions", key,
		`{"model":"surplus/jev-1.14","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("provider text row must stay chattable: %d %s", resp.StatusCode, body)
	}

	resp, body = postJSONImage(t, srv.URL+"/v1/systemone", key,
		`{"model":"surplus/jev-1.14","state":"x","questions":{"q":{"type":"noul","instructions":"y"}}}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "not a decision model") {
		t.Fatalf("text row must be refused on systemone: %d %s", resp.StatusCode, body)
	}
}

// TestDecisionPathSettingOverridesDialect proves the per-provider override is
// honoured for a provider whose decision endpoint follows neither known
// spelling.
func TestDecisionPathSettingOverridesDialect(t *testing.T) {
	srv, database, key, paths := decisionRowServer(t)
	pid := providerIDFor(t, database, "surplus")
	insertDecisionRow(t, database, pid, "jev-1.13", true, `{"input":["text"],"output":["decision"]}`)
	if _, err := database.Exec(`INSERT INTO system_config(key,value,updated_at) VALUES('provider.surplus.decision_path','/v2/decide',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}

	resp, body := postJSONImage(t, srv.URL+"/v1/systemone", key,
		`{"model":"surplus/jev-1.13","state":"x","questions":{"q":{"type":"noul","instructions":"y"}}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("override path request failed: %d %s", resp.StatusCode, body)
	}
	if got := paths(); len(got) != 1 || got[0] != "/v2/decide" {
		t.Fatalf("upstream paths = %v, want [/v2/decide]", got)
	}
}

// providerIDFor resolves a provider's id by name (the fixture knows the name,
// the rows are keyed by id).
func providerIDFor(t *testing.T, database *sql.DB, name string) string {
	t.Helper()
	var id string
	if err := database.QueryRow(`SELECT id FROM providers WHERE name=?`, name).Scan(&id); err != nil {
		t.Fatalf("provider %s: %v", name, err)
	}
	return id
}
