package devin

import (
	"sort"
	"strings"
)

// Reasoning-level handling for Devin model configs.
//
// Devin advertises one wire config per (model, reasoning level) as
// "<model>-<level>" (e.g. "swe-2-max"). Storing every variant as its own
// provider_models row breaks enrichment: no catalog entry can match
// "swe-2-max", so context/pricing/reasoning metadata stays empty while the
// row claims source "enriched". Instead discovery collapses variants to a
// single base row carrying the observed levels, and the proxy routes an
// explicit reasoning_effort back to the suffixed wire id (mirroring the
// Antigravity collapseRuntime/ResolveRuntime pattern). When the base id
// itself was never observed on the wire, the collapsed row also carries a
// catch-all route so no-effort requests never emit a bare id the backend
// rejects.

// canonicalLevelRank orders observed levels low→high for stable storage.
var canonicalLevelRank = map[string]int{
	"extra-low": 0,
	"minimal":   1,
	"low":       2,
	"medium":    3,
	"high":      4,
	"xhigh":     5,
	"max":       6,
}

// DefaultReasoningLevels is the level set for base models with no observed
// suffixed variants (static fallback seeds). Bare ids are sent when the
// client requests no effort, so this only matters for explicit effort
// requests — generous, matching the previous "reasoning=true, no gating"
// posture.
func DefaultReasoningLevels() []string {
	return []string{"minimal", "low", "medium", "high", "xhigh", "max"}
}

func normalizeLevelToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	switch s {
	case "x-high", "xhigh":
		return "xhigh"
	case "extra-low", "extralow", "extra_low":
		return "extra-low"
	}
	return s
}

func isLevelToken(s string) bool {
	_, ok := canonicalLevelRank[normalizeLevelToken(s)]
	return ok
}

// SplitReasoningSuffix splits a "<model>-<level>" wire id into its base and
// canonical level. It reports ok=false for bare ids and for trailing
// segments outside the reasoning vocabulary ("swe-1-7" keeps its "7").
func SplitReasoningSuffix(id string) (base, level string, ok bool) {
	t := strings.TrimSpace(id)
	if t == "" {
		return "", "", false
	}
	if tail := "-extra-low"; len(t) > len(tail) && strings.EqualFold(t[len(t)-len(tail):], tail) {
		if base := strings.TrimSpace(t[:len(t)-len(tail)]); base != "" {
			return base, "extra-low", true
		}
	}
	if tail := "-x-high"; len(t) > len(tail) && strings.EqualFold(t[len(t)-len(tail):], tail) {
		if base := strings.TrimSpace(t[:len(t)-len(tail)]); base != "" {
			return base, "xhigh", true
		}
	}
	i := strings.LastIndex(t, "-")
	if i <= 0 || i+1 >= len(t) {
		return t, "", false
	}
	if tok := normalizeLevelToken(t[i+1:]); isLevelToken(tok) {
		if base := strings.TrimSpace(t[:i]); base != "" {
			return base, tok, true
		}
	}
	return t, "", false
}

// CollapseReasoningVariant maps a possibly-suffixed wire id to its base
// ("swe-2-max" -> "swe-2"); bare ids pass through unchanged.
func CollapseReasoningVariant(id string) string {
	base, _, ok := SplitReasoningSuffix(id)
	if !ok {
		return strings.TrimSpace(id)
	}
	return base
}

// ResolveRuntime maps a client-facing model id + reasoning effort to the
// wire id. With no effort an explicit variant is honored verbatim and a
// bare id stays bare; an explicit off/none strips to the base (or its
// declared off route); a known effort level routes to the server-declared
// uid when routing carries it, else to "<base>-<level>" under a nil routing
// (legacy suffix convention). A non-nil but empty routing means the server
// declared no routable levels: the id passes through verbatim rather than
// inventing a suffixed uid. Unknown effort tokens leave the id untouched.
//
// The catch-all route (defaultRouteKey) covers families whose base id was
// never observed on the wire: no-effort and unrouted-effort requests fall
// back to it instead of sending a bare id the backend rejects.
func ResolveRuntime(model, effort string, routing map[string]string) string {
	m := strings.TrimSpace(StripPrefix(model))
	if m == "" {
		return m
	}
	base := CollapseReasoningVariant(m)
	_, _, suffixed := SplitReasoningSuffix(m)
	defaultUID := func() string {
		if routing == nil {
			return ""
		}
		return strings.TrimSpace(routing[defaultRouteKey])
	}
	switch e := strings.ToLower(strings.TrimSpace(effort)); e {
	case "":
		if suffixed {
			return m
		}
		if uid := defaultUID(); uid != "" {
			return uid
		}
		return m
	case "off", "none", "disabled":
		if routing != nil {
			if uid, ok := routing["off"]; ok && strings.TrimSpace(uid) != "" {
				return uid
			}
			// Stripping to the base only helps when the base itself is a
			// wire id; the catch-all member is the safe target otherwise.
			if uid := defaultUID(); uid != "" {
				return uid
			}
		}
		return base
	default:
		tok := normalizeLevelToken(e)
		if !isLevelToken(tok) {
			return m
		}
		if routing != nil {
			if uid, ok := routing[tok]; ok && strings.TrimSpace(uid) != "" {
				return uid
			}
			if !suffixed {
				if uid := defaultUID(); uid != "" {
					return uid
				}
			}
			return m
		}
		return base + "-" + tok
	}
}

