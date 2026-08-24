package zedauth

import "strings"

// Zed service endpoints, client identity, and provider names.
const (
	ChannelType       = "zed"
	NativeSignInURL   = "https://zed.dev/native_app_signin"
	CloudBaseURL      = "https://cloud.zed.dev"
	CompletionsURL    = CloudBaseURL + "/completions"
	ModelsURL         = CloudBaseURL + "/models"
	LLMTokensURL      = CloudBaseURL + "/client/llm_tokens"
	CurrentUserURL    = CloudBaseURL + "/client/users/me"
	ZedVersion        = "1.8.2"
	maxCredentialSize = 256 << 10
	maxResponseSize   = 1 << 20
	ProviderOpenAI    = "open_ai"
	ProviderAnthropic = "anthropic"
	ProviderGoogle    = "google"
)

// ProviderForModel returns the Zed provider selected by the native client.
// Zed's catalog currently uses disjoint model prefixes, so persisting another
// provider field beside every channel model would only duplicate that source of truth.
func ProviderForModel(model string) (string, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(model, "gpt-"):
		return ProviderOpenAI, true
	case strings.HasPrefix(model, "claude-"):
		return ProviderAnthropic, true
	case strings.HasPrefix(model, "gemini-"):
		return ProviderGoogle, true
	default:
		return "", false
	}
}
