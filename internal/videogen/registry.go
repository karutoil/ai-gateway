package videogen

import (
	"encoding/json"
	"strings"
)

// Model describes one known video-generation model. The catalog uses these
// entries to seed listable, priced rows even when the models.dev snapshot has
// not caught up with a release, and the proxy uses them to pick the upstream
// dialect and bill per generated second.
type Model struct {
	// ID is the canonical upstream model id ("sora-2",
	// "veo-3.1-generate-preview", "grok-imagine-video").
	ID string
	// Namespace is the catalog provider namespace used for the qualified
	// catalog id ("openai/sora-2").
	Namespace string
	Name      string
	Family    string
	// Aliases are additional exact selectors that resolve to this entry.
	Aliases []string
	// Dialect is the upstream wire protocol this model speaks.
	Dialect Dialect
	// CostPerSecond is USD per second of generated video (0 = unpriced).
	// Video is billed by duration, which is why this is per-second rather than
	// a flat per-clip price.
	CostPerSecond float64
	// SupportedSeconds / SupportedSizes are the values the model documents.
	// Empty means "the OpenAI defaults".
	SupportedSeconds []string
	SupportedSizes   []string
	// SupportsImageInput marks image-to-video (a reference frame).
	SupportsImageInput bool
	// TextOutput marks models that also emit text. No curated video model does
	// today — every one outputs video only — so the field exists to mirror the
	// imagegen capability model and to keep the text-endpoint guard correct if
	// a hybrid model appears.
	TextOutput bool
}

// registry is the curated set of video-generation models known as of 2026-10.
//
// Pricing provenance (representative, per second of output):
//   - OpenAI Sora 2: sora-2 ~$0.10/s, sora-2-pro ~$0.30/s.
//   - Google Veo: Veo 3.1 ~$0.40/s, Veo 3.1 Fast ~$0.15/s, Veo 3.1 Lite
//     ~$0.05/s, Veo 3 ~$0.40/s, Veo 2 ~$0.35/s.
//   - xAI Grok Imagine video: from $0.05/s.
var registry = []Model{
	// ---- OpenAI Sora ----
	{
		ID: "sora-2", Namespace: "openai", Name: "Sora 2", Family: "sora",
		Dialect: DialectOpenAI, CostPerSecond: 0.10,
		SupportedSeconds:   []string{"4", "8", "12"},
		SupportedSizes:     []string{"720x1280", "1280x720", "1024x1792", "1792x1024"},
		SupportsImageInput: true,
	},
	{
		ID: "sora-2-pro", Namespace: "openai", Name: "Sora 2 Pro", Family: "sora",
		Dialect: DialectOpenAI, CostPerSecond: 0.30,
		SupportedSeconds:   []string{"4", "8", "12"},
		SupportedSizes:     []string{"720x1280", "1280x720", "1024x1792", "1792x1024"},
		SupportsImageInput: true,
	},

	// ---- Google Veo ----
	{
		ID: "veo-3.1-generate-preview", Namespace: "google", Name: "Veo 3.1", Family: "veo",
		Aliases: []string{"veo-3.1", "veo-3.1-generate"},
		Dialect: DialectVeo, CostPerSecond: 0.40,
		SupportedSeconds: []string{"4", "8"}, SupportsImageInput: true,
	},
	{
		ID: "veo-3.1-fast-generate-preview", Namespace: "google", Name: "Veo 3.1 Fast", Family: "veo",
		Aliases: []string{"veo-3.1-fast"},
		Dialect: DialectVeo, CostPerSecond: 0.15,
		SupportedSeconds: []string{"4", "8"}, SupportsImageInput: true,
	},
	{
		ID: "veo-3.1-lite-generate-preview", Namespace: "google", Name: "Veo 3.1 Lite", Family: "veo",
		Aliases: []string{"veo-3.1-lite"},
		Dialect: DialectVeo, CostPerSecond: 0.05,
		SupportedSeconds: []string{"4", "8"}, SupportsImageInput: true,
	},
	{
		ID: "veo-3.0-generate-preview", Namespace: "google", Name: "Veo 3", Family: "veo",
		Aliases: []string{"veo-3.0", "veo-3"},
		Dialect: DialectVeo, CostPerSecond: 0.40,
		SupportedSeconds: []string{"4", "8"}, SupportsImageInput: true,
	},
	{
		ID: "veo-2.0-generate-001", Namespace: "google", Name: "Veo 2", Family: "veo",
		Aliases: []string{"veo-2.0", "veo-2"},
		Dialect: DialectVeo, CostPerSecond: 0.35,
		SupportedSeconds: []string{"4", "8"}, SupportsImageInput: true,
	},

	// ---- xAI Grok Imagine ----
	{
		ID: "grok-imagine-video", Namespace: "xai", Name: "Grok Imagine Video", Family: "grok-video",
		Dialect: DialectXAI, CostPerSecond: 0.05,
		SupportedSeconds: []string{"4", "8", "12"}, SupportsImageInput: true,
	},
	{
		ID: "grok-imagine-video-1.5", Namespace: "xai", Name: "Grok Imagine Video 1.5", Family: "grok-video",
		Dialect: DialectXAI, CostPerSecond: 0.07,
		SupportedSeconds: []string{"4", "8", "12"}, SupportsImageInput: true,
	},
	{
		ID: "grok-imagine-video-1.5-lite", Namespace: "xai", Name: "Grok Imagine Video 1.5 Lite", Family: "grok-video",
		Dialect: DialectXAI, CostPerSecond: 0.05,
		SupportedSeconds: []string{"4", "8", "12"}, SupportsImageInput: true,
	},
}

