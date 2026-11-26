// Package gemini provides request translation functionality for Antigravity to Gemini API compatibility.
// It handles parsing and transforming Antigravity API requests into Gemini API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Antigravity API format and Gemini API's expected format.
package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	"ccLoad/internal/protocol/cliproxy/gemini/common"
	"ccLoad/internal/protocol/cliproxy/signature"
	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/MoeForge/moego/ojson"
)

// ConvertGeminiRequestToAntigravity parses and transforms a Antigravity API request into Gemini API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Gemini API.
// The function performs the following transformations:
// 1. Extracts the model information from the request
// 2. Restructures the JSON to match Gemini API format
// 3. Converts system instructions to the expected format
// 4. Fixes CLI tool response format and grouping
//
// Parameters:
//   - modelName: The name of the model to use for the request (unused in current implementation)
//   - rawJSON: The raw JSON request data from the Antigravity API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini API format
func ConvertGeminiRequestToAntigravity(modelName string, inputRawJSON []byte, _ bool) ([]byte, error) {
	rawJSON := inputRawJSON
	functionNameMap := util.SanitizedFunctionNameMap(inputRawJSON)
	// Keep the envelope in []byte form. Round-tripping through string copies the
	// entire request, which dominates allocations for large inline data. Fill the
	// small envelope fields first so the payload is only spliced in once.
	envelope, _ := ojson.SetBytes([]byte(`{"project":"","request":{},"model":""}`), "model", modelName)
	rawJSON, _ = ojson.SetRawBytes(envelope, "request", rawJSON)
	if util.GetGJSONBytesNoCopy(rawJSON, "request.model").Exists() {
		rawJSON, _ = ojson.DeleteBytes(rawJSON, "request.model")
	}

	fixedJSON, errFixCLIToolResponse := fixCLIToolResponse(rawJSON)
	if errFixCLIToolResponse != nil {
		return []byte{}, nil
	}
	rawJSON = fixedJSON

	rawJSON = signature.SanitizeGeminiSystemInstruction(rawJSON, "request.contents")
	if systemInstructionResult := util.GetGJSONBytesNoCopy(rawJSON, "request.system_instruction"); systemInstructionResult.Exists() {
		camel := util.GetGJSONBytesNoCopy(rawJSON, "request.systemInstruction")
		if !hasAntigravitySystemInstruction(camel) && hasAntigravitySystemInstruction(systemInstructionResult) {
			rawJSON, _ = ojson.SetRawBytes(rawJSON, "request.systemInstruction", []byte(systemInstructionResult.Raw))
		}
		rawJSON, _ = ojson.DeleteBytes(rawJSON, "request.system_instruction")
	}

	rawJSON = normalizeAntigravityMedia(rawJSON)
	rawJSON = normalizeGeminiGenerationConfigResponseSchema(rawJSON)

	// Normalize roles in request.contents: default to valid values if missing/invalid.
	// The contents array is only materialized when a role actually changes; copying
	// every content up front duplicates the whole payload for large inline data.
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if contents.IsArray() && geminiContentRolesNeedNormalization(contents) {
		contentItems := translatorcommon.NewRawArrayItems(contents.Get("#").Int())
		previousRole := ""
		contents.ForEach(func(_, value ojson.Result) bool {
			role := value.Get("role").String()
			content := []byte(value.Raw)
			if role != "user" && role != "model" {
				if translatorcommon.ContentHasGeminiFunctionResponse([]byte(value.Raw)) {
					role = "user"
				} else if previousRole == "" || previousRole == "model" {
					role = "user"
				} else {
					role = "model"
				}
				content, _ = ojson.SetBytes(content, "role", role)
			}
			previousRole = role
			contentItems = append(contentItems, content)
			return true
		})
		rawJSON, _ = ojson.SetRawBytes(rawJSON, "request.contents", translatorcommon.JoinRawArray(contentItems))
	}

	toolsResult := util.GetGJSONBytesNoCopy(rawJSON, "request.tools")
	if toolsResult.IsArray() {
		seenFunctionNames := make(map[string]struct{})
		toolsChanged := false
		var toolItems [][]byte
		toolsResult.ForEach(func(toolIndex, tool ojson.Result) bool {
			toolJSON := []byte(tool.Raw)
			toolChanged := false
			for _, key := range []string{"functionDeclarations", "function_declarations"} {
				declarations := tool.Get(key)
				if !declarations.IsArray() {
					continue
				}

				declarationsChanged := false
				var declarationItems [][]byte
				declarations.ForEach(func(_, declaration ojson.Result) bool {
					nameResult := declaration.Get("name")
					originalName := nameResult.String()
					mappedName := util.MapSanitizedFunctionName(functionNameMap, originalName)
					if mappedName != "" {
						if _, exists := seenFunctionNames[mappedName]; exists {
							declarationsChanged = true
							return true
						}
						seenFunctionNames[mappedName] = struct{}{}
					}

					declarationJSON := []byte(declaration.Raw)
					if nameResult.Type != ojson.String || mappedName != originalName {
						declarationJSON, _ = ojson.SetBytes(declarationJSON, "name", mappedName)
						declarationsChanged = true
					}
					if normalized, changed := normalizeAntigravityParameterSchema(declaration, declarationJSON); changed {
						declarationJSON = normalized
						declarationsChanged = true
					}
					declarationItems = append(declarationItems, declarationJSON)
					return true
				})
				if declarationsChanged {
					var errSet error
					toolJSON, errSet = ojson.SetRawBytes(toolJSON, key, translatorcommon.JoinRawArray(declarationItems))
					if errSet != nil {
					} else {
						toolChanged = true
					}
				}
			}
			toolsChanged = toolsChanged || toolChanged
			toolItems = append(toolItems, toolJSON)
			return true
		})
		if toolsChanged {
			rawJSON, _ = ojson.SetRawBytes(rawJSON, "request.tools", translatorcommon.JoinRawArray(toolItems))
		}
		rawJSON = removeEmptyGeminiFunctionTools(rawJSON)
	}
	rawJSON = rewriteGeminiFunctionNames(rawJSON, functionNameMap)

	if strings.Contains(strings.ToLower(modelName), "claude") {
		rawJSON = SanitizeAntigravityClaudeGeminiRequestSignatures(modelName, rawJSON)
	} else {
		rawJSON = signature.SanitizeGeminiRequestThoughtSignatures(rawJSON, "request.contents")
	}

	return common.AttachDefaultSafetySettings(rawJSON, "request.safetySettings"), nil
}

