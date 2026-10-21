package decision

import "strings"

// Model describes one known decision model. The catalog uses these entries to
// seed pricing/capability rows so decision models are listable and billable
// even when the models.dev snapshot has not caught up with a release.
type Model struct {
	// ID is the canonical short selector ("clef", "kev-4b", "jev-latest").
	ID string
	// Namespace is the catalog provider namespace used for the qualified
	// catalog id ("cloudflare/clef").
	Namespace string
	Name      string
	Family    string
	// ContextWindow in tokens (0 = unknown).
	ContextWindow int
	// InputCostPerM / OutputCostPerM in USD per 1M tokens. Decision models
	// bill input only; OutputCostPerM is 0 for every hosted decision model.
	InputCostPerM  float64
	OutputCostPerM float64
	// Modalities the model accepts beyond text.
	Vision bool
	Audio  bool
	Video  bool
	// Aliases are additional exact selectors that resolve to this entry.
	Aliases []string
}

// registry is the curated set of decision models known as of 2026-10.
//
// Pricing provenance:
//   - Cloudflare Clef family: Workers AI pricing page / changelog
//     (clef $0.240/M, clef-flash $0.038/M, clef-omni $0.150/M; input only).
//   - TypeSafe Jev: $0.042/M input, output free (TypeSafe launch post).
//   - Kev family: open weights, self-hosted (no per-token price).
var registry = []Model{
	// ---- Cloudflare Workers AI (@cf/cloudflare/*) ----
	{
		ID: "clef", Namespace: "cloudflare", Name: "Clef", Family: "clef",
		ContextWindow: 65536, InputCostPerM: 0.240, Vision: true,
	},
	{
		ID: "clef-flash", Namespace: "cloudflare", Name: "Clef Flash", Family: "clef",
		ContextWindow: 24576, InputCostPerM: 0.038, Vision: true,
	},
	{
		ID: "clef-omni", Namespace: "cloudflare", Name: "Clef Omni", Family: "clef",
		ContextWindow: 65536, InputCostPerM: 0.150, Vision: true, Audio: true, Video: true,
	},

	// ---- TypeSafe AI (hosted Jev) ----
	{
		ID: "jev-latest", Namespace: "typesafe", Name: "Jev", Family: "jev",
		ContextWindow: 32768, InputCostPerM: 0.042,
		Aliases: []string{"jev", "system-one"},
	},

	// ---- Open-weight Kev family (self-hosted) ----
	{
		ID: "kev-0.8b", Namespace: "kev", Name: "Kev 0.8B", Family: "kev",
		ContextWindow: 8192,
	},
	{
		ID: "kev-4b", Namespace: "kev", Name: "Kev 4B", Family: "kev",
		ContextWindow: 8192,
	},
	{
		ID: "kev-9b", Namespace: "kev", Name: "Kev 9B", Family: "kev",
		ContextWindow: 8192,
	},
	{
		ID: "kev-27b", Namespace: "kev", Name: "Kev 27B", Family: "kev",
		ContextWindow: 65536,
	},

	// ---- Other open decision models ----
	{
		ID: "laya", Namespace: "convai", Name: "Laya", Family: "laya",
		ContextWindow: 8192,
	},
	{
		ID: "openjev", Namespace: "open", Name: "OpenJev", Family: "openjev",
		ContextWindow: 32768, Vision: true,
	},
}

// familyPrefixes maps a distinctive model-id prefix to the registry entry that
// should back unknown versions of that family (e.g. "kev-5b" -> Kev 4B row).
//
// `prefix` must end at a real family boundary — a trailing hyphen, or the whole
// id. A bare family name is matched EXACTLY (see `boundary`), never as a raw
// substring prefix: the earlier rule was
//
//	base == strings.TrimSuffix(fp.prefix, "-") || strings.HasPrefix(base, fp.prefix)
//
// which claimed any id merely STARTING with the family name. That silently
// classified unrelated models as decision models — "cleffa", "clefnet" and
// "clef-guard" were all reported as Clef, as were "kevin", "jevolution" and
// "jevon" (via the "jev" prefix). The consequence is not cosmetic: a
// mis-classified id is REFUSED on /v1/chat/completions with a pointer to
// /v1/systemone and then fails upstream, so a legitimate chat model becomes
// uncallable. The open-weight Kev family is exactly the kind of thing a reseller
// names loosely ("kevin", "kev2"), which is how this surfaced.
//
// Matching rules, in order:
//  1. exact id or declared alias (handled before this table);
//  2. exact match on `boundary` (the family name, e.g. "clef" -> Clef);
//  3. `prefix` match when the boundary char is followed by a separator: "-"
//     ("clef-flash", "kev-5b"), a digit ("clef2", "kev4b"), or end-of-id.
//
// Anything else is NOT this family.
var familyPrefixes = []struct {
	// prefix is the literal text a family id starts with, INCLUDING the
	// trailing hyphen where the family always uses one ("kev-").
	prefix string
	// boundary is the exact family name; an id equal to it is a match.
	boundary string
	model    Model
}{
	{"kev-", "kev", Model{Namespace: "kev", Family: "kev", ContextWindow: 8192}},
	{"clef-", "clef", Model{Namespace: "cloudflare", Family: "clef", ContextWindow: 65536, Vision: true}},
	{"jev-", "jev", Model{Namespace: "typesafe", Family: "jev", ContextWindow: 32768}},
	{"openjev-", "openjev", Model{Namespace: "open", Family: "openjev", ContextWindow: 32768, Vision: true}},
	{"laya-", "laya", Model{Namespace: "convai", Family: "laya", ContextWindow: 8192}},
}

