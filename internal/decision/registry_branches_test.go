package decision

import (
	"strings"
	"testing"
)

// TestRegistryProjectionsForSeeding covers CatalogID / Description /
// ModalitiesJSON for every curated entry. They had no coverage, and they are
// what the catalog seeder writes: a wrong modality string would make the row
// invisible to every capability listing that keys off modalities.
func TestRegistryProjectionsForSeeding(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("empty decision registry")
	}
	for _, m := range all {
		if m.CatalogID() == "" || !strings.Contains(m.CatalogID(), "/") {
			t.Errorf("%s: catalog id = %q, want namespace-qualified", m.ID, m.CatalogID())
		}
		if m.Description() == "" {
			t.Errorf("%s: empty description", m.ID)
		}
		mod := m.ModalitiesJSON()
		if !strings.Contains(mod, `"output":["decision"]`) {
			t.Errorf("%s: modalities %q must declare the decision output", m.ID, mod)
		}
		if !strings.Contains(mod, `"text"`) {
			t.Errorf("%s: modalities %q must accept text input", m.ID, mod)
		}
		if m.Vision && !strings.Contains(mod, `"image"`) {
			t.Errorf("%s: vision model without an image input modality: %s", m.ID, mod)
		}
		if m.Audio && !strings.Contains(mod, `"audio"`) {
			t.Errorf("%s: audio model without an audio input modality: %s", m.ID, mod)
		}
		if m.Video && !strings.Contains(mod, `"video"`) {
			t.Errorf("%s: video model without a video input modality: %s", m.ID, mod)
		}
	}
}

// TestCatalogIDWithoutNamespace pins the unqualified fallback.
func TestCatalogIDWithoutNamespace(t *testing.T) {
	if got := (Model{ID: "clef"}).CatalogID(); got != "clef" {
		t.Fatalf("CatalogID without namespace = %q", got)
	}
	// The generic description is used for families outside kev/clef/jev.
	if got := (Model{Family: "other"}).Description(); !strings.Contains(got, "System One") {
		t.Fatalf("generic description = %q", got)
	}
}

// TestValidateRemainingBranches closes the validation branches the table-driven
// test does not reach: over-long question ids, empty instruction objects/arrays,
// non-JSON instruction scalars, noul criteria of the wrong JSON type, and
// choice/score criteria submitted as the wrong shape.
func TestValidateRemainingBranches(t *testing.T) {
	longID := strings.Repeat("q", MaxQuestionIDLen+1)
	cases := []struct{ name, body, want string }{
		{"question id too long",
			`{"model":"clef","state":"x","questions":{"` + longID + `":{"type":"noul","instructions":"q"}}}`,
			"exceeds"},
		{"instructions empty object",
			`{"model":"clef","state":"x","questions":{"a":{"type":"noul","instructions":{}}}}`,
			"empty object"},
		{"instructions empty array",
			`{"model":"clef","state":"x","questions":{"a":{"type":"noul","instructions":[]}}}`,
			"empty array"},
		{"instructions scalar number",
			`{"model":"clef","state":"x","questions":{"a":{"type":"noul","instructions":5}}}`,
			"string, object, or array"},
		{"noul criteria array",
			`{"model":"clef","state":"x","questions":{"a":{"type":"noul","instructions":"q","criteria":["x"]}}}`,
			"true/false"},
		{"choice criteria option empty key",
			`{"model":"clef","state":"x","questions":{"a":{"type":"choice","instructions":"q","criteria":{"":"x","b":"y"}}}}`,
			"non-empty"},
		{"score criteria object",
			`{"model":"clef","state":"x","questions":{"a":{"type":"score","instructions":"q","criteria":{"a":"x","b":"y"}}}}`,
			"ordered array"},
		{"question not an object",
			`{"model":"clef","state":"x","questions":{"a":"noul"}}`,
			"typed question object"},
		{"null question id",
			`{"model":"clef","state":"x","questions":{"":{"type":"noul","instructions":"q"}}}`,
			"non-empty"},
		{"empty model string",
			`{"model":"   ","state":"x","questions":{"a":{"type":"noul","instructions":"q"}}}`,
			"model"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate([]byte(c.body))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("message %q missing %q", err.Error(), c.want)
			}
			if _, ok := err.(*ValidateError); !ok {
				t.Fatalf("expected ValidateError, got %T", err)
			}
		})
	}
}

// TestValidateDeterministicQuestionOrder proves the validation error is stable
// when several questions are invalid: the response must not flicker between
// runs because of Go map iteration order (clients retry and diff messages).
func TestValidateDeterministicQuestionOrder(t *testing.T) {
	body := `{"model":"clef","state":"x","questions":{
		"zz":{"type":"bogus","instructions":"q"},
		"aa":{"type":"bogus","instructions":"q"},
		"mm":{"type":"bogus","instructions":"q"}}}`
	first := ""
	for i := 0; i < 20; i++ {
		err := Validate([]byte(body))
		if err == nil {
			t.Fatal("expected an error")
		}
		if first == "" {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("error is nondeterministic:\n  %s\n  %s", first, err.Error())
		}
	}
	if !strings.Contains(first, "questions.aa.type") {
		t.Fatalf("expected the lexicographically-first id to be reported, got %q", first)
	}
}

