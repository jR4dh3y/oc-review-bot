package runner

import "strings"

// Review models are addressed as provider/model, and the provider prefix
// selects the gateway that receives the pooled key. "opencode" is the built-in
// OpenCode Zen provider in both reviewer engines; "orcarouter" is provisioned
// per run as a custom OpenAI-compatible provider declared in the isolated
// engine configuration.
const (
	ProviderOpenCode   = "opencode"
	ProviderOrcaRouter = "orcarouter"
)

// OrcaRouterBaseURL is OrcaRouter's OpenAI-compatible API endpoint. The
// provider ID and this URL are fixed by the gateway; only the pooled key
// varies per deployment.
const OrcaRouterBaseURL = "https://api.orcarouter.ai/v1"

// gatewayProviderForModel maps a review model to the gateway provider that
// must hold the run's key. Every prefix except orcarouter keeps the engines'
// built-in OpenCode Zen provider, matching the behavior before additional
// gateways existed.
func gatewayProviderForModel(model string) string {
	provider, id, _ := strings.Cut(model, "/")
	if provider == ProviderOrcaRouter && id != "" {
		return ProviderOrcaRouter
	}
	return ProviderOpenCode
}

// modelIDForProvider strips the provider prefix and any #variant suffix so a
// custom provider config declares the exact model identifier a run requests.
func modelIDForProvider(model string) string {
	_, id, _ := strings.Cut(model, "/")
	id, _, _ = strings.Cut(id, "#")
	return id
}

// openCodeOrcaRouterProvider is the custom provider block for the isolated
// opencode.json. The key never enters this block: it travels through the
// isolated auth store, keyed by the provider ID.
func openCodeOrcaRouterProvider(modelID string) map[string]any {
	return map[string]any{
		"npm":  "@ai-sdk/openai-compatible",
		"name": "OrcaRouter",
		"options": map[string]any{
			"baseURL": OrcaRouterBaseURL,
		},
		"models": map[string]any{
			modelID: map[string]any{"name": modelID},
		},
	}
}

// piOrcaRouterModels is the models.json content declaring the OrcaRouter
// custom provider for pi. The chat-completions API is the most compatible
// wire format pi offers for an OpenAI-compatible gateway.
func piOrcaRouterModels(apiKey, modelID string) map[string]any {
	return map[string]any{
		"providers": map[string]any{
			ProviderOrcaRouter: map[string]any{
				"baseUrl": OrcaRouterBaseURL,
				"api":     "openai-completions",
				"apiKey":  apiKey,
				"models":  []map[string]any{{"id": modelID, "name": modelID}},
			},
		},
	}
}
