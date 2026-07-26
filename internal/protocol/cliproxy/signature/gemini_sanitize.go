package signature

import (
	"strings"

	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// GeminiReplaySignatureOrBypass returns a Gemini-replayable thoughtSignature.
// Compatible Gemini signatures are normalized and preserved. Missing, unknown,
// or cross-provider signatures are replaced with Gemini's bypass sentinel.
func GeminiReplaySignatureOrBypass(rawSignature string, blockKind SignatureBlockKind) string {
	if signature, ok := CompatibleSignatureForProviderBlock(SignatureProviderGemini, rawSignature, blockKind); ok {
		return signature
	}
	decision := DecideSignatureCompatibility(SignatureProviderGemini, rawSignature, blockKind)
	if decision.Action == SignatureActionReplaceWithGeminiBypass && decision.ReplacementSignature != "" {
		return decision.ReplacementSignature
	}
	return GeminiSkipThoughtSignatureValidator
}

// SanitizeGeminiRequestThoughtSignatures applies Gemini replay policy to a
// Gemini-shaped request. Existing provider signatures stay on their original
// model parts. Only a missing or incompatible first functionCall gets the bypass
// sentinel; unsigned sibling calls remain unsigned, matching native Gemini
// parallel-call history. functionResponse parts never carry signatures.
func SanitizeGeminiRequestThoughtSignatures(payload []byte, contentsPath string) []byte {
	contentsPath = strings.TrimSpace(contentsPath)
	if contentsPath == "" {
		contentsPath = "contents"
	}

	contents := util.GetGJSONBytesNoCopy(payload, contentsPath)
	if !contents.IsArray() || !geminiContentsThoughtSignaturesNeedSanitize(contents) {
		return payload
	}

	contentsChanged := false
	contentItems := make([][]byte, 0, int(contents.Get("#").Int()))
	contents.ForEach(func(_, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			contentItems = append(contentItems, []byte(content.Raw))
			return true
		}

		isModelTurn := content.Get("role").String() == "model"
		firstFunctionCallSeen := false
		partsChanged := false
		partItems := make([][]byte, 0, int(parts.Get("#").Int()))
		parts.ForEach(func(_, part gjson.Result) bool {
			partJSON := []byte(part.Raw)
			rawSignature, hasSignature := geminiPartThoughtSignature(part)
			if part.Get("functionResponse").Exists() {
				if hasSignature {
					partJSON = deleteGeminiPartThoughtSignatureFields(partJSON)
					partsChanged = true
				}
				partItems = append(partItems, partJSON)
				return true
			}
			if !isModelTurn {
				partItems = append(partItems, partJSON)
				return true
			}

			hasFunctionCall := part.Get("functionCall").Exists()
			isFirstFunctionCall := hasFunctionCall && !firstFunctionCallSeen
			if hasFunctionCall {
				firstFunctionCallSeen = true
			}
			if !hasFunctionCall && !hasSignature {
				partItems = append(partItems, partJSON)
				return true
			}

			blockKind := SignatureBlockKindGeminiModelPart
			if hasFunctionCall {
				blockKind = SignatureBlockKindGeminiFunctionCall
			}
			decision := DecideSignatureCompatibility(SignatureProviderGemini, rawSignature, blockKind)
			replaySignature := ""
			switch {
			case isFirstFunctionCall:
				replaySignature = GeminiReplaySignatureOrBypass(rawSignature, blockKind)
			case hasSignature && decision.Action == SignatureActionPreserve && !IsGeminiThoughtSignatureBypass(SignaturePayloadWithoutProviderPrefix(rawSignature)):
				replaySignature = decision.NormalizedSignature
			case hasSignature:
				decision.Action = SignatureActionDropSignature
				decision.ReplacementSignature = ""
				if hasFunctionCall {
					decision.Reason = "unsigned sibling functionCalls preserve native parallel-call shape"
				} else {
					decision.Reason = "non-function model parts do not synthesize Gemini bypass signatures"
				}
			}

			partChanged := false
			if replaySignature != "" {
				if !hasNormalizedGeminiPartThoughtSignature(part, replaySignature) {
					partJSON = deleteGeminiPartThoughtSignatureFields(partJSON)
					partJSON, _ = sjson.SetBytes(partJSON, "thoughtSignature", replaySignature)
					partChanged = true
				}
			} else if hasSignature {
				partJSON = deleteGeminiPartThoughtSignatureFields(partJSON)
				partChanged = true
			}
			if partChanged {
				partsChanged = true
			}
			partItems = append(partItems, partJSON)
			return true
		})

		contentJSON := []byte(content.Raw)
		if partsChanged {
			contentJSON, _ = sjson.SetRawBytes(contentJSON, "parts", joinGeminiSignatureRawArray(partItems))
			contentsChanged = true
		}
		contentItems = append(contentItems, contentJSON)
		return true
	})

	if !contentsChanged {
		return payload
	}
	updated, errSet := sjson.SetRawBytes(payload, contentsPath, joinGeminiSignatureRawArray(contentItems))
	if errSet != nil {
		return payload
	}
	return updated
}

