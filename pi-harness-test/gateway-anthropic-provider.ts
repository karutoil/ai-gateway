import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { createProvider, envApiKeyAuth } from "@earendil-works/pi-ai";
import { anthropicMessagesApi } from "@earendil-works/pi-ai/api/anthropic-messages.lazy";

/**
 * PI extension that registers the local AI Gateway as an Anthropic provider.
 *
 * Routes `muse-spark-1.2-contributor` via `http://localhost:8989` (ckff-muse, anthropic @ https://ckff.dev).
 * The gateway is STRICT: /v1/messages is the only valid endpoint for anthropic models — openai endpoints 400.
 * This provider uses `api: "anthropic-messages"` so PI sends native Anthropic requests through the gateway.
 *
 * Usage:
 *   pi --extension /home/karutoil/ai-gateway/pi-harness-test/gateway-anthropic-provider.js -p "do thing"
 *   # or set gateway key in env:
 *   GATEWAY_API_KEY=sk-gw-xxx pi --extension ... --provider gateway-anthropic --model muse-spark-1.2-contributor -p "hi"
 *
 * Env: GATEWAY_API_KEY / GATEWAY_KEY (the gateway's sk-gw-* key)
 */

const GATEWAY_URL = process.env.GATEWAY_URL ?? "http://localhost:8989";
const GATEWAY_KEY = process.env.GATEWAY_API_KEY ?? process.env.GATEWAY_KEY ?? process.env.ANTHROPIC_API_KEY ?? "";

// The ckff-muse provider's model
const modelId = "muse-spark-1.2-contributor";

export default function extension(pi: ExtensionAPI) {
  const provider = createProvider({
    id: "gateway-anthropic",
    name: "AI Gateway (Anthropic)",
    baseUrl: GATEWAY_URL,
    auth: {
      apiKey: envApiKeyAuth("Gateway key", ["GATEWAY_API_KEY", "GATEWAY_KEY", "ANTHROPIC_API_KEY"]),
    },
    models: [
      {
        id: modelId,
        name: "Muse Spark 1.2 (via gateway ckff-muse)",
        api: "anthropic-messages",
        provider: "gateway-anthropic",
        baseUrl: GATEWAY_URL,
        reasoning: true,
        input: ["text", "image"],
        cost: { input: 0.3, output: 1.2, cacheRead: 0.06, cacheWrite: 0 },
        contextWindow: 1_000_000,
        maxTokens: 131072,
        thinkingLevelMap: {
          off: null,
          minimal: null,
          low: "low",
          medium: "medium",
          high: "high",
          xhigh: "xhigh",
          max: "max",
        },
        headers: {
          // PI sends x-api-key; gateway accepts that or Authorization. No extra headers needed.
        },
      },
      // Also expose same model under a second id to avoid colliding with built-in opencode-go's id
      {
        id: "gateway-muse-spark-1.1",
        name: "Muse Spark 1.1 (via gateway)",
        api: "anthropic-messages",
        provider: "gateway-anthropic",
        baseUrl: GATEWAY_URL,
        reasoning: true,
        input: ["text", "image"],
        cost: { input: 0.3, output: 1.2, cacheRead: 0.06, cacheWrite: 0 },
        contextWindow: 1_000_000,
        maxTokens: 64000,
      },
    ],
    api: anthropicMessagesApi(),
  });

  // Register the provider with the PI models collection
  // PI's extension API exposes models via pi.models or pi.registerProvider depending on version
  // Fallback: use the global models collection if pi.models is available
  if ((pi as any).models?.setProvider) {
    (pi as any).models.setProvider(provider);
  } else if ((pi as any).registerProvider) {
    (pi as any).registerProvider(provider);
  } else {
    // Fabric: register as a provider via events if needed
    pi.events?.emit?.("fabric:provider:register" as any, { provider } as any);
    // Also try direct import fallback
    try {
      // dynamic import for pi-ai models singleton
      import("@earendil-works/pi-ai").then((m: any) => {
        if (m.builtinModels) {
          // no-op, rely on models.setProvider above
        }
      });
    } catch {}
  }

  pi.log?.info?.(`[gateway-anthropic] registered provider gateway-anthropic -> ${GATEWAY_URL} model=${modelId} key=${GATEWAY_KEY ? GATEWAY_KEY.slice(0, 12) + "..." : "(no key — set GATEWAY_API_KEY)"}`);
}
