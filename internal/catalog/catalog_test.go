package catalog

import (
	"testing"

	"ai-gateway/internal/db"
)

// Gateway-qualified IDs ("oc1/muse-spark-1.3-contributor") carry the
// gateway's own provider prefix, which never matches catalog IDs keyed by
// models.dev namespace. The lookup must strip to the suffix — and prefer a
// priced row, since the suffix match can hit zero-price mirrors.
func TestGetByShortIDGatewayQualifiedPrefersPriced(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)

	body := []byte(`{
		"meta": {"id":"meta","name":"meta","api":"openai","models":{
			"muse-spark-1.3-contributor": {"id":"meta/muse-spark-1.3-contributor","name":"Muse Spark","cost":{"input":0.1,"output":0.2}}
		}},
		"ollama-cloud": {"id":"ollama-cloud","name":"ollama-cloud","api":"openai","models":{
			"muse-spark-1.3-contributor": {"id":"ollama-cloud/muse-spark-1.3-contributor","name":"Muse Spark","cost":{"input":0,"output":0}}
		}}
	}`)
	if _, err := s.SyncFromBytes(body); err != nil {
		t.Fatal(err)
	}

	// Gateway-qualified form, as recorded in request_logs.model.
	m, err := s.GetByShortID("oc1/muse-spark-1.3-contributor")
	if err != nil {
		t.Fatalf("gateway-qualified lookup failed: %v", err)
	}
	if m.InputCost != 0.1 || m.OutputCost != 0.2 {
		t.Fatalf("gateway-qualified lookup hit wrong row: %s in=%.4f out=%.4f", m.ID, m.InputCost, m.OutputCost)
	}

	// Bare short ID must also prefer the priced row over the zero-price mirror.
	m, err = s.GetByShortID("muse-spark-1.3-contributor")
	if err != nil {
		t.Fatalf("short lookup failed: %v", err)
	}
	if m.InputCost != 0.1 || m.OutputCost != 0.2 {
		t.Fatalf("short lookup hit zero-price row: %s in=%.4f out=%.4f", m.ID, m.InputCost, m.OutputCost)
	}

	if got := CostFor(m, 1_000_000, 1_000_000); got < 0.299 || got > 0.301 {
		t.Fatalf("CostFor = %v, want ~0.3", got)
	}
}

