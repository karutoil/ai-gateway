package antigravity

// Public model catalog mirrors pi-antigravity ANTIGRAVITY_MODELS.
// Costs are per 1M tokens (input/output). Kept here so discovery can seed
// provider_models without a live OAuth token.
type PublicModel struct {
	ID             string
	Name           string
	ContextWindow  int
	MaxTokens      int
	InputCost      float64
	OutputCost     float64
	CacheReadCost  float64
	CacheWriteCost float64
}

var PublicModels = []PublicModel{
	{ID: "gemini-3.8-flash", Name: "Gemini 3.8 Flash (Antigravity)", ContextWindow: 1048576, MaxTokens: 65536, InputCost: 0.1, OutputCost: 0.4, CacheReadCost: 0.025, CacheWriteCost: 0.1},
	{ID: "gemini-3.7-flash", Name: "Gemini 3.7 Flash (Antigravity)", ContextWindow: 1048576, MaxTokens: 65536, InputCost: 0.1, OutputCost: 0.4, CacheReadCost: 0.025, CacheWriteCost: 0.1},
	{ID: "gemini-3.6-flash", Name: "Gemini 3.6 Flash (Antigravity)", ContextWindow: 1048576, MaxTokens: 65536, InputCost: 0.1, OutputCost: 0.4, CacheReadCost: 0.025, CacheWriteCost: 0.1},
	{ID: "gemini-3.5-flash", Name: "Gemini 3.5 Flash (Antigravity)", ContextWindow: 1048576, MaxTokens: 65536, InputCost: 0.1, OutputCost: 0.4, CacheReadCost: 0.025, CacheWriteCost: 0.1},
	{ID: "gemini-3.1-pro", Name: "Gemini 3.1 Pro (Antigravity)", ContextWindow: 1048576, MaxTokens: 65535, InputCost: 1.25, OutputCost: 5.0, CacheReadCost: 0.3125, CacheWriteCost: 1.25},
	{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6 (Antigravity)", ContextWindow: 200000, MaxTokens: 64000, InputCost: 3.0, OutputCost: 15.0, CacheReadCost: 0.3, CacheWriteCost: 3.75},
	{ID: "claude-opus-4-6", Name: "Claude Opus 4.6 (Antigravity)", ContextWindow: 250000, MaxTokens: 64000, InputCost: 15.0, OutputCost: 75.0, CacheReadCost: 1.5, CacheWriteCost: 18.75},
	{ID: "gpt-oss-120b", Name: "GPT-OSS 120B (Antigravity)", ContextWindow: 131072, MaxTokens: 32768, InputCost: 0.6, OutputCost: 2.4, CacheReadCost: 0.15, CacheWriteCost: 0.6},
}

type routing struct {
	Off              string
	Minimal, Low     string
	Medium, High     string
	XHigh            string
	DefaultRequestID string
}

var routingTable = map[string]routing{
	"claude-opus-4-6":   {Minimal: "claude-opus-4-6-thinking", Low: "claude-opus-4-6-thinking", Medium: "claude-opus-4-6-thinking", High: "claude-opus-4-6-thinking", DefaultRequestID: "claude-opus-4-6-thinking"},
	"claude-sonnet-4-6": {Off: "claude-sonnet-4-6", Minimal: "claude-sonnet-4-6", Low: "claude-sonnet-4-6", Medium: "claude-sonnet-4-6", High: "claude-sonnet-4-6", XHigh: "claude-sonnet-4-6", DefaultRequestID: "claude-sonnet-4-6"},
	"gemini-3.1-pro":    {Off: "gemini-3.1-pro-low", Minimal: "gemini-3.1-pro-low", Low: "gemini-3.1-pro-low", Medium: "gemini-3.1-pro-low", High: "gemini-pro-agent", XHigh: "gemini-pro-agent", DefaultRequestID: "gemini-3.1-pro-low"},
	"gemini-3.8-flash":  {Off: "gemini-3.8-flash-low", Minimal: "gemini-3.8-flash-low", Low: "gemini-3.8-flash-low", Medium: "gemini-3.8-flash-medium", High: "gemini-3.8-flash-high", XHigh: "gemini-3.8-flash-high", DefaultRequestID: "gemini-3.8-flash-low"},
	"gemini-3.7-flash":  {Off: "gemini-3.7-flash-low", Minimal: "gemini-3.7-flash-low", Low: "gemini-3.7-flash-low", Medium: "gemini-3.7-flash-medium", High: "gemini-3.7-flash-high", XHigh: "gemini-3.7-flash-high", DefaultRequestID: "gemini-3.7-flash-low"},
	"gemini-3.6-flash":  {Off: "gemini-3.6-flash-low", Minimal: "gemini-3.6-flash-low", Low: "gemini-3.6-flash-low", Medium: "gemini-3.6-flash-medium", High: "gemini-3.6-flash-high", XHigh: "gemini-3.6-flash-high", DefaultRequestID: "gemini-3.6-flash-low"},
	"gemini-3.5-flash":  {Off: "gemini-3.5-flash-extra-low", Minimal: "gemini-3.5-flash-extra-low", Low: "gemini-3.5-flash-extra-low", Medium: "gemini-3.5-flash-low", High: "gemini-3-flash-agent", XHigh: "gemini-3-flash-agent", DefaultRequestID: "gemini-3.5-flash-extra-low"},
	"gpt-oss-120b":      {Off: "gpt-oss-120b-medium", Minimal: "gpt-oss-120b-medium", Low: "gpt-oss-120b-medium", Medium: "gpt-oss-120b-medium", High: "gpt-oss-120b-medium", DefaultRequestID: "gpt-oss-120b-medium"},
}

// ResolveRuntime maps a public model id + reasoning effort to a backend runtime id.
func ResolveRuntime(publicID, effort string) string {
	r, ok := routingTable[publicID]
	if !ok {
		return publicID
	}
	switch effort {
	case "", "off":
		if r.Off != "" {
			return r.Off
		}
		if r.Minimal != "" {
			return r.Minimal
		}
		if r.Low != "" {
			return r.Low
		}
		return r.DefaultRequestID
	case "minimal":
		if r.Minimal != "" {
			return r.Minimal
		}
	case "low":
		if r.Low != "" {
			return r.Low
		}
	case "medium":
		if r.Medium != "" {
			return r.Medium
		}
	case "high":
		if r.High != "" {
			return r.High
		}
	case "xhigh", "max":
		if r.XHigh != "" {
			return r.XHigh
		}
		if r.High != "" {
			return r.High
		}
	}
	if r.Low != "" {
		return r.Low
	}
	if r.DefaultRequestID != "" {
		return r.DefaultRequestID
	}
	return publicID
}

// FallbackRuntime returns the previous-generation runtime when a next-gen id 404s.
func FallbackRuntime(runtime string) string {
	switch {
	case startsWith(runtime, "gemini-3.8-flash-"):
		return replacePrefix(runtime, "gemini-3.8-flash-", "gemini-3.7-flash-")
	case runtime == "gemini-3.8-flash":
		return "gemini-3.7-flash-low"
	case startsWith(runtime, "gemini-3.7-flash-"):
		return replacePrefix(runtime, "gemini-3.7-flash-", "gemini-3.6-flash-")
	case runtime == "gemini-3.7-flash":
		return "gemini-3.6-flash-low"
	}
	return ""
}

// ThinkingBudget mirrors pi-antigravity getThinkingConfig wire values.
func ThinkingBudget(runtime, effort string) (includeThoughts bool, budget int) {
	if effort == "" || effort == "off" {
		return false, 0
	}
	switch {
	case startsWith(runtime, "claude-"):
		return true, 1024
	case startsWith(runtime, "gpt-oss-"):
		return true, 8192
	case startsWith(runtime, "gemini-3.5-flash") || runtime == "gemini-3-flash-agent":
		switch effort {
		case "high", "xhigh", "max":
			return true, 10000
		case "medium":
			return true, 4000
		default:
			return true, 1000
		}
	case startsWith(runtime, "gemini-3.1-pro") || runtime == "gemini-pro-agent":
		if effort == "high" || effort == "xhigh" || effort == "max" {
			return true, 10001
		}
		return true, 1001
	case startsWith(runtime, "gemini-"):
		switch effort {
		case "high", "xhigh", "max":
			return true, -1
		case "medium":
			return true, 4000
		default:
			return true, 1000
		}
	}
	return false, 0
}

// MaxOutputTokens returns the verified backend cap per runtime id.
func MaxOutputTokens(publicID, runtime string) int {
	caps := map[string]int{
		"gemini-3.8-flash": 65536, "gemini-3.8-flash-low": 65536, "gemini-3.8-flash-medium": 65536, "gemini-3.8-flash-high": 65536,
		"gemini-3.7-flash-low": 65536, "gemini-3.7-flash-medium": 65536, "gemini-3.7-flash-high": 65536,
		"gemini-3.6-flash-low": 65536, "gemini-3.6-flash-medium": 65536, "gemini-3.6-flash-high": 65536,
		"gemini-3.5-flash-extra-low": 65536, "gemini-3.5-flash-low": 65536, "gemini-3-flash-agent": 65536,
		"gemini-3.1-pro-low": 65535, "gemini-pro-agent": 65535,
		"claude-opus-4-6": 64000, "claude-opus-4-6-thinking": 64000, "claude-sonnet-4-6": 64000,
		"gpt-oss-120b": 32768, "gpt-oss-120b-medium": 32768,
	}
	if v, ok := caps[runtime]; ok {
		return v
	}
	if v, ok := caps[publicID]; ok {
		return v
	}
	switch {
	case startsWith(runtime, "claude-"):
		return 64000
	case startsWith(runtime, "gpt-oss-"):
		return 32768
	case startsWith(runtime, "gemini-"):
		return 65536
	}
	return 8192
}

func startsWith(s, pre string) bool {
	return len(s) >= len(pre) && s[:len(pre)] == pre
}

func replacePrefix(s, old, new string) string {
	if startsWith(s, old) {
		return new + s[len(old):]
	}
	return s
}