// hasAntigravitySystemInstruction treats empty aliases as absent while retaining
// instructions carried by non-text parts.
func hasAntigravitySystemInstruction(instruction ojson.Result) bool {
	if instruction.Type == ojson.String {
		return strings.TrimSpace(instruction.Str) != ""
	}
	parts := instruction.Get("parts")
	if !parts.IsArray() {
		return false
	}
	for _, part := range parts.Array() {
		if !part.IsObject() {
			continue
		}
		hasContent := false
		part.ForEach(func(key, value ojson.Result) bool {
			hasContent = key.Str != "text" || strings.TrimSpace(value.Str) != ""
			return !hasContent
		})
		if hasContent {
			return true
		}
	}
	return false
}

func normalizeAntigravityParameterSchema(declaration ojson.Result, raw []byte) ([]byte, bool) {
	for _, key := range []string{"parameters", "parametersJsonSchema", "parameters_json_schema"} {
		schema := declaration.Get(key)
		if schema.IsObject() {
			normalized := util.NormalizeJSONSchemaNumericConstraints(schema.Raw)
			if key == "parameters" {
				raw, _ = ojson.SetRawBytes(raw, "parametersJsonSchema", []byte(normalized))
				raw, _ = ojson.DeleteBytes(raw, key)
			} else if normalized != schema.Raw {
				raw, _ = ojson.SetRawBytes(raw, key, []byte(normalized))
			} else {
				return raw, false
			}
			return raw, true
		}
		// Only the JSON Schema carrier can hold a boolean schema.
		if key != "parameters" && (schema.Type == ojson.True || schema.Type == ojson.False) {
			raw, _ = ojson.SetRawBytes(raw, "parametersJsonSchema", []byte(schema.Raw))
			raw, _ = ojson.DeleteBytes(raw, "parameters")
			raw, _ = ojson.DeleteBytes(raw, "parameters_json_schema")
			return raw, true
		}
	}
	return raw, false
}

func normalizeAntigravityMedia(payload []byte) []byte {
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if normalized := rewriteAntigravityMediaArray(contents, normalizeAntigravityContentMedia); normalized != contents.Raw {
		payload, _ = ojson.SetRawBytes(payload, "request.contents", []byte(normalized))
	}
	system := util.GetGJSONBytesNoCopy(payload, "request.systemInstruction")
	if normalized := normalizeAntigravityContentMedia(system); normalized != system.Raw {
		payload, _ = ojson.SetRawBytes(payload, "request.systemInstruction", []byte(normalized))
	}
	return payload
}

func normalizeAntigravityContentMedia(content ojson.Result) string {
	return normalizeAntigravityNestedMedia(content.Raw, content.Get("parts"), "parts")
}

func normalizeAntigravityMediaPart(part ojson.Result) string {
	raw := moveAntigravityMediaAlias(part.Raw, "inline_data", "inlineData")
	raw = moveAntigravityMediaAlias(raw, "inlineData.mime_type", "inlineData.mimeType")
	raw = moveAntigravityMediaAlias(raw, "file_data", "fileData")
	raw = moveAntigravityMediaAlias(raw, "fileData.mime_type", "fileData.mimeType")
	raw = moveAntigravityMediaAlias(raw, "fileData.file_uri", "fileData.fileUri")
	for _, path := range []string{"functionResponse.parts", "function_response.parts"} {
		raw = normalizeAntigravityNestedMedia(raw, part.Get(path), path)
	}
	return raw
}

func normalizeAntigravityNestedMedia(raw string, parts ojson.Result, path string) string {
	if normalized := rewriteAntigravityMediaArray(parts, normalizeAntigravityMediaPart); normalized != parts.Raw {
		raw, _ = ojson.SetRaw(raw, path, normalized)
	}
	return raw
}

// rewriteAntigravityMediaArray keeps unchanged raw elements and rebuilds an
// array only when a child needs a rewrite, including for large inline payloads.
func rewriteAntigravityMediaArray(array ojson.Result, rewrite func(ojson.Result) string) string {
	if !array.IsArray() {
		return array.Raw
	}
	values := array.Array()
	var items []string
	for i, value := range values {
		normalized := rewrite(value)
		if items == nil && normalized != value.Raw {
			items = make([]string, len(values))
			for j := 0; j < i; j++ {
				items[j] = values[j].Raw
			}
		}
		if items != nil {
			items[i] = normalized
		}
	}
	if items == nil {
		return array.Raw
	}
	return "[" + strings.Join(items, ",") + "]"
}

