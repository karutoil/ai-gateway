package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"ai-gateway/internal/decision"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/imagegen"
	"ai-gateway/internal/lb"
	"ai-gateway/internal/models"
	"ai-gateway/internal/translate"
	"ai-gateway/internal/videogen"
)

// DefaultImageTTFB is the client-facing first-byte budget for the image
// endpoints. Image generation is legitimately slow (Gemini/Imagen and
// gpt-image can take tens of seconds), so it must not inherit the 85s chat
// budget designed for a first TOKEN.
const DefaultImageTTFB = 300 * time.Second

// Image multipart limits. The JSON path already bounds prompt/n/size; the
// multipart edit path had NO bounds at all beyond the request-body cap, so an
// edit could carry an unbounded number of parts and (within the body cap) an
// unbounded aggregate of image bytes — each of which is decoded, re-encoded
// into base64 for Google and rebuilt into a fresh multipart body for the
// passthrough dialects.
const (
	// MaxImageEditParts bounds the number of multipart parts in one edit
	// request: fields plus files. It is deliberately generous (a full
	// reference set + mask + every documented scalar field) but finite.
	MaxImageEditParts = 64
	// MaxImageEditBytes bounds the aggregate bytes of all file parts. The
	// reference set is at most imagegen.MaxReferenceImages images plus a mask,
	// so 32 MiB leaves room for full-resolution photos while keeping the
	// decoded + re-encoded copies bounded.
	MaxImageEditBytes = 32 << 20
)

// ImageGenerations handles POST /v1/images/generations — the standardized
// OpenAI-compatible image-generation endpoint.
//
// The gateway accepts the OpenAI Images API request shape and speaks whichever
// dialect the routed provider needs:
//
//   - openai / azure / openai_compatible / xai: passthrough to
//     {base}/images/generations (xAI Grok and resellers are OpenAI-compatible).
//   - google: Google's Gemini image models (Nano Banana) via
//     {base}/v1beta/models/{model}:generateContent with IMAGE response
//     modalities, or Imagen via :predict. The native response is translated
//     back into the OpenAI Images shape.
//
// Image models bill per generated image, so the response is metered with the
// curated per-image price (falling back to the catalog when unknown).
func (h *Handler) ImageGenerations(w http.ResponseWriter, r *http.Request) {
	body, bodyOK := h.readProxyBody(w, r)
	if !bodyOK {
		return
	}
	r = r.WithContext(WithAttribution(r.Context(), attributionFromRequest(r, body)))
	rawModel := translate.ExtractModel(body)
	model := h.resolveAlias(rawModel)
	if !h.enforceModelAllowlist(w, r, rawModel, model) {
		return
	}
	if !h.checkTPM(w, r, body) {
		return
	}
	norm, err := imagegen.ParseRequest(body)
	if err != nil {
		httperr.Invalid(w, err.Error())
		return
	}
	if h.rejectNonImageModelOnImages(w, model) {
		return
	}
	// Image models must not be routed to a decision/System One provider, and
	// decision models must not receive image traffic.
	candidates, rule := h.imageCandidates(r, rawModel, model, func(p *models.Provider) bool {
		return !isDecisionProvider(p) && p.Type != models.ProviderAnthropic
	})
	if len(candidates) == 0 {
		h.imageNoRoute(w, model, rawModel, rule)
		return
	}
	// A model on a multi-protocol provider that is NOT an image model cannot
	// serve image traffic; fail fast with the right endpoint.
	if p0 := candidates[0].Provider; isMultiProtocolProvider(p0) && !h.imageModelKnown(model) {
		httperr.Invalid(w, "model '"+model+"' is not an image model; POST /v1/images/generations requires an image model (e.g. gpt-image-1, gemini-2.5-flash-image, grok-2-image)")
		return
	}
	start := time.Now()
	keyPrefix := r.Header.Get("X-Gateway-Key-Prefix")
	// billModel tracks the model id actually sent upstream (the lb_rules
	// model_override when the rule set one) so per-image pricing follows the
	// model that really ran. The prepare closure runs immediately before the
	// attempt's response is transformed, on the same goroutine.
	billModel := model
	h.proxyCandidatesWithOpts(w, r, body, false, model, "images.generations", keyPrefix, start, h.billableImageCandidates(candidates), rule,
		func(p *models.Provider, in []byte) (string, string, []byte, bool, string, error) {
			n, perr := imagegen.ParseRequest(in)
			if perr != nil {
				return "", "", nil, false, "", perr
			}
			// Derive the upstream model from the body the caller handed us: it
			// already carries the lb_rules model_override when the rule set one
			// (proxyCandidates rewrites `model` per candidate). Using the outer
			// `model` here billed the override while sending the alias — the
			// upstream then ran a different model than the one billed.
			sent := upstreamModelFor(n.Model, in, model)
			billModel = sent
			return h.imageUpstreamRequest(p, n, in, sent)
		},
		h.imageProxyOpts(h.imageTransformFunc(func() string { return billModel }, norm.ResponseFormat)))
}