// familyRule maps a distinctive model-id prefix to the representative registry
// entry backing unknown versions of that family (e.g. "sora-3" -> Sora 2 row).
type familyRule struct {
	match func(string) bool
	base  Model
}

var familyRules = []familyRule{
	{match: func(s string) bool { return strings.HasPrefix(s, "sora-") }, base: Model{Namespace: "openai", Family: "sora", Dialect: DialectOpenAI, CostPerSecond: 0.10, SupportsImageInput: true}},
	{match: func(s string) bool { return strings.HasPrefix(s, "veo-") }, base: Model{Namespace: "google", Family: "veo", Dialect: DialectVeo, CostPerSecond: 0.40, SupportsImageInput: true}},
	{match: func(s string) bool { return strings.HasPrefix(s, "grok-imagine-video") }, base: Model{Namespace: "xai", Family: "grok-video", Dialect: DialectXAI, CostPerSecond: 0.05, SupportsImageInput: true}},
}

// All returns the curated registry (copy) for catalog seeding.
func All() []Model {
	out := make([]Model, len(registry))
	copy(out, registry)
	return out
}

// ForNamespace returns the curated registry entries for one provider
// namespace ("openai", "google", "xai").
func ForNamespace(ns string) []Model {
	ns = strings.ToLower(strings.TrimSpace(ns))
	var out []Model
	for _, m := range registry {
		if m.Namespace == ns {
			out = append(out, m)
		}
	}
	return out
}

// Lookup resolves a model id (bare, namespaced, or vendor-qualified) to a
// registry entry. Unknown versions of a known family resolve to the family's
// representative entry with the requested id substituted, so the model is
// still recognized as a video model and routed with a sensible default
// dialect/price.
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
	for _, rule := range familyRules {
		if rule.match(base) {
			m := rule.base
			m.ID = base
			return m, true
		}
	}
	return Model{}, false
}

// IsVideoModel reports whether an id names a video-generation model. It is used
// to tag provider models, to open the videos endpoint, and to keep video
// traffic off the text endpoints (and vice versa).
func IsVideoModel(id string) bool {
	_, ok := Lookup(id)
	return ok
}

// IsVideoOnlyModel reports whether a curated id generates only video (no text
// output). Every curated video model is video-only today; the helper mirrors
// imagegen.IsImageOnlyModel so the text-endpoint guard reads identically.
func IsVideoOnlyModel(id string) bool {
	m, ok := Lookup(id)
	return ok && !m.TextOutput
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
	switch m.Family {
	case "sora":
		return "OpenAI Sora video model — text-to-video and image-to-video via the async Videos API."
	case "veo":
		return "Google Veo video model — high-fidelity text-to-video and image-to-video."
	case "grok-video":
		return "xAI Grok Imagine video model — text-to-video and image-to-video with native audio."
	}
	return "Video-generation model served through the standardized /v1/videos endpoint."
}

// ModalitiesJSON renders the modalities column for the catalog row. Video
// models accept text (and usually images) and output video only.
func (m Model) ModalitiesJSON() string {
	in := []string{"text"}
	if m.SupportsImageInput {
		in = append(in, "image")
	}
	out := []string{"video"}
	if m.TextOutput {
		out = append([]string{"text"}, out...)
	}
	return `{"input":["` + strings.Join(in, `","`) + `"],"output":["` + strings.Join(out, `","`) + `"]}`
}

// OutputModalities parses the `output` list from a models.dev modalities blob.
func OutputModalities(modalities string) []string {
	if strings.TrimSpace(modalities) == "" {
		return nil
	}
	var mm struct {
		Output []string `json:"output"`
	}
	if json.Unmarshal([]byte(modalities), &mm) != nil {
		return nil
	}
	return mm.Output
}

// VideoModality reports whether a modalities JSON blob declares the "video"
// output modality. This is THE models.dev marker for a video-generation model
// (models.dev's `type` field only carries embedding/reranking/video, and it is
// unset for most video rows, so `modalities.output` is the reliable signal).
func VideoModality(modalities string) bool {
	for _, o := range OutputModalities(modalities) {
		if o == "video" {
			return true
		}
	}
	return false
}

// VideoOnlyFromModalities reports whether a modalities blob describes a model
// that can ONLY produce video (video output, no text output). Mirrors
// imagegen.ImageOnlyFromModalities.
func VideoOnlyFromModalities(modalities string) bool {
	outs := OutputModalities(modalities)
	if len(outs) == 0 {
		return false
	}
	video, text := false, false
	for _, o := range outs {
		switch o {
		case "video":
			video = true
		case "text":
			text = true
		}
	}
	return video && !text
}

// CostForSeconds returns the billed USD for a clip of the given duration.
func (m Model) CostForSeconds(seconds string) float64 {
	secs := 0
	for _, c := range strings.TrimSpace(seconds) {
		if c < '0' || c > '9' {
			return 0
		}
		secs = secs*10 + int(c-'0')
	}
	if secs <= 0 {
		return 0
	}
	return m.CostPerSecond * float64(secs)
}

// CostForModelSeconds returns the curated USD price for a model and duration.
func CostForModelSeconds(model, seconds string) (float64, bool) {
	m, ok := Lookup(model)
	if !ok || m.CostPerSecond <= 0 {
		return 0, false
	}
	cost := m.CostForSeconds(seconds)
	if cost <= 0 {
		return 0, false
	}
	return cost, true
}