func moveAntigravityMediaAlias(raw, snake, camel string) string {
	if alias := ojson.Get(raw, snake); alias.Exists() {
		if !ojson.Get(raw, camel).Exists() {
			raw, _ = ojson.SetRaw(raw, camel, alias.Raw)
		}
		raw, _ = ojson.Delete(raw, snake)
	}
	return raw
}

// normalizeGeminiGenerationConfigResponseSchema converts generationConfig.responseJsonSchema
// (and snake_case response_json_schema) to generationConfig.responseSchema for Antigravity compatibility.
func normalizeGeminiGenerationConfigResponseSchema(rawJSON []byte) []byte {
	for _, container := range []string{"request.generationConfig", "request.generation_config"} {
		if !util.GetGJSONBytesNoCopy(rawJSON, container).Exists() {
			continue
		}
		for _, schemaKey := range []string{"responseJsonSchema", "response_json_schema"} {
			oldPath := container + "." + schemaKey
			if schema := util.GetGJSONBytesNoCopy(rawJSON, oldPath); schema.Exists() {
				targetPath := container + ".responseSchema"
				if schema.IsObject() && !util.GetGJSONBytesNoCopy(rawJSON, targetPath).IsObject() && !util.GetGJSONBytesNoCopy(rawJSON, container+".response_schema").Exists() {
					rawJSON, _ = ojson.SetRawBytes(rawJSON, targetPath, []byte(schema.Raw))
				}
				rawJSON, _ = ojson.DeleteBytes(rawJSON, oldPath)
			}
		}
	}
	return rawJSON
}

// geminiContentRolesNeedNormalization reports whether any content role is missing
// or invalid and therefore requires rebuilding the contents array.
func geminiContentRolesNeedNormalization(contents ojson.Result) bool {
	needsNormalization := false
	contents.ForEach(func(_, value ojson.Result) bool {
		role := value.Get("role").String()
		if role != "user" && role != "model" {
			needsNormalization = true
			return false
		}
		return true
	})
	return needsNormalization
}

func removeEmptyGeminiFunctionTools(rawJSON []byte) []byte {
	tools := util.GetGJSONBytesNoCopy(rawJSON, "request.tools")
	if tools.IsArray() && len(tools.Array()) == 0 {
		rawJSON, _ = ojson.DeleteBytes(rawJSON, "request.tools")
		return rawJSON
	}
	changed := false
	var cleanedTools [][]byte
	for _, tool := range tools.Array() {
		toolJSON := []byte(tool.Raw)
		if tool.IsObject() {
			for _, key := range []string{"functionDeclarations", "function_declarations"} {
				if declarations := tool.Get(key); declarations.IsArray() && len(declarations.Array()) == 0 {
					toolJSON, _ = ojson.DeleteBytes(toolJSON, key)
					changed = true
				}
			}
			if len(util.ParseGJSONBytesNoCopy(toolJSON).Map()) == 0 {
				changed = true
				continue
			}
		}
		cleanedTools = append(cleanedTools, toolJSON)
	}
	if !changed {
		return rawJSON
	}
	if len(cleanedTools) == 0 {
		rawJSON, _ = ojson.DeleteBytes(rawJSON, "request.tools")
		return rawJSON
	}
	rawJSON, _ = ojson.SetRawBytes(rawJSON, "request.tools", translatorcommon.JoinRawArray(cleanedTools))
	return rawJSON
}

// geminiFunctionNameFields lists the part fields that can carry a function name.
var geminiFunctionNameFields = []string{"functionCall", "functionResponse", "function_call", "function_response"}

// geminiFunctionNamesNeedRewrite reports whether any part carries a function name
// that must be remapped or coerced to a string.
func geminiFunctionNamesNeedRewrite(contents ojson.Result, functionNameMap map[string]string) bool {
	needsRewrite := false
	contents.ForEach(func(_, content ojson.Result) bool {
		content.Get("parts").ForEach(func(_, part ojson.Result) bool {
			for _, field := range geminiFunctionNameFields {
				nameResult := part.Get(field + ".name")
				name := nameResult.String()
				if name == "" {
					continue
				}
				if nameResult.Type == ojson.String && util.MapSanitizedFunctionName(functionNameMap, name) == name {
					continue
				}
				needsRewrite = true
				return false
			}
			return true
		})
		return !needsRewrite
	})
	return needsRewrite
}

