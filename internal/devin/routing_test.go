package devin

import (
	"reflect"
	"testing"
)

func TestSplitReasoningSuffix(t *testing.T) {
	cases := []struct {
		in    string
		base  string
		level string
		ok    bool
	}{
		{"swe-2-max", "swe-2", "max", true},
		{"swe-2-high", "swe-2", "high", true},
		{"swe-1-7-low", "swe-1-7", "low", true},
		{"SWE-2-High", "SWE-2", "high", true},
		{"swe-2-X-HIGH", "swe-2", "xhigh", true},
		{"swe-2-extra-low", "swe-2", "extra-low", true},
		{"swe-2-minimal", "swe-2", "minimal", true},
		{"swe-2-medium", "swe-2", "medium", true},
		// Bare ids and non-level trailing segments must not collapse.
		{"swe-2", "swe-2", "", false},
		{"swe-1-7", "swe-1-7", "", false},
		{"swe-1-6", "swe-1-6", "", false},
		{"swe-2-ultra", "swe-2-ultra", "", false},
		{"low", "low", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		base, level, ok := SplitReasoningSuffix(c.in)
		if base != c.base || level != c.level || ok != c.ok {
			t.Errorf("Split(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, base, level, ok, c.base, c.level, c.ok)
		}
	}
}

func TestResolveRuntime(t *testing.T) {
	cases := []struct {
		model, effort, want string
	}{
		// Bare base + effort routes to the suffixed wire id.
		{"devin/swe-2", "high", "swe-2-high"},
		{"swe-2", "max", "swe-2-max"},
		{"swe-1-7", "LOW", "swe-1-7-low"},
		{"swe-2", "x-high", "swe-2-xhigh"},
		// No effort: explicit variants pass through, bare stays bare.
		{"devin/swe-2-max", "", "swe-2-max"},
		{"devin/swe-2", "", "swe-2"},
		// Explicit off strips to the base.
		{"devin/swe-2-max", "off", "swe-2"},
		{"swe-2-high", "none", "swe-2"},
		// Effort re-routes an explicit variant.
		{"devin/swe-2-max", "low", "swe-2-low"},
		// Unknown effort never mangles the id.
		{"devin/swe-2", "ultra", "swe-2"},
	}
	for _, c := range cases {
		if got := ResolveRuntime(c.model, c.effort, nil); got != c.want {
			t.Errorf("ResolveRuntime(%q,%q) = %q, want %q", c.model, c.effort, got, c.want)
		}
	}
}

func TestResolveRuntimeWithRouting(t *testing.T) {
	routing := map[string]string{
		"off": "claude-opus-4-6", "high": "claude-opus-4-6-thinking", "max": "claude-opus-4-6-thinking",
	}
	cases := []struct {
		model, effort, want string
	}{
		{"devin/fam", "high", "claude-opus-4-6-thinking"},
		{"devin/fam", "off", "claude-opus-4-6"},
		{"devin/fam", "", "fam"},
		// Unmapped effort passes through verbatim — never invent a uid.
		{"devin/fam", "low", "fam"},
		{"devin/some-uid", "ultra", "some-uid"},
	}
	for _, c := range cases {
		if got := ResolveRuntime(c.model, c.effort, routing); got != c.want {
			t.Errorf("ResolveRuntime(%q,%q) = %q, want %q", c.model, c.effort, got, c.want)
		}
	}
	// Empty non-nil routing: no routable levels, verbatim passthrough.
	if got := ResolveRuntime("devin/frontier-x", "high", map[string]string{}); got != "frontier-x" {
		t.Errorf("empty routing high = %q, want frontier-x", got)
	}
	if got := ResolveRuntime("devin/frontier-x", "off", map[string]string{}); got != "frontier-x" {
		t.Errorf("empty routing off = %q, want frontier-x", got)
	}
}

func TestCollapseModels(t *testing.T) {
	raw := []DiscoveredModel{
		{ID: "swe-2", Name: "SWE-2", ContextWindow: 200000, MaxTokens: 64000},
		{ID: "swe-2-max", Name: "SWE-2 Max", ContextWindow: 200000, MaxTokens: 64000},
		{ID: "swe-2-high", Name: "SWE-2 High", ContextWindow: 200000, MaxTokens: 32000, ImageInput: true},
		// No bare member: seed name applies, levels still collapse.
		{ID: "swe-1-7-low", Name: "SWE-1.7 Low", ContextWindow: 200000, MaxTokens: 64000},
	}
	got := CollapseModels(raw)
	if len(got) != 2 {
		t.Fatalf("groups = %+v, want 2", got)
	}
	if got[0].ID != "swe-1-7" || got[1].ID != "swe-2" {
		t.Fatalf("sorted ids: %+v", got)
	}
	if !reflect.DeepEqual(got[1].Levels, []string{"high", "max"}) {
		t.Errorf("swe-2 levels = %v, want [high max]", got[1].Levels)
	}
	if !reflect.DeepEqual(got[1].Routing, map[string]string{"high": "swe-2-high", "max": "swe-2-max"}) {
		t.Errorf("swe-2 routing = %v, want constructed suffix map", got[1].Routing)
	}
	if _, ok := got[1].Routing[defaultRouteKey]; ok {
		t.Errorf("swe-2 (bare member present) must not carry a catch-all: %v", got[1].Routing)
	}
	if got[1].Name != "SWE-2" {
		t.Errorf("swe-2 name = %q, want bare member name", got[1].Name)
	}
	if !got[1].ImageInput {
		t.Error("swe-2 image must OR across variants")
	}
	if got[1].MaxTokens != 64000 {
		t.Errorf("swe-2 maxTokens = %d, want max across variants", got[1].MaxTokens)
	}
	if got[0].Name != "SWE-1.7" {
		t.Errorf("swe-1-7 name = %q, want seed name", got[0].Name)
	}
	if !reflect.DeepEqual(got[0].Levels, []string{"low"}) {
		t.Errorf("swe-1-7 levels = %v, want [low]", got[0].Levels)
	}
}

func TestCollapseModelsBareOnlyDefaults(t *testing.T) {
	got := CollapseModels([]DiscoveredModel{
		{ID: "swe-9", Name: "SWE-9", ContextWindow: 200000, MaxTokens: 64000},
	})
	if len(got) != 1 {
		t.Fatalf("groups = %+v", got)
	}
	if !reflect.DeepEqual(got[0].Levels, DefaultReasoningLevels()) {
		t.Errorf("levels = %v, want defaults", got[0].Levels)
	}
	// Bare-only groups carry an empty (verbatim-passthrough) routing map,
	// never nil: the proxy must not invent suffixed uids for them.
	if got[0].Routing == nil || len(got[0].Routing) != 0 {
		t.Errorf("routing = %v, want empty non-nil map", got[0].Routing)
	}
}

func TestCollapseSuffixOnlyGroupCatchAll(t *testing.T) {
	raw := []DiscoveredModel{
		{ID: "swe-2-medium", Name: "SWE-2 Medium", ContextWindow: 200000, MaxTokens: 64000, Reasoning: true, ToolCalls: true, ImageInput: true},
		{ID: "swe-2-high", Name: "SWE-2 High", ContextWindow: 200000, MaxTokens: 64000, Reasoning: true, ToolCalls: true, ImageInput: true},
		{ID: "swe-2-max", Name: "SWE-2 Max", ContextWindow: 200000, MaxTokens: 64000, Reasoning: true, ToolCalls: true, ImageInput: true, DefaultInFamily: true},
	}
	got := CollapseModels(raw)
	if len(got) != 1 || got[0].ID != "swe-2" {
		t.Fatalf("groups = %+v, want single swe-2 base row", got)
	}
	g := got[0]
	if !reflect.DeepEqual(g.Levels, []string{"medium", "high", "max"}) {
		t.Errorf("levels = %v, want [medium high max]", g.Levels)
	}
	// The base id never appeared on the wire: the catch-all must target the
	// server-declared default member, never the bare base.
	want := map[string]string{
		"medium": "swe-2-medium", "high": "swe-2-high", "max": "swe-2-max",
		defaultRouteKey: "swe-2-max",
	}
	if !reflect.DeepEqual(g.Routing, want) {
		t.Fatalf("routing = %v, want %v", g.Routing, want)
	}
	cases := []struct{ effort, want string }{
		{"", "swe-2-max"},      // no effort: declared default member
		{"off", "swe-2-max"},   // no off route: catch-all, never the bare base
		{"low", "swe-2-max"},   // unrouted level: catch-all
		{"high", "swe-2-high"}, // routed level wins
	}
	for _, c := range cases {
		if got := ResolveRuntime("devin/swe-2", c.effort, g.Routing); got != c.want {
			t.Errorf("ResolveRuntime(swe-2, %q) = %q, want %q", c.effort, got, c.want)
		}
	}
	// Explicit variants keep the verbatim passthrough regardless.
	if got := ResolveRuntime("devin/swe-2-max", "", g.Routing); got != "swe-2-max" {
		t.Errorf("explicit variant = %q, want swe-2-max", got)
	}
}

func TestCollapseSuffixOnlyGroupFallbackCatchAll(t *testing.T) {
	// No server-declared default: the lightest observed tier becomes the
	// catch-all so no-effort requests still reach a real wire id.
	raw := []DiscoveredModel{
		{ID: "swe-2-medium", Reasoning: true},
		{ID: "swe-2-high", Reasoning: true},
		{ID: "swe-2-max", Reasoning: true},
	}
	got := CollapseModels(raw)
	if len(got) != 1 {
		t.Fatalf("groups = %+v", got)
	}
	if got[0].Routing[defaultRouteKey] != "swe-2-medium" {
		t.Errorf("catch-all = %q, want lightest tier swe-2-medium", got[0].Routing[defaultRouteKey])
	}
	if u := ResolveRuntime("devin/swe-2", "", got[0].Routing); u != "swe-2-medium" {
		t.Errorf("no-effort = %q, want swe-2-medium", u)
	}
}

func TestCollapseBareMemberKeepsVerbatim(t *testing.T) {
	// A live bare member is a valid wire id: no catch-all may be invented
	// and bare no-effort requests must stay verbatim.
	raw := []DiscoveredModel{
		{ID: "swe-1-7", Reasoning: true},
		{ID: "swe-1-7-medium", Reasoning: true},
	}
	got := CollapseModels(raw)
	if len(got) != 1 || got[0].ID != "swe-1-7" {
		t.Fatalf("groups = %+v, want single swe-1-7 base row", got)
	}
	g := got[0]
	if _, ok := g.Routing[defaultRouteKey]; ok {
		t.Fatalf("routing = %v, catch-all must not exist when a bare member was observed", g.Routing)
	}
	if u := ResolveRuntime("devin/swe-1-7", "", g.Routing); u != "swe-1-7" {
		t.Errorf("no-effort = %q, want verbatim swe-1-7", u)
	}
	if u := ResolveRuntime("devin/swe-1-7", "off", g.Routing); u != "swe-1-7" {
		t.Errorf("off = %q, want base swe-1-7", u)
	}
	if u := ResolveRuntime("devin/swe-1-7", "high", g.Routing); u != "swe-1-7" {
		t.Errorf("unrouted level = %q, want bare base passthrough", u)
	}
}
