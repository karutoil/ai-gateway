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