func rewriteGeminiFunctionNames(rawJSON []byte, functionNameMap map[string]string) []byte {
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	canBatchContents := contents.IsArray()
	if canBatchContents {
		contents.ForEach(func(_, content ojson.Result) bool {
			parts := content.Get("parts")
			if parts.Exists() && !parts.IsArray() {
				canBatchContents = false
				return false
			}
			return true
		})
	}
	// Rebuilding the contents array copies every content and part, so only pay for
	// it once a name actually needs rewriting.
	if canBatchContents && geminiFunctionNamesNeedRewrite(contents, functionNameMap) {
		contentItems := translatorcommon.NewRawArrayItems(contents.Get("#").Int())
		contents.ForEach(func(_, content ojson.Result) bool {
			contentJSON := []byte(content.Raw)
			partsChanged := false
			partItems := make([][]byte, 0, 4)
			content.Get("parts").ForEach(func(_, part ojson.Result) bool {
				partJSON := []byte(part.Raw)
				for _, field := range geminiFunctionNameFields {
					nameResult := part.Get(field + ".name")
					name := nameResult.String()
					if name == "" {
						continue
					}
					mappedName := util.MapSanitizedFunctionName(functionNameMap, name)
					if nameResult.Type == ojson.String && mappedName == name {
						continue
					}
					partJSON, _ = ojson.SetBytes(partJSON, field+".name", mappedName)
					partsChanged = true
				}
				partItems = append(partItems, partJSON)
				return true
			})
			if partsChanged {
				contentJSON, _ = ojson.SetRawBytes(contentJSON, "parts", translatorcommon.JoinRawArray(partItems))
			}
			contentItems = append(contentItems, contentJSON)
			return true
		})
		rawJSON, _ = ojson.SetRawBytes(rawJSON, "request.contents", translatorcommon.JoinRawArray(contentItems))
	} else if !canBatchContents {
		for contentIndex, content := range contents.Array() {
			for partIndex, part := range content.Get("parts").Array() {
				for _, field := range geminiFunctionNameFields {
					nameResult := part.Get(field + ".name")
					name := nameResult.String()
					if name == "" {
						continue
					}
					mappedName := util.MapSanitizedFunctionName(functionNameMap, name)
					if nameResult.Type == ojson.String && mappedName == name {
						continue
					}
					path := fmt.Sprintf("request.contents.%d.parts.%d.%s.name", contentIndex, partIndex, field)
					rawJSON, _ = ojson.SetBytes(rawJSON, path, mappedName)
				}
			}
		}
	}

	for _, allowedPath := range []string{
		"request.toolConfig.functionCallingConfig.allowedFunctionNames",
		"request.tool_config.function_calling_config.allowed_function_names",
	} {
		allowedNames := util.GetGJSONBytesNoCopy(rawJSON, allowedPath)
		if allowedNames.IsArray() {
			namesChanged := false
			nameItems := make([][]byte, 0, 4)
			allowedNames.ForEach(func(_, name ojson.Result) bool {
				mappedName := util.MapSanitizedFunctionName(functionNameMap, name.String())
				namesChanged = namesChanged || name.Type != ojson.String || mappedName != name.String()
				mappedNameJSON, _ := json.Marshal(mappedName)
				nameItems = append(nameItems, mappedNameJSON)
				return true
			})
			if namesChanged {
				rawJSON, _ = ojson.SetRawBytes(rawJSON, allowedPath, translatorcommon.JoinRawArray(nameItems))
			}
		} else {
			for index, name := range allowedNames.Array() {
				mappedName := util.MapSanitizedFunctionName(functionNameMap, name.String())
				if name.Type == ojson.String && mappedName == name.String() {
					continue
				}
				path := fmt.Sprintf("%s.%d", allowedPath, index)
				rawJSON, _ = ojson.SetBytes(rawJSON, path, mappedName)
			}
		}
	}
	return rawJSON
}