func TestNormalizeModelIDResellerTags(t *testing.T) {
	cases := map[string]string{
		"[Kiro3][手动标记]  claude-opus-4-6 [不补]":       "claude-opus-4-6",
		"[Kiro3][正价] claude-opus-4-5-thinking [不补]": "claude-opus-4-5-thinking",
		"[aws] grok-4.6":     "grok-4.6",
		"[aws][量] grok-4.6":  "grok-4.6",
		"[aws]deepseek-v3.1": "deepseek-v3.1",
		"[三方4][量][反重力]claude-opus-4-6-thinking[不补]": "claude-opus-4-6-thinking",
		"[codex] gpt-5.6-sol  [不补]":                 "gpt-5.6-sol",
		"[官转1] deepseek-3.2":                        "deepseek-3.2",
		"[反重力][自营][次][假流] gemini-2.5-flash":         "gemini-2.5-flash",
		"[量7][贵] claude-fable-5-1[不补]":              "claude-fable-5-1",
		"oc1/muse-spark-1.3-contributor":            "muse-spark-1.3-contributor",
		"ck-default/[aws] grok-4.6":                 "grok-4.6",
		"test-model":                                "test-model",
		"":                                          "",
		"[only-tags]":                               "",
	}
	for in, want := range cases {
		if got := NormalizeModelID(in); got != want {
			t.Errorf("NormalizeModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func seedWildcardCatalog(t *testing.T, s *Store) {
	t.Helper()
	body := []byte(`{
		"acme": {"id":"acme","name":"acme","api":"openai","models":{
			"grok-4.6": {"id":"acme/grok-4.6","name":"Grok","cost":{"input":1,"output":2},"limit":{"context":1000,"output":100},"reasoning":false,"tool_call":true},
			"deepseek-v3.2": {"id":"acme/deepseek-v3.2","name":"DeepSeek","cost":{"input":3,"output":4},"limit":{"context":2000,"output":200},"reasoning":false,"tool_call":true},
			"bar-1-think": {"id":"acme/bar-1-think","name":"Bar","cost":{"input":5,"output":6},"limit":{"context":3000,"output":300},"reasoning":true,"tool_call":true},
			"foo-9": {"id":"acme/foo-9","name":"Foo","cost":{"input":7,"output":8},"limit":{"context":4000,"output":400},"reasoning":false,"tool_call":true},
			"gemini-3.5-flash": {"id":"acme/gemini-3.5-flash","name":"Flash","cost":{"input":11,"output":12},"limit":{"context":6000,"output":600},"reasoning":false,"tool_call":true},
			"glm-5.2": {"id":"acme/glm-5.2","name":"GLM","cost":{"input":13,"output":14},"limit":{"context":7000,"output":700},"reasoning":false,"tool_call":true},
			"claude-opus-4-1-20250805": {"id":"acme/claude-opus-4-1-20250805","name":"Opus","cost":{"input":9,"output":10},"limit":{"context":5000,"output":500},"reasoning":false,"tool_call":true}
		}}
	}`)
	if _, err := s.SyncFromBytes(body); err != nil {
		t.Fatal(err)
	}
}

func TestFindBestMatchWildcard(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)
	seedWildcardCatalog(t, s)

	// Reseller tags strip to an exact suffix hit.
	m, kind, err := s.FindBestMatch("[aws][量] grok-4.6")
	if err != nil {
		t.Fatalf("tagged lookup failed: %v", err)
	}
	if kind != "normalized" {
		t.Errorf("tagged lookup kind = %q, want normalized", kind)
	}
	if m.InputCost != 1 || m.ContextWindow != 1000 {
		t.Errorf("tagged lookup wrong row: %s in=%.2f ctx=%d", m.ID, m.InputCost, m.ContextWindow)
	}

	// Missing "v" infix resolves to the catalog spelling.
	m, kind, err = s.FindBestMatch("[官转1] deepseek-3.2")
	if err != nil {
		t.Fatalf("v-variant lookup failed: %v", err)
	}
	if kind != "wildcard" {
		t.Errorf("v-variant kind = %q, want wildcard", kind)
	}
	if m.InputCost != 3 {
		t.Errorf("v-variant wrong row: %s", m.ID)
	}

	// "-thinking" finds the "-think" snapshot row with reasoning forced.
	m, kind, err = s.FindBestMatch("[x] bar-1-thinking")
	if err != nil {
		t.Fatalf("think-swap lookup failed: %v", err)
	}
	if kind != "wildcard" || !m.Reasoning {
		t.Errorf("think-swap = kind %q reasoning %v, want wildcard/true", kind, m.Reasoning)
	}

	// Thinking suffix with only a non-reasoning base forces reasoning on.
	m, kind, err = s.FindBestMatch("[x] foo-9-thinking")
	if err != nil {
		t.Fatalf("thinking-base lookup failed: %v", err)
	}
	if kind != "wildcard" || !m.Reasoning || m.InputCost != 7 {
		t.Errorf("thinking-base = kind %q reasoning %v cost %.2f, want wildcard/true/7", kind, m.Reasoning, m.InputCost)
	}

	// Dated snapshot variant matches by containment.
	m, kind, err = s.FindBestMatch("[三方1][次][2] claude-opus-4-1 [不补]")
	if err != nil {
		t.Fatalf("dated lookup failed: %v", err)
	}
	if kind != "wildcard" || m.InputCost != 9 {
		t.Errorf("dated = kind %q cost %.2f, want wildcard/9", kind, m.InputCost)
	}

	// Truly unknown slugs still miss.
	if _, _, err := s.FindBestMatch("test-model"); err == nil {
		t.Errorf("unknown slug should miss")
	}
	if _, _, err := s.FindBestMatch("[only-tags]"); err == nil {
		t.Errorf("tags-only slug should miss")
	}

	// Suffixed variants back off to the base slug.
	m, kind, err = s.FindBestMatch("[gcli] gemini-3.5-flash-high [不补]")
	if err != nil {
		t.Fatalf("backoff lookup failed: %v", err)
	}
	if kind != "wildcard" || m.InputCost != 11 {
		t.Errorf("backoff = kind %q cost %.2f, want wildcard/11", kind, m.InputCost)
	}
	m, kind, err = s.FindBestMatch("[按量6] glm-5.2-venice [不补]")
	if err != nil {
		t.Fatalf("venice lookup failed: %v", err)
	}
	if kind != "wildcard" || m.InputCost != 13 {
		t.Errorf("venice = kind %q cost %.2f, want wildcard/13", kind, m.InputCost)
	}
}

func TestStripCodingPrefixFreeSuffix(t *testing.T) {
	if got, ok := stripCodingPrefix("coding-glm-5.3"); !ok || got != "glm-5.3" {
		t.Errorf("stripCodingPrefix = %q,%v want glm-5.3,true", got, ok)
	}
	if got, ok := stripCodingPrefix("Coding-GLM-5.3"); !ok || got != "GLM-5.3" {
		t.Errorf("stripCodingPrefix case = %q,%v", got, ok)
	}
	if _, ok := stripCodingPrefix("glm-5.3"); ok {
		t.Errorf("stripCodingPrefix on base should not strip")
	}
	if got, ok := stripFreeSuffix("glm-5.3-free"); !ok || got != "glm-5.3" {
		t.Errorf("stripFreeSuffix = %q,%v want glm-5.3,true", got, ok)
	}
	if _, ok := stripFreeSuffix("glm-5.3"); ok {
		t.Errorf("stripFreeSuffix on paid should not strip")
	}
	if !isFreeSlug("coding-glm-5.3-free") || isFreeSlug("coding-glm-5.3") {
		t.Errorf("isFreeSlug mismatch")
	}
}

// AIHubMix coding-plan channel ("coding-<base>", "<base>-free") has no exact
// models.dev row for new generations (e.g. coding-glm-5.3): enrichment must
// fall back to the base row as wildcard, zeroing costs for *-free so free
// traffic never inherits paid pricing.
func TestFindBestMatchCodingPlan(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)
	body := []byte(`{
		"aihubmix": {"id":"aihubmix","name":"aihubmix","api":"openai","models":{
			"glm-5.3": {"id":"aihubmix/glm-5.3","name":"GLM-5.3","cost":{"input":1.1268,"output":3.9438,"cache_read":0.28},"limit":{"context":1000000,"output":128000},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high","max"]}],"tool_call":true,"structured_output":true},
			"coding-glm-5.1": {"id":"aihubmix/coding-glm-5.1","name":"Coding GLM 5.1","cost":{"input":0.06,"output":0.22},"limit":{"context":200000,"output":128000},"reasoning":true,"tool_call":true}
		}},
		"tokenrouter": {"id":"tokenrouter","name":"tokenrouter","api":"openai","models":{
			"glm-5.3-free": {"id":"z-ai/glm-5.3-free","name":"GLM free","cost":{"input":0,"output":0},"limit":{"context":1000000,"output":131072},"reasoning":true,"tool_call":true}
		}}
	}`)
	if _, err := s.SyncFromBytes(body); err != nil {
		t.Fatal(err)
	}

	// Paid coding-plan slug inherits base context/reasoning/pricing.
	m, kind, err := s.FindBestMatch("coding-glm-5.3")
	if err != nil {
		t.Fatalf("coding paid lookup failed: %v", err)
	}
	if kind != "wildcard" {
		t.Errorf("coding paid kind = %q, want wildcard", kind)
	}
	if m.ContextWindow != 1000000 || m.MaxOutput != 128000 || !m.Reasoning || !m.ToolCall {
		t.Errorf("coding paid wrong detail: %+v", m)
	}
	if m.InputCost != 1.1268 || m.OutputCost != 3.9438 {
		t.Errorf("coding paid cost = %.4f/%.4f, want base 1.1268/3.9438", m.InputCost, m.OutputCost)
	}
	if m.ReasoningType != "effort" {
		t.Errorf("coding paid reasoning_type = %q, want effort", m.ReasoningType)
	}

	// Free coding-plan slug resolves with zeroed pricing but kept detail.
	m, kind, err = s.FindBestMatch("coding-glm-5.3-free")
	if err != nil {
		t.Fatalf("coding free lookup failed: %v", err)
	}
	if kind != "wildcard" {
		t.Errorf("coding free kind = %q, want wildcard", kind)
	}
	if m.InputCost != 0 || m.OutputCost != 0 || m.CacheReadCost != 0 || m.CacheWriteCost != 0 {
		t.Errorf("coding free must be zeroed, got in=%.4f out=%.4f cr=%.4f cw=%.4f", m.InputCost, m.OutputCost, m.CacheReadCost, m.CacheWriteCost)
	}
	if m.ContextWindow == 0 || !m.Reasoning {
		t.Errorf("coding free missing detail: %+v", m)
	}

	// Exact coding rows still match exactly (not wildcard).
	m, kind, err = s.FindBestMatch("coding-glm-5.1")
	if err != nil {
		t.Fatalf("exact coding lookup failed: %v", err)
	}
	if kind != "exact" || m.InputCost != 0.06 {
		t.Errorf("exact coding = kind %q cost %.4f, want exact/0.06", kind, m.InputCost)
	}
}
