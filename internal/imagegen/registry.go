package imagegen

import (
	"encoding/json"
	"strings"
)

// Model describes one known image-generation model. The catalog uses these
// entries to seed listable, priced rows even when the models.dev snapshot has
// not caught up with a release, and the proxy uses them to pick the upstream
// dialect and bill per generated image.
type Model struct {
	// ID is the canonical upstream model id ("gpt-image-1",
	// "gemini-2.5-flash-image", "grok-2-image").
	ID string
	// Namespace is the catalog provider namespace used for the qualified
	// catalog id ("openai/gpt-image-1", "google/gemini-2.5-flash-image").
	Namespace string
	Name      string
	Family    string
	// Aliases are additional exact selectors that resolve to this entry.
	Aliases []string
	// Dialect is the upstream wire protocol this model speaks.
	Dialect Dialect
	// CostPerImage is USD per generated image (0 = token-priced only).
	// Curated from the provider pricing pages as of 2026-10; representative
	// of the default quality/size tier.
	CostPerImage float64
	// InputCostPerM / OutputCostPerM are USD per 1M tokens, when the provider
	// prices image models per token instead of (or in addition to) per image.
	InputCostPerM  float64
	OutputCostPerM float64
	// MaxN is the largest accepted `n` (0 = protocol default).
	MaxN int
	// SupportsEdit marks models that accept reference images (edits /
	// image-to-image).
	SupportsEdit bool
	// TextOutput marks models that also emit text alongside the image (Gemini
	// image models narrate; Imagen/DALL·E/Grok do not). It is what separates an
	// IMAGE-ONLY model (barred from the text endpoints) from an IMAGE-CAPABLE
	// one (also a legitimate chat model). Mirrors models.dev's
	// `modalities.output` containing "text".
	TextOutput bool
	// AspectRatios / Sizes are the values the model documents.
	AspectRatios []string
	Sizes        []string
}

