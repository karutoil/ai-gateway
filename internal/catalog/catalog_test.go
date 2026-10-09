package catalog

import (
	"math"
	"testing"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
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

// AIHubMix coding-plan slugs embed the vendor name ("coding-xiaomi-…"), but
// models.dev keys the base rows without it ("xiaomi/mimo-v2.6-pro") and may
// have no coding- row for new generations at all. The lookup must fall back
// to the bare base as wildcard instead of leaving the model unenriched.
func TestFindBestMatchCodingVendorInfix(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)
	body := []byte(`{
		"xiaomi": {"id":"xiaomi","name":"xiaomi","api":"openai","models":{
			"mimo-v2.6-pro": {"id":"xiaomi/mimo-v2.6-pro","name":"MiMo-V2.6-Pro","cost":{"input":0.435,"output":0.87},"limit":{"context":1048576,"output":131072},"reasoning":true,"tool_call":true}
		}},
		"aihubmix": {"id":"aihubmix","name":"aihubmix","api":"openai","models":{
			"coding-xiaomi-mimo-v2.5-pro": {"id":"aihubmix/coding-xiaomi-mimo-v2.5-pro","name":"Coding MiMo V2.5 Pro","cost":{"input":0.5,"output":1.0},"limit":{"context":1048576,"output":131072},"reasoning":true,"tool_call":true}
		}}
	}`)
	if _, err := s.SyncFromBytes(body); err != nil {
		t.Fatal(err)
	}

	// Vendor-infixed coding slug with no coding- catalog row: bare base.
	m, kind, err := s.FindBestMatch("AIHubMix/coding-xiaomi-mimo-v2.6-pro")
	if err != nil {
		t.Fatalf("coding vendor lookup failed: %v", err)
	}
	if kind != "wildcard" {
		t.Errorf("coding vendor kind = %q, want wildcard", kind)
	}
	if m.ID != "xiaomi/mimo-v2.6-pro" {
		t.Errorf("coding vendor row = %q, want xiaomi/mimo-v2.6-pro", m.ID)
	}
	if m.ContextWindow != 1048576 || m.InputCost != 0.435 || m.OutputCost != 0.87 {
		t.Errorf("coding vendor detail = ctx %d in %.4f out %.4f, want 1048576/0.435/0.87",
			m.ContextWindow, m.InputCost, m.OutputCost)
	}

	// Exact coding rows still win over the bare base.
	m, kind, err = s.FindBestMatch("coding-xiaomi-mimo-v2.5-pro")
	if err != nil {
		t.Fatalf("exact vendor coding lookup failed: %v", err)
	}
	if kind != "exact" || m.ID != "aihubmix/coding-xiaomi-mimo-v2.5-pro" {
		t.Errorf("exact vendor coding = kind %q row %q, want exact/aihubmix/coding-xiaomi-mimo-v2.5-pro", kind, m.ID)
	}
}

func TestStripUmansChannel(t *testing.T) {
	if got, ok := stripUmansPrefix("umans-glm-5.2"); !ok || got != "glm-5.2" {
		t.Errorf("stripUmansPrefix = %q,%v want glm-5.2,true", got, ok)
	}
	if got, ok := stripUmansPrefix("Umans-GLM-5.2"); !ok || got != "GLM-5.2" {
		t.Errorf("stripUmansPrefix case = %q,%v", got, ok)
	}
	if _, ok := stripUmansPrefix("glm-5.2"); ok {
		t.Errorf("stripUmansPrefix on base should not strip")
	}
	if _, ok := stripUmansPrefix("umans-"); ok {
		t.Errorf("stripUmansPrefix on bare prefix should not strip")
	}
	if got, ok := stripLabSuffix("umans-glm-5.3-flash-lab"); !ok || got != "umans-glm-5.3-flash" {
		t.Errorf("stripLabSuffix = %q,%v want umans-glm-5.3-flash,true", got, ok)
	}
	if _, ok := stripLabSuffix("glm-5.3-flash"); ok {
		t.Errorf("stripLabSuffix on non-lab should not strip")
	}
	if _, ok := stripLabSuffix("lab"); ok {
		t.Errorf("stripLabSuffix on bare suffix should not strip")
	}

	// Lab ids propose minimal strips first so the channel's own non-lab
	// sibling row stays preferred over the bare base.
	alts := umansAlternates("umans-glm-5.3-flash-lab")
	want := []string{"umans-glm-5.3-flash", "glm-5.3-flash-lab", "glm-5.3-flash"}
	if len(alts) != len(want) {
		t.Fatalf("umansAlternates lab = %v, want %v", alts, want)
	}
	for i := range want {
		if alts[i] != want[i] {
			t.Fatalf("umansAlternates lab = %v, want %v", alts, want)
		}
	}
	alts = umansAlternates("umans-glm-5.2")
	if len(alts) != 1 || alts[0] != "glm-5.2" {
		t.Errorf("umansAlternates plain = %v, want [glm-5.2]", alts)
	}
	if alts := umansAlternates("glm-5.2"); len(alts) != 0 {
		t.Errorf("umansAlternates on non-umans slug = %v, want none", alts)
	}
}

