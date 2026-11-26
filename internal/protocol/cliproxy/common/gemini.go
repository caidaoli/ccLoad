package common

import (
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
)

// GeminiClaudeMessageID keeps a valid provider response ID or creates an ID for
// Claude clients when Gemini does not send one.
func GeminiClaudeMessageID(responseID ojson.Result) string {
	if id := strings.TrimSpace(responseID.Str); id != "" {
		return id
	}
	return "msg_" + uuid.NewString()
}

// HasAntigravityResponsePayload reports whether a chunk carries actual generated
// content or non-zero output token accounting. Presence alone is not enough: an
// envelope such as `{}`, `{"response":{}}`, `{"response":{"candidates":[]}}`, or
// a signature-only empty-text part with zero output tokens must not count as a
// started stream, otherwise a keepalive or zero-output event would be enough to
// finalize a stream that produced nothing.
func HasAntigravityResponsePayload(rawJSON []byte) bool {
	for _, path := range []string{"response.candidates", "candidates"} {
		if candidates := ojson.GetBytesNoCopy(rawJSON, path); candidates.IsArray() {
			for _, cand := range candidates.Array() {
				if strings.TrimSpace(cand.Get("finishReason").String()) != "" {
					return true
				}
				if chunks := cand.Get("groundingMetadata.groundingChunks"); chunks.IsArray() && len(chunks.Array()) > 0 {
					return true
				}
				for _, part := range cand.Get("content.parts").Array() {
					hasVisibleInlineData := !part.Get("thought").Bool() &&
						(part.Get("inlineData.data").String() != "" || part.Get("inline_data.data").String() != "")
					if part.Get("text").String() != "" ||
						strings.TrimSpace(GeminiPartFunctionCall(part).Get("name").String()) != "" ||
						hasVisibleInlineData ||
						part.Get("fileData").Exists() || part.Get("file_data").Exists() ||
						part.Get("executableCode").Exists() || part.Get("executable_code").Exists() ||
						part.Get("codeExecutionResult").Exists() || part.Get("code_execution_result").Exists() {
						return true
					}
				}
			}
		}
	}
	// FilterSSEUsageMetadata renames non-terminal usage to cpaUsageMetadata before
	// the translator sees it, so both spellings must be accepted.
	for _, path := range []string{
		"response.usageMetadata", "response.cpaUsageMetadata",
		"usageMetadata", "cpaUsageMetadata",
	} {
		if usage := ojson.GetBytesNoCopy(rawJSON, path); usage.IsObject() {
			if usage.Get("candidatesTokenCount").Int() > 0 || usage.Get("thoughtsTokenCount").Int() > 0 {
				return true
			}
		}
	}
	return false
}

// IsGeminiThoughtPart reports whether a Gemini part contains hidden model thought.
func IsGeminiThoughtPart(part ojson.Result) bool {
	return part.Get("thought").Bool()
}

// GeminiUsageMetadataFromOpenAI maps Chat and Responses usage into Gemini's
// separate candidate and thought counts. Cached tokens stay part of the prompt.
func GeminiUsageMetadataFromOpenAI(usage ojson.Result, out []byte) []byte {
	prompt, hasPrompt := tokenCountFromUsage(usage, "prompt_tokens", "input_tokens")
	output, hasOutput := tokenCountFromUsage(usage, "completion_tokens", "output_tokens")
	total, hasTotal := tokenCountFromUsage(usage, "total_tokens")
	thoughts, hasThoughts := tokenCountFromUsage(usage, "completion_tokens_details.reasoning_tokens", "output_tokens_details.reasoning_tokens")
	if hasPrompt {
		out, _ = ojson.SetBytes(out, "promptTokenCount", prompt)
	}
	if hasOutput {
		out, _ = ojson.SetBytes(out, "candidatesTokenCount", max(0, output-thoughts))
	}
	if hasTotal {
		out, _ = ojson.SetBytes(out, "totalTokenCount", total)
	} else if hasPrompt || hasOutput {
		out, _ = ojson.SetBytes(out, "totalTokenCount", prompt+output)
	}
	if hasThoughts {
		out, _ = ojson.SetBytes(out, "thoughtsTokenCount", thoughts)
	}
	if cached, ok := tokenCountFromUsage(usage, "prompt_tokens_details.cached_tokens", "input_tokens_details.cached_tokens"); ok {
		out, _ = ojson.SetBytes(out, "cachedContentTokenCount", cached)
	}
	return out
}