// ImageEdits handles POST /v1/images/edits — multipart image editing /
// image-to-image, normalized to the same provider dialects as generation.
//
// For OpenAI-compatible providers the multipart body is re-emitted (with the
// provider prefix stripped from `model`); for Google the images are inlined
// into a generateContent body, because the Gemini API has no multipart edit
// endpoint.
func (h *Handler) ImageEdits(w http.ResponseWriter, r *http.Request) {
	body, bodyOK := h.readProxyBody(w, r)
	if !bodyOK {
		return
	}
	r = r.WithContext(WithAttribution(r.Context(), attributionFromRequest(r, body)))
	contentType := r.Header.Get("Content-Type")
	norm, _, err := parseEditMultipart(body, contentType)
	if err != nil {
		httperr.Invalid(w, err.Error())
		return
	}
	rawModel := norm.Model
	model := h.resolveAlias(rawModel)
	if !h.enforceModelAllowlist(w, r, rawModel, model) {
		return
	}
	if !h.checkTPM(w, r, body) {
		return
	}
	// P1-3: the same capability guard the JSON path runs. Without it a
	// decision or text model was forwarded to the provider's edit endpoint,
	// burning an upstream round trip on an opaque (and, on metered providers,
	// billable) failure.
	if h.rejectNonImageModelOnImages(w, model) {
		return
	}
	if len(norm.Images) == 0 {
		httperr.Invalid(w, "image: at least one image file is required for edits")
		return
	}
	candidates, rule := h.imageCandidates(r, rawModel, model, func(p *models.Provider) bool {
		return !isDecisionProvider(p) && p.Type != models.ProviderAnthropic
	})
	if len(candidates) == 0 {
		h.imageNoRoute(w, model, rawModel, rule)
		return
	}
	start := time.Now()
	keyPrefix := r.Header.Get("X-Gateway-Key-Prefix")
	billModel := model
	h.proxyCandidatesWithOpts(w, r, body, false, model, "images.edits", keyPrefix, start, h.billableImageCandidates(candidates), rule,
		func(p *models.Provider, in []byte) (string, string, []byte, bool, string, error) {
			n, f, perr := parseEditMultipart(in, contentType)
			if perr != nil {
				return "", "", nil, false, "", perr
			}
			// The rebuilt multipart body carries `model`, so the upstream id is
			// whatever the (possibly override-rewritten) body says.
			sent := upstreamModelFor(n.Model, nil, model)
			billModel = sent
			return h.imageEditUpstreamRequest(p, n, f, sent)
		},
		h.imageProxyOpts(h.imageTransformFunc(func() string { return billModel }, norm.ResponseFormat)))
}

// billableImageCandidates narrows the candidate list to the single serving
// provider. Image creation is billable and non-idempotent, so the gateway must
// not walk a failover chain: pre-fix one client request could generate (and
// bill) an asset on TWO providers. proxyCandidates still honours
// opts.noFailover for the retry path; this removes the second provider from
// the chain structurally.
func (h *Handler) billableImageCandidates(candidates []lb.Candidate) []lb.Candidate {
	if len(candidates) <= 1 {
		return candidates
	}
	return candidates[:1]
}

// upstreamModelFor resolves the model id actually sent upstream for one
// attempt. The body is authoritative when it carries a usable `model` (the
// proxy rewrites it to the lb_rules model_override before calling prepare), and
// the requested model is the fallback.
func upstreamModelFor(bodyModel string, body []byte, fallback string) string {
	if s := strings.TrimSpace(bodyModel); s != "" {
		return s
	}
	if len(body) > 0 {
		if s := strings.TrimSpace(translate.ExtractModel(body)); s != "" {
			return s
		}
	}
	return fallback
}