// SanitizeAntigravityClaudeGeminiRequestSignatures preserves compatible Claude thinking signatures,
// gives the first function call in each model turn Antigravity's validator bypass, and strips
// signatures from parts that cannot own them. Parallel sibling calls remain unsigned.
func SanitizeAntigravityClaudeGeminiRequestSignatures(modelName string, rawJSON []byte) []byte {
	rawJSON = signature.SanitizeGeminiSystemInstruction(rawJSON, "request.contents")
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if !contents.IsArray() {
		return rawJSON
	}

	contentsArray := contents.Array()
	changed := false
	rewrittenContents := make([][]byte, 0, len(contentsArray))

	for contentIndex, content := range contentsArray {
		parts := content.Get("parts")
		if !parts.IsArray() {
			rewrittenContents = append(rewrittenContents, []byte(content.Raw))
			continue
		}

		isModelTurn := content.Get("role").String() == "model"
		firstFunctionCallSeen := false
		partsArray := parts.Array()
		contentChanged := false
		rewrittenParts := make([][]byte, 0, len(partsArray))

		for partIndex, partResult := range partsArray {
			var part map[string]any
			decoder := json.NewDecoder(strings.NewReader(partResult.Raw))
			decoder.UseNumber()
			if err := decoder.Decode(&part); err != nil {
				rewrittenParts = append(rewrittenParts, []byte(partResult.Raw))
				continue
			}

			rawSignature := antigravityClaudeGeminiPartThoughtSignature(part)
			canonicalCount, hasAlias := antigravityClaudeGeminiPartSignatureFields(partResult)
			hasSignatureKey := canonicalCount != 0 || hasAlias
			hasCanonicalSignature := canonicalCount == 1 && !hasAlias

			if hasFunctionResponsePart(part) {
				if hasSignatureKey {
					changed = true
					contentChanged = true
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_signature", "functionResponse parts cannot replay Claude thinking signatures", contentIndex, partIndex, rawSignature)
					partBytes := deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
					rewrittenParts = append(rewrittenParts, partBytes)
				} else {
					rewrittenParts = append(rewrittenParts, []byte(partResult.Raw))
				}
				continue
			}

			if !isModelTurn {
				if hasSignatureKey {
					changed = true
					contentChanged = true
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_signature", "non-model parts cannot replay Claude thinking signatures", contentIndex, partIndex, rawSignature)
					partBytes := deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
					rewrittenParts = append(rewrittenParts, partBytes)
				} else {
					rewrittenParts = append(rewrittenParts, []byte(partResult.Raw))
				}
				continue
			}

			if hasFunctionCallPart(part) {
				isFirstFunctionCall := !firstFunctionCallSeen
				firstFunctionCallSeen = true
				hasBypass := hasCanonicalSignature && rawSignature == signature.GeminiSkipThoughtSignatureValidator
				if (isFirstFunctionCall && hasBypass) || (!isFirstFunctionCall && !hasSignatureKey) {
					rewrittenParts = append(rewrittenParts, []byte(partResult.Raw))
					continue
				}
				changed = true
				contentChanged = true
				partBytes := deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
				if isFirstFunctionCall {
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "replace_signature", "first functionCall requires Antigravity bypass signature", contentIndex, partIndex, rawSignature)
					var setErr error
					partBytes, setErr = ojson.SetBytes(partBytes, "thoughtSignature", signature.GeminiSkipThoughtSignatureValidator)
					if setErr != nil {
						return rawJSON
					}
				} else {
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_signature", "parallel sibling functionCalls remain unsigned", contentIndex, partIndex, rawSignature)
				}
				rewrittenParts = append(rewrittenParts, partBytes)
				continue
			}

			if part["thought"] == true {
				normalized, compatible := signature.CompatibleAntigravityClaudeThinkingSignature(rawSignature)
				if !compatible {
					changed = true
					contentChanged = true
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_thinking_block", "missing_or_incompatible_signature", contentIndex, partIndex, rawSignature)
					continue
				}
				text, _ := part["text"].(string)
				if strings.TrimSpace(text) == "" {
					changed = true
					contentChanged = true
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_thinking_block", "empty_thinking_text", contentIndex, partIndex, rawSignature)
					continue
				}
				if hasCanonicalSignature && normalized == rawSignature {
					rewrittenParts = append(rewrittenParts, []byte(partResult.Raw))
					continue
				}
				changed = true
				contentChanged = true
				logAntigravityClaudeGeminiSignatureSanitize(modelName, "normalize_signature", "compatible_claude_signature", contentIndex, partIndex, rawSignature)
				partBytes := deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
				var setErr error
				partBytes, setErr = ojson.SetBytes(partBytes, "thoughtSignature", normalized)
				if setErr != nil {
					return rawJSON
				}
				rewrittenParts = append(rewrittenParts, partBytes)
				continue
			}

			if hasSignatureKey {
				changed = true
				contentChanged = true
				logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_signature", "non-thinking parts should not carry Claude thinking signatures", contentIndex, partIndex, rawSignature)
				partBytes := deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
				rewrittenParts = append(rewrittenParts, partBytes)
			} else {
				rewrittenParts = append(rewrittenParts, []byte(partResult.Raw))
			}
		}

		if len(rewrittenParts) == 0 {
			changed = true
			continue
		}
		if contentChanged || len(rewrittenParts) != len(partsArray) {
			contentBytes := []byte(content.Raw)
			var setErr error
			contentBytes, setErr = ojson.SetRawBytes(contentBytes, "parts", translatorcommon.JoinRawArray(rewrittenParts))
			if setErr != nil {
				return rawJSON
			}
			rewrittenContents = append(rewrittenContents, contentBytes)
		} else {
			rewrittenContents = append(rewrittenContents, []byte(content.Raw))
		}
	}

	if !changed {
		return rawJSON
	}
	out, errSet := ojson.SetRawBytes(rawJSON, "request.contents", translatorcommon.JoinRawArray(rewrittenContents))
	if errSet != nil {
		return rawJSON
	}
	return out
}

var antigravityClaudeGeminiThoughtSignaturePaths = [][]string{
	{"thoughtSignature"},
	{"thought_signature"},
	{"functionCall", "thoughtSignature"},
	{"functionCall", "thought_signature"},
	{"function_call", "thoughtSignature"},
	{"function_call", "thought_signature"},
	{"functionResponse", "thoughtSignature"},
	{"functionResponse", "thought_signature"},
	{"function_response", "thoughtSignature"},
	{"function_response", "thought_signature"},
	{"extra_content", "google", "thought_signature"},
}

// antigravityClaudeGeminiPartSignatureFields distinguishes a single canonical
// key from alternate signatures. Inspect only the known paths, never tool data.
func antigravityClaudeGeminiPartSignatureFields(part ojson.Result) (canonicalCount int, hasAlias bool) {
	part.ForEach(func(key, value ojson.Result) bool {
		name := key.String()
		if name == "thoughtSignature" {
			canonicalCount++
		} else if !hasAlias {
			for _, path := range antigravityClaudeGeminiThoughtSignaturePaths[1:] {
				if name != path[0] || (len(path) > 1 && !hasAntigravitySignaturePath(value, path[1:])) {
					continue
				}
				hasAlias = true
				break
			}
		}
		return true
	})
	return canonicalCount, hasAlias
}

func hasAntigravitySignaturePath(value ojson.Result, path []string) bool {
	if !value.IsObject() {
		return false
	}
	if len(path) == 1 {
		return value.Get(path[0]).Exists()
	}
	found := false
	value.ForEach(func(key, child ojson.Result) bool {
		if key.String() == path[0] && hasAntigravitySignaturePath(child, path[1:]) {
			found = true
		}
		return !found
	})
	return found
}