func geminiContentsThoughtSignaturesNeedSanitize(contents gjson.Result) bool {
	needsSanitize := false
	contents.ForEach(func(_, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		isModelTurn := content.Get("role").String() == "model"
		firstFunctionCallSeen := false
		parts.ForEach(func(_, part gjson.Result) bool {
			rawSignature, hasSignature := geminiPartThoughtSignature(part)
			if part.Get("functionResponse").Exists() {
				needsSanitize = hasSignature
				return !needsSanitize
			}
			if !isModelTurn {
				return true
			}
			hasFunctionCall := part.Get("functionCall").Exists()
			isFirstFunctionCall := hasFunctionCall && !firstFunctionCallSeen
			if hasFunctionCall {
				firstFunctionCallSeen = true
			}
			if isFirstFunctionCall {
				replaySignature := GeminiReplaySignatureOrBypass(rawSignature, SignatureBlockKindGeminiFunctionCall)
				needsSanitize = !hasNormalizedGeminiPartThoughtSignature(part, replaySignature)
				return !needsSanitize
			}
			if !hasSignature {
				return true
			}
			blockKind := SignatureBlockKindGeminiModelPart
			if hasFunctionCall {
				blockKind = SignatureBlockKindGeminiFunctionCall
			}
			decision := DecideSignatureCompatibility(SignatureProviderGemini, rawSignature, blockKind)
			if decision.Action != SignatureActionPreserve || IsGeminiThoughtSignatureBypass(SignaturePayloadWithoutProviderPrefix(rawSignature)) {
				needsSanitize = true
				return false
			}
			needsSanitize = !hasNormalizedGeminiPartThoughtSignature(part, decision.NormalizedSignature)
			return !needsSanitize
		})
		return !needsSanitize
	})
	return needsSanitize
}

var geminiPartThoughtSignaturePaths = []string{
	"thoughtSignature",
	"thought_signature",
	"functionCall.thoughtSignature",
	"functionCall.thought_signature",
	"functionResponse.thoughtSignature",
	"functionResponse.thought_signature",
	"extra_content.google.thought_signature",
}

func geminiPartThoughtSignature(part gjson.Result) (string, bool) {
	for _, path := range geminiPartThoughtSignaturePaths {
		result := part.Get(path)
		if result.Exists() {
			return result.String(), true
		}
	}
	return "", false
}

func hasNormalizedGeminiPartThoughtSignature(part gjson.Result, replaySignature string) bool {
	canonicalCount := 0
	part.ForEach(func(key, _ gjson.Result) bool {
		if key.String() == "thoughtSignature" {
			canonicalCount++
		}
		return true
	})
	canonical := part.Get("thoughtSignature")
	if canonicalCount != 1 || canonical.Type != gjson.String || canonical.String() != replaySignature {
		return false
	}
	for _, path := range geminiPartThoughtSignaturePaths[1:] {
		if part.Get(path).Exists() {
			return false
		}
	}
	return true
}

func deleteGeminiPartThoughtSignatureFields(payload []byte) []byte {
	for _, path := range geminiPartThoughtSignaturePaths {
		for gjson.GetBytes(payload, path).Exists() {
			updated, errDelete := sjson.DeleteBytes(payload, path)
			if errDelete != nil || len(updated) >= len(payload) {
				break
			}
			payload = updated
		}
	}
	return payload
}

func joinGeminiSignatureRawArray(items [][]byte) []byte {
	size := len(items) + 1
	for _, item := range items {
		size += len(item)
	}
	out := make([]byte, 0, size)
	out = append(out, '[')
	for index, item := range items {
		if index > 0 {
			out = append(out, ',')
		}
		out = append(out, item...)
	}
	return append(out, ']')
}