// imageTTFB returns the effective first-byte budget for image requests under the
// IMAGE_TIMEOUT_SECONDS tri-state (P1-12):
//
//	ImageTTFB <  0                => DefaultImageTTFB (300s)  [unset / -1]
//	ImageTTFB == 0                => DISABLED (no watchdog)   [IMAGE_TIMEOUT_SECONDS=0]
//	ImageTTFB >  0                => that exact budget
//	ImageTTFBDisabled == true     => DISABLED (wins over any value)
//
// Returning 0 here is NOT sufficient to disable the watchdog: 0 is also
// proxyCandidatesOpts.ttfb's "no override" value for every chat caller. The
// disabled state is therefore ALSO carried on proxyCandidatesOpts.ttfbDisable
// (see imageProxyOpts), which resolveTTFBBudget in fallback.go treats as the
// highest-priority signal. Both signals are honoured so the endpoint behaves
// correctly whichever one the boot wiring sets.
func (h *Handler) imageTTFB() time.Duration {
	if h.ImageTTFBDisabled {
		return 0
	}
	switch {
	case h.ImageTTFB < 0:
		return DefaultImageTTFB
	case h.ImageTTFB == 0:
		// Documented "disabled" sentinel. main.go reaching this state used to
		// fall through to the 300s default while logging "disabled".
		return 0
	default:
		return h.ImageTTFB
	}
}

// imageTTFBDisabled reports whether the image first-byte watchdog is off,
// merging the two disable signals: the explicit flag and the ==0 sentinel.
func (h *Handler) imageTTFBDisabled() bool {
	return h.ImageTTFBDisabled || h.ImageTTFB == 0
}

// imageProxyOpts builds the per-endpoint proxy options shared by the two image
// routes. It is the single place the image endpoints communicate their TTFB
// tri-state, billable-create semantics and cache bypass to the shared proxy
// machinery, so a new image route cannot silently acquire the chat defaults.
//
// transform is supplied by the caller (it needs the response_format), and
// requestTotal comes from the same handler resolution chat uses.
func (h *Handler) imageProxyOpts(transform func(p *models.Provider, body []byte, status int) (out []byte, costUSD float64, hasCost bool)) proxyCandidatesOpts {
	return proxyCandidatesOpts{
		ttfb: h.imageTTFB(),
		// P1-12: "disabled" cannot be expressed by ttfb==0 alone (that is
		// "no override" for every other endpoint), so it also travels on this
		// flag, which resolveTTFBBudget gives priority over any caller default.
		ttfbDisable:       h.imageTTFBDisabled(),
		transformResponse: transform,
		// P0-2: an image create is billable and NOT idempotent. Neither a
		// same-provider retry nor a failover to another provider may turn one
		// client request into two generated (and billed) assets.
		noRetry:    true,
		noFailover: true,
		// P2-2: generated media must never be replayed from the exact-match
		// response cache (stale re-rolls, cost_usd=0 accounting).
		noCache: true,
	}
}

// imageCandidates resolves routing for an image request. It mirrors the chat
// candidate logic but adds image-specific guards: a model that the curated
// registry marks image-only is never routed to a provider whose type cannot
// speak images, and a qualified/pinned provider still wins.
func (h *Handler) imageCandidates(r *http.Request, rawModel, model string, pred func(*models.Provider) bool) ([]lb.Candidate, *lb.Rule) {
	providerHint := r.Header.Get("X-Provider")
	return h.candidateProvidersWithRuleContext(r.Context(), rawModel, model, providerHint, h.requestKeyOrg(r), pred)
}

// imageNoRoute writes the appropriate unroutable error for an image request.
func (h *Handler) imageNoRoute(w http.ResponseWriter, model, rawModel string, rule *lb.Rule) {
	if rule != nil {
		httperr.Proxy(w, http.StatusServiceUnavailable, "no routing member is below its OAuth quota threshold")
		return
	}
	if h.unroutedModel() {
		noRouteFor(w, model)
		return
	}
	if !h.imageModelKnown(model) && !h.imageModelKnown(rawModel) {
		httperr.Invalid(w, "model '"+model+"' is not an image model; POST /v1/images/generations requires an image model (e.g. gpt-image-1, gemini-2.5-flash-image, grok-2-image)")
		return
	}
	httperr.Proxy(w, http.StatusServiceUnavailable, "no image-capable provider configured for this model")
}

// imageModelKnown reports whether the model is positively known to generate
// images, consulting the catalog first (its `image` flag and modalities output)
// and the curated registry second. Unknown ids stay unknown — the caller
// decides whether that is a passthrough or a refusal.
func (h *Handler) imageModelKnown(model string) bool {
	if h.CatalogStore != nil {
		if cm := h.catalogRow(model); cm != nil {
			if cm.Image || imagegen.ImageModality(cm.Modalities) {
				return true
			}
		}
	}
	return imagegen.IsImageModel(model)
}

// imageModelOnly reports whether the model can ONLY generate images, with the
// catalog as the authoritative source (its `image`/modalities flags) and the
// curated registry as the offline fallback. Catalog-only image rows are
// therefore image-only: the endpoint guards used to consult the registry alone
// and disagreed with what /v1/models listed.
func (h *Handler) imageModelOnly(model string) bool {
	if h.CatalogStore != nil {
		if cm := h.catalogRow(model); cm != nil && cm.Modalities != "" {
			return imagegen.ImageOnlyFromModalities(cm.Modalities)
		}
	}
	return imagegen.IsImageOnlyModel(model)
}