// tokenCountFromUsage preserves explicit zeros and skips null aliases. Some
// Chat-compatible upstreams encode token counts as JSON strings.
func tokenCountFromUsage(usage ojson.Result, paths ...string) (int64, bool) {
	for _, path := range paths {
		value := usage.Get(path)
		switch value.Type {
		case ojson.Number:
			if count := value.Int(); count >= 0 && float64(count) == value.Num {
				return count, true
			}
		case ojson.String:
			if count, err := strconv.ParseInt(value.Str, 10, 64); err == nil && count >= 0 {
				return count, true
			}
		}
	}
	return 0, false
}

// ChatStopSequences reads nonempty stop strings from the Chat stop union.
func ChatStopSequences(stop ojson.Result) []string {
	if stop.Type == ojson.String && stop.Str != "" {
		return []string{stop.Str}
	}
	if !stop.IsArray() {
		return nil
	}
	var sequences []string
	stop.ForEach(func(_, item ojson.Result) bool {
		if item.Type == ojson.String && item.Str != "" {
			sequences = append(sequences, item.Str)
		}
		return true
	})
	return sequences
}

// GeminiConfigObject selects the first valid object among Gemini SDK aliases.
func GeminiConfigObject(parent ojson.Result, keys ...string) ojson.Result {
	for _, key := range keys {
		if value := parent.Get(key); value.IsObject() {
			return value
		}
	}
	return ojson.Result{}
}

// GeminiConfigNumber selects a numeric field while distinguishing zero from null.
func GeminiConfigNumber(parent ojson.Result, keys ...string) ojson.Result {
	for _, key := range keys {
		if value := parent.Get(key); value.Type == ojson.Number {
			return value
		}
	}
	return ojson.Result{}
}

// GeminiConfigInt selects the first integer that meets the field's minimum.
func GeminiConfigInt(parent ojson.Result, minValue int64, keys ...string) (int64, bool) {
	for _, key := range keys {
		value := parent.Get(key)
		if value.Type == ojson.Number {
			if number := value.Int(); number >= minValue && float64(number) == value.Num {
				return number, true
			}
		}
	}
	return 0, false
}

// GeminiConfigStrings selects a string list, skipping entries of other JSON types.
func GeminiConfigStrings(parent ojson.Result, keys ...string) []string {
	for _, key := range keys {
		value := parent.Get(key)
		if !value.IsArray() {
			continue
		}
		var result []string
		for _, item := range value.Array() {
			if item.Type == ojson.String {
				result = append(result, item.Str)
			}
		}
		return result
	}
	return nil
}

// GeminiThinkingLevel selects the first nonempty SDK spelling of a thinking level.
func GeminiThinkingLevel(config ojson.Result) string {
	for _, key := range []string{"thinkingLevel", "thinking_level"} {
		if value := config.Get(key); value.Type == ojson.String {
			if level := strings.TrimSpace(value.Str); level != "" {
				return strings.ToLower(level)
			}
		}
	}
	return ""
}

// GeminiThinkingConfig reads the first valid nested level and budget independently across SDK spellings.
func GeminiThinkingConfig(root ojson.Result) (level string, budget int64, hasBudget bool) {
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
func GeminiIncludeThoughts(root ojson.Result) ojson.Type {
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
			if value := generation.Get(path); value.Type == ojson.True || value.Type == ojson.False {
				return value.Type
			}
		}
	}
	return ojson.Null
}

