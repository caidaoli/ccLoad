package common

import (
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// GeminiClaudeMessageID keeps a valid provider response ID or creates an ID for
// Claude clients when Gemini does not send one.
func GeminiClaudeMessageID(responseID gjson.Result) string {
	if id := strings.TrimSpace(responseID.Str); id != "" {
		return id
	}
	return "msg_" + uuid.NewString()
}

// IsGeminiThoughtPart reports whether a Gemini part contains hidden model thought.
func IsGeminiThoughtPart(part gjson.Result) bool {
	return part.Get("thought").Bool()
}

// GeminiUsageMetadataFromOpenAI maps Chat and Responses usage into Gemini's
// separate candidate and thought counts. Cached tokens stay part of the prompt.
func GeminiUsageMetadataFromOpenAI(usage gjson.Result, out []byte) []byte {
	prompt, hasPrompt := tokenCountFromUsage(usage, "prompt_tokens", "input_tokens")
	output, hasOutput := tokenCountFromUsage(usage, "completion_tokens", "output_tokens")
	total, hasTotal := tokenCountFromUsage(usage, "total_tokens")
	thoughts, hasThoughts := tokenCountFromUsage(usage, "completion_tokens_details.reasoning_tokens", "output_tokens_details.reasoning_tokens")
	if hasPrompt {
		out, _ = sjson.SetBytes(out, "promptTokenCount", prompt)
	}
	if hasOutput {
		out, _ = sjson.SetBytes(out, "candidatesTokenCount", max(0, output-thoughts))
	}
	if hasTotal {
		out, _ = sjson.SetBytes(out, "totalTokenCount", total)
	} else if hasPrompt || hasOutput {
		out, _ = sjson.SetBytes(out, "totalTokenCount", prompt+output)
	}
	if hasThoughts {
		out, _ = sjson.SetBytes(out, "thoughtsTokenCount", thoughts)
	}
	if cached, ok := tokenCountFromUsage(usage, "prompt_tokens_details.cached_tokens", "input_tokens_details.cached_tokens"); ok {
		out, _ = sjson.SetBytes(out, "cachedContentTokenCount", cached)
	}
	return out
}

// tokenCountFromUsage preserves explicit zeros and skips null aliases. Some
// Chat-compatible upstreams encode token counts as JSON strings.
func tokenCountFromUsage(usage gjson.Result, paths ...string) (int64, bool) {
	for _, path := range paths {
		value := usage.Get(path)
		switch value.Type {
		case gjson.Number:
			if count := value.Int(); count >= 0 && float64(count) == value.Num {
				return count, true
			}
		case gjson.String:
			if count, err := strconv.ParseInt(value.Str, 10, 64); err == nil && count >= 0 {
				return count, true
			}
		}
	}
	return 0, false
}

// ChatStopSequences reads nonempty stop strings from the Chat stop union.
func ChatStopSequences(stop gjson.Result) []string {
	if stop.Type == gjson.String && stop.Str != "" {
		return []string{stop.Str}
	}
	if !stop.IsArray() {
		return nil
	}
	var sequences []string
	stop.ForEach(func(_, item gjson.Result) bool {
		if item.Type == gjson.String && item.Str != "" {
			sequences = append(sequences, item.Str)
		}
		return true
	})
	return sequences
}

// GeminiConfigObject selects the first valid object among Gemini SDK aliases.
func GeminiConfigObject(parent gjson.Result, keys ...string) gjson.Result {
	for _, key := range keys {
		if value := parent.Get(key); value.IsObject() {
			return value
		}
	}
	return gjson.Result{}
}

// GeminiConfigNumber selects a numeric field while distinguishing zero from null.
func GeminiConfigNumber(parent gjson.Result, keys ...string) gjson.Result {
	for _, key := range keys {
		if value := parent.Get(key); value.Type == gjson.Number {
			return value
		}
	}
	return gjson.Result{}
}

// GeminiConfigInt selects the first integer that meets the field's minimum.
func GeminiConfigInt(parent gjson.Result, minValue int64, keys ...string) (int64, bool) {
	for _, key := range keys {
		value := parent.Get(key)
		if value.Type == gjson.Number {
			if number := value.Int(); number >= minValue && float64(number) == value.Num {
				return number, true
			}
		}
	}
	return 0, false
}

// GeminiConfigStrings selects a string list, skipping entries of other JSON types.
func GeminiConfigStrings(parent gjson.Result, keys ...string) []string {
	for _, key := range keys {
		value := parent.Get(key)
		if !value.IsArray() {
			continue
		}
		var result []string
		for _, item := range value.Array() {
			if item.Type == gjson.String {
				result = append(result, item.Str)
			}
		}
		return result
	}
	return nil
}

// GeminiThinkingLevel selects the first nonempty SDK spelling of a thinking level.
func GeminiThinkingLevel(config gjson.Result) string {
	for _, key := range []string{"thinkingLevel", "thinking_level"} {
		if value := config.Get(key); value.Type == gjson.String {
			if level := strings.TrimSpace(value.Str); level != "" {
				return strings.ToLower(level)
			}
		}
	}
	return ""
}

// GeminiThinkingConfig reads the first valid nested level and budget independently across SDK spellings.
func GeminiThinkingConfig(root gjson.Result) (level string, budget int64, hasBudget bool) {
	for _, group := range [...]struct {
		generationKey string
		thinkingKeys  [2]string
	}{
		{"generationConfig", [...]string{"thinkingConfig", "thinking_config"}},
		{"generation_config", [...]string{"thinking_config", "thinkingConfig"}},
	} {
		generation := root.Get(group.generationKey)
		if !generation.IsObject() {
			continue
		}
		for _, key := range group.thinkingKeys {
			config := generation.Get(key)
			if level == "" {
				level = GeminiThinkingLevel(config)
			}
			if !hasBudget {
				budget, hasBudget = GeminiConfigInt(config, -1, "thinkingBudget", "thinking_budget")
			}
			if level != "" && hasBudget {
				return level, budget, hasBudget
			}
		}
	}
	return level, budget, hasBudget
}

// GeminiIncludeThoughts returns the explicit summary visibility flag, if set.
func GeminiIncludeThoughts(root gjson.Result) gjson.Type {
	for _, group := range [...]struct {
		generationKey string
		paths         [4]string
	}{
		{"generationConfig", [...]string{
			"thinkingConfig.includeThoughts",
			"thinkingConfig.include_thoughts",
			"thinking_config.include_thoughts",
			"thinking_config.includeThoughts",
		}},
		{"generation_config", [...]string{
			"thinking_config.include_thoughts",
			"thinking_config.includeThoughts",
			"thinkingConfig.include_thoughts",
			"thinkingConfig.includeThoughts",
		}},
	} {
		generation := root.Get(group.generationKey)
		if !generation.IsObject() {
			continue
		}
		for _, path := range group.paths {
			if value := generation.Get(path); value.Type == gjson.True || value.Type == gjson.False {
				return value.Type
			}
		}
	}
	return gjson.Null
}

// GeminiMessageRole maps Gemini content roles to roles accepted by the other
// supported wire protocols. Missing roles are user content unless the turn
// contains a function call from the model.
func GeminiMessageRole(role string, parts gjson.Result) string {
	switch role {
	case "model":
		return "assistant"
	case "":
		if geminiPartsHaveFunctionCall(parts) {
			return "assistant"
		}
		return "user"
	case "function", "tool":
		return "user"
	default:
		return role
	}
}

// MergeAdjacentGeminiContents merges consecutive user Content turns.
// Mid-conversation system messages in Claude requests are downgraded to user
// reminder turns. When followed or preceded by other user turns or tool results,
// their parts are merged into a single user turn.
// Consecutive model turns are strictly kept unmerged to avoid shifting part
// indices and breaking cryptographic thought signatures and reasoning replay.
func MergeAdjacentGeminiContents(contents [][]byte) [][]byte {
	if len(contents) <= 1 {
		return contents
	}
	merged := make([][]byte, 0, len(contents))
	for _, content := range contents {
		if len(content) == 0 {
			continue
		}
		role := gjson.GetBytes(content, "role").String()
		partsResult := gjson.GetBytes(content, "parts")
		if !partsResult.IsArray() || len(partsResult.Array()) == 0 {
			continue
		}
		if len(merged) > 0 {
			lastIndex := len(merged) - 1
			lastJSON := merged[lastIndex]
			lastRole := gjson.GetBytes(lastJSON, "role").String()
			if lastRole == "user" && role == "user" {
				lastParts := gjson.GetBytes(lastJSON, "parts").Array()
				combinedParts := make([][]byte, 0, len(lastParts)+len(partsResult.Array()))
				for _, p := range lastParts {
					combinedParts = append(combinedParts, []byte(p.Raw))
				}
				for _, p := range partsResult.Array() {
					combinedParts = append(combinedParts, []byte(p.Raw))
				}
				combinedParts = ReorderGeminiUserParts(combinedParts)
				updated, err := sjson.SetRawBytes(lastJSON, "parts", JoinRawArray(combinedParts))
				if err == nil {
					merged[lastIndex] = updated
					continue
				}
			}
		}
		merged = append(merged, content)
	}
	return merged
}

// ContentHasGeminiFunctionCall reports whether a Gemini content turn contains any functionCall part.
func ContentHasGeminiFunctionCall(content []byte) bool {
	return geminiPartsHaveFunctionCall(gjson.GetBytes(content, "parts"))
}

func geminiPartsHaveFunctionCall(parts gjson.Result) bool {
	hasFC := false
	parts.ForEach(func(_, part gjson.Result) bool {
		if GeminiPartFunctionCall(part).Exists() {
			hasFC = true
			return false
		}
		return true
	})
	return hasFC
}

// ContentHasGeminiFunctionResponse reports whether a Gemini content turn contains any functionResponse part.
func ContentHasGeminiFunctionResponse(content []byte) bool {
	hasFR := false
	gjson.GetBytes(content, "parts").ForEach(func(_, part gjson.Result) bool {
		if GeminiPartFunctionResponse(part).Exists() {
			hasFR = true
			return false
		}
		return true
	})
	return hasFR
}

// GeminiPartFunctionCall returns the functionCall or function_call field.
func GeminiPartFunctionCall(part gjson.Result) gjson.Result {
	return geminiPartToolObject(part, "functionCall", "function_call")
}

// GeminiPartFunctionResponse returns the functionResponse or function_response field.
func GeminiPartFunctionResponse(part gjson.Result) gjson.Result {
	return geminiPartToolObject(part, "functionResponse", "function_response")
}

func geminiPartToolObject(part gjson.Result, keys ...string) gjson.Result {
	for _, key := range keys {
		value := part.Get(key)
		if !value.IsObject() {
			continue
		}
		hasMember := false
		value.ForEach(func(_, _ gjson.Result) bool {
			hasMember = true
			return false
		})
		if hasMember {
			return value
		}
	}
	return gjson.Result{}
}

// GeminiSystemInstructionText reads the first alias with visible system text.
func GeminiSystemInstructionText(root gjson.Result, keys ...string) string {
	for _, key := range keys {
		parts := root.Get(key + ".parts")
		if !parts.IsArray() {
			continue
		}
		var text strings.Builder
		for _, part := range parts.Array() {
			value := part.Get("text").Str
			if IsGeminiThoughtPart(part) || strings.TrimSpace(value) == "" {
				continue
			}
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(value)
		}
		if text.Len() > 0 {
			return text.String()
		}
	}
	return ""
}

// ReorderGeminiUserParts reorders parts within a Gemini user turn so that
// text parts (such as prompt text and system reminders) precede functionResponse
// parts. This resolves upstream provider validation failures (such as Google Cloud
// Vertex AI returning 400 "Requests ending with a model turn are not supported" when
// functionResponse is followed by text in the same turn).
func ReorderGeminiUserParts(parts [][]byte) [][]byte {
	hasFR := false
	hasTrailingText := false
	for _, p := range parts {
		isFR := gjson.GetBytes(p, "functionResponse").Exists() || gjson.GetBytes(p, "function_response").Exists()
		if isFR {
			hasFR = true
		} else if hasFR && gjson.GetBytes(p, "text").Exists() {
			hasTrailingText = true
			break
		}
	}
	if !hasFR || !hasTrailingText {
		return parts
	}

	promptParts := make([][]byte, 0, len(parts))
	toolParts := make([][]byte, 0, len(parts))
	for _, p := range parts {
		if gjson.GetBytes(p, "text").Exists() {
			promptParts = append(promptParts, p)
		} else {
			toolParts = append(toolParts, p)
		}
	}
	return append(promptParts, toolParts...)
}

// MergeAdjacentGeminiUserContents merges consecutive user Content turns,
// but leaves turns containing functionResponse unmerged to preserve tool-call/response boundaries.
func MergeAdjacentGeminiUserContents(contents [][]byte) [][]byte {
	if len(contents) <= 1 {
		return contents
	}
	merged := make([][]byte, 0, len(contents))
	for _, content := range contents {
		if len(content) == 0 {
			continue
		}
		role := gjson.GetBytes(content, "role").String()
		partsResult := gjson.GetBytes(content, "parts")
		if !partsResult.IsArray() || partsResult.Raw == "[]" || !partsResult.Get("0").Exists() {
			continue
		}
		if len(merged) > 0 {
			lastIndex := len(merged) - 1
			lastJSON := merged[lastIndex]
			lastRole := gjson.GetBytes(lastJSON, "role").String()
			if lastRole == "user" && role == "user" && !ContentHasGeminiFunctionResponse(lastJSON) && !ContentHasGeminiFunctionResponse(content) {
				lastParts := gjson.GetBytes(lastJSON, "parts").Array()
				currentParts := partsResult.Array()
				combinedParts := make([][]byte, 0, len(lastParts)+len(currentParts))
				for _, p := range lastParts {
					combinedParts = append(combinedParts, []byte(p.Raw))
				}
				for _, p := range currentParts {
					combinedParts = append(combinedParts, []byte(p.Raw))
				}
				updated, err := sjson.SetRawBytes(lastJSON, "parts", JoinRawArray(combinedParts))
				if err == nil {
					merged[lastIndex] = updated
					continue
				}
			}
		}
		merged = append(merged, content)
	}
	return merged
}

// SplitGeminiFunctionResponseTurns separates functionResponse parts from
// other user parts. Gemini/Antigravity requires a function response turn to
// immediately follow the model turn containing the corresponding call; text
// or reminders in the same user turn or intervening reminder turns can otherwise
// make the response appear orphaned to the upstream validator.
func SplitGeminiFunctionResponseTurns(contents [][]byte) [][]byte {
	if len(contents) == 0 {
		return contents
	}
	split := make([][]byte, 0, len(contents))
	for _, content := range contents {
		if gjson.GetBytes(content, "role").String() != "user" || !ContentHasGeminiFunctionResponse(content) {
			split = append(split, content)
			continue
		}
		parts := gjson.GetBytes(content, "parts").Array()
		responseParts := make([][]byte, 0, len(parts))
		otherParts := make([][]byte, 0, len(parts))
		for _, part := range parts {
			if GeminiPartFunctionResponse(part).Exists() {
				responseParts = append(responseParts, []byte(part.Raw))
			} else {
				otherParts = append(otherParts, []byte(part.Raw))
			}
		}
		var responseTurn, otherTurn []byte
		var responseErr, otherErr error
		if len(responseParts) > 0 {
			responseTurn, responseErr = sjson.SetRawBytes(content, "parts", JoinRawArray(responseParts))
		}
		if len(otherParts) > 0 {
			otherTurn, otherErr = sjson.SetRawBytes(content, "parts", JoinRawArray(otherParts))
		}
		if responseErr != nil || otherErr != nil {
			// Preserve the original turn if rebuilding either split turn fails.
			split = append(split, content)
			continue
		}
		if len(responseTurn) > 0 {
			split = append(split, responseTurn)
		}
		if len(otherTurn) > 0 {
			split = append(split, otherTurn)
		}
	}

	// For any consecutive block of user turns immediately following a model turn
	// containing function calls, ensure that turns containing functionResponse
	// precede pure text/reminder turns. This guarantees that function responses
	// immediately follow the preceding model turn even when mid-session system
	// or developer reminders were inserted between the model and tool_result turns.
	out := make([][]byte, 0, len(split))
	n := len(split)
	for i := 0; i < n; {
		if gjson.GetBytes(split[i], "role").String() != "user" {
			out = append(out, split[i])
			i++
			continue
		}
		precedingModelHasFC := len(out) > 0 &&
			gjson.GetBytes(out[len(out)-1], "role").String() == "model" &&
			ContentHasGeminiFunctionCall(out[len(out)-1])

		j := i
		hasFR := false
		for j < n && gjson.GetBytes(split[j], "role").String() == "user" {
			if ContentHasGeminiFunctionResponse(split[j]) {
				hasFR = true
			}
			j++
		}
		userRun := split[i:j]
		if precedingModelHasFC && hasFR && len(userRun) > 1 {
			var combinedFRParts [][]byte
			otherTurns := make([][]byte, 0, len(userRun))
			for _, turn := range userRun {
				if ContentHasGeminiFunctionResponse(turn) {
					parts := gjson.GetBytes(turn, "parts").Array()
					for _, p := range parts {
						combinedFRParts = append(combinedFRParts, []byte(p.Raw))
					}
				} else {
					otherTurns = append(otherTurns, turn)
				}
			}
			if len(combinedFRParts) > 0 {
				frTurn := []byte(`{"role":"user","parts":[]}`)
				frTurn, _ = sjson.SetRawBytes(frTurn, "parts", JoinRawArray(combinedFRParts))
				out = append(out, frTurn)
			}
			out = append(out, otherTurns...)
		} else {
			out = append(out, userRun...)
		}
		i = j
	}
	return out
}

// ContainsJSONRef reports whether value (recursively) contains a string-valued "$ref" property.
func ContainsJSONRef(value gjson.Result) bool {
	if !value.IsObject() && !value.IsArray() {
		return false
	}
	found := false
	value.ForEach(func(key, child gjson.Result) bool {
		if value.IsObject() && key.String() == "$ref" && child.Type == gjson.String {
			found = true
			return false
		}
		if ContainsJSONRef(child) {
			found = true
			return false
		}
		return true
	})
	return found
}

// SetGeminiFunctionResponseResult sets the Gemini functionResponse result or response field.
// If the result contains a JSON Schema or OpenAPI "$ref" property with a string value, it is
// preserved as opaque JSON text with sjson.SetBytes. This prevents Gemini/Vertex AI from
// interpreting "$ref" as a reference to a media part in function_response.parts and rejecting
// the request with HTTP 400.
// If path points to "functionResponse.response" or "response", a stringified result is placed
// under the ".result" child object to satisfy Gemini's requirement that "response" is an object.
func SetGeminiFunctionResponseResult(part []byte, path string, result gjson.Result) []byte {
	if !result.Exists() {
		part, _ = sjson.SetBytes(part, path, "")
		return part
	}
	if ContainsJSONRef(result) {
		targetPath := path
		if strings.HasSuffix(path, "response") {
			targetPath = path + ".result"
		}
		part, _ = sjson.SetBytes(part, targetPath, result.Raw)
		return part
	}
	part, _ = sjson.SetRawBytes(part, path, []byte(result.Raw))
	return part
}

// SetGeminiFunctionResponseRaw is a convenience wrapper around SetGeminiFunctionResponseResult
// for raw JSON strings.
func SetGeminiFunctionResponseRaw(part []byte, path string, rawJSON string) []byte {
	trimmed := strings.TrimSpace(rawJSON)
	if trimmed == "" {
		part, _ = sjson.SetBytes(part, path, "")
		return part
	}
	return SetGeminiFunctionResponseResult(part, path, gjson.Parse(trimmed))
}