// catalogRow resolves a model id against the catalog store, tolerating both the
// qualified id and the bare selector. nil means "no row".
func (h *Handler) catalogRow(model string) *models.CatalogModel {
	if h.CatalogStore == nil {
		return nil
	}
	if cm, err := h.CatalogStore.Get(model); err == nil && cm != nil {
		return cm
	}
	if cm, err := h.CatalogStore.GetByShortID(model); err == nil && cm != nil {
		return cm
	}
	return nil
}

// imageUpstreamRequest builds the upstream URL and body for a generation
// request against a specific provider.
func (h *Handler) imageUpstreamRequest(p *models.Provider, n *imagegen.Normalized, rawBody []byte, model string) (string, string, []byte, bool, string, error) {
	apiKey, err := h.ProviderStore.DecryptKey(p)
	if err != nil {
		return "", "", nil, false, "", err
	}
	upstream := stripProviderPrefix(model, p)
	if p.Type == models.ProviderGoogle {
		if verr := validateGoogleModelID(upstream); verr != nil {
			return "", "", nil, false, "", &imagegen.ValidateError{Field: "model", Msg: verr.Error()}
		}
		body, berr := googleImageBody(n, upstream)
		if berr != nil {
			return "", "", nil, false, "", berr
		}
		return googleImageTarget(p.BaseURL, upstream, imagegen.DialectForModel(upstream)), apiKey, body, false, "", nil
	}
	// OpenAI-compatible dialects (openai / azure / xai / resellers): passthrough
	// the normalized request with the provider prefix stripped from `model`.
	out := replaceModelInBody(rawBody, upstream)
	target := upstreamTarget(p.BaseURL, "/images/generations", upstream, p.Type == models.ProviderAzure)
	return target, apiKey, out, false, "", nil
}

// imageEditUpstreamRequest builds the upstream URL and body for an edit
// request. Google has no multipart edit endpoint, so images are inlined into
// a generateContent body; everything else receives a rebuilt multipart form.
func (h *Handler) imageEditUpstreamRequest(p *models.Provider, n *imagegen.Normalized, files []multipartFile, model string) (string, string, []byte, bool, string, error) {
	apiKey, err := h.ProviderStore.DecryptKey(p)
	if err != nil {
		return "", "", nil, false, "", err
	}
	upstream := stripProviderPrefix(model, p)
	if p.Type == models.ProviderGoogle {
		if verr := validateGoogleModelID(upstream); verr != nil {
			return "", "", nil, false, "", &imagegen.ValidateError{Field: "model", Msg: verr.Error()}
		}
		body, berr := googleImageBody(n, upstream)
		if berr != nil {
			return "", "", nil, false, "", berr
		}
		return googleImageTarget(p.BaseURL, upstream, imagegen.DialectForModel(upstream)), apiKey, body, false, "", nil
	}
	body, ctype, berr := buildEditMultipart(n, files, upstream)
	if berr != nil {
		return "", "", nil, false, "", berr
	}
	target := upstreamTarget(p.BaseURL, "/images/edits", upstream, p.Type == models.ProviderAzure)
	return target, apiKey, body, false, ctype, nil
}

// googleImageBody renders a Gemini or Imagen request body for a Google
// provider, choosing the dialect from the model id.
func googleImageBody(n *imagegen.Normalized, model string) ([]byte, error) {
	if imagegen.DialectForModel(model) == imagegen.DialectImagen {
		return imagegen.ImagenRequestBody(n)
	}
	return imagegen.GeminiRequestBody(n)
}

// validateGoogleModelID rejects a model id that cannot be placed safely in a
// Google/Cloudflare native URL path. The id becomes a path segment
// ({base}/v1beta/models/{model}:predict), so an id carrying a query separator, a
// fragment marker, or a second path separator would let the client retarget the
// request. Known ids are plain model names; rejecting the rest is cheap and
// keeps url.PathEscape's output unambiguous.
func validateGoogleModelID(model string) error {
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("model id is empty")
	}
	if strings.ContainsAny(model, "?#") {
		return fmt.Errorf("model id %q must not contain '?' or '#'", model)
	}
	if strings.Count(model, "/") > 1 {
		return fmt.Errorf("model id %q must contain at most one '/' (namespace/model)", model)
	}
	return nil
}