// registry is the curated set of image-generation models known as of 2026-10.
//
// Pricing provenance:
//   - OpenAI: gpt-image-1 / gpt-image-1-mini / dall-e-3 / dall-e-2 pricing pages
//     (representative default tier).
//   - Google: Gemini 2.5 Flash Image ~$0.039/image (1290 output tokens at
//     $30/M), Gemini 3 Pro Image ~$0.134/image (1K/2K), Imagen 4 family.
//   - xAI: Grok Imagine image generation pricing (~$0.07/image, low tier).
var registry = []Model{
	// ---- OpenAI ----
	{
		ID: "gpt-image-1", Namespace: "openai", Name: "GPT Image 1", Family: "gpt-image",
		Dialect: DialectOpenAI, CostPerImage: 0.042, SupportsEdit: true,
		Sizes:        []string{"1024x1024", "1536x1024", "1024x1536", "auto"},
		AspectRatios: []string{"1:1", "3:2", "2:3"},
	},
	{
		ID: "gpt-image-1-mini", Namespace: "openai", Name: "GPT Image 1 Mini", Family: "gpt-image",
		Dialect: DialectOpenAI, CostPerImage: 0.011, SupportsEdit: true,
		Sizes: []string{"1024x1024", "1536x1024", "1024x1536", "auto"},
	},
	{
		ID: "gpt-image-1.5", Namespace: "openai", Name: "GPT Image 1.5", Family: "gpt-image",
		Dialect: DialectOpenAI, CostPerImage: 0.05, SupportsEdit: true,
		Sizes: []string{"1024x1024", "1536x1024", "1024x1536", "auto"},
	},
	{
		ID: "gpt-image-2", Namespace: "openai", Name: "GPT Image 2", Family: "gpt-image",
		Dialect: DialectOpenAI, CostPerImage: 0.06, SupportsEdit: true,
		Sizes: []string{"1024x1024", "1536x1024", "1024x1536", "auto"},
	},
	{
		ID: "dall-e-3", Namespace: "openai", Name: "DALL·E 3", Family: "dall-e",
		Dialect: DialectOpenAI, CostPerImage: 0.04,
		Sizes:        []string{"1024x1024", "1792x1024", "1024x1792"},
		AspectRatios: []string{"1:1", "16:9", "9:16"},
	},
	{
		ID: "dall-e-2", Namespace: "openai", Name: "DALL·E 2", Family: "dall-e",
		Dialect: DialectOpenAI, CostPerImage: 0.02, SupportsEdit: true,
		Sizes: []string{"256x256", "512x512", "1024x1024"},
	},

	// ---- Google Gemini (Nano Banana) ----
	{
		ID: "gemini-2.5-flash-image", Namespace: "google", Name: "Gemini 2.5 Flash Image (Nano Banana)", Family: "gemini-image",
		Aliases: []string{"nano-banana", "gemini-2.5-flash-image-preview"},
		Dialect: DialectGemini, CostPerImage: 0.039, SupportsEdit: true, TextOutput: true,
		AspectRatios: []string{"1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "21:9"},
	},
	{
		ID: "gemini-3-pro-image-preview", Namespace: "google", Name: "Gemini 3 Pro Image (Nano Banana Pro)", Family: "gemini-image",
		Aliases: []string{"nano-banana-pro"},
		Dialect: DialectGemini, CostPerImage: 0.134, SupportsEdit: true, TextOutput: true,
		AspectRatios: []string{"1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "21:9"},
	},
	{
		ID: "imagen-4.0-generate-001", Namespace: "google", Name: "Imagen 4", Family: "imagen",
		Dialect: DialectImagen, CostPerImage: 0.04,
		AspectRatios: []string{"1:1", "3:4", "4:3", "9:16", "16:9"},
	},
	{
		ID: "imagen-4.0-ultra-generate-001", Namespace: "google", Name: "Imagen 4 Ultra", Family: "imagen",
		Dialect: DialectImagen, CostPerImage: 0.06,
		AspectRatios: []string{"1:1", "3:4", "4:3", "9:16", "16:9"},
	},
	{
		ID: "imagen-4.0-fast-generate-001", Namespace: "google", Name: "Imagen 4 Fast", Family: "imagen",
		Dialect: DialectImagen, CostPerImage: 0.02,
		AspectRatios: []string{"1:1", "3:4", "4:3", "9:16", "16:9"},
	},
	{
		ID: "imagen-3.0-generate-002", Namespace: "google", Name: "Imagen 3", Family: "imagen",
		Dialect: DialectImagen, CostPerImage: 0.03,
		AspectRatios: []string{"1:1", "3:4", "4:3", "9:16", "16:9"},
	},

	// ---- xAI Grok ----
	{
		ID: "grok-2-image", Namespace: "xai", Name: "Grok 2 Image", Family: "grok-image",
		Aliases: []string{"grok-2-image-1212", "grok-2-image-latest"},
		Dialect: DialectOpenAI, CostPerImage: 0.07, MaxN: 10,
		Sizes: []string{"1024x1024", "1536x1024", "1024x1536"},
	},
	{
		ID: "grok-imagine-image-2.0", Namespace: "xai", Name: "Grok Imagine Image 2.0", Family: "grok-image",
		Aliases: []string{"grok-imagine-image"},
		Dialect: DialectOpenAI, CostPerImage: 0.07, MaxN: 10, SupportsEdit: true,
		AspectRatios: []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "2:1", "1:2", "19.5:9", "9:19.5", "20:9", "9:20", "21:9", "5:2", "auto"},
	},
}

// familyDefaults maps a distinctive model-id prefix to the representative
// registry entry that should back unknown versions of that family
// (e.g. "gpt-image-3" -> GPT Image 1 row, "gemini-4-pro-image" -> Nano Banana).
type familyRule struct {
	// match reports whether the bare id belongs to this family.
	match func(string) bool
	base  Model
}

var familyRules = []familyRule{
	{match: func(s string) bool { return strings.HasPrefix(s, "gpt-image") }, base: Model{Namespace: "openai", Family: "gpt-image", Dialect: DialectOpenAI, CostPerImage: 0.042, SupportsEdit: true}},
	{match: func(s string) bool { return strings.HasPrefix(s, "dall-e") || strings.HasPrefix(s, "dalle") }, base: Model{Namespace: "openai", Family: "dall-e", Dialect: DialectOpenAI, CostPerImage: 0.04}},
	{match: func(s string) bool { return strings.HasPrefix(s, "imagen") }, base: Model{Namespace: "google", Family: "imagen", Dialect: DialectImagen, CostPerImage: 0.04}},
	{match: func(s string) bool { return strings.HasPrefix(s, "gemini-") && strings.Contains(s, "image") }, base: Model{Namespace: "google", Family: "gemini-image", Dialect: DialectGemini, CostPerImage: 0.039, SupportsEdit: true, TextOutput: true}},
	{match: func(s string) bool { return strings.HasPrefix(s, "nano-banana") }, base: Model{Namespace: "google", Family: "gemini-image", Dialect: DialectGemini, CostPerImage: 0.039, SupportsEdit: true, TextOutput: true}},
	{match: func(s string) bool {
		// xAI's "grok-imagine" prefix covers BOTH image and video models
		// (grok-imagine-image, grok-imagine-video). Matching the bare prefix
		// classified the video models as image models, which let
		// POST /v1/images/generations serve a video model and made the chat
		// guard point video traffic at the images API. Video is excluded here
		// and owned by videogen's registry.
		if strings.Contains(s, "video") {
			return false
		}
		return strings.HasPrefix(s, "grok-imagine") || (strings.HasPrefix(s, "grok-") && strings.Contains(s, "image"))
	}, base: Model{Namespace: "xai", Family: "grok-image", Dialect: DialectOpenAI, CostPerImage: 0.07, MaxN: 10}},
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
// still recognized as an image model and routed with a sensible default
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

// IsImageModel reports whether an id names an image-generation model. It is
// used to tag provider models, to keep image traffic off chat endpoints (and
// vice versa), and to expose the `image` capability in /v1/models.
func IsImageModel(id string) bool {
	_, ok := Lookup(id)
	return ok
}

// IsImageOnlyModel reports whether a curated id is a pure image generator (no
// text output). It is the offline fallback for the text-endpoint guard when
// the catalog carries no row for the model.
func IsImageOnlyModel(id string) bool {
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
	case "gpt-image":
		return "OpenAI GPT Image model — text-to-image and image editing via /v1/images/generations."
	case "dall-e":
		return "OpenAI DALL·E image model — text-to-image via /v1/images/generations."
	case "gemini-image":
		return "Google Gemini image model (Nano Banana) — conversational generation and editing, multimodal input."
	case "imagen":
		return "Google Imagen model — high-fidelity text-to-image generation."
	case "grok-image":
		return "xAI Grok image model — text-to-image with aspect ratio, resolution and quality controls."
	}
	return "Image-generation model served through the standardized /v1/images/generations endpoint."
}

// ModalitiesJSON renders the modalities column for the catalog row. Image
// models accept text (and usually images); their OUTPUT is "image", plus
// "text" for models that narrate alongside the image (Gemini image models).
// The output list is what distinguishes an image-only model from an
// image-capable one, so it must mirror models.dev's semantics exactly.
func (m Model) ModalitiesJSON() string {
	in := []string{"text"}
	if m.SupportsEdit {
		in = append(in, "image")
	}
	out := []string{"image"}
	if m.TextOutput {
		out = append([]string{"text"}, out...)
	}
	return `{"input":["` + strings.Join(in, `","`) + `"],"output":["` + strings.Join(out, `","`) + `"]}`
}

// OutputModalities parses the `output` list from a models.dev modalities blob
// ({"input":[...],"output":[...]}). Unknown/empty input yields nil.
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

// ImageModality reports whether a modalities JSON blob declares the "image"
// output modality. This is THE models.dev marker for an image-generation
// model — models.dev has no dedicated image flag (its `type` field only ever
// carries embedding/reranking/video), so `modalities.output` containing
// "image" is the tag the gateway keys off.
func ImageModality(modalities string) bool {
	for _, o := range OutputModalities(modalities) {
		if o == "image" {
			return true
		}
	}
	return false
}

// ImageOnlyFromModalities reports whether a modalities blob describes a model
// that can ONLY produce images (image output, no text output).
//
// This is the distinction that keeps the API from refusing legitimate traffic:
// models.dev gives `output: ["image"]` for pure generators (DALL·E, Imagen,
// gpt-image-1, grok-imagine-image) but `output: ["text","image"]` for models
// that also chat (Gemini image models, gpt-image-1-mini, chatgpt-image-latest).
// Only the former may be barred from the text endpoints.
func ImageOnlyFromModalities(modalities string) bool {
	outs := OutputModalities(modalities)
	if len(outs) == 0 {
		return false
	}
	image, text := false, false
	for _, o := range outs {
		switch o {
		case "image":
			image = true
		case "text":
			text = true
		}
	}
	return image && !text
}
