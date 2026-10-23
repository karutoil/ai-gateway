package proxy

// Regression: an lb_rules member must satisfy the endpoint's capability
// predicate.
//
// The rule branch of resolveCandidates built its candidate list from rule
// members and never called `pred` — pred was applied only by consider(), which
// the X-Provider hint / qualified-id pin / legacy tiers use. A rule for an image
// model that also listed an Anthropic provider therefore posted an
// OpenAI/Gemini image body to /v1/messages, and a rule for a System One model
// listing a text provider sent the decision body to a chat endpoint. Both are
// capability-isolation violations the endpoint guards exist to prevent.

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/lb"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

func TestLBRuleMembersRespectCapabilityPredicate(t *testing.T) {
	var anthropicHits int32
	anth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&anthropicHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","content":[]}`))
	}))
	defer anth.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)

	// An Anthropic provider — cannot speak the image/decision wire formats.
	p, err := ps.Create("anth", models.ProviderAnthropic, anth.URL, "sk-ant")
	if err != nil {
		t.Fatal(err)
	}
	lbs := lb.NewStore(database)
	// A rule that pins a REAL curated decision model ("clef", from
	// decision.registry) to that provider. It must be a genuine decision model or
	// SystemOne rejects the request as invalid before routing and the test would
	// prove nothing.
	if err := lbs.ReplaceRule("clef", lb.StrategyFailover, []lb.RuleMemberInput{
		{ProviderID: p.ID},
	}); err != nil {
		t.Fatalf("replace rule: %v", err)
	}

	k, _ := ks.Create("cap-key")
	h := newLegacyHandler(ps, database)
	h.LB = lbs
	r := chi.NewRouter()
	r.Post("/v1/systemone", h.SystemOne)
	srv := httptest.NewServer(r)
	defer srv.Close()

	// The decision predicate excludes Anthropic providers, so the rule has no
	// eligible member and the request must be refused — never forwarded.
	resp, body := postJSONImage(t, srv.URL+"/v1/systemone", k.Key,
		`{"model":"clef","state":{"transcript":[{"role":"user","content":"hi"}]},"questions":{"q1":{"type":"score","instructions":"rate","criteria":["low","high"]}}}`)
	if n := atomic.LoadInt32(&anthropicHits); n != 0 {
		t.Fatalf("an Anthropic provider received %d System One request(s) via an lb rule — capability predicate bypassed (status %d: %s)",
			n, resp.StatusCode, body)
	}
}