// googleImageTarget builds the native Google endpoint URL:
//
//	{base}/v1beta/models/{model}:generateContent   (Nano Banana)
//	{base}/v1beta/models/{model}:predict           (Imagen)
//
// The base may already carry a /v1beta segment; it is never doubled. The model
// id is path-escaped (P3-2: it was concatenated raw, so an id could inject
// path/query structure into the upstream URL).
func googleImageTarget(baseURL, model string, dialect imagegen.Dialect) string {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	action := ":generateContent"
	if dialect == imagegen.DialectImagen {
		action = ":predict"
	}
	// Strip a trailing version/models segment if the operator included it, so
	// the version is never doubled (…/v1beta/v1beta/models/…).
	b = strings.TrimSuffix(b, "/models")
	for _, suffix := range []string{"/v1beta", "/v1"} {
		b = strings.TrimSuffix(b, suffix)
	}
	return b + "/v1beta/models/" + url.PathEscape(model) + action
}

// imageTransformFunc is imageTransform with a lazily-read billing model, so the
// price follows the id actually sent upstream (an lb_rules model_override) rather
// than the id the client asked for (P1-5).
func (h *Handler) imageTransformFunc(billingModel func() string, responseFormat string) func(p *models.Provider, body []byte, status int) ([]byte, float64, bool) {
	return func(p *models.Provider, body []byte, status int) ([]byte, float64, bool) {
		model := billingModel()
		if p != nil && p.Type == models.ProviderGoogle {
			dialect := imagegen.DialectForModel(model)
			var out []byte
			if dialect == imagegen.DialectImagen {
				out = imagegen.ImagenToOpenAI(body, model, responseFormat)
			} else {
				out = imagegen.GeminiToOpenAI(body, model, responseFormat)
			}
			if out != nil {
				return out, h.imageCost(model, out), true
			}
			return nil, 0, false
		}
		// OpenAI-compatible upstream: the body is already the OpenAI Images shape.
		// Only the cost override is needed (per-image billing).
		return nil, h.imageCost(model, body), true
	}
}

// imageCost computes the billed USD for an image response: the curated
// per-image price times the number of images returned, falling back to the
// catalog/token pricing when the model is unknown.
func (h *Handler) imageCost(model string, respBody []byte) float64 {
	perImage, ok := imagegen.CostPerImage(model)
	if !ok {
		return 0
	}
	count := imagegen.ImageCount(respBody)
	if count <= 0 {
		count = 1
	}
	return perImage * float64(count)
}

// multipartFile is one uploaded part of a multipart edit request.
type multipartFile struct {
	Field       string
	FileName    string
	ContentType string
	Data        []byte
}

// parseEditMultipart parses a multipart/form-data image-edit request into the
// normalized request shape plus the raw file parts (so a passthrough rebuild
// preserves filenames and content types).
//
// P1-3: this path applies the SAME scalar validation as the JSON path, using
// the same constants (imagegen.MaxPromptLen, the size vocabulary,
// imagegen.MaxImagesPerRequest, the response_format enum), plus two limits the
// JSON path does not need: a part-count cap and an aggregate byte cap. Pre-fix
// an edit could carry an unbounded prompt, n, size and body.
func parseEditMultipart(body []byte, contentType string) (*imagegen.Normalized, []multipartFile, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return nil, nil, &imagegen.ValidateError{Field: "", Msg: "body must be multipart/form-data"}
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, nil, &imagegen.ValidateError{Field: "", Msg: "multipart boundary missing"}
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	norm := &imagegen.Normalized{N: 1}
	var files []multipartFile
	var totalBytes int64
	parts := 0
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			return nil, nil, &imagegen.ValidateError{Field: "", Msg: "malformed multipart body"}
		}
		parts++
		if parts > MaxImageEditParts {
			return nil, nil, &imagegen.ValidateError{Field: "", Msg: fmt.Sprintf("multipart body exceeds the %d-part limit", MaxImageEditParts)}
		}
		data, rerr := io.ReadAll(io.LimitReader(part, 64<<20))
		if rerr != nil {
			return nil, nil, &imagegen.ValidateError{Field: part.FormName(), Msg: "failed to read part"}
		}
		name := part.FormName()
		filename := part.FileName()
		if filename == "" && name != "image" && name != "images" && name != "mask" {
			// Plain form field.
			totalBytes += int64(len(data))
			if totalBytes > MaxImageEditBytes {
				return nil, nil, &imagegen.ValidateError{Field: name, Msg: fmt.Sprintf("request exceeds the %d-byte aggregate limit", MaxImageEditBytes)}
			}
			if ferr := applyEditField(norm, name, strings.TrimSpace(string(data))); ferr != nil {
				return nil, nil, ferr
			}
			continue
		}
		totalBytes += int64(len(data))
		if totalBytes > MaxImageEditBytes {
			return nil, nil, &imagegen.ValidateError{Field: name, Msg: fmt.Sprintf("request exceeds the %d-byte aggregate limit", MaxImageEditBytes)}
		}
		ct := part.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/octet-stream"
		}
		// The part header value is echoed into the rebuilt upstream part, so it
		// is reduced to a token here as well as at build time (P3-1).
		ct = sanitizeMultipartToken(ct)
		files = append(files, multipartFile{Field: name, FileName: filename, ContentType: ct, Data: data})
		img := imagegen.InputImage{MimeType: ct, Data: data}
		switch name {
		case "mask":
			norm.Mask = &img
		case "image", "images":
			if len(norm.Images) >= imagegen.MaxReferenceImages {
				return nil, nil, &imagegen.ValidateError{Field: "image", Msg: "too many reference images"}
			}
			norm.Images = append(norm.Images, img)
		}
	}
	if strings.TrimSpace(norm.Prompt) == "" {
		return nil, nil, &imagegen.ValidateError{Field: "prompt", Msg: "required non-empty string"}
	}
	if strings.TrimSpace(norm.Model) == "" {
		return nil, nil, &imagegen.ValidateError{Field: "model", Msg: "required non-empty string"}
	}
	if err := validateEditScalars(norm); err != nil {
		return nil, nil, err
	}
	return norm, files, nil
}

