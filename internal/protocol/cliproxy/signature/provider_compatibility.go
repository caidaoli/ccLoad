package signature

import "strings"

// SignatureProvider identifies the provider family that issued a signature.
type SignatureProvider string

const (
	// SignatureProviderUnknown marks signatures with no recognized provider.
	SignatureProviderUnknown SignatureProvider = "unknown"
	// SignatureProviderClaude identifies Claude signatures.
	SignatureProviderClaude SignatureProvider = "claude"
	// SignatureProviderGemini identifies Gemini signatures.
	SignatureProviderGemini SignatureProvider = "gemini"
	// SignatureProviderGeminiBypass identifies Gemini bypass sentinels.
	SignatureProviderGeminiBypass SignatureProvider = "gemini_bypass"
	// SignatureProviderGPT identifies GPT or Codex signatures.
	SignatureProviderGPT SignatureProvider = "gpt"
)

// SignatureBlockKind identifies the signed content block carrying a signature.
type SignatureBlockKind string

const (
	// SignatureBlockKindUnknown marks signatures without block context.
	SignatureBlockKindUnknown SignatureBlockKind = "unknown"
	// SignatureBlockKindClaudeThinking identifies Claude thinking blocks.
	SignatureBlockKindClaudeThinking SignatureBlockKind = "claude_thinking"
	// SignatureBlockKindGeminiModelPart identifies signed Gemini model parts.
	SignatureBlockKindGeminiModelPart SignatureBlockKind = "gemini_model_part"
	// SignatureBlockKindGeminiFunctionCall identifies signed Gemini calls.
	SignatureBlockKindGeminiFunctionCall SignatureBlockKind = "gemini_function_call"
	// SignatureBlockKindGPTReasoning identifies GPT reasoning blocks.
	SignatureBlockKindGPTReasoning SignatureBlockKind = "gpt_reasoning"
)

// SignatureCompatibilityAction describes how to handle an incompatible signature.
type SignatureCompatibilityAction string

const (
	// SignatureActionPreserve keeps a provider-compatible signature.
	SignatureActionPreserve SignatureCompatibilityAction = "preserve"
	// SignatureActionDropBlock removes an incompatible signed block.
	SignatureActionDropBlock SignatureCompatibilityAction = "drop_block"
	// SignatureActionDropSignature removes only the incompatible signature.
	SignatureActionDropSignature SignatureCompatibilityAction = "drop_signature"
	// SignatureActionReplaceWithGeminiBypass substitutes Gemini's bypass sentinel.
	SignatureActionReplaceWithGeminiBypass SignatureCompatibilityAction = "replace_with_gemini_bypass"
	// SignatureActionNoCompatibleReplacement reports that no safe substitute exists.
	SignatureActionNoCompatibleReplacement SignatureCompatibilityAction = "no_compatible_replacement"
)

// SignatureCompatibilityDecision records the policy selected for one signature.
type SignatureCompatibilityDecision struct {
	TargetProvider       SignatureProvider
	DetectedProvider     SignatureProvider
	BlockKind            SignatureBlockKind
	Compatible           bool
	Action               SignatureCompatibilityAction
	ReplacementSignature string
	NormalizedSignature  string
	Reason               string
}

// SignatureProviderFromModelName maps common model names to the provider family
// whose signed history can be safely replayed for that model.
func SignatureProviderFromModelName(modelName string) SignatureProvider {
	lower := strings.ToLower(strings.TrimSpace(modelName))
	switch {
	case strings.Contains(lower, "claude"):
		return SignatureProviderClaude
	case strings.Contains(lower, "gemini"):
		return SignatureProviderGemini
	case strings.Contains(lower, "gpt"),
		strings.Contains(lower, "openai"),
		strings.Contains(lower, "codex"),
		strings.HasPrefix(lower, "o1"),
		strings.HasPrefix(lower, "o3"),
		strings.HasPrefix(lower, "o4"):
		return SignatureProviderGPT
	default:
		return SignatureProviderUnknown
	}
}

// DetectSignatureProvider classifies the provider family that can replay
// rawSignature. It intentionally uses Claude strict validation before Gemini
// detection because Gemini 3 signatures also decode from an E-prefixed base64
// string and can look Claude-like under shallow prefix checks.
func DetectSignatureProvider(rawSignature string) SignatureProvider {
	return DetectSignatureProviderForBlock(rawSignature, SignatureBlockKindUnknown)
}

