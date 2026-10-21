package proxy

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ai-gateway/internal/decision"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/imagegen"
	"ai-gateway/internal/models"
	"ai-gateway/internal/translate"
	"ai-gateway/internal/videogen"
)

// decisionRequestTotal bounds a System One request end to end. Decision models
// answer with a short typed response, so the unbounded default (0) is wrong for
// a metered endpoint: without a ceiling a stalled upstream keeps the request
// (and its billing row) alive indefinitely.
const decisionRequestTotal = 5 * time.Minute

// SystemOne handles POST /v1/systemone — the System One / decision-model
// endpoint.
//
// A decision model (TypeSafe Jev, Cloudflare Clef, open-weight Kev, llama.cpp
// /v1/systemone) takes an input `state` and a map of typed `questions`
// (noul / choice / score) and returns a calibrated probability for every
// allowed answer. It is not a chat endpoint: the request and response shapes
// are distinct, there is no streaming, and output tokens are free.
//
// The handler validates the System One body locally (so malformed questions
// fail fast with a 400 naming the field), restricts routing to
// decision-capable providers, translates the request for the target provider
// dialect (Cloudflare addresses the model in the URL path), and reuses the
// shared proxy machinery for retries, failover, usage accounting and logs.
func (h *Handler) SystemOne(w http.ResponseWriter, r *http.Request) {
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
	if err := decision.Validate(body); err != nil {
		httperr.Invalid(w, err.Error())
		return
	}
	// The model must actually BE a decision model. This is a request-validity
	// check, not a routing concern, so it runs BEFORE candidate resolution:
	// it used to sit inside the "no candidates" branch behind
	// `if h.unroutedModel()`, which in the default configuration (LB rules
	// configured, legacy fallback off) returned a generic 404 model_not_routed
	// and made this actionable message unreachable.
	//
	// P1-4: the check consults the catalog as well as the curated registry, so a
	// row synced with decision=1 (which /v1/models advertises as decision) is
	// accepted here instead of being rejected as "not a decision model".
	if !h.modelIsDecisionModel(model) {
		httperr.Invalid(w, notDecisionModelMessage(model))
		return
	}
	if model != rawModel {
		body = replaceModelInBody(body, model)
	}
	providerHint := r.Header.Get("X-Provider")
	candidates, rule := h.candidateProvidersWithRuleContext(r.Context(), rawModel, model, providerHint, h.requestKeyOrg(r), isDecisionProvider)
	if len(candidates) == 0 {
		if rule != nil {
			httperr.Proxy(w, http.StatusServiceUnavailable, "no routing member is below its OAuth quota threshold")
			return
		}
		if h.unroutedModel() {
			noRouteFor(w, model)
			return
		}
		httperr.Proxy(w, http.StatusServiceUnavailable, "no decision-capable provider configured")
		return
	}
	start := time.Now()
	h.proxyCandidatesWithOpts(w, r, body, false, model, "systemone", r.Header.Get("X-Gateway-Key-Prefix"), start, candidates, rule,
		func(p *models.Provider, in []byte) (string, string, []byte, bool, string, error) {
			apiKey, err := h.ProviderStore.DecryptKey(p)
			if err != nil {
				return "", "", nil, false, "", err
			}
			// P1-5: the model actually sent upstream is the one in THIS body —
			// proxyCandidates rewrote it to the lb_rules model_override before
			// calling us. Using the outer `model` billed the override while the
			// upstream ran the alias.
			sent := upstreamModelFor("", in, model)
			target, out, err := decisionUpstream(p, in, sent)
			if err != nil {
				return "", "", nil, false, "", err
			}
			return target, apiKey, out, false, "", nil
		},
		proxyCandidatesOpts{requestTotal: decisionRequestTotal})
}