func antigravityClaudeGeminiPartThoughtSignature(part map[string]any) string {
	for _, path := range antigravityClaudeGeminiThoughtSignaturePaths {
		if value, ok := stringAtPath(part, path...); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part map[string]any) []byte {
	for _, path := range antigravityClaudeGeminiThoughtSignaturePaths {
		current := part
		for _, key := range path[:len(path)-1] {
			next, ok := current[key].(map[string]any)
			if !ok {
				current = nil
				break
			}
			current = next
		}
		delete(current, path[len(path)-1])
	}
	pruneEmptyExtraContentGoogle(part)
	// Reuse the UseNumber-decoded object: duplicate parent/signature keys collapse
	// before filtering, and large numeric tool arguments retain their JSON value.
	cleaned, _ := json.Marshal(part)
	return cleaned
}

func pruneEmptyExtraContentGoogle(part map[string]any) {
	extraContent, ok := part["extra_content"].(map[string]any)
	if !ok {
		return
	}
	if google, okGoogle := extraContent["google"].(map[string]any); okGoogle && len(google) == 0 {
		delete(extraContent, "google")
	}
	if len(extraContent) == 0 {
		delete(part, "extra_content")
	}
}

func hasFunctionResponsePart(part map[string]any) bool {
	if _, ok := part["functionResponse"]; ok {
		return true
	}
	_, ok := part["function_response"]
	return ok
}

func hasFunctionCallPart(part map[string]any) bool {
	if _, ok := part["functionCall"]; ok {
		return true
	}
	_, ok := part["function_call"]
	return ok
}

func stringAtPath(value map[string]any, path ...string) (string, bool) {
	var current any = value
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = m[key]
		if !ok {
			return "", false
		}
	}
	s, ok := current.(string)
	return s, ok
}

func logAntigravityClaudeGeminiSignatureSanitize(modelName, action, reason string, contentIndex, partIndex int, rawSignature string) {
	_, _, _, _, _, _ = modelName, action, reason, contentIndex, partIndex, rawSignature
}

// FunctionCallGroup represents a group of function calls and their responses
type FunctionCallGroup struct {
	ResponsesNeeded int
	CallIDs         []string // ordered function call IDs used to match responses
	CallNames       []string // ordered function call names for backfilling empty response names
}

func selectGroupResponses(collected []ojson.Result, group *FunctionCallGroup) ([]ojson.Result, []ojson.Result) {
	needed := group.ResponsesNeeded
	if len(collected) == 1 {
		return collected, nil
	}

	hasCallID := false
	for _, id := range group.CallIDs {
		if id != "" {
			hasCallID = true
			break
		}
	}
	if !hasCallID {
		return collected[:needed], collected[needed:]
	}

	// Single O(M) pass over collected responses to index id and name once per part.
	byID := make(map[string][]int, len(collected))
	byName := make(map[string][]int, len(collected))
	var emptyNameIdxs []int
	for idx, resp := range collected {
		funcResp := translatorcommon.GeminiPartFunctionResponse(resp)
		if id := funcResp.Get("id").String(); id != "" {
			byID[id] = append(byID[id], idx)
		}
		if name := funcResp.Get("name").String(); name != "" {
			byName[name] = append(byName[name], idx)
		} else {
			emptyNameIdxs = append(emptyNameIdxs, idx)
		}
	}

	used := make([]bool, len(collected))
	selected := make([]ojson.Result, needed)
	matchedSlot := make([]bool, needed)
	popUnused := func(queue []int) (int, []int, bool) {
		for len(queue) > 0 {
			idx := queue[0]
			queue = queue[1:]
			if !used[idx] {
				return idx, queue, true
			}
		}
		return 0, queue, false
	}

	for slot := 0; slot < needed; slot++ {
		if slot >= len(group.CallIDs) || group.CallIDs[slot] == "" {
			continue
		}
		callID := group.CallIDs[slot]
		if idx, rest, ok := popUnused(byID[callID]); ok {
			byID[callID] = rest
			used[idx] = true
			selected[slot] = collected[idx]
			matchedSlot[slot] = true
		} else {
			delete(byID, callID)
		}
	}

	for slot := 0; slot < needed; slot++ {
		if matchedSlot[slot] || slot >= len(group.CallNames) || group.CallNames[slot] == "" {
			continue
		}
		callName := group.CallNames[slot]
		if idx, rest, ok := popUnused(byName[callName]); ok {
			byName[callName] = rest
			used[idx] = true
			selected[slot] = collected[idx]
			matchedSlot[slot] = true
			continue
		}
		delete(byName, callName)
		if idx, rest, ok := popUnused(emptyNameIdxs); ok {
			emptyNameIdxs = rest
			used[idx] = true
			selected[slot] = collected[idx]
			matchedSlot[slot] = true
		} else {
			emptyNameIdxs = nil
		}
	}

	freeCursor := 0
	for slot := 0; slot < needed; slot++ {
		if matchedSlot[slot] {
			continue
		}
		for freeCursor < len(collected) && used[freeCursor] {
			freeCursor++
		}
		if freeCursor < len(collected) {
			used[freeCursor] = true
			selected[slot] = collected[freeCursor]
			matchedSlot[slot] = true
			freeCursor++
		}
	}

	remaining := make([]ojson.Result, 0, len(collected)-needed)
	for idx, resp := range collected {
		if !used[idx] {
			remaining = append(remaining, resp)
		}
	}
	return selected, remaining
}

