package proxy

import (
	"strings"

	"ai-gateway/internal/models"
)

// UpstreamAPI is the wire protocol a model speaks on its provider.
type UpstreamAPI string

const (
	UpstreamChat      UpstreamAPI = "chat"
	UpstreamResponses UpstreamAPI = "responses"
	UpstreamMessages  UpstreamAPI = "messages"
	UpstreamUnknown   UpstreamAPI = "unknown"
)

// isMultiProtocolProvider reports whether one provider entry can serve models
// across several upstream endpoints (chat completions, responses, anthropic
// messages) behind a single base URL and key.
//
// OpenCode Go/Zen are the canonical examples: a single subscription key at
// https://opencode.ai/zen/go/v1 (or .../zen/v1) serves glm/kimi via
// /chat/completions, grok/gpt/muse-spark via /responses, and qwen/minimax/
// claude via /v1/messages. The gateway previously keyed the upstream path off
// the provider TYPE, so no single type could serve all three.
func isMultiProtocolProvider(p *models.Provider) bool {
	if p == nil {
		return false
	}
	base := strings.ToLower(strings.TrimSpace(p.BaseURL))
	name := strings.ToLower(strings.TrimSpace(p.Name))
	if strings.Contains(base, "opencode.ai/zen") || strings.Contains(base, "opencode.ai/go") {
		return true
	}
	for _, pre := range []string{"opencode-go", "opencode_go", "opencodego", "opencode-zen", "opencode_zen"} {
		if name == pre || strings.HasPrefix(name, pre+"/") || strings.HasPrefix(name, pre+"-") || strings.HasPrefix(name, pre+"_") {
			return true
		}
	}
	return false
}

// isOpencodeGo distinguishes the Go product (minimax served via messages)
// from Zen (minimax served via chat completions). Go bases contain /zen/go;
// Zen bases contain /zen without /go.
func isOpencodeGo(p *models.Provider) bool {
	if p == nil {
		return false
	}
	base := strings.ToLower(p.BaseURL)
	if strings.Contains(base, "/zen/go") {
		return true
	}
	name := strings.ToLower(p.Name)
	return strings.Contains(name, "go") && isMultiProtocolProvider(p) && !strings.Contains(base, "/zen/v1")
}

// UpstreamAPIForModel returns which upstream endpoint a model speaks on a
// multi-protocol provider, using the published OpenCode Go/Zen tables.
// Prefix matching keeps future dated versions working (glm-5.4, qwen3.9).
// Non-multi providers return UpstreamUnknown (caller falls back to type).
func UpstreamAPIForModel(p *models.Provider, model string) UpstreamAPI {
	if !isMultiProtocolProvider(p) {
		return UpstreamUnknown
	}
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 && i+1 < len(m) {
		m = m[i+1:]
	}
	if m == "" {
		return UpstreamUnknown
	}
	// Gemini uses a bespoke /v1/models/<id> path the gateway does not speak.
	if strings.HasPrefix(m, "gemini-") {
		return UpstreamUnknown
	}
	// Responses family: gpt-*, grok-*, muse-spark-* (both Go and Zen).
	for _, pre := range []string{"grok-", "gpt-", "muse-spark-"} {
		if strings.HasPrefix(m, pre) {
			return UpstreamResponses
		}
	}
	// Messages family is product-specific for minimax.
	if isOpencodeGo(p) {
		for _, pre := range []string{"minimax-", "qwen", "claude-"} {
			if strings.HasPrefix(m, pre) {
				return UpstreamMessages
			}
		}
		return UpstreamChat
	}
	for _, pre := range []string{"claude-", "qwen"} {
		if strings.HasPrefix(m, pre) {
			return UpstreamMessages
		}
	}
	return UpstreamChat
}

// correctInboundFor hints which gateway endpoint a client should call for a
// model on a multi-protocol provider. Empty when unknown or already correct.
func correctInboundFor(api UpstreamAPI) string {
	switch api {
	case UpstreamChat:
		return "/v1/chat/completions"
	case UpstreamMessages:
		return "/v1/messages"
	case UpstreamResponses:
		return "/v1/responses"
	default:
		return ""
	}
}
