package proxy

import (
	"testing"

	"ai-gateway/internal/models"
)

func multiProvider(name, base string) *models.Provider {
	return &models.Provider{Name: name, Type: models.ProviderOpenAICompatible, BaseURL: base}
}

func TestIsMultiProtocolProvider(t *testing.T) {
	if !isMultiProtocolProvider(multiProvider("opencode-go", "https://opencode.ai/zen/go/v1")) {
		t.Fatal("expected opencode go base to be multi-protocol")
	}
	if !isMultiProtocolProvider(multiProvider("zen", "https://opencode.ai/zen/v1")) {
		t.Fatal("expected opencode zen base to be multi-protocol")
	}
	if isMultiProtocolProvider(&models.Provider{Name: "openai", Type: models.ProviderOpenAI, BaseURL: "https://api.openai.com/v1"}) {
		t.Fatal("plain openai must not be multi-protocol")
	}
}

func TestUpstreamAPIForModelGo(t *testing.T) {
	p := multiProvider("opencode-go", "https://opencode.ai/zen/go/v1")
	cases := map[string]UpstreamAPI{
		"glm-5.3-flash":              UpstreamChat,
		"opencode-go/glm-5.3-flash":  UpstreamChat,
		"kimi-k3":                    UpstreamChat,
		"deepseek-v4-pro":            UpstreamChat,
		"grok-4.6":                   UpstreamResponses,
		"gpt-5.6-luna":               UpstreamResponses,
		"muse-spark-1.2-contributor": UpstreamResponses,
		"qwen3.8-max":                UpstreamMessages,
		"minimax-m3":                 UpstreamMessages,
	}
	for model, want := range cases {
		if got := UpstreamAPIForModel(p, model); got != want {
			t.Fatalf("model %q: got %q want %q", model, got, want)
		}
	}
}

func TestUpstreamAPIForModelZenMinimaxDiffers(t *testing.T) {
	goP := multiProvider("opencode-go", "https://opencode.ai/zen/go/v1")
	zenP := multiProvider("opencode-zen", "https://opencode.ai/zen/v1")
	if got := UpstreamAPIForModel(goP, "minimax-m3"); got != UpstreamMessages {
		t.Fatalf("go minimax: got %q want messages", got)
	}
	if got := UpstreamAPIForModel(zenP, "minimax-m3"); got != UpstreamChat {
		t.Fatalf("zen minimax: got %q want chat", got)
	}
	if got := UpstreamAPIForModel(zenP, "claude-sonnet-4-5"); got != UpstreamMessages {
		t.Fatalf("zen claude: got %q want messages", got)
	}
}

func TestUpstreamAPIForModelNonMulti(t *testing.T) {
	p := &models.Provider{Name: "openai", Type: models.ProviderOpenAI, BaseURL: "https://api.openai.com/v1"}
	if got := UpstreamAPIForModel(p, "gpt-4o"); got != UpstreamUnknown {
		t.Fatalf("non-multi must be unknown, got %q", got)
	}
}