func normalizeAntigravityInlineDataPart(part ojson.Result) ([]byte, bool) {
	inline := part.Get("inlineData")
	if !inline.Exists() {
		inline = part.Get("inline_data")
	}
	if !inline.Exists() {
		return nil, false
	}
	data := inline.Get("data").String()
	if data == "" {
		return nil, false
	}
	mimeType := inline.Get("mimeType").String()
	if mimeType == "" {
		mimeType = inline.Get("mime_type").String()
	}
	if mimeType == "" {
		// Cloud Code Assist ignores inlineData without mimeType.
		mimeType = "image/png"
	}
	out := []byte(`{"inlineData":{"mimeType":"","data":""}}`)
	out, _ = ojson.SetBytes(out, "inlineData.mimeType", mimeType)
	out, _ = ojson.SetBytes(out, "inlineData.data", data)
	return out, true
}

func attachInlineDataToFunctionResponse(response ojson.Result, images [][]byte) ojson.Result {
	if len(images) == 0 {
		return response
	}
	target := []byte(response.Raw)
	partsPath := geminiFunctionResponseField(response) + ".parts.-1"
	for _, img := range images {
		target, _ = ojson.SetRawBytes(target, partsPath, img)
	}
	return ojson.ParseBytes(target)
}

// collectFunctionResponsesWithSiblingInlineData keeps functionResponse parts and
// moves sibling inline_data/inlineData onto the nearest preceding functionResponse.
// Leading images before the first functionResponse attach to that first response.
// Other parts remain ordinary user input.
func collectFunctionResponsesWithSiblingInlineData(parts ojson.Result) ([]ojson.Result, []string) {
	responses := make([]ojson.Result, 0)
	var otherParts []string
	leadingImages := make([][]byte, 0)
	current := -1
	parts.ForEach(func(_, part ojson.Result) bool {
		if translatorcommon.GeminiPartFunctionResponse(part).Exists() {
			responses = append(responses, part)
			current = len(responses) - 1
			if len(leadingImages) > 0 {
				responses[current] = attachInlineDataToFunctionResponse(responses[current], leadingImages)
				leadingImages = nil
			}
			return true
		}
		imagePart, ok := normalizeAntigravityInlineDataPart(part)
		if !ok {
			otherParts = append(otherParts, part.Raw)
			return true
		}
		if current >= 0 {
			responses[current] = attachInlineDataToFunctionResponse(responses[current], [][]byte{imagePart})
			return true
		}
		leadingImages = append(leadingImages, imagePart)
		return true
	})
	return responses, otherParts
}

func geminiFunctionResponseField(part ojson.Result) string {
	if part.Get("functionResponse").Exists() {
		return "functionResponse"
	}
	return "function_response"
}

// parseFunctionResponseRaw attempts to normalize a function response part into a JSON object string.
// Falls back to a minimal "functionResponse" object when parsing fails.
// fallbackName is used when the response's own name is empty.
func parseFunctionResponseRaw(response ojson.Result, fallbackName string) string {
	funcResp := translatorcommon.GeminiPartFunctionResponse(response)
	if response.IsObject() && ojson.Valid(response.Raw) {
		raw := response.Raw
		name := funcResp.Get("name").String()
		if strings.TrimSpace(name) == "" && fallbackName != "" {
			updated, _ := ojson.SetBytes([]byte(raw), geminiFunctionResponseField(response)+".name", fallbackName)
			raw = string(updated)
		}
		return raw
	}

	if funcResp.Exists() {
		fr := []byte(`{"functionResponse":{"name":"","response":{"result":""}}}`)
		name := funcResp.Get("name").String()
		if strings.TrimSpace(name) == "" {
			name = fallbackName
		}
		fr, _ = ojson.SetBytes(fr, "functionResponse.name", name)
		fr, _ = ojson.SetBytes(fr, "functionResponse.response.result", funcResp.Get("response").String())
		if id := funcResp.Get("id").String(); id != "" {
			fr, _ = ojson.SetBytes(fr, "functionResponse.id", id)
		}
		return string(fr)
	}

	useName := fallbackName
	if useName == "" {
		useName = "unknown"
	}
	fr := []byte(`{"functionResponse":{"name":"","response":{"result":""}}}`)
	fr, _ = ojson.SetBytes(fr, "functionResponse.name", useName)
	fr, _ = ojson.SetBytes(fr, "functionResponse.response.result", response.String())
	return string(fr)
}