// isDecisionProvider reports whether a provider entry can serve System One
// decision requests. Providers typed systemone/cloudflare qualify outright;
// an [OI]-compatible or other entry whose base URL points at a System One
// endpoint (a llama.cpp / Kev server, or Workers AI's /ai/run) qualifies too,
// so operators are not forced to re-create an existing local provider.
func isDecisionProvider(p *models.Provider) bool {
	if p == nil {
		return false
	}
	switch p.Type {
	case models.ProviderSystemOne, models.ProviderCloudflare:
		return true
	}
	base := strings.ToLower(strings.TrimSpace(p.BaseURL))
	return strings.Contains(base, "systemone") || strings.Contains(base, "/ai/run")
}

// decisionUpstream builds the upstream URL and (dialect-specific) request body
// for a decision request. model is the id actually being sent (the resolved
// alias, or a routing rule's model_override).
func decisionUpstream(p *models.Provider, body []byte, model string) (string, []byte, error) {
	selector := decision.BaseName(model)
	if selector == "" {
		selector = decision.BaseName(translate.ExtractModel(body))
	}
	if p.Type == models.ProviderCloudflare {
		// Workers AI addresses the model in the path and requires the short
		// selector ("clef", "clef-flash", "clef-omni") in the body. The selector
		// arrives from a client-supplied model id, so it is validated before it
		// is concatenated into the URL (P3-2); the path segment is escaped.
		if err := validateDecisionSelector(selector); err != nil {
			return "", nil, err
		}
		return cloudflareDecisionTarget(p.BaseURL, selector), replaceModelInBody(body, selector), nil
	}
	out := body
	if cur := translate.ExtractModel(body); cur != selector && selector != "" {
		out = replaceModelInBody(body, selector)
	}
	return systemOneTarget(p.BaseURL), out, nil
}

// validateDecisionSelector rejects a selector that cannot be a single path
// segment. Decision selectors are plain model names ("clef-flash", "kev-4b").
func validateDecisionSelector(selector string) error {
	if selector == "" {
		return nil
	}
	if strings.ContainsAny(selector, "?#") {
		return errors.New("model id must not contain '?' or '#'")
	}
	if strings.Contains(selector, "/") {
		return errors.New("model id must not contain '/'")
	}
	return nil
}

// systemOneTarget appends the System One path to a provider base URL without
// doubling an existing /v1 segment.
func systemOneTarget(baseURL string) string {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	switch {
	case strings.HasSuffix(b, "/systemone"):
		return b
	case strings.HasSuffix(b, "/v1"):
		return b + "/systemone"
	default:
		return b + "/v1/systemone"
	}
}

// cloudflareDecisionTarget builds the Workers AI run URL for a decision model:
//
//	<base>/ai/run/@cf/cloudflare/<selector>
//
// The base may be given with or without the trailing /ai/run segment. The
// selector is path-escaped (P3-2: it was concatenated raw into the URL).
func cloudflareDecisionTarget(baseURL, selector string) string {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if selector == "" {
		selector = "clef"
	}
	seg := url.PathEscape(selector)
	if strings.Contains(b, "/ai/run/@cf/") {
		return b
	}
	if i := strings.Index(b, "/ai/run"); i >= 0 {
		return b[:i+len("/ai/run")] + "/@cf/cloudflare/" + seg
	}
	return b + "/@cf/cloudflare/" + seg
}

// notDecisionModelMessage builds the actionable rejection for a model that is
// not a System One decision model. It names the correct endpoint based on what
// the model actually IS, so a client that sent a chat or image model to
// /v1/systemone is told where to go instead of receiving a generic 404.
func notDecisionModelMessage(model string) string {
	if videogen.IsVideoModel(model) {
		return "model '" + model + "' is a video-generation model, not a decision model; POST /v1/videos is the video endpoint (POST /v1/systemone accepts noul/choice/score decision models)"
	}
	if imagegen.IsImageModel(model) {
		return "model '" + model + "' is an image model, not a decision model; POST /v1/images/generations is the image endpoint (POST /v1/systemone accepts noul/choice/score decision models)"
	}
	return "model '" + model + "' is not a decision model; POST /v1/systemone accepts noul/choice/score decision models (for text use /v1/chat/completions)"
}
