package lb

import (
	"testing"
)

func testRule(strategy string, members ...Member) *Rule {
	return &Rule{Model: "test-model", Strategy: strategy, Members: members}
}

func member(id string, weight int) Member {
	return Member{ProviderID: id, Name: id, Weight: weight}
}

// Round-robin rotates the start position across calls.
func TestSelectRoundRobinRotates(t *testing.T) {
	s := &Store{DB: nil} // Select ordering logic does not touch the DB
	rule := testRule(StrategyRoundRobin,
		member("a", 1), member("b", 1), member("c", 1))

	first := -1
	for i := 0; i < 6; i++ {
		ordered := s.orderMembers(rule)
		if len(ordered) != 3 {
			t.Fatalf("expected 3 ordered members, got %d", len(ordered))
		}
		if i == 0 {
			first = 0
		}
		_ = first
	}
	// Two consecutive calls must not always start with the same member.
	starts := map[string]bool{}
	for i := 0; i < 12; i++ {
		ordered := s.orderMembers(rule)
		starts[ordered[0].ProviderID] = true
	}
	if len(starts) < 2 {
		t.Fatalf("round-robin should rotate across calls, saw only %v", starts)
	}
}

// Failover preserves position order strictly.
func TestSelectFailoverPreservesOrder(t *testing.T) {
	s := &Store{DB: nil}
	rule := testRule(StrategyFailover, member("a", 1), member("b", 1), member("c", 1))
	for i := 0; i < 5; i++ {
		ordered := s.orderMembers(rule)
		if ordered[0].ProviderID != "a" || ordered[1].ProviderID != "b" || ordered[2].ProviderID != "c" {
			t.Fatalf("failover order must be positional, got %v", memberIDs(ordered))
		}
	}
}

// Random visits multiple distinct heads and always returns every member.
func TestSelectRandomShuffles(t *testing.T) {
	s := &Store{DB: nil}
	rule := testRule(StrategyRandom, member("a", 1), member("b", 1), member("c", 1), member("d", 1))
	heads := map[string]bool{}
	for i := 0; i < 60; i++ {
		ordered := s.orderMembers(rule)
		if len(ordered) != 4 {
			t.Fatalf("expected 4 members, got %d", len(ordered))
		}
		seen := map[string]bool{}
		for _, p := range ordered {
			if seen[p.ProviderID] {
				t.Fatalf("duplicate member in ordering: %v", memberIDs(ordered))
			}
			seen[p.ProviderID] = true
		}
		heads[ordered[0].ProviderID] = true
	}
	if len(heads) < 2 {
		t.Fatalf("random strategy never varied its head: %v", heads)
	}
}

// Down members are filtered out of the ordering; if all are down the full set
// returns (honest errors preferred over silence).
func TestSelectFiltersDownMembers(t *testing.T) {
	s := &Store{DB: nil}
	down := "down"
	rule := testRule(StrategyFailover,
		Member{ProviderID: "a", Name: "a", HealthStatus: &down},
		Member{ProviderID: "b", Name: "b"},
	)
	ordered := s.orderMembers(rule)
	if len(ordered) != 1 || ordered[0].ProviderID != "b" {
		t.Fatalf("expected only healthy member b, got %v", memberIDs(ordered))
	}

	allDown := testRule(StrategyFailover,
		Member{ProviderID: "a", Name: "a", HealthStatus: &down},
	)
	if got := s.orderMembers(allDown); len(got) != 1 {
		t.Fatalf("all-down rule should return full set, got %v", memberIDs(got))
	}
}

// Weighted heavily favors the heavy member as the primary pick.
func TestSelectWeightedDistribution(t *testing.T) {
	s := &Store{DB: nil}
	rule := testRule(StrategyWeighted, member("light", 10), member("heavy", 90))
	heavy := 0
	const n = 200
	for i := 0; i < n; i++ {
		ordered := s.orderMembers(rule)
		if len(ordered) != 2 {
			t.Fatalf("expected 2 members, got %d", len(ordered))
		}
		if ordered[0].ProviderID == "heavy" {
			heavy++
		}
	}
	// 90% expectation; generous bounds tolerate variance (99% binomial
	// two-sided band at n=200 is roughly 83%-96%).
	if heavy < 160 || heavy > 195 {
		t.Fatalf("weighted pick of heavy member: got %d/%d (want ~180)", heavy, n)
	}
}

// Weight + strategy validation.
func TestReplaceRuleWeightValidation(t *testing.T) {
	cases := []struct {
		strategy string
		weight   int
		wantErr  bool
	}{
		{StrategyWeighted, 0, true},
		{StrategyWeighted, -5, true},
		{StrategyWeighted, 101, true},
		{StrategyRoundRobin, 0, false}, // non-weighted: weight normalized
		{"bogus", 1, true},
	}
	for _, tc := range cases {
		err := validateStrategyAndInputs(tc.strategy, []RuleMemberInput{{ProviderID: "p", Weight: tc.weight}})
		if tc.wantErr && err == nil {
			t.Errorf("strategy=%s weight=%d: expected error", tc.strategy, tc.weight)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("strategy=%s weight=%d: unexpected error %v", tc.strategy, tc.weight, err)
		}
	}
}

