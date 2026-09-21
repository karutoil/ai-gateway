package lb

import (
	"testing"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
)

func groupStoreEnv(t *testing.T) (*Store, *provider.Store, *models.Provider, *models.Provider) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ps := provider.NewStore(database, make([]byte, 32))
	pa, err := ps.Create("prov-a", models.ProviderOpenAI, "http://localhost:9/v1", "sk-a")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := ps.Create("prov-b", models.ProviderOpenAI, "http://localhost:9/v1", "sk-b")
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(database), ps, pa, pb
}

func TestGroupRoundTrip(t *testing.T) {
	s, _, pa, pb := groupStoreEnv(t)
	g, err := s.ReplaceGroup("subagent-dispatcher", "Subagent dispatcher", "failover", "", []RuleMemberInput{
		{ProviderID: pa.ID, ModelOverride: "openai/gpt-5"},
		{ProviderID: pb.ID, ModelOverride: "anthropic/claude-opus-4-6"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "subagent-dispatcher" || g.DisplayName != "Subagent dispatcher" {
		t.Fatalf("group metadata mismatch: %+v", g)
	}
	if len(g.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(g.Members))
	}

	// RuleForModelOrGroup resolves the group.
	rule := s.RuleForModelOrGroup("subagent-dispatcher")
	if rule == nil {
		t.Fatal("expected group rule")
	}
	if rule.Strategy != StrategyFailover || len(rule.Members) != 2 {
		t.Fatalf("rule mismatch: %+v", rule)
	}
	if got, ok := rule.ModelOverrideFor(pa.ID); got != "openai/gpt-5" || !ok {
		t.Fatalf("member override not preserved: got %q ok=%v", got, ok)
	}

	// AllGroups lists it.
	groups, err := s.AllGroups("")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Name != "subagent-dispatcher" {
		t.Fatalf("AllGroups mismatch: %+v", groups)
	}

	// Delete removes it.
	if err := s.DeleteGroup("subagent-dispatcher", ""); err != nil {
		t.Fatal(err)
	}
	if s.IsGroup("subagent-dispatcher", "") {
		t.Fatal("group still exists after delete")
	}
	if s.RuleForModelOrGroup("subagent-dispatcher") != nil {
		t.Fatal("rule for deleted group should be nil")
	}
}

func TestGroupValidation(t *testing.T) {
	s, _, pa, _ := groupStoreEnv(t)
	cases := []struct {
		name    string
		members []RuleMemberInput
	}{
		{"bad name", []RuleMemberInput{{ProviderID: pa.ID, ModelOverride: "m"}}},
		{"", []RuleMemberInput{{ProviderID: pa.ID, ModelOverride: "m"}}},
		{"good-name", []RuleMemberInput{}},
		{"good-name", []RuleMemberInput{{ProviderID: pa.ID}}},
	}
	for i, tc := range cases {
		_, err := s.ReplaceGroup(tc.name, "", "round_robin", "", tc.members)
		if err == nil {
			t.Errorf("expected error for case %d %q", i, tc.name)
		}
	}
	// slash is not a legal group-name character.
	if _, err := s.ReplaceGroup("org/team", "", "round_robin", "", []RuleMemberInput{{ProviderID: pa.ID, ModelOverride: "m"}}); err == nil {
		t.Error("expected slash in group name to be rejected")
	}
}

func TestGroupOrgScoping(t *testing.T) {
	s, _, pa, pb := groupStoreEnv(t)
	if _, err := s.ReplaceGroup("shared", "", "failover", "", []RuleMemberInput{{ProviderID: pa.ID, ModelOverride: "m"}}); err != nil {
		t.Fatal(err)
	}
	if !s.IsGroup("shared", "org-1") {
		t.Error("global group should be visible to a scoped org")
	}
	// A different org cannot replace a group owned by another org.
	if _, err := s.ReplaceGroup("owned-by-1", "", "failover", "org-1", []RuleMemberInput{{ProviderID: pa.ID, ModelOverride: "m"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceGroup("owned-by-1", "", "failover", "org-2", []RuleMemberInput{{ProviderID: pb.ID, ModelOverride: "n"}}); err == nil {
		t.Error("expected cross-org replace to be rejected")
	}
	// A scoped org can create and delete its own group.
	if _, err := s.ReplaceGroup("org-1-private", "", "round_robin", "org-1", []RuleMemberInput{{ProviderID: pa.ID, ModelOverride: "m"}}); err != nil {
		t.Fatal(err)
	}
	if !s.IsGroup("org-1-private", "org-1") {
		t.Error("org group should be visible to its own org")
	}
	if s.IsGroup("org-1-private", "org-2") {
		t.Error("org group should not be visible to a different org")
	}
	if err := s.DeleteGroup("org-1-private", "org-1"); err != nil {
		t.Fatal(err)
	}
}