// TestBaseNameStripsQualifiers pins the model-id reduction used by the Cloudflare
// dialect (which addresses the model in the URL path).
func TestBaseNameStripsQualifiers(t *testing.T) {
	cases := map[string]string{
		"clef":                  "clef",
		"@cf/cloudflare/clef":   "clef",
		"cloudflare/clef-flash": "clef-flash",
		"  systemone/kev-4b  ":  "kev-4b",
		"":                      "",
		"trailing/":             "trailing/",
		"provider/sub/model":    "model",
	}
	for in, want := range cases {
		if got := BaseName(in); got != want {
			t.Errorf("BaseName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLookupNeverMatchesChatFamilies proves the registry does not claim real
// chat families.
func TestLookupNeverMatchesChatFamilies(t *testing.T) {
	for _, id := range []string{
		"gpt-4o", "claude-3-7-sonnet", "gemini-2.5-pro", "llama-3.1-8b",
		"systemone", "systemone/",
	} {
		if m, ok := Lookup(id); ok {
			t.Errorf("%q must not resolve to a decision model, got %+v", id, m)
		}
	}
}

// TestFamilyPrefixOverMatch pins the family-boundary rules.
//
// The family fallback used to match any id that merely STARTED WITH a decision
// family name, so "cleffa", "jevon", "laya-chat" and "clef-anything" were all
// classified as decision models. That is not cosmetic: a mis-classified id is
// refused on /v1/chat/completions with a pointer to /v1/systemone and then fails
// upstream, making a legitimate chat model uncallable. A family match must land
// on a real family boundary instead.
func TestFamilyPrefixOverMatch(t *testing.T) {
	// Merely sharing a leading substring with a family name is NOT membership.
	// "kevin" is the classic case: it is what a reseller calls an open-weight
	// Kev model, but it is also a human name, and "kev-" correctly requires the
	// separator.
	overMatches := []string{
		"cleffa", "clefnet", "clefguard", "jevon", "jevolution",
		"kevin", "kev2chat", "layer", "layaway", "openjevish",
	}
	for _, id := range overMatches {
		if IsDecisionModel(id) {
			t.Errorf("%q must NOT be classified as a decision model (no family boundary)", id)
		}
	}

	// Real family members must keep resolving, including the bare family name
	// and the separator forms a provider might actually publish.
	familyMembers := []string{
		"clef", "clef-flash", "clef-omni",
		"kev", "kev-5b", "kev-4b",
		"jev", "jev-latest",
		"openjev", "openjev-1",
		"laya", "laya-8b",
	}
	for _, id := range familyMembers {
		if !IsDecisionModel(id) {
			t.Errorf("%q must stay a decision model", id)
		}
	}
}

// TestIsDecisionModelWithLookup proves the catalog capability source is
// authoritative when it has a row, and that the curated registry remains the
// offline fallback when it does not.
//
// This is the P1-4 fix at the library level: /v1/models advertises the catalog
// row's decision flag, so the guards that accept or refuse a model must consult
// the same evidence. A row synced with decision=1 used to be rejected on
// /v1/systemone (the curated registry had never heard of it) while a curated
// model with no catalog row was refused on chat — the exact disagreement.
func TestIsDecisionModelWithLookup(t *testing.T) {
	fake := func(known map[string]bool) CapabilityLookup {
		return func(id string) Capability {
			d, ok := known[id]
			return Capability{Known: ok, Decision: d}
		}
	}

	cases := []struct {
		name   string
		id     string
		lookup CapabilityLookup
		want   bool
	}{
		{"nil lookup falls back to the registry", "clef", nil, true},
		{"nil lookup, unknown id", "acme-decide", nil, false},
		{"catalog row says decision", "acme-decide", fake(map[string]bool{"acme-decide": true}), true},
		{"catalog row says not decision, overriding the registry", "clef", fake(map[string]bool{"clef": false}), false},
		{"catalog row for the qualified id", "cloudflare/acme-decide", fake(map[string]bool{"cloudflare/acme-decide": true}), true},
		{"catalog row for the bare selector", "cloudflare/acme-decide", fake(map[string]bool{"acme-decide": true}), true},
		{"no catalog row for an unrelated id", "acme-chat", fake(map[string]bool{"acme-decide": true}), false},
		{"no catalog row, curated family fallback", "kev-5b", fake(map[string]bool{"acme-decide": true}), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDecisionModelWithLookup(c.id, c.lookup); got != c.want {
				t.Fatalf("IsDecisionModelWithLookup(%q) = %v, want %v", c.id, got, c.want)
			}
		})
	}
}