// DetectSignatureProviderForBlock classifies rawSignature with block-kind
// context. UUID-shaped payloads are deliberately not classified as replay-safe
// provider signatures; callers targeting Gemini should replace them with the
// bypass sentinel.
func DetectSignatureProviderForBlock(rawSignature string, blockKind SignatureBlockKind) SignatureProvider {
	sig := strings.TrimSpace(rawSignature)
	if sig == "" {
		return SignatureProviderUnknown
	}

	if prefixedProvider, unprefixed, ok := SplitSignatureProviderPrefix(sig); ok {
		switch prefixedProvider {
		case SignatureProviderGemini:
			if IsGeminiThoughtSignatureBypass(unprefixed) {
				return SignatureProviderGeminiBypass
			}
			if isRecognizedGeminiProviderSignature(unprefixed, blockKind) {
				return SignatureProviderGemini
			}
		case SignatureProviderClaude:
			if IsValidClaudeThinkingSignature(unprefixed, ClaudeSignatureValidationOptions{Strict: true}) {
				return SignatureProviderClaude
			}
		case SignatureProviderGPT:
			if IsValidGPTReasoningSignature(unprefixed) {
				return SignatureProviderGPT
			}
		}
		return SignatureProviderUnknown
	}
	if strings.Contains(sig, "#") {
		return SignatureProviderUnknown
	}

	if IsGeminiThoughtSignatureBypass(sig) {
		return SignatureProviderGeminiBypass
	}
	if IsValidGPTReasoningSignature(sig) {
		return SignatureProviderGPT
	}
	if IsValidClaudeThinkingSignature(sig, ClaudeSignatureValidationOptions{Strict: true}) {
		return SignatureProviderClaude
	}
	if isRecognizedGeminiProviderSignature(sig, blockKind) {
		return SignatureProviderGemini
	}
	return SignatureProviderUnknown
}

// IsSignatureCompatibleWithProvider reports whether a signature can be replayed to a provider.
func IsSignatureCompatibleWithProvider(targetProvider SignatureProvider, rawSignature string) bool {
	decision := DecideSignatureCompatibility(targetProvider, rawSignature, SignatureBlockKindUnknown)
	return decision.Compatible
}

// DecideSignatureCompatibility returns the safe handling policy for replaying a
// signed block into targetProvider.
func DecideSignatureCompatibility(targetProvider SignatureProvider, rawSignature string, blockKind SignatureBlockKind) SignatureCompatibilityDecision {
	targetProvider = normalizeSignatureTargetProvider(targetProvider)
	if blockKind == "" {
		blockKind = SignatureBlockKindUnknown
	}

	detected := DetectSignatureProviderForBlock(rawSignature, blockKind)
	decision := SignatureCompatibilityDecision{
		TargetProvider:   targetProvider,
		DetectedProvider: detected,
		BlockKind:        blockKind,
	}

	if signatureProviderMatchesTarget(targetProvider, detected) {
		decision.Compatible = true
		decision.Action = SignatureActionPreserve
		decision.NormalizedSignature = normalizeCompatibleSignatureForProvider(targetProvider, rawSignature, blockKind)
		decision.Reason = "signature provider matches target provider"
		return decision
	}

	decision.Compatible = false
	switch targetProvider {
	case SignatureProviderGemini:
		if blockKind == SignatureBlockKindGeminiFunctionCall || blockKind == SignatureBlockKindGeminiModelPart || blockKind == SignatureBlockKindUnknown {
			decision.Action = SignatureActionReplaceWithGeminiBypass
			decision.ReplacementSignature = GeminiSkipThoughtSignatureValidator
			decision.Reason = "Gemini can bypass synthetic or incompatible model-part signatures with the documented sentinel"
			return decision
		}
		decision.Action = SignatureActionDropBlock
		decision.Reason = "signature is not compatible with Gemini and this block is not a bypass-safe Gemini model part"
	case SignatureProviderClaude:
		decision.Action = SignatureActionDropBlock
		decision.Reason = "Claude has no cross-provider bypass sentinel for thinking blocks"
	case SignatureProviderGPT:
		decision.Action = SignatureActionDropBlock
		decision.Reason = "GPT reasoning encrypted_content cannot be synthesized from another provider signature"
	default:
		decision.Action = SignatureActionNoCompatibleReplacement
		decision.Reason = "unknown target provider"
	}
	return decision
}

// SplitSignatureProviderPrefix separates a recognized provider cache prefix.
func SplitSignatureProviderPrefix(rawSignature string) (SignatureProvider, string, bool) {
	prefix, rest, ok := strings.Cut(strings.TrimSpace(rawSignature), "#")
	if !ok {
		return SignatureProviderUnknown, rawSignature, false
	}
	provider := SignatureProviderFromCachePrefix(prefix)
	if provider == SignatureProviderUnknown {
		return SignatureProviderUnknown, rawSignature, false
	}
	return provider, strings.TrimSpace(rest), true
}