// defaultRouteKey names the catch-all entry in a collapsed group's routing
// map: the wire uid sent when the client asks for no effort, or for an
// effort/off the family does not route, on a base id that was never observed
// as a wire uid itself (e.g. swe-2 -> swe-2-medium/swe-2-high/swe-2-max; the
// backend rejects the bare id). Groups with a live bare member never carry
// this key — bare ids stay verbatim for them.
const defaultRouteKey = "default"

// pickDefaultUID selects the catch-all wire uid for a suffix-collapsed group
// with no server-declared default member and no bare member: the lightest
// observed tier (levels are rank-sorted ascending), matching the
// least-surprise posture of the Antigravity tables, where unspecified effort
// lands on the model's cheapest runtime.
func pickDefaultUID(base string, levels []string) string {
	if len(levels) == 0 {
		return ""
	}
	return base + "-" + levels[0]
}

// CollapsedModel is one base model with the union of its observed reasoning
// levels, ready for a single enriched provider_models row.
type CollapsedModel struct {
	ID            string
	Name          string
	ContextWindow int
	MaxTokens     int
	ImageInput    bool
	Reasoning     bool
	// ReasoningType is "effort" for reasoning lanes, "none" otherwise.
	ReasoningType string
	Levels        []string
	// Routing maps effort (and "off") to wire uids when the server declares
	// it (family lanes) or variants were observed on the wire (suffix map).
	// Nil selects the legacy "<base>-<level>" suffix convention; a non-nil
	// but empty map means no routable levels — pass ids through verbatim.
	// Suffix-collapsed groups whose base id was never observed as a wire uid
	// carry a catch-all under defaultRouteKey.
	Routing map[string]string
	// Costs are per-million-token rates parsed from cost dimensions.
	InputCost, OutputCost, CacheReadCost float64
	ToolCalls                            bool
}

// familyEffortValue maps a normalized family effort name to its canonical
// level ("off" for disabled tiers).
func familyEffortValue(norm string) (string, bool) {
	switch norm {
	case "none", "nothinking":
		return "off", true
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return norm, true
	}
	return "", false
}