// The umans channel namespaces every model id ("umans-<base>") and lab mode
// wraps it as "umans-<base>-lab", while models.dev keys base rows without the
// decorations (and mixes id casing). Lookup must resolve through the
// decorations to the base row — preferring the channel's own rows when they
// exist — instead of leaving lab models unenriched.
func TestFindBestMatchUmansChannel(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)
	body := []byte(`{
		"umans-ai-coding-plan": {"id":"umans-ai-coding-plan","name":"umans-ai-coding-plan","api":"openai","models":{
			"umans-kimi-k2.7": {"id":"umans-ai-coding-plan/umans-kimi-k2.7","name":"Kimi K2.7","cost":{"input":0,"output":0},"limit":{"context":262144,"output":262144},"reasoning":true,"tool_call":true}
		}},
		"zai": {"id":"zai","name":"zai","api":"openai","models":{
			"glm-5.2": {"id":"zai/glm-5.2","name":"GLM-5.2","cost":{"input":1.4,"output":4.4},"limit":{"context":1000000,"output":131072},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high","max"]}],"tool_call":true},
			"glm-5.3-flash": {"id":"zai/glm-5.3-flash","name":"GLM-5.3-Flash","cost":{"input":0.075,"output":0.25},"limit":{"context":1000000,"output":131072},"reasoning":true,"tool_call":true}
		}},
		"amd": {"id":"amd","name":"amd","api":"openai","models":{
			"Qwen3.8-Flash-Next": {"id":"amd/Qwen3.8-Flash-Next","name":"Qwen3.8-Flash-Next","cost":{"input":0.15,"output":0.47},"limit":{"context":262144,"output":65536},"reasoning":true,"tool_call":true}
		}},
		"crof": {"id":"crof","name":"crof","api":"openai","models":{
			"deepseek-v4-flash-vision-exp-20260115": {"id":"crof/deepseek-v4-flash-vision-exp-20260115","name":"DeepSeek-V4-Flash-Vision-Exp","cost":{"input":0.08,"output":0.2},"limit":{"context":1000000,"output":393215},"reasoning":true,"tool_call":true}
		}},
		"acme": {"id":"acme","name":"acme","api":"openai","models":{
			"gemini-3.5-flash": {"id":"acme/gemini-3.5-flash","name":"Gemini Flash","cost":{"input":11,"output":12},"limit":{"context":6000,"output":600},"reasoning":false,"tool_call":true}
		}}
	}`)
	if _, err := s.SyncFromBytes(body); err != nil {
		t.Fatal(err)
	}

	// Exact channel rows win over stripped bases (non-lab sibling form).
	m, kind, err := s.FindBestMatch("umans-kimi-k2.7")
	if err != nil {
		t.Fatalf("channel row lookup failed: %v", err)
	}
	if kind != "exact" || m.ID != "umans-ai-coding-plan/umans-kimi-k2.7" {
		t.Errorf("channel row = kind %q row %q, want exact/umans-ai-coding-plan/umans-kimi-k2.7", kind, m.ID)
	}

	// Plain channel id without a channel row falls back to the bare base.
	m, kind, err = s.FindBestMatch("umans-glm-5.2")
	if err != nil {
		t.Fatalf("plain base lookup failed: %v", err)
	}
	if kind != "wildcard" || m.ID != "zai/glm-5.2" {
		t.Errorf("plain base = kind %q row %q, want wildcard/zai/glm-5.2", kind, m.ID)
	}
	if m.ContextWindow != 1000000 || m.InputCost != 1.4 || m.ReasoningType != "effort" {
		t.Errorf("plain base detail = ctx %d in %.2f rtype %q, want 1000000/1.4/effort",
			m.ContextWindow, m.InputCost, m.ReasoningType)
	}

	// Lab-mode id resolves through both decorations to the base row.
	m, kind, err = s.FindBestMatch("umans-glm-5.3-flash-lab")
	if err != nil {
		t.Fatalf("lab lookup failed: %v", err)
	}
	if kind != "wildcard" || m.ID != "zai/glm-5.3-flash" {
		t.Errorf("lab = kind %q row %q, want wildcard/zai/glm-5.3-flash", kind, m.ID)
	}

	// Lab-mode base whose catalog row uses different casing: suffix matching
	// must stay case-insensitive (Postgres LIKE is case-sensitive).
	m, kind, err = s.FindBestMatch("umans-qwen3.8-flash-next-lab")
	if err != nil {
		t.Fatalf("mixed-case lab lookup failed: %v", err)
	}
	if kind != "wildcard" || m.ID != "amd/Qwen3.8-Flash-Next" {
		t.Errorf("mixed-case lab = kind %q row %q, want wildcard/amd/Qwen3.8-Flash-Next", kind, m.ID)
	}

	// Dated catalog variant resolves via containment on the stripped base.
	m, kind, err = s.FindBestMatch("umans-deepseek-v4-flash-vision-exp-lab")
	if err != nil {
		t.Fatalf("dated lab lookup failed: %v", err)
	}
	if kind != "wildcard" || m.ID != "crof/deepseek-v4-flash-vision-exp-20260115" {
		t.Errorf("dated lab = kind %q row %q, want wildcard/crof/deepseek-v4-flash-vision-exp-20260115", kind, m.ID)
	}
}