// validateEditScalars applies the JSON path's scalar rules to a multipart edit
// request. The limits are the SAME constants imagegen.ParseRequest uses, so the
// two request encodings cannot drift apart.
func validateEditScalars(n *imagegen.Normalized) error {
	if len(n.Prompt) > imagegen.MaxPromptLen {
		return &imagegen.ValidateError{Field: "prompt", Msg: fmt.Sprintf("exceeds %d characters", imagegen.MaxPromptLen)}
	}
	if n.N < 1 || n.N > imagegen.MaxImagesPerRequest {
		return &imagegen.ValidateError{Field: "n", Msg: fmt.Sprintf("must be between 1 and %d", imagegen.MaxImagesPerRequest)}
	}
	if n.Size != "" {
		if !imagegen.ValidSize(n.Size) {
			return &imagegen.ValidateError{Field: "size", Msg: "unsupported size " + n.Size}
		}
	}
	if n.ResponseFormat != "" {
		if !imagegen.ValidResponseFormat(n.ResponseFormat) {
			return &imagegen.ValidateError{Field: "response_format", Msg: `must be "url" or "b64_json"`}
		}
	}
	return nil
}

// applyEditField folds a multipart text field into the normalized request. It
// returns an error for a field whose value is present but unusable: silently
// keeping the default (the old behaviour for a non-numeric n) hid a
// client mistake and made the billed image count differ from the request.
func applyEditField(n *imagegen.Normalized, name, value string) error {
	switch name {
	case "model":
		n.Model = value
	case "prompt":
		n.Prompt = value
	case "n":
		v, ok := atoiStrict(value)
		if !ok || v < 1 || v > imagegen.MaxImagesPerRequest {
			return &imagegen.ValidateError{Field: "n", Msg: fmt.Sprintf("must be an integer between 1 and %d", imagegen.MaxImagesPerRequest)}
		}
		n.N = v
	case "size":
		n.Size = strings.ToLower(value)
	case "quality":
		n.Quality = value
	case "style":
		n.Style = value
	case "response_format":
		n.ResponseFormat = value
	case "aspect_ratio":
		n.AspectRatio = value
	case "resolution":
		n.Resolution = value
	}
	return nil
}