// SignatureProviderFromCachePrefix maps this repo's explicit provider-prefix
// envelope to a provider family. This is intentionally stricter than
// SignatureProviderFromModelName so arbitrary model names such as
// "claude-cache#..." cannot be mistaken for trusted provider provenance.
func SignatureProviderFromCachePrefix(prefix string) SignatureProvider {
	switch strings.ToLower(strings.TrimSpace(prefix)) {
	case "claude", "anthropic":
		return SignatureProviderClaude
	case "gemini", "google":
		return SignatureProviderGemini
	case "openai", "gpt", "codex":
		return SignatureProviderGPT
	default:
		return SignatureProviderUnknown
	}
}

// SignaturePayloadWithoutProviderPrefix strips this repo's provider cache prefix
// when present. The returned string is the value that should be replayed to an
// upstream provider.
func SignaturePayloadWithoutProviderPrefix(rawSignature string) string {
	if _, unprefixed, ok := SplitSignatureProviderPrefix(rawSignature); ok {
		return unprefixed
	}
	return strings.TrimSpace(rawSignature)
}

// CompatibleSignatureForProvider returns a replayable provider-native signature
// for targetProvider. It strips this repo's provider prefix and normalizes
// Claude signatures to the format expected by the target when possible.
func CompatibleSignatureForProvider(targetProvider SignatureProvider, rawSignature string) (string, bool) {
	return CompatibleSignatureForProviderBlock(targetProvider, rawSignature, SignatureBlockKindUnknown)
}

// CompatibleSignatureForProviderBlock returns a replayable provider-native
// signature for targetProvider when the source block kind is known.
func CompatibleSignatureForProviderBlock(targetProvider SignatureProvider, rawSignature string, blockKind SignatureBlockKind) (string, bool) {
	decision := DecideSignatureCompatibility(targetProvider, rawSignature, blockKind)
	if !decision.Compatible || decision.NormalizedSignature == "" {
		return "", false
	}
	return decision.NormalizedSignature, true
}

// CompatibleAntigravityClaudeThinkingSignature returns the double-layer R-form
// required by Antigravity Claude replay. It only accepts signatures that are
// strictly identifiable as Claude, so Gemini E-prefixed envelopes cannot slip
// through the looser Antigravity bypass normalization path.
func CompatibleAntigravityClaudeThinkingSignature(rawSignature string) (string, bool) {
	if DetectSignatureProviderForBlock(rawSignature, SignatureBlockKindClaudeThinking) != SignatureProviderClaude {
		return "", false
	}
	normalized, err := NormalizeClaudeThinkingSignature(
		SignaturePayloadWithoutProviderPrefix(rawSignature),
		ClaudeSignatureValidationOptions{Strict: true},
	)
	if err != nil {
		return "", false
	}
	return normalized, true
}

func normalizeSignatureTargetProvider(provider SignatureProvider) SignatureProvider {
	switch provider {
	case SignatureProviderGeminiBypass:
		return SignatureProviderGemini
	default:
		return provider
	}
}

func signatureProviderMatchesTarget(target, detected SignatureProvider) bool {
	switch target {
	case SignatureProviderGemini:
		return detected == SignatureProviderGemini || detected == SignatureProviderGeminiBypass
	case SignatureProviderClaude:
		return detected == SignatureProviderClaude
	case SignatureProviderGPT:
		return detected == SignatureProviderGPT
	default:
		return false
	}
}

func normalizeCompatibleSignatureForProvider(targetProvider SignatureProvider, rawSignature string, blockKind SignatureBlockKind) string {
	payload := SignaturePayloadWithoutProviderPrefix(rawSignature)
	switch normalizeSignatureTargetProvider(targetProvider) {
	case SignatureProviderClaude:
		normalized, err := NormalizeClaudeProviderNativeThinkingSignature(payload)
		if err != nil {
			return ""
		}
		return normalized
	case SignatureProviderGemini:
		if IsGeminiThoughtSignatureBypass(payload) {
			return payload
		}
		if isRecognizedGeminiProviderSignature(payload, blockKind) {
			return payload
		}
	case SignatureProviderGPT:
		if IsValidGPTReasoningSignature(payload) {
			return payload
		}
	}
	return ""
}

func isRecognizedGeminiProviderSignature(rawSignature string, blockKind SignatureBlockKind) bool {
	return IsValidGeminiThoughtSignature(rawSignature, GeminiThoughtSignatureValidationOptions{RequireKnownEnvelope: true})
}