// matchesFamily reports whether base names a member of the family described by
// prefix/boundary, using the boundary rules documented on familyPrefixes.
func matchesFamily(base, prefix, boundary string) bool {
	if base == boundary {
		return true
	}
	if !strings.HasPrefix(base, prefix) {
		return false
	}
	// The character right after the prefix decides: a hyphen-terminated prefix
	// ("clef-") already ends at a separator; a family whose canonical ids carry
	// no hyphen ("clef2", "kev4b") must be followed by a digit.
	if strings.HasSuffix(prefix, "-") {
		return true
	}
	if len(base) == len(prefix) {
		return true
	}
	next := base[len(prefix)]
	return next >= '0' && next <= '9'
}

// All returns the curated registry (copy) for catalog seeding.
func All() []Model {
	out := make([]Model, len(registry))
	copy(out, registry)
	return out
}

// BaseName reduces any provider-qualified model id to its bare selector:
//
//	"@cf/cloudflare/clef-flash" -> "clef-flash"
//	"cloudflare/clef"           -> "clef"
//	"clef"                      -> "clef"
func BaseName(id string) string {
	s := strings.TrimSpace(id)
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, "/"); i >= 0 && i+1 < len(s) {
		s = s[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// Lookup resolves a model id (bare, namespaced, or @cf/... qualified) to a
// registry entry. Unknown versions of a known family resolve to the family's
// representative entry with the requested id substituted, so the model is
// still recognized as a decision model.
func Lookup(id string) (Model, bool) {
	base := BaseName(id)
	if base == "" {
		return Model{}, false
	}
	for _, m := range registry {
		if base == m.ID {
			return m, true
		}
		for _, a := range m.Aliases {
			if base == a {
				return m, true
			}
		}
	}
	for _, fp := range familyPrefixes {
		if matchesFamily(base, fp.prefix, fp.boundary) {
			m := fp.model
			m.ID = base
			return m, true
		}
	}
	return Model{}, false
}

// IsDecisionModel reports whether an id names a decision model. It is used to
// tag provider models and to reject chat-shaped traffic against decision
// endpoints (and vice versa).
//
// It knows only the curated registry. A model whose decision capability comes
// from the synced catalog ("decision": true on /v1/models, a models.dev row
// with output modality "decision", or an operator-added row) is invisible to
// it — use IsDecisionModelWithLookup at any call site that has a catalog store
// in reach, otherwise the listing and the endpoint guards disagree.
func IsDecisionModel(id string) bool {
	_, ok := Lookup(id)
	return ok
}

// Capability is the subset of an external capability source (the catalog) that
// this package needs. Known=false means "no row": the curated registry is then
// the only evidence available.
type Capability struct {
	Known    bool
	Decision bool
}

// CapabilityLookup resolves a model id against an external capability source
// (in practice *catalog.Store). Returning Known=false is always safe: the
// curated registry fallback still applies.
type CapabilityLookup func(id string) Capability

// IsDecisionModelWithLookup is IsDecisionModel plus a catalog capability
// source. The catalog is authoritative when it has a row for the id (it is
// where operator edits and models.dev's decision modality land); the curated
// registry remains the offline fallback for unknown ids.
//
// This exists because /v1/models advertises every catalog row's decision flag,
// so a row synced with decision=1 must be accepted on /v1/systemone and refused
// on the text endpoints — the two must never disagree about the same id.
func IsDecisionModelWithLookup(id string, lookup CapabilityLookup) bool {
	if lookup != nil {
		if cap, ok := lookupCapability(lookup, id); ok {
			return cap
		}
	}
	return IsDecisionModel(id)
}

// lookupCapability asks the lookup for the id (and, when it has no row, for the
// bare selector — "cloudflare/acme-decide" and "acme-decide" name the same
// model). ok=false means "the source has no opinion".
func lookupCapability(lookup CapabilityLookup, id string) (decision bool, ok bool) {
	c := lookup(id)
	if !c.Known {
		if base := BaseName(id); base != "" && base != id {
			c = lookup(base)
		}
	}
	if !c.Known {
		return false, false
	}
	return c.Decision, true
}

// CatalogID returns the qualified catalog id for a registry entry.
func (m Model) CatalogID() string {
	if m.Namespace == "" {
		return m.ID
	}
	return m.Namespace + "/" + m.ID
}

// Description is a one-line summary used to seed the catalog.
func (m Model) Description() string {
	if m.Family == "kev" {
		return "Open-weight decision (System One) model — self-hostable, returns typed probabilities."
	}
	if m.Family == "clef" {
		return "Cloudflare decision model — returns typed answers with calibrated probabilities."
	}
	if m.Family == "jev" {
		return "TypeSafe System One decision model — typed probabilistic decisions, no text generation."
	}
	return "Decision (System One) model — returns typed answers with calibrated probabilities."
}

// ModalitiesJSON renders the modalities column for the catalog row.
func (m Model) ModalitiesJSON() string {
	in := []string{"text"}
	if m.Vision {
		in = append(in, "image")
	}
	if m.Audio {
		in = append(in, "audio")
	}
	if m.Video {
		in = append(in, "video")
	}
	// Output modality "decision" is what marks the row as a decision model in
	// listings that key off modalities.
	return `{"input":["` + strings.Join(in, `","`) + `"],"output":["decision"]}`
}