// Bare-word bases ("umans-flash-lab" -> "flash") are channel product names,
// not versioned model slugs. Fuzzy containment must never snap them to
// unrelated rows ("acme/gemini-3.5-flash"); a wrong enrichment is worse than
// none.
func TestFindBestMatchUmansBareWordGuard(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)
	body := []byte(`{
		"acme": {"id":"acme","name":"acme","api":"openai","models":{
			"gemini-3.5-flash": {"id":"acme/gemini-3.5-flash","name":"Gemini Flash","cost":{"input":11,"output":12},"limit":{"context":6000,"output":600},"reasoning":false,"tool_call":true}
		}}
	}`)
	if _, err := s.SyncFromBytes(body); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FindBestMatch("umans-flash-lab"); err == nil {
		t.Errorf("bare-word base should miss, got a match")
	}

	// With the channel sibling present, the lab id resolves to it precisely.
	body = []byte(`{
		"umans-ai-coding-plan": {"id":"umans-ai-coding-plan","name":"umans-ai-coding-plan","api":"openai","models":{
			"umans-flash": {"id":"umans-ai-coding-plan/umans-flash","name":"Umans Flash","cost":{"input":0,"output":0},"limit":{"context":262144,"output":262144},"reasoning":true,"tool_call":true}
		}}
	}`)
	if _, err := s.SyncFromBytes(body); err != nil {
		t.Fatal(err)
	}
	m, kind, err := s.FindBestMatch("umans-flash-lab")
	if err != nil {
		t.Fatalf("sibling lookup failed: %v", err)
	}
	if kind != "wildcard" || m.ID != "umans-ai-coding-plan/umans-flash" {
		t.Errorf("sibling = kind %q row %q, want wildcard/umans-ai-coding-plan/umans-flash", kind, m.ID)
	}
}

// TestCatalogReadsTolerateNullColumns pins NULL-tolerance across the catalog
// read paths. Only id/provider/name are NOT NULL in the schema; before this
// was fixed the other columns were scanned straight into string/int/float64/
// time.Time, so a row with ANY NULL failed the scan and the caller treated it
// as "no such model". The model then silently became unpriceable — recording
// $0 cost and bypassing every per-key budget.
func TestCatalogReadsTolerateNullColumns(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)

	// Deliberately NULL in every optional column, including the ones the scan
	// used to require: description, family, context_window, max_output,
	// modalities, knowledge_cutoff and updated_at.
	if _, err := database.Exec(`INSERT INTO models_catalog(id,provider,name,input_cost,output_cost)
		VALUES('nullvendor/null-model','nullvendor','Null Model',1.5,3.0)`); err != nil {
		t.Fatal(err)
	}

	m, err := s.Get("nullvendor/null-model")
	if err != nil {
		t.Fatalf("Get on NULL-heavy row: %v", err)
	}
	if m.InputCost != 1.5 || m.OutputCost != 3.0 {
		t.Fatalf("pricing lost: input=%v output=%v", m.InputCost, m.OutputCost)
	}
	if m.Description != "" || m.Family != "" || m.ContextWindow != 0 || !m.UpdatedAt.IsZero() {
		t.Fatalf("NULLs not folded to zero values: %+v", m)
	}

	// The pricing path is the one that actually matters: FindBestMatch must
	// reach the row rather than reporting it missing.
	got, _, err := s.FindBestMatch("nullvendor/null-model")
	if err != nil {
		t.Fatalf("FindBestMatch on NULL-heavy row: %v", err)
	}
	if got.InputCost != 1.5 {
		t.Fatalf("FindBestMatch input cost = %v, want 1.5", got.InputCost)
	}

	// List reads the same projection through the row-loop path.
	list, err := s.List("", "", false, 10, 0)
	if err != nil {
		t.Fatalf("List with a NULL-heavy row present: %v", err)
	}
	if len(list) != 1 || list[0].InputCost != 1.5 {
		t.Fatalf("List lost the NULL-heavy row: %+v", list)
	}
}

