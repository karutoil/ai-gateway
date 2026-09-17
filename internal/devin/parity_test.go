package devin

import (
	"encoding/binary"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// OMP-parity tests: wire behavior cross-checked against
// can1357/oh-my-pi packages/ai/src/providers/devin.ts,
// packages/catalog/{discovery/devin.ts, discovery/devin-proto.ts, wire/devin.ts}.

func floatField(num uint64, v float32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
	return append([]byte{byte(num*8 + 5)}, b[:]...)
}

func metaFields(t *testing.T, req []byte) map[uint64][]field {
	t.Helper()
	outer, err := parseFields(req)
	if err != nil || len(outer) == 0 {
		t.Fatalf("request parse: %v", err)
	}
	inner, err := parseFields(outer[0].value)
	if err != nil {
		t.Fatalf("metadata parse: %v", err)
	}
	out := map[uint64][]field{}
	for _, f := range inner {
		out[f.number] = append(out[f.number], f)
	}
	return out
}

func metaString(fs map[uint64][]field, n uint64) string {
	if len(fs[n]) == 0 {
		return ""
	}
	return string(fs[n][0].value)
}

// Chat metadata must carry the released CLI identity (devin-cli/chisel):
// the backend gates router assignment and the CLI model surface on
// ideType "chisel".
func TestMetadataUsesCliIdentity(t *testing.T) {
	fs := metaFields(t, BuildUserJWTRequest("tok-1"))
	if got := metaString(fs, 1); got != "devin-cli" {
		t.Errorf("ideName = %q, want devin-cli", got)
	}
	if got := metaString(fs, 28); got != "chisel" {
		t.Errorf("ideType = %q, want chisel", got)
	}
	if got := metaString(fs, 7); got != "3000.6.2" {
		t.Errorf("ideVersion = %q", got)
	}
	if got := metaString(fs, 2); got != "3000.6.2" {
		t.Errorf("extensionVersion = %q", got)
	}
	if got := metaString(fs, 12); got != "chisel" {
		t.Errorf("extensionName = %q", got)
	}
	if got := metaString(fs, 3); got != "devin-session-token$tok-1" {
		t.Errorf("apiKey = %q", got)
	}
	if got := metaString(fs, 4); got != "en" {
		t.Errorf("locale = %q", got)
	}
	if got := metaString(fs, 5); got != runtime.GOOS {
		t.Errorf("os = %q", got)
	}
	// OMP omits these: no request/session/trigger ids, no plan name, no
	// user JWT on session-token calls.
	for _, n := range []uint64{9, 10, 21, 25, 26} {
		if len(fs[n]) != 0 {
			t.Errorf("field %d must be omitted, got %d value(s)", n, len(fs[n]))
		}
	}
}

// Discovery metadata uses the dev-channel identity plus advertised display
// slots (field 30) that make the server return its full catalog.
func TestDiscoveryMetadata(t *testing.T) {
	fs := metaFields(t, BuildDiscoveryRequest("tok-1"))
	if got := metaString(fs, 1); got != "chisel" {
		t.Errorf("ideName = %q, want chisel", got)
	}
	if got := metaString(fs, 7); got != "0.0.0-dev" {
		t.Errorf("ideVersion = %q", got)
	}
	if len(fs[28]) != 0 {
		t.Error("ideType must be omitted on discovery calls")
	}
	var displays []uint64
	for _, f := range fs[30] {
		displays = append(displays, f.vint)
	}
	if !reflect.DeepEqual(displays, []uint64{3, 4, 6, 7, 8}) {
		t.Errorf("displays = %v, want [3 4 6 7 8]", displays)
	}
}

func testFeatures(thinking, images, tools bool) []byte {
	var parts [][]byte
	if images {
		parts = append(parts, varintField(11, 1))
	}
	if tools {
		parts = append(parts, varintField(12, 1))
	}
	if thinking {
		parts = append(parts, varintField(15, 1))
	}
	return message(6, concat(parts...))
}

func testFamValue(order int, name string) []byte {
	return message(2, concat(varintField(1, uint64(order)), stringField(2, name)))
}

func testFamEntry(key string, order int, name string) []byte {
	return message(2, concat(stringField(1, key), testFamValue(order, name)))
}

func testFamily(label string, entries ...[]byte) []byte {
	parts := [][]byte{stringField(1, label)}
	return message(30, concat(append(parts, entries...)...))
}

func testDim(label string, value float32, denom string, kind uint64) []byte {
	return message(32, concat(stringField(1, label), floatField(2, value), stringField(3, denom), varintField(6, kind)))
}

func testModelInfo(maxOut, display int, router bool, harness []string, feat []byte) []byte {
	var parts [][]byte
	if maxOut > 0 {
		parts = append(parts, varintField(13, uint64(maxOut)))
	}
	if display != 0 {
		parts = append(parts, varintField(22, uint64(display)))
	}
	if router {
		parts = append(parts, varintField(25, 1))
	}
	for _, h := range harness {
		parts = append(parts, stringField(20, h))
	}
	if feat != nil {
		parts = append(parts, feat)
	}
	return message(23, concat(parts...))
}

// Full ClientModelConfig parity: costs, max output, features, family,
// description and flags must all decode per the OMP schema numbers.
func TestDecodeFullModelConfig(t *testing.T) {
	cfg := concat(
		stringField(1, "Claude X High"),
		varintField(4, 0),
		varintField(5, 1),
		varintField(9, 1),
		varintField(18, 200000),
		stringField(22, "claude-x-high"),
		stringField(27, "A test model"),
		testModelInfo(32000, 0, false, nil, testFeatures(true, true, true)),
		testFamily("Claude X", testFamEntry("effort", 3, "High")),
		testDim("input", 3.0, "1M tokens", 1),
		testDim("cached input", 0.3, "1M tokens", 1),
		testDim("output", 15.0, "1M tokens", 2),
		testDim("input", 99.0, "1M tokens", 9),
	)
	models, err := DecodeModels(message(1, cfg))
	if err != nil || len(models) != 1 {
		t.Fatalf("decode: %v %+v", err, models)
	}
	m := models[0]
	if m.ID != "claude-x-high" || m.Name != "Claude X High" || m.Description != "A test model" {
		t.Errorf("identity = %+v", m)
	}
	if m.ContextWindow != 200000 || m.MaxTokens != 32000 {
		t.Errorf("ctx/max = %d/%d, want 200000/32000", m.ContextWindow, m.MaxTokens)
	}
	if !m.Reasoning || !m.ImageInput || !m.ToolCalls {
		t.Errorf("features = %+v, want all true", m)
	}
	if m.InputCost != 3 || m.OutputCost != 15 || m.CacheReadCost != 0.3 {
		t.Errorf("costs = %v/%v/%v, want 3/15/0.3", m.InputCost, m.OutputCost, m.CacheReadCost)
	}
	if !m.IsBeta || m.Family == nil || m.Family.Label != "Claude X" {
		t.Errorf("flags/family = %+v", m)
	}
}

// Internal display slots and harness-less routers never surface.
func TestDecodeFiltersInternalAndRouters(t *testing.T) {
	mk := func(uid string, info []byte) []byte {
		return message(1, concat(stringField(1, uid), stringField(22, uid), info))
	}
	payload := concat(
		mk("keep-1", nil),
		mk("quick", testModelInfo(0, 4, false, nil, nil)),
		mk("internal", testModelInfo(0, 6, false, nil, nil)),
		mk("router", testModelInfo(0, 3, true, nil, nil)),
		mk("composite", testModelInfo(0, 0, true, []string{"h1"}, nil)),
	)
	models, err := DecodeModels(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]bool{}
	for _, m := range models {
		got[m.ID] = true
	}
	if !got["keep-1"] || !got["composite"] {
		t.Errorf("kept = %v, want keep-1 + composite", got)
	}
	for _, drop := range []string{"quick", "internal", "router"} {
		if got[drop] {
			t.Errorf("%s must be filtered", drop)
		}
	}
}

// Uids whose backends silently drop prompt images must not advertise them.
func TestDecodeImageBlindUIDs(t *testing.T) {
	cfg := message(1, concat(stringField(1, "SWE-1.6"), stringField(22, "swe-1-6"), varintField(5, 1)))
	models, err := DecodeModels(cfg)
	if err != nil || len(models) != 1 {
		t.Fatalf("decode: %v %+v", err, models)
	}
	if models[0].ImageInput {
		t.Error("swe-1-6 must not advertise image input")
	}
}

// Server-declared family lanes collapse with explicit effort routing,
// including the Claude off-twin disambiguated by its Thinking axis.
func TestCollapseFamilyLane(t *testing.T) {
	fam := func(thinkingOrder int) *FamilyInfo {
		return &FamilyInfo{Label: "Claude X", Entries: []FamilyEntry{
			{Key: "effort", Order: 3, Name: "High"},
			{Key: "thinking", Order: thinkingOrder, Name: "Thinking"},
		}}
	}
	raw := []DiscoveredModel{
		{ID: "uid-high", Name: "X High", ContextWindow: 200000, MaxTokens: 32000,
			Reasoning: true, ImageInput: true, ToolCalls: true,
			InputCost: 3, OutputCost: 15, Family: fam(1)},
		{ID: "uid-base", Name: "X", ContextWindow: 200000, MaxTokens: 32000,
			Reasoning: false, ImageInput: true, ToolCalls: true,
			InputCost: 3, OutputCost: 15, Family: fam(0)},
	}
	got := CollapseModels(raw)
	if len(got) != 1 {
		t.Fatalf("groups = %+v, want 1 lane", got)
	}
	g := got[0]
	if g.ID != "claude-x" || g.Name != "Claude X" {
		t.Errorf("lane = %q/%q", g.ID, g.Name)
	}
	if !reflect.DeepEqual(g.Levels, []string{"high"}) {
		t.Errorf("levels = %v, want [high]", g.Levels)
	}
	if !reflect.DeepEqual(g.Routing, map[string]string{"high": "uid-high", "off": "uid-base"}) {
		t.Errorf("routing = %v", g.Routing)
	}
	if got2 := ResolveRuntime("devin/claude-x", "high", g.Routing); got2 != "uid-high" {
		t.Errorf("route high = %q", got2)
	}
	if got2 := ResolveRuntime("devin/claude-x", "off", g.Routing); got2 != "uid-base" {
		t.Errorf("route off = %q", got2)
	}
}

// Auth responses may redirect chat traffic at a per-account host (field 2).
func TestGetUserJWTCustomBase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(concat(stringField(1, "jwt-1"), stringField(2, "https://custom.example.com/")))
	}))
	defer srv.Close()
	jwt, custom, err := GetUserJWT("sess", srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if jwt != "jwt-1" {
		t.Errorf("jwt = %q", jwt)
	}
	if custom != "https://custom.example.com" {
		t.Errorf("custom base = %q", custom)
	}
}

// Inline data: images ride prompt field 10; remote URLs are skipped.
func TestFromOpenAIImages(t *testing.T) {
	body := `{"model":"devin/swe-2","messages":[{"role":"user","content":[
		{"type":"text","text":"look"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0="}},
		{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}
	]}]}`
	in, err := FromOpenAI([]byte(body), "devin/swe-2")
	if err != nil {
		t.Fatalf("from: %v", err)
	}
	if len(in.Messages) != 1 {
		t.Fatalf("messages = %+v", in.Messages)
	}
	if in.Messages[0].Text != "look" {
		t.Errorf("text = %q", in.Messages[0].Text)
	}
	if len(in.Messages[0].Images) != 1 {
		t.Fatalf("images = %+v", in.Messages[0].Images)
	}
	img := in.Messages[0].Images[0]
	if img.Base64 != "iVBORw0=" || img.Mime != "image/png" {
		t.Errorf("image = %+v", img)
	}
	raw := BuildChatRequest(in, "sess", "jwt")
	if !strings.Contains(string(raw), "iVBORw0=") {
		t.Error("image bytes missing from wire request")
	}
}
