/**
 * PI extension — registers the local AI Gateway as an Anthropic provider.
 *
 * ckff-muse (anthropic @ https://ckff.dev) is exposed only via /v1/messages.
 * OpenAI endpoints (chat/completions, completions, responses) 400 for that model.
 * This provider uses `api: "anthropic-messages"` + the gateway's local URL so PI
 * sends native Anthropic requests. The gateway key is read from env at runtime.
 *
 * Usage:
 *   GATEWAY_API_KEY=sk-gw-xxx pi -e /home/karutoil/ai-gateway/pi-harness-test/gateway-anthropic-provider.js \
 *     --provider gateway-anthropic --model muse-spark-1.2-contributor -p "hi"
 */

export default function extension(pi) {
  pi.registerProvider("gateway-anthropic", {
    baseUrl: process.env.GATEWAY_URL || "http://localhost:8989",
    apiKey: "$GATEWAY_API_KEY",
    api: "anthropic-messages",
    models: [
      {
        id: "muse-spark-1.2-contributor",
        name: "Muse Spark 1.2 (gateway ckff-muse)",
        reasoning: true,
        input: ["text", "image"],
        cost: { input: 0.3, output: 1.2, cacheRead: 0.06, cacheWrite: 0 },
        contextWindow: 1_000_000,
        maxTokens: 131072,
      },
      {
        id: "muse-spark-1.1",
        name: "Muse Spark 1.1 (gateway ckff-muse)",
        reasoning: true,
        input: ["text", "image"],
        cost: { input: 0.3, output: 1.2, cacheRead: 0.06, cacheWrite: 0 },
        contextWindow: 1_000_000,
        maxTokens: 32000,
      },
    ],
  });
}