// GeminiMessageRole maps Gemini content roles to roles accepted by the other
// supported wire protocols. Missing roles are user content unless the turn
// contains a function call from the model.
func GeminiMessageRole(role string, parts ojson.Result) string {
	switch role {
	case "model":
		return "assistant"
	case "":
		if GeminiPartsHaveFunctionCall(parts) {
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
		role := ojson.GetBytesNoCopy(content, "role").String()
		partsResult := ojson.GetBytesNoCopy(content, "parts")
		if !partsResult.IsArray() || len(partsResult.Array()) == 0 {
			continue
		}
		if len(merged) > 0 {
			lastIndex := len(merged) - 1
			lastJSON := merged[lastIndex]
			lastRole := ojson.GetBytesNoCopy(lastJSON, "role").String()
			if lastRole == "user" && role == "user" {
				lastParts := ojson.GetBytesNoCopy(lastJSON, "parts").Array()
				combinedParts := make([][]byte, 0, len(lastParts)+len(partsResult.Array()))
				for _, p := range lastParts {
					combinedParts = append(combinedParts, []byte(p.Raw))
				}
				for _, p := range partsResult.Array() {
					combinedParts = append(combinedParts, []byte(p.Raw))
				}
				combinedParts = ReorderGeminiUserParts(combinedParts)
				updated, err := ojson.SetRawBytes(lastJSON, "parts", JoinRawArray(combinedParts))
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
	return GeminiPartsHaveFunctionCall(ojson.GetBytesNoCopy(content, "parts"))
}

// GeminiPartsHaveFunctionCall reports whether a parsed Gemini parts array contains any functionCall part
// without copying or re-parsing the enclosing content raw bytes.
func GeminiPartsHaveFunctionCall(parts ojson.Result) bool {
	hasFC := false
	parts.ForEach(func(_, part ojson.Result) bool {
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
	return GeminiPartsHaveFunctionResponse(ojson.GetBytesNoCopy(content, "parts"))
}

// GeminiPartsHaveFunctionResponse reports whether a parsed Gemini parts array contains any functionResponse part
// without copying or re-parsing the enclosing content raw bytes.
func GeminiPartsHaveFunctionResponse(parts ojson.Result) bool {
	hasFR := false
	parts.ForEach(func(_, part ojson.Result) bool {
		if GeminiPartFunctionResponse(part).Exists() {
			hasFR = true
			return false
		}
		return true
	})
	return hasFR
}

// GeminiPartFunctionCall returns the functionCall or function_call field.
func GeminiPartFunctionCall(part ojson.Result) ojson.Result {
	return geminiPartToolObject(part, "functionCall", "function_call")
}

// GeminiPartFunctionResponse returns the functionResponse or function_response field.
func GeminiPartFunctionResponse(part ojson.Result) ojson.Result {
	return geminiPartToolObject(part, "functionResponse", "function_response")
}

func geminiPartToolObject(part ojson.Result, keys ...string) ojson.Result {
	for _, key := range keys {
		value := part.Get(key)
		if !value.IsObject() {
			continue
		}
		hasMember := false
		value.ForEach(func(_, _ ojson.Result) bool {
			hasMember = true
			return false
		})
		if hasMember {
			return value
		}
	}
	return ojson.Result{}
}

// GeminiSystemInstructionText reads the first alias with visible system text.
func GeminiSystemInstructionText(root ojson.Result, keys ...string) string {
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

// NormalizeGeminiTurnRole maps a Gemini content role to "user" or "assistant",
// inferring missing or unrecognized roles from functionCall/functionResponse parts
// or alternating from previousRole (matching Gemini ingress normalization).
func NormalizeGeminiTurnRole(rawRole string, parts ojson.Result, previousRole string) string {
	switch strings.ToLower(strings.TrimSpace(rawRole)) {
	case "model", "assistant":
		return "assistant"
	case "user", "function", "tool":
		return "user"
	default:
		if GeminiPartsHaveFunctionCall(parts) {
			return "assistant"
		}
		if GeminiPartsHaveFunctionResponse(parts) {
			return "user"
		}
		if previousRole == "user" {
			return "assistant"
		}
		return "user"
	}
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
		isFR := ojson.GetBytesNoCopy(p, "functionResponse").Exists() || ojson.GetBytesNoCopy(p, "function_response").Exists()
		if isFR {
			hasFR = true
		} else if hasFR && ojson.GetBytesNoCopy(p, "text").Exists() {
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
		if ojson.GetBytesNoCopy(p, "text").Exists() {
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
	pendingContents := make([][]byte, 0, len(contents))
	pendingParts := make([][]byte, 0, len(contents))
	flushPending := func() {
		if len(pendingContents) == 0 {
			return
		}
		if len(pendingContents) == 1 {
			merged = append(merged, pendingContents[0])
		} else if updated, errSet := ojson.SetRawBytes(pendingContents[0], "parts", JoinRawArray(pendingParts)); errSet == nil {
			merged = append(merged, updated)
		} else {
			merged = append(merged, pendingContents...)
		}
		pendingContents = pendingContents[:0]
		pendingParts = pendingParts[:0]
	}

	for _, content := range contents {
		if len(content) == 0 {
			continue
		}
		role := ojson.GetBytesNoCopy(content, "role").String()
		partsResult := ojson.GetBytesNoCopy(content, "parts")
		if !partsResult.IsArray() || partsResult.Raw == "[]" || !partsResult.Get("0").Exists() {
			continue
		}
		if role == "user" && !ContentHasGeminiFunctionResponse(content) {
			pendingContents = append(pendingContents, content)
			for _, part := range partsResult.Array() {
				pendingParts = append(pendingParts, []byte(part.Raw))
			}
			continue
		}
		flushPending()
		merged = append(merged, content)
	}
	flushPending()
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
		if ojson.GetBytesNoCopy(content, "role").String() != "user" || !ContentHasGeminiFunctionResponse(content) {
			split = append(split, content)
			continue
		}
		parts := ojson.GetBytesNoCopy(content, "parts").Array()
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
			responseTurn, responseErr = ojson.SetRawBytes(content, "parts", JoinRawArray(responseParts))
		}
		if len(otherParts) > 0 {
			otherTurn, otherErr = ojson.SetRawBytes(content, "parts", JoinRawArray(otherParts))
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
		if ojson.GetBytesNoCopy(split[i], "role").String() != "user" {
			out = append(out, split[i])
			i++
			continue
		}
		precedingModelHasFC := len(out) > 0 &&
			ojson.GetBytesNoCopy(out[len(out)-1], "role").String() == "model" &&
			ContentHasGeminiFunctionCall(out[len(out)-1])

		j := i
		hasFR := false
		for j < n && ojson.GetBytesNoCopy(split[j], "role").String() == "user" {
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
					parts := ojson.GetBytesNoCopy(turn, "parts").Array()
					for _, p := range parts {
						combinedFRParts = append(combinedFRParts, []byte(p.Raw))
					}
				} else {
					otherTurns = append(otherTurns, turn)
				}
			}
			if len(combinedFRParts) > 0 {
				frTurn := []byte(`{"role":"user","parts":[]}`)
				frTurn, _ = ojson.SetRawBytes(frTurn, "parts", JoinRawArray(combinedFRParts))
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
func ContainsJSONRef(value ojson.Result) bool {
	if !value.IsObject() && !value.IsArray() {
		return false
	}
	found := false
	value.ForEach(func(key, child ojson.Result) bool {
		if value.IsObject() && key.String() == "$ref" && child.Type == ojson.String {
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
// preserved as opaque JSON text with ojson.SetBytes. This prevents Gemini/Vertex AI from
// interpreting "$ref" as a reference to a media part in function_response.parts and rejecting
// the request with HTTP 400.
// If path points to "functionResponse.response" or "response", a stringified result is placed
// under the ".result" child object to satisfy Gemini's requirement that "response" is an object.
func SetGeminiFunctionResponseResult(part []byte, path string, result ojson.Result) []byte {
	if !result.Exists() {
		part, _ = ojson.SetBytes(part, path, "")
		return part
	}
	if ContainsJSONRef(result) {
		targetPath := path
		if strings.HasSuffix(path, "response") {
			targetPath = path + ".result"
		}
		part, _ = ojson.SetBytes(part, targetPath, result.Raw)
		return part
	}
	part, _ = ojson.SetRawBytes(part, path, []byte(result.Raw))
	return part
}

// SetGeminiFunctionResponseRaw is a convenience wrapper around SetGeminiFunctionResponseResult
// for raw JSON strings.
func SetGeminiFunctionResponseRaw(part []byte, path string, rawJSON string) []byte {
	trimmed := strings.TrimSpace(rawJSON)
	if trimmed == "" {
		part, _ = ojson.SetBytes(part, path, "")
		return part
	}
	return SetGeminiFunctionResponseResult(part, path, ojson.Parse(trimmed))
}

// SetGeminiFunctionResponseResultDoc sets the Gemini functionResponse result or response field on doc.
func SetGeminiFunctionResponseResultDoc(doc *ojson.Doc, path string, result ojson.Result) {
	if doc == nil {
		return
	}
	if !result.Exists() {
		_ = doc.Set(path, "")
		return
	}
	if ContainsJSONRef(result) {
		targetPath := path
		if strings.HasSuffix(path, "response") {
			targetPath = path + ".result"
		}
		_ = doc.Set(targetPath, result.Raw)
		return
	}
	_ = doc.SetRaw(path, result.Raw)
}

// SetGeminiFunctionResponseRawDoc is a convenience wrapper around SetGeminiFunctionResponseResultDoc
// for raw JSON strings.
func SetGeminiFunctionResponseRawDoc(doc *ojson.Doc, path string, rawJSON string) {
	if doc == nil {
		return
	}
	trimmed := strings.TrimSpace(rawJSON)
	if trimmed == "" {
		_ = doc.Set(path, "")
		return
	}
	SetGeminiFunctionResponseResultDoc(doc, path, ojson.Parse(trimmed))
}

// SetGeminiFunctionCallArgsDoc writes a valid JSON object to path on doc for Gemini functionCall.args.
// Valid JSON-object strings are unwrapped; all other non-object or invalid inputs fall back to {}.
func SetGeminiFunctionCallArgsDoc(doc *ojson.Doc, path string, value ojson.Result) {
	if doc == nil {
		return
	}
	if value.IsObject() {
		raw := strings.TrimSpace(value.Raw)
		if ojson.Valid(raw) {
			_ = doc.SetRaw(path, raw)
			return
		}
	}
	if value.Type == ojson.String {
		raw := strings.TrimSpace(value.String())
		if raw != "" && ojson.Valid(raw) && ojson.Parse(raw).IsObject() {
			_ = doc.SetRaw(path, raw)
			return
		}
	}
	_ = doc.SetRaw(path, `{}`)
}

// NormalizeGeminiFunctionCallPart ensures a native Gemini functionCall part carries a valid JSON object
// in functionCall.args while preserving all other fields on the part.
func NormalizeGeminiFunctionCallPart(part ojson.Result) []byte {
	if args := part.Get("functionCall.args"); args.IsObject() && ojson.Valid(strings.TrimSpace(args.Raw)) {
		return []byte(part.Raw)
	}
	partDoc := ojson.NewDoc([]byte(part.Raw))
	SetGeminiFunctionCallArgsDoc(partDoc, "functionCall.args", part.Get("functionCall.args"))
	return partDoc.Bytes()
}