// fixCLIToolResponse performs sophisticated tool response format conversion and grouping.
// This function transforms the CLI tool response format by intelligently grouping function calls
// with their corresponding responses, ensuring proper conversation flow and API compatibility.
// It converts from a linear format (1.json) to a grouped format (2.json) where function calls
// and their responses are properly associated and structured.
//
// Parameters:
//   - input: The input JSON string to be processed
//
// Returns:
//   - string: The processed JSON string with grouped function calls and responses
//   - error: An error if the processing fails
func fixCLIToolResponse(input []byte) ([]byte, error) {
	// Parse the input JSON to extract the conversation structure.
	// The parsed result references input directly; input must not be mutated
	// while the result and its raw slices are still in use.
	parsed := util.ParseGJSONBytesNoCopy(input)

	// Extract the contents array which contains the conversation messages
	contents := parsed.Get("request.contents")
	if !contents.Exists() {
		// log.Printf(input)
		return input, fmt.Errorf("contents not found in input")
	}

	needsGrouping := false
	allContentsAreObjects := true
	contents.ForEach(func(_, content ojson.Result) bool {
		if !content.IsObject() {
			allContentsAreObjects = false
			return true
		}
		content.Get("parts").ForEach(func(_, part ojson.Result) bool {
			if translatorcommon.GeminiPartFunctionResponse(part).Exists() {
				needsGrouping = true
				return false
			}
			return true
		})
		return !needsGrouping
	})
	if contents.IsArray() && allContentsAreObjects && !needsGrouping {
		return input, nil
	}

	// Initialize data structures for processing and grouping
	contentItems := translatorcommon.NewRawArrayItems(contents.Get("#").Int())
	var pendingGroups []*FunctionCallGroup // Groups awaiting completion with responses
	var collectedResponses []ojson.Result  // Standalone responses to be matched
	var mixedTurnFollowups [][]byte
	appendFunctionResponses := func(responses []ojson.Result, callNames []string) {
		partItems := make([][]byte, 0, len(responses))
		for responseIndex, response := range responses {
			partRaw := parseFunctionResponseRaw(response, callNames[responseIndex])
			if partRaw != "" {
				partItems = append(partItems, []byte(partRaw))
			}
		}
		if len(partItems) > 0 {
			functionResponseContent := []byte(`{"parts":[],"role":"function"}`)
			functionResponseContent, _ = ojson.SetRawBytes(functionResponseContent, "parts", translatorcommon.JoinRawArray(partItems))
			contentItems = append(contentItems, functionResponseContent)
		}
	}

	// Process each content object in the conversation
	// This iterates through messages and groups function calls with their responses
	contents.ForEach(func(key, value ojson.Result) bool {
		role := value.Get("role").String()
		parts := value.Get("parts")

		// Collect function responses and attach sibling inlineData to the nearest one.
		responsePartsInThisContent, otherParts := collectFunctionResponsesWithSiblingInlineData(parts)

		// If this content has function responses, collect them
		if len(responsePartsInThisContent) > 0 {
			collectedResponses = append(collectedResponses, responsePartsInThisContent...)
			if len(otherParts) > 0 {
				followup, _ := ojson.SetBytes([]byte(value.Raw), "role", "user")
				followup, _ = ojson.SetRawBytes(followup, "parts", []byte("["+strings.Join(otherParts, ",")+"]"))
				mixedTurnFollowups = append(mixedTurnFollowups, followup)
			}

			// Check if pending groups can be satisfied (FIFO: oldest group first)
			for len(pendingGroups) > 0 && len(collectedResponses) >= pendingGroups[0].ResponsesNeeded {
				group := pendingGroups[0]
				pendingGroups = pendingGroups[1:]

				// Take the needed responses for this group
				var groupResponses []ojson.Result
				groupResponses, collectedResponses = selectGroupResponses(collectedResponses, group)

				appendFunctionResponses(groupResponses, group.CallNames)
			}
			if len(pendingGroups) == 0 {
				// Responses without a pending call group must not leak into later turns.
				collectedResponses = nil
				contentItems = append(contentItems, mixedTurnFollowups...)
				mixedTurnFollowups = nil
			}

			return true // Mixed-turn user input follows the completed response group.
		}

		// If this is a model turn (or an untagged/non-user turn carrying function calls), create a new group
		if role == "model" || (role != "user" && translatorcommon.GeminiPartsHaveFunctionCall(parts)) {
			var callIDs []string
			var callNames []string
			parts.ForEach(func(_, part ojson.Result) bool {
				if call := translatorcommon.GeminiPartFunctionCall(part); call.Exists() {
					callIDs = append(callIDs, call.Get("id").String())
					callNames = append(callNames, call.Get("name").String())
				}
				return true
			})

			if len(callNames) > 0 {
				// Add the model content
				if !value.IsObject() {
					return true
				}
				contentItems = append(contentItems, []byte(value.Raw))

				// Create a new group for tracking responses
				group := &FunctionCallGroup{
					ResponsesNeeded: len(callNames),
					CallIDs:         callIDs,
					CallNames:       callNames,
				}
				pendingGroups = append(pendingGroups, group)
			} else {
				// Regular model content without function calls
				if !value.IsObject() {
					return true
				}
				contentItems = append(contentItems, []byte(value.Raw))
			}
		} else {
			// Non-model content (user, etc.)
			if !value.IsObject() {
				return true
			}
			contentItems = append(contentItems, []byte(value.Raw))
		}

		return true
	})

	// Handle any remaining pending groups with remaining responses
	for _, group := range pendingGroups {
		if len(collectedResponses) >= group.ResponsesNeeded {
			groupResponses, remaining := selectGroupResponses(collectedResponses, group)
			collectedResponses = remaining

			appendFunctionResponses(groupResponses, group.CallNames)
		}
	}
	contentItems = append(contentItems, mixedTurnFollowups...)

	// Update the original JSON with the new contents
	result, _ := ojson.SetRawBytes(input, "request.contents", translatorcommon.JoinRawArray(contentItems))

	return result, nil
}