// normalizeFamilyKey collapses punctuation to single spaces ("Reasoning
// Effort" -> "reasoning effort").
func normalizeFamilyKey(s string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// normalizeEffortName drops punctuation entirely ("X High" -> "xhigh").
func normalizeEffortName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizeLaneID maps a family label to a gateway id ("GPT-5.6 Sol" ->
// "gpt-5-6-sol").
func normalizeLaneID(label string) string {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(label) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(strings.TrimSpace(b.String()), "-")
}

// laneAccum builds one server-declared family lane.
type laneAccum struct {
	id, name string
	members  []DiscoveredModel
	uids     []string
	routing  map[string]string
	defUID   string
	hasDef   bool
}

// collectLane files a config under its family lane. It reports whether the
// config was consumed (false when it carries no usable family lane).
func collectLane(lanes map[string]*laneAccum, order *[]string, m DiscoveredModel, uid string) bool {
	if m.Family == nil {
		return false
	}
	label := strings.TrimSpace(m.Family.Label)
	if label == "" {
		return false
	}
	var effort string
	var hasEffort bool
	var thinking *bool
	fast, oneM := false, false
	for _, e := range m.Family.Entries {
		if e.Key == "" {
			continue
		}
		key := normalizeFamilyKey(e.Key)
		switch {
		case key == "fast mode":
			fast = e.Order == 1
		case key == "thinking":
			t := e.Order == 1
			thinking = &t
		case key == "1m context":
			oneM = e.Order == 1
		case key == "effort" || key == "reasoning effort":
			if lv, ok := familyEffortValue(normalizeEffortName(e.Name)); ok {
				effort, hasEffort = lv, true
			}
		}
	}
	// Paired non-thinking configs share the thinking twin's effort label;
	// the explicit Thinking axis decides whether the route is off.
	if thinking != nil && !*thinking {
		effort, hasEffort = "off", true
	}
	baseID := normalizeLaneID(label)
	if baseID == "" {
		return false
	}
	laneID := baseID
	laneName := label
	if oneM {
		laneID += "-1m"
		laneName += " 1M"
	}
	if fast {
		laneID += "-fast"
		laneName += " Fast"
	}
	lane, ok := lanes[laneID]
	if !ok {
		lane = &laneAccum{id: laneID, name: laneName, routing: map[string]string{}}
		lanes[laneID] = lane
		*order = append(*order, laneID)
	}
	lane.members = append(lane.members, m)
	lane.uids = append(lane.uids, uid)
	if !lane.hasDef && (m.DefaultInFamily || m.Family.IsDefault) {
		lane.defUID, lane.hasDef = uid, true
	}
	if hasEffort {
		if _, claimed := lane.routing[effort]; !claimed {
			lane.routing[effort] = uid
		}
	}
	return true
}

// laneLevels returns the lane's routable effort levels in canonical order
// (excluding the "off" and catch-all routes, which are not reasoning levels).
func laneLevels(routing map[string]string) []string {
	var out []string
	for lv := range routing {
		if lv == "off" || lv == defaultRouteKey {
			continue
		}
		out = append(out, lv)
	}
	sort.Slice(out, func(i, j int) bool { return canonicalLevelRank[out[i]] < canonicalLevelRank[out[j]] })
	return out
}

func fallbackName(base string) string {
	for _, m := range PublicModels {
		if m.ID == base {
			return m.Name
		}
	}
	return ""
}

// CollapseModels groups raw discovered configs into base rows. Configs with
// server-declared family lanes collapse by lane (explicit effort→uid
// routing); the rest collapse by "<base>-<level>" suffix (routing maps are
// constructed from the observed variants). Display names prefer the live
// bare member, then the static seed, then the upper-cased base. Groups with
// no observed suffix fall back to DefaultReasoningLevels with an empty
// (verbatim-passthrough) routing map.
func CollapseModels(raw []DiscoveredModel) []CollapsedModel {
	lanes := map[string]*laneAccum{}
	var laneOrder []string
	var standalone []DiscoveredModel
	for _, m := range raw {
		if !collectLane(lanes, &laneOrder, m, m.ID) {
			standalone = append(standalone, m)
		}
	}
	out := make([]CollapsedModel, 0, len(lanes)+len(standalone))
	laneByID := map[string]*laneAccum{}
	for _, id := range laneOrder {
		lane := lanes[id]
		// A lane with no non-"off" effort route has nothing to route: its
		// members fall back to standalone handling.
		if len(laneLevels(lane.routing)) == 0 {
			standalone = append(standalone, lane.members...)
			continue
		}
		laneByID[id] = lane
		out = append(out, laneModel(lane))
	}
	type accum struct {
		rep      DiscoveredModel
		hasRep   bool
		hasBare  bool
		defUID   string
		bareName string
		ctx, max int
		image    bool
		reason   bool
		tools    bool
		levels   map[string]bool
	}
	groups := map[string]*accum{}
	absorb := func(key string, m DiscoveredModel, suffixed bool, lvl string) {
		// A standalone config whose base matches a family lane folds into
		// the lane (lane routes win; suffix levels fill unclaimed gaps).
		if lane, ok := laneByID[key]; ok {
			if suffixed {
				if _, claimed := lane.routing[lvl]; !claimed {
					lane.routing[lvl] = m.ID
				}
			}
			for i := range out {
				if out[i].ID != key {
					continue
				}
				if m.ContextWindow > out[i].ContextWindow {
					out[i].ContextWindow = m.ContextWindow
				}
				out[i].ImageInput = out[i].ImageInput || m.ImageInput
				out[i].Reasoning = out[i].Reasoning || m.Reasoning
				out[i].ToolCalls = out[i].ToolCalls || m.ToolCalls
				out[i].Levels = laneLevels(lane.routing)
				if out[i].Routing == nil {
					out[i].Routing = map[string]string{}
				}
				for lv, uid := range lane.routing {
					out[i].Routing[lv] = uid
				}
				break
			}
			return
		}
		a, ok := groups[key]
		if !ok {
			a = &accum{levels: map[string]bool{}}
			groups[key] = a
		}
		if !suffixed {
			a.rep, a.hasRep, a.bareName = m, true, m.Name
			a.hasBare = true
		} else if !a.hasRep {
			a.rep, a.hasRep = m, true
		}
		// A suffixed member the server flagged as its family default is the
		// best catch-all when the bare base never shows up on the wire.
		if suffixed && m.DefaultInFamily && a.defUID == "" {
			a.defUID = m.ID
		}
		if m.ContextWindow > a.ctx {
			a.ctx = m.ContextWindow
		}
		if m.MaxTokens > a.max {
			a.max = m.MaxTokens
		}
		a.image = a.image || m.ImageInput
		a.reason = a.reason || m.Reasoning
		a.tools = a.tools || m.ToolCalls
		if suffixed {
			a.levels[lvl] = true
		}
	}
	for _, m := range standalone {
		base, lvl, suffixed := SplitReasoningSuffix(m.ID)
		key := m.ID
		if suffixed {
			key = base
		}
		absorb(key, m, suffixed, lvl)
	}
	for base, a := range groups {
		name := a.bareName
		if name == "" {
			name = fallbackName(base)
		}
		if name == "" && a.hasRep {
			name = a.rep.Name
		}
		if name == "" {
			name = strings.ToUpper(base)
		}
		var levels []string
		for lv := range a.levels {
			levels = append(levels, lv)
		}
		sort.Slice(levels, func(i, j int) bool {
			return canonicalLevelRank[levels[i]] < canonicalLevelRank[levels[j]]
		})
		var routing map[string]string
		if len(levels) == 0 {
			levels = DefaultReasoningLevels()
			routing = map[string]string{}
		} else {
			routing = suffixRouting(base, levels)
			// The base id never showed up on the wire (suffix-only family,
			// e.g. swe-2): requests with no effort, or an effort this group
			// cannot route, must land on a real member instead of a bare id
			// the backend rejects. Prefer the server-declared default, else
			// the lightest observed tier.
			if !a.hasBare {
				uid := a.defUID
				if uid == "" {
					uid = pickDefaultUID(base, levels)
				}
				if uid != "" {
					routing[defaultRouteKey] = uid
				}
			}
		}
		ctx, max := a.ctx, a.max
		if ctx <= 0 {
			ctx = defaultContextWindow
		}
		if max <= 0 {
			max = defaultMaxTokens
		}
		rType := "none"
		if a.reason {
			rType = "effort"
		}
		out = append(out, CollapsedModel{
			ID: base, Name: name,
			ContextWindow: ctx, MaxTokens: max,
			ImageInput: a.image, Reasoning: a.reason, ReasoningType: rType,
			Levels: levels, Routing: routing,
			InputCost: a.rep.InputCost, OutputCost: a.rep.OutputCost, CacheReadCost: a.rep.CacheReadCost,
			ToolCalls: a.tools,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// suffixRouting builds an explicit effort→uid map for observed
// "<base>-<level>" variants.
func suffixRouting(base string, levels []string) map[string]string {
	routing := make(map[string]string, len(levels))
	for _, lv := range levels {
		routing[lv] = base + "-" + lv
	}
	return routing
}

// laneModel renders one qualified family lane as a collapsed model. Scalars
// come from the server's default member (else the first member); the context
// window takes the lane max.
func laneModel(lane *laneAccum) CollapsedModel {
	rep := lane.members[0]
	if lane.hasDef {
		for _, m := range lane.members {
			if m.ID == lane.defUID {
				rep = m
				break
			}
		}
	}
	ctx := rep.ContextWindow
	image, reason, tools := false, false, false
	for _, m := range lane.members {
		if m.ContextWindow > ctx {
			ctx = m.ContextWindow
		}
		image = image || m.ImageInput
		reason = reason || m.Reasoning
		tools = tools || m.ToolCalls
	}
	if ctx <= 0 {
		ctx = defaultContextWindow
	}
	maxOut := rep.MaxTokens
	if maxOut <= 0 {
		maxOut = defaultMaxTokens
	}
	routing := make(map[string]string, len(lane.routing))
	for k, v := range lane.routing {
		routing[k] = v
	}
	return CollapsedModel{
		ID: lane.id, Name: lane.name,
		ContextWindow: ctx, MaxTokens: maxOut,
		ImageInput: image, Reasoning: reason, ReasoningType: "effort",
		Levels: laneLevels(lane.routing), Routing: routing,
		InputCost: rep.InputCost, OutputCost: rep.OutputCost, CacheReadCost: rep.CacheReadCost,
		ToolCalls: tools,
	}
}