// ModelOverrideFor: override wins, fall back to rule model, miss reports false.
func TestModelOverrideFor(t *testing.T) {
	rule := &Rule{
		Model:    "gpt-4o",
		Strategy: StrategyRoundRobin,
		Members: []Member{
			{ProviderID: "a", ModelOverride: "gpt-4o-2024-11-20"},
			{ProviderID: "b"},
		},
	}
	if got, ok := rule.ModelOverrideFor("a"); !ok || got != "gpt-4o-2024-11-20" {
		t.Fatalf("override member: got %q ok=%v", got, ok)
	}
	if got, ok := rule.ModelOverrideFor("b"); !ok || got != "gpt-4o" {
		t.Fatalf("plain member should inherit rule model: got %q ok=%v", got, ok)
	}
	if _, ok := rule.ModelOverrideFor("c"); ok {
		t.Fatal("non-member should report false")
	}
	if _, ok := (*Rule)(nil).ModelOverrideFor("a"); ok {
		t.Fatal("nil rule should report false")
	}
}

// ValidStrategy accepts exactly the supported set.
func TestValidStrategy(t *testing.T) {
	for _, s := range []string{StrategyRoundRobin, StrategyRandom, StrategyWeighted, StrategyFailover} {
		if !ValidStrategy(s) {
			t.Errorf("expected %q valid", s)
		}
	}
	for _, s := range []string{"", "cheapest", "latency"} {
		if ValidStrategy(s) {
			t.Errorf("expected %q invalid", s)
		}
	}
}

// A rule may list the same provider several times as distinct
// (provider, model) options; an exact duplicate pair is rejected.
func TestValidateRuleInputsDuplicateMembers(t *testing.T) {
	mk := func(override string) RuleMemberInput {
		return RuleMemberInput{ProviderID: "openai", ModelOverride: override, Weight: 1}
	}
	// Same provider, different models: allowed — as many options as wanted.
	if err := validateRuleInputs(StrategyRoundRobin, []RuleMemberInput{mk("gpt-4o"), mk("gpt-4o-mini"), {ProviderID: "groq", ModelOverride: "llama-3", Weight: 1}}); err != nil {
		t.Fatalf("distinct provider+model options must be accepted: %v", err)
	}
	// Exact duplicate (same provider AND same model): rejected.
	if err := validateStrategyAndInputs(StrategyRoundRobin, []RuleMemberInput{mk("gpt-4o"), mk("gpt-4o")}); err == nil {
		t.Fatal("exact duplicate member must be rejected")
	}
	// Same provider twice with no override at all: also an exact duplicate.
	if err := validateStrategyAndInputs(StrategyRoundRobin, []RuleMemberInput{mk(""), mk("")}); err == nil {
		t.Fatal("duplicate bare member must be rejected")
	}
}

// SelectCandidates carries each member's own model override — with several
// members on one provider, the provider id alone must not decide the model.
func TestSelectCandidatesPerMemberOverride(t *testing.T) {
	rule := testRule(StrategyFailover,
		Member{ProviderID: "a", ModelOverride: "gpt-4o-2024-11-20"},
		Member{ProviderID: "a", ModelOverride: "gpt-4o-mini"},
		Member{ProviderID: "b"},
	)
	// orderMembers is the DB-free ordering core; verify each position keeps
	// its own override rather than collapsing to the first "a" entry.
	ordered := (&Store{}).orderMembers(rule)
	if len(ordered) != 3 {
		t.Fatalf("expected 3 ordered members, got %d", len(ordered))
	}
	overrides := map[string]int{}
	for _, m := range ordered {
		key := m.ProviderID + "\x00" + m.ModelOverride
		overrides[key]++
	}
	if overrides["a\x00gpt-4o-2024-11-20"] != 1 || overrides["a\x00gpt-4o-mini"] != 1 || overrides["b\x00"] != 1 {
		t.Fatalf("per-member overrides lost in ordering: %v", overrides)
	}
	// Candidate helper: override travels with the member.
	c := Candidate{Member: ordered[0]}
	if got := c.ModelOverrideForRequest(); got != "gpt-4o-2024-11-20" {
		t.Fatalf("candidate override: got %q", got)
	}
	if got := (Candidate{Member: ordered[2]}).ModelOverrideForRequest(); got != "" {
		t.Fatalf("plain member should keep the requested model, got %q", got)
	}
}

func memberIDs(members []Member) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.ProviderID)
	}
	return out
}