// TestCostForSplitCacheAware pins the cache-aware pricing math, including the
// property that a catalog carrying no cache rates reproduces the old
// flat-input behaviour exactly (so this change is a no-op for such models).
func TestCostForSplitCacheAware(t *testing.T) {
	const eps = 1e-12
	priced := &models.CatalogModel{
		InputCost:      1.0,
		OutputCost:     2.0,
		CacheReadCost:  0.25,
		CacheWriteCost: 1.25,
	}
	// OpenAI shape: 1000 prompt of which 400 were cached, 500 completion.
	openai := CacheSplit{Fresh: 600, CacheRead: 400, Completion: 500}
	// expected: 600*1.0 + 400*0.25 + 500*2.0, all per 1M
	wantOpenAI := (600*1.0 + 400*0.25 + 500*2.0) / 1_000_000
	if got := CostForSplit(priced, openai); math.Abs(got-wantOpenAI) > eps {
		t.Fatalf("openai-shaped cost = %v, want %v", got, wantOpenAI)
	}
	// The whole point: it must be strictly cheaper than billing every prompt
	// token at the input rate, which is what the gateway did before.
	naive := (1000*1.0 + 500*2.0) / 1_000_000
	if got := CostForSplit(priced, openai); !(got < naive) {
		t.Fatalf("cache-aware cost %v not below naive %v", got, naive)
	}

	// Anthropic shape: input_tokens is FRESH; cache counts are additional.
	anthropic := CacheSplit{Fresh: 1000, CacheRead: 400, CacheWrite: 100, Completion: 500}
	wantAnthropic := (1000*1.0 + 400*0.25 + 100*1.25 + 500*2.0) / 1_000_000
	if got := CostForSplit(priced, anthropic); math.Abs(got-wantAnthropic) > eps {
		t.Fatalf("anthropic-shaped cost = %v, want %v", got, wantAnthropic)
	}

	// No cache rates in the catalog: cache tokens fall back to the standard
	// input rate, which is exactly the pre-existing behaviour.
	noCacheRates := &models.CatalogModel{InputCost: 1.0, OutputCost: 2.0}
	wantFallback := (600*1.0 + 400*1.0 + 500*2.0) / 1_000_000
	if got := CostForSplit(noCacheRates, openai); math.Abs(got-wantFallback) > eps {
		t.Fatalf("fallback cost = %v, want %v", got, wantFallback)
	}

	// nil model and empty split are both zero, never panics.
	if got := CostForSplit(nil, openai); got != 0 {
		t.Fatalf("nil model cost = %v, want 0", got)
	}
	if got := CostForSplit(priced, CacheSplit{}); got != 0 {
		t.Fatalf("empty split cost = %v, want 0", got)
	}
}

// TestCacheSplitHelpers covers the accessors used by the zero-cost reasons.
func TestCacheSplitHelpers(t *testing.T) {
	empty := CacheSplit{}
	if empty.Any() {
		t.Fatal("empty split reported usage")
	}
	if empty.TotalInput() != 0 {
		t.Fatalf("empty TotalInput = %d", empty.TotalInput())
	}
	s := CacheSplit{Fresh: 1, CacheRead: 2, CacheWrite: 3, Completion: 4}
	if !s.Any() {
		t.Fatal("populated split reported no usage")
	}
	if got := s.TotalInput(); got != 6 {
		t.Fatalf("TotalInput = %d, want 6 (cache buckets are input)", got)
	}
	// Completion-only usage is still usage — a request can return output from
	// an empty prompt (e.g. a canned response) and must still be priced.
	if !(CacheSplit{Completion: 1}).Any() {
		t.Fatal("completion-only split reported no usage")
	}
}