func atoiStrict(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// buildEditMultipart rebuilds a multipart edit request for an OpenAI-compatible
// upstream, overriding the model id (provider prefix stripped).
func buildEditMultipart(n *imagegen.Normalized, files []multipartFile, model string) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fields := []struct{ k, v string }{
		{"model", model},
		{"prompt", n.Prompt},
	}
	if n.N > 0 {
		fields = append(fields, struct{ k, v string }{"n", itoa(n.N)})
	}
	for _, f := range fields {
		if err := mw.WriteField(f.k, f.v); err != nil {
			return nil, "", err
		}
	}
	for _, opt := range []struct{ k, v string }{
		{"size", n.Size}, {"quality", n.Quality}, {"style", n.Style},
		{"response_format", n.ResponseFormat}, {"aspect_ratio", n.AspectRatio}, {"resolution", n.Resolution},
	} {
		if opt.v != "" {
			if err := mw.WriteField(opt.k, opt.v); err != nil {
				return nil, "", err
			}
		}
	}
	for _, f := range files {
		hdr := make(textproto.MIMEHeader)
		hdr.Set("Content-Disposition", `form-data; name="`+sanitizeMultipartToken(f.Field)+`"; filename="`+sanitizeMultipartToken(f.FileName)+`"`)
		// P3-1: the part Content-Type was copied verbatim from the client into
		// the rebuilt header. A CR/LF in it is a header-injection primitive, so
		// it is reduced to a bare token and never allowed to carry structure.
		hdr.Set("Content-Type", sanitizeMultipartToken(f.ContentType))
		part, err := mw.CreatePart(hdr)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(f.Data); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// sanitizeMultipartToken reduces a client-supplied header value to a bare token
// safe to place in a rebuilt multipart header: control characters (CR/LF
// included), quotes, semicolons and whitespace runs are dropped or folded, and
// the result is bounded. An empty result becomes application/octet-stream so the
// part stays parseable.
func sanitizeMultipartToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "application/octet-stream"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\r' || r == '\n' || r == 0:
			continue
		case r < 0x20 || r == 0x7f:
			continue
		case r == '"' || r == ';' || r == '\\':
			continue
		case r == ' ' || r == '\t':
			continue
		default:
			b.WriteRune(r)
		}
		if b.Len() >= 128 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "application/octet-stream"
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// rejectImageModelOnText refuses IMAGE-ONLY models on the text endpoints
// (chat/completions/embeddings/messages/responses). A pure generator speaks a
// different protocol entirely, so forwarding a chat body would burn an
// upstream round trip on an opaque error.
//
// Image-CAPABLE models (Gemini image models, gpt-image-1-mini,
// chatgpt-image-latest — anything whose models.dev modalities output both text
// and image) are deliberately ALLOWED through: they are legitimate chat models
// too, and refusing them was the bug that made the API "yell" at valid
// traffic. The image capability is what opens /v1/images/generations; it is
// not, by itself, a reason to close the text endpoints.
//
// Returns true when the request was rejected (the caller must return).
func (h *Handler) rejectImageModelOnText(w http.ResponseWriter, model, endpoint string) bool {
	if !h.modelIsImageOnly(model) {
		return false
	}
	httperr.Invalid(w, "model '"+model+"' is an image-only model; use POST /v1/images/generations instead of "+endpoint)
	return true
}

// rejectDecisionModelOnText refuses System One / decision models on the text
// endpoints. A decision model reads a `state` plus typed `questions` and
// answers with calibrated probabilities — it is not a chat model, so a chat
// body against it would either be rejected upstream or, worse, silently
// mis-answered. Like the image guard this is a request-validity check, so it
// runs before routing and returns an actionable 400 naming /v1/systemone
// instead of the generic 404 model_not_routed.
//
// It consults the catalog as well as the curated registry (P1-4): a row synced
// with decision=1 is advertised by /v1/models as a decision model, so the guard
// must refuse it here too rather than forwarding a chat body to it.
//
// Returns true when the request was rejected (the caller must return).
func (h *Handler) rejectDecisionModelOnText(w http.ResponseWriter, model, endpoint string) bool {
	if !h.modelIsDecisionModel(model) {
		return false
	}
	httperr.Invalid(w, "model '"+model+"' is a System One / decision model (typed noul/choice/score questions, no text generation); use POST /v1/systemone instead of "+endpoint)
	return true
}

// rejectDecisionModelOnText is the catalog-less variant of
// (*Handler).rejectDecisionModelOnText, kept for the text-endpoint call sites in
// internal/proxy/proxy.go (owned by another agent). Those call sites should
// switch to the handler method, which also honours a catalog row synced with
// decision=1; until then they keep the curated-registry behaviour.
//
// Returns true when the request was rejected (the caller must return).
func rejectDecisionModelOnText(w http.ResponseWriter, model, endpoint string) bool {
	if !decision.IsDecisionModel(model) {
		return false
	}
	httperr.Invalid(w, "model '"+model+"' is a System One / decision model (typed noul/choice/score questions, no text generation); use POST /v1/systemone instead of "+endpoint)
	return true
}

// modelIsDecisionModel reports whether a model is a decision (System One)
// model. The catalog is authoritative when it has a row (its decision flag or a
// "decision" output modality — this is where operator edits and models.dev
// discoveries land); the curated registry is the offline fallback.
func (h *Handler) modelIsDecisionModel(model string) bool {
	return decision.IsDecisionModelWithLookup(model, h.catalogCapability)
}

// catalogCapability exposes the catalog row's decision flag as the capability
// source decision.IsDecisionModelWithLookup consults.
func (h *Handler) catalogCapability(model string) decision.Capability {
	cm := h.catalogRow(model)
	if cm == nil {
		return decision.Capability{}
	}
	return decision.Capability{
		Known:    true,
		Decision: cm.Decision || decisionModalityJSON(cm.Modalities),
	}
}

// decisionModalityJSON reports whether a modalities blob declares a "decision"
// output. It mirrors the unexported helper in internal/catalog so the guard and
// the catalog synchronizer agree on what a decision row is.
func decisionModalityJSON(modalities string) bool {
	if strings.TrimSpace(modalities) == "" {
		return false
	}
	var mm struct {
		Output []string `json:"output"`
	}
	if json.Unmarshal([]byte(modalities), &mm) != nil {
		return false
	}
	for _, o := range mm.Output {
		if o == "decision" {
			return true
		}
	}
	return false
}

// rejectVideoModelOnText refuses video-generation models on the text endpoints.
// A video model is a job-based generator: it has no chat/completions surface,
// so a text body against it would burn an upstream round trip on an opaque
// error. Like the image and decision guards this is a request-validity check
// and runs before routing.
//
// Returns true when the request was rejected (the caller must return).
func (h *Handler) rejectVideoModelOnText(w http.ResponseWriter, model, endpoint string) bool {
	if !h.modelIsVideoOnly(model) {
		return false
	}
	httperr.Invalid(w, "model '"+model+"' is a video-generation model; use POST /v1/videos instead of "+endpoint)
	return true
}

// modelIsVideoOnly reports whether a model can ONLY generate video. Mirrors
// modelIsImageOnly: catalog modalities are authoritative, the curated registry
// is the fallback, and unknown models are not video-only.
func (h *Handler) modelIsVideoOnly(model string) bool {
	if cm := h.catalogRow(model); cm != nil && cm.Modalities != "" {
		return videogen.VideoOnlyFromModalities(cm.Modalities)
	}
	return videogen.IsVideoOnlyModel(model)
}

// modelIsImageOnly reports whether a model can ONLY generate images, i.e. it is
// positively known to produce no text. Evidence, in order of authority:
//
//  1. the catalog row's modalities (`output` contains image but not text) —
//     this is the models.dev tag and the authoritative source;
//  2. the curated image registry, for rows the catalog has not caught up with.
//
// Unknown models are NOT image-only, so they pass through to the provider and
// an upstream error (if any) is relayed verbatim.
func (h *Handler) modelIsImageOnly(model string) bool {
	return h.imageModelOnly(model)
}

// isImageOnlyProvider reports whether a provider can serve ONLY image
// generation (no chat/completions/messages/responses). Google's
// generativelanguage endpoints are the canonical case: the gateway speaks
// their native image dialects but not their chat protocol (for Gemini chat,
// point an openai_compatible provider at Google's [OI]-compatibility base URL
// `/v1beta/openai/`).
func isImageOnlyProvider(p *models.Provider) bool {
	return p != nil && p.Type == models.ProviderGoogle
}

// rejectNonImageModelOnImages refuses models that are positively known NOT to
// be image models on the images endpoint. Unknown ids are deliberately allowed
// through: resellers expose image models the gateway cannot enumerate, and
// rejecting them would make the standardized endpoint less capable than the
// upstream. Only positive evidence is refused — a System One / decision model
// (curated or catalog-declared), a video model, or a catalog row whose
// modalities positively say "text".
//
// The catalog is consulted FIRST (P1-4): a row synced with image=1 makes the
// model an image model even when the curated registry has never heard of it,
// and /v1/models already advertises that flag.
func (h *Handler) rejectNonImageModelOnImages(w http.ResponseWriter, model string) bool {
	if h.imageModelKnown(model) {
		return false
	}
	if h.modelIsDecisionModel(model) {
		httperr.Invalid(w, "model '"+model+"' is a decision (System One) model, not an image model; POST /v1/images/generations requires an image model (e.g. gpt-image-1, gemini-2.5-flash-image, grok-2-image)")
		return true
	}
	if videogen.IsVideoModel(model) || h.videoModelKnown(model) {
		httperr.Invalid(w, "model '"+model+"' is a video-generation model, not an image model; use POST /v1/videos for video")
		return true
	}
	// Only refuse on POSITIVE text evidence. A catalog row discovered from a
	// reseller carries no modalities at all, and treating "image=0" as "text"
	// turned every unknown reseller generator into a 400.
	if cm := h.catalogRow(model); cm != nil && cm.Modalities != "" {
		if outs := imagegen.OutputModalities(cm.Modalities); len(outs) > 0 && !imagegen.ImageModality(cm.Modalities) {
			httperr.Invalid(w, "model '"+model+"' is a text model, not an image model; POST /v1/images/generations requires an image model")
			return true
		}
	}
	return false
}

// videoModelKnown reports whether the catalog positively declares a video
// output for the model (its `video` flag or a "video" output modality).
func (h *Handler) videoModelKnown(model string) bool {
	cm := h.catalogRow(model)
	return cm != nil && (cm.Video || videogen.VideoModality(cm.Modalities))
}
