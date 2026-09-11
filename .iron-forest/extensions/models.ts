import { join } from "node:path";
import { ModelRuntime, type ExtensionAPI } from "@earendil-works/pi-coding-agent";

// Static OpenRouter catalog metadata from /api/v1/models, 2026-09-10.
// Pi 0.84.4 predates this model. Keep its other OpenRouter models and transport;
// Forest's explicit models.json supplies trace metadata and session affinity.
export default async function (pi: ExtensionAPI) {
  // Retain the Runner's explicit overrides without operator state, catalog
  // refresh, or auth checks. The public SDK also works in standalone Pi.
  const agentDir = process.env.PI_CODING_AGENT_DIR;
  if (!agentDir) throw new Error("PI_CODING_AGENT_DIR must name the Forest Run directory");
  const catalog = await ModelRuntime.create({
    modelsPath: join(agentDir, "models.json"),
    refreshOnCreate: false,
    allowModelNetwork: false,
  });
  const id = "deepseek/deepseek-v4.1-flash";
  pi.registerProvider("openrouter", {
    baseUrl: "https://openrouter.ai/api/v1",
    api: "openai-completions",
    apiKey: "$OPENROUTER_API_KEY",
    models: [
      ...catalog.getModels("openrouter").filter((model) => model.id !== id),
      {
        id,
        name: "DeepSeek: DeepSeek V4.1 Flash",
        reasoning: true,
        thinkingLevelMap: {
          minimal: null,
          low: "low",
          medium: null,
          high: "high",
          xhigh: null,
          max: "max",
        },
        input: ["text", "image"],
        contextWindow: 1048576,
        maxTokens: 384000,
        // Catalog estimates per million tokens, not provider billing authority.
        cost: { input: 0.3, output: 1.2, cacheRead: 0.006, cacheWrite: 0 },
        // Pi applies provider-wide compat before extension model replacement.
        // Preserve the Runner's session-affinity contract on this new entry.
        compat: {
          supportsDeveloperRole: false,
          thinkingFormat: "openrouter",
          sendSessionAffinityHeaders: true,
          sessionAffinityFormat: "openrouter",
        },
      },
    ],
  });
}
