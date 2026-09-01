package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	cliproxycommon "ccLoad/internal/protocol/cliproxy/common"
	cliproxyutil "ccLoad/internal/protocol/cliproxy/util"
	"ccLoad/internal/util"
	"ccLoad/internal/zedauth"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func isZedResponsesRequest(cfg *model.Config, upstream protocol.Protocol) bool {
	return cfg != nil && cfg.UsesZedOAuth() && upstream == protocol.Codex
}

type zedWirePlan struct {
	model             string
	provider          string
	providerProtocol  protocol.Protocol
	originalRequest   []byte
	translatedRequest []byte
	toolIdentities    map[string]cliproxyutil.ResponsesToolIdentity
}

func finalizeZedResponsesBody(registry *protocol.Registry, body, originalAnthropicRequest []byte) ([]byte, *zedWirePlan, error) {
	if !isMutableJSONObject(body) {
		return nil, nil, errors.New("finalize Zed Responses request: invalid JSON object")
	}
	// Reject trailing JSON (e.g. `{} []`). gjson.ValidBytes accepts it,
	// so check via the stdlib decoder which stops at the first value boundary.
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&json.RawMessage{}); err != nil {
		return nil, nil, errors.New("finalize Zed Responses request: invalid JSON object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("finalize Zed Responses request: trailing JSON")
	}

	modelName := strings.TrimSpace(jsonStringValue(gjson.GetBytes(body, "model")))
	if modelName == "" {
		return nil, nil, errors.New("finalize Zed Responses request: model is required")
	}
	if modelName == "gpt-5.6" {
		modelName = "gpt-5.6-sol"
		body = setJSONValue(body, "model", modelName)
	}
	provider, providerProtocol, err := zedProviderProtocol(modelName)
	if err != nil {
		return nil, nil, err
	}
	body = setJSONValue(body, "stream", true)
	body = normalizeZedCodexInput(body)
	body = stripZedEncryptedContent(body)
	originalRequest := body

	translatedRequest := originalRequest
	if provider == zedauth.ProviderOpenAI {
		translatedRequest, err = normalizeZedOpenAIProviderRequest(originalRequest, modelName)
	} else {
		if registry == nil {
			return nil, nil, errors.New("finalize Zed Responses request: protocol registry is unavailable")
		}
		translatedRequest, err = registry.TranslateRequest(protocol.Codex, providerProtocol, modelName, originalRequest, true)
		if err == nil {
			switch provider {
			case zedauth.ProviderAnthropic:
				translatedRequest, err = finalizeZedAnthropicProviderRequest(translatedRequest, originalRequest, originalAnthropicRequest)
			case zedauth.ProviderGoogle:
				translatedRequest, err = finalizeZedGoogleProviderRequest(translatedRequest, modelName)
			}
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("finalize Zed Responses request for %s: %w", provider, err)
	}
	envelope := struct {
		ThreadID        string          `json:"thread_id"`
		PromptID        string          `json:"prompt_id"`
		Intent          string          `json:"intent"`
		Provider        string          `json:"provider"`
		Model           string          `json:"model"`
		ProviderRequest json.RawMessage `json:"provider_request"`
	}{
		ThreadID: util.NewUUIDv4(), PromptID: util.NewUUIDv4(), Intent: "user_prompt",
		Provider: provider, Model: modelName, ProviderRequest: translatedRequest,
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, nil, errors.New("finalize Zed Responses request: encode failed")
	}
	var toolIdentities map[string]cliproxyutil.ResponsesToolIdentity
	if providerProtocol == protocol.Codex {
		toolIdentities = cliproxyutil.ResponsesToolReverseIdentityMap(originalRequest)
	}
	return encoded, &zedWirePlan{
		model: modelName, provider: provider, providerProtocol: providerProtocol,
		originalRequest: originalRequest, translatedRequest: translatedRequest,
		toolIdentities: toolIdentities,
	}, nil
}

func zedProviderProtocol(modelName string) (string, protocol.Protocol, error) {
	provider, ok := zedauth.ProviderForModel(modelName)
	if !ok {
		return "", "", fmt.Errorf("finalize Zed Responses request: unsupported model %q", modelName)
	}
	switch provider {
	case zedauth.ProviderOpenAI:
		return provider, protocol.Codex, nil
	case zedauth.ProviderAnthropic:
		return provider, protocol.Anthropic, nil
	case zedauth.ProviderGoogle:
		return provider, protocol.Gemini, nil
	default:
		return "", "", fmt.Errorf("finalize Zed Responses request: unsupported provider %q", provider)
	}
}

func normalizeZedCodexInput(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	switch {
	case input.Type == gjson.String:
		return setJSONRaw(body, "input", "["+zedMessageRaw("user", input.String())+"]")
	case !input.IsArray():
		return body
	}
	items := input.Array()
	rendered := make([]string, 0, len(items))
	changed := false
	for _, item := range items {
		if !item.IsObject() {
			rendered = append(rendered, item.Raw)
			continue
		}
		raw := []byte(item.Raw)
		itemChanged := false

		// Delete null-valued fields.
		item.ForEach(func(key, value gjson.Result) bool {
			if value.Type == gjson.Null {
				raw = deleteJSONPath(raw, key.String())
				itemChanged = true
			}
			return true
		})

		itemType := jsonStringValue(gjson.ParseBytes(raw).Get("type"))
		if itemType == "agent_message" {
			raw = setJSONValue(raw, "type", "message")
			raw = setJSONValue(raw, "role", "user")
			raw = deleteJSONPath(raw, "author")
			raw = deleteJSONPath(raw, "recipient")

			content := gjson.GetBytes(raw, "content")
			if content.IsArray() {
				// Rewrite encrypted_content parts → input_text.
				parts := content.Array()
				contentItems := make([]string, 0, len(parts))
				contentChanged := false
				for _, part := range parts {
					if part.IsObject() && jsonStringValue(part.Get("type")) == "encrypted_content" {
						encrypted := jsonStringValue(part.Get("encrypted_content"))
						if encrypted != "" {
							replacement := setJSONValue(setJSONValue([]byte(`{}`), "type", "input_text"), "text", encrypted)
							contentItems = append(contentItems, string(replacement))
							contentChanged = true
							continue
						}
					}
					contentItems = append(contentItems, part.Raw)
				}
				if contentChanged {
					raw = setJSONRaw(raw, "content", joinJSONRaw(contentItems))
				}
			} else if content.Type == gjson.String {
				// String content → input_text content block.
				contentType := "input_text"
				raw = setJSONRaw(raw, "content", "["+zedInputTextBlockRaw(contentType, content.String())+"]")
			}
			itemChanged = true
		}

		role := jsonStringValue(gjson.ParseBytes(raw).Get("role"))
		if role == "developer" {
			raw = setJSONValue(raw, "role", "system")
			role = "system"
			itemChanged = true
		}
		parsed := gjson.ParseBytes(raw)
		if role != "" && !parsed.Get("type").Exists() {
			raw = setJSONValue(raw, "type", "message")
			itemChanged = true
		}
		if parsed.Get("content").Type == gjson.String {
			contentType := "input_text"
			if role == "assistant" {
				contentType = "output_text"
			}
			raw = setJSONRaw(raw, "content", "["+zedInputTextBlockRaw(contentType, parsed.Get("content").String())+"]")
			itemChanged = true
		}

		if itemChanged {
			changed = true
		}
		rendered = append(rendered, string(raw))
	}
	if !changed {
		return body
	}
	return setJSONRaw(body, "input", joinJSONRaw(rendered))
}

func stripZedEncryptedContent(body []byte) []byte {
	// Recursively remove all encrypted_content fields.
	body, _ = rewriteJSONMembers(body, func(key string, _ gjson.Result) (string, bool) {
		return "", key == "encrypted_content"
	})
	// Filter reasoning.* from include array.
	body = filterZedThinkingIncludes(body)
	// Drop compaction items and empty reasoning items from input.
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	items := input.Array()
	rendered := make([]string, 0, len(items))
	changed := false
	for _, item := range items {
		if !item.IsObject() {
			rendered = append(rendered, item.Raw)
			continue
		}
		itemType := jsonStringValue(item.Get("type"))
		switch itemType {
		case "compaction":
			changed = true
			continue
		case "reasoning":
			if !zedReasoningHasSummary(item) {
				changed = true
				continue
			}
		}
		rendered = append(rendered, item.Raw)
	}
	if !changed {
		return body
	}
	return setJSONRaw(body, "input", joinJSONRaw(rendered))
}

func zedReasoningHasSummary(item gjson.Result) bool {
	summary := item.Get("summary")
	if !summary.IsArray() {
		return false
	}
	for _, part := range summary.Array() {
		if !part.IsObject() {
			continue
		}
		if strings.TrimSpace(jsonStringValue(part.Get("text"))) != "" {
			return true
		}
	}
	return false
}

// filterZedThinkingIncludes removes reasoning.* entries from the include array.
func filterZedThinkingIncludes(body []byte) []byte {
	include := gjson.GetBytes(body, "include")
	if !include.IsArray() {
		return body
	}
	items := include.Array()
	rendered := make([]string, 0, len(items))
	removed := false
	for _, item := range items {
		if item.Type == gjson.String && strings.HasPrefix(item.String(), "reasoning.") {
			removed = true
			continue
		}
		rendered = append(rendered, item.Raw)
	}
	if !removed {
		return body
	}
	if len(rendered) == 0 {
		return deleteJSONPath(body, "include")
	}
	return setJSONRaw(body, "include", joinJSONRaw(rendered))
}

func normalizeZedOpenAIProviderRequest(body []byte, modelName string) ([]byte, error) {
	// Collect declarations before removing Responses Lite's additional_tools
	// input items. The shared helper expands namespace children and applies the
	// same qualified-name and first-wins rules as the normal Responses paths.
	root := gjson.ParseBytes(body)
	descriptors := cliproxyutil.CollectResponsesToolDescriptors(root)
	winners := cliproxyutil.CollectResponsesToolWinners(root)

	// Remove additional_tools input items.
	if input := gjson.GetBytes(body, "input"); input.IsArray() {
		items := input.Array()
		rendered := make([]string, 0, len(items))
		changed := false
		for _, item := range items {
			if item.IsObject() && jsonStringValue(item.Get("type")) == "additional_tools" {
				changed = true
				continue
			}
			rendered = append(rendered, item.Raw)
		}
		if changed {
			body = setJSONRaw(body, "input", joinJSONRaw(rendered))
		}
	}
	body = normalizeZedOpenAIToolCallHistory(body)

	selectedTool := zedSelectedToolName(gjson.GetBytes(body, "tool_choice"))
	if selectedTool != "" {
		body = setJSONValue(body, "tool_choice", "required")
	}
	toolsRaw := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		winner, ok := winners[descriptor.Name]
		if !ok || winner.Order != descriptor.Order {
			continue
		}
		if selectedTool != "" && descriptor.Name != selectedTool {
			continue
		}
		tool := []byte(descriptor.Tool.Raw)
		var err error
		tool, err = sjson.SetBytes(tool, "type", descriptor.ToolType)
		if err != nil {
			return nil, fmt.Errorf("finalize Zed Responses request: encode tool type")
		}
		tool, err = sjson.SetBytes(tool, "name", descriptor.Name)
		if err != nil {
			return nil, fmt.Errorf("finalize Zed Responses request: encode tool name")
		}
		tool, err = sjson.DeleteBytes(tool, "namespace")
		if err != nil {
			return nil, fmt.Errorf("finalize Zed Responses request: strip tool namespace")
		}
		toolsRaw = append(toolsRaw, string(tool))
	}
	if gjson.GetBytes(body, "tools").Exists() || len(descriptors) > 0 {
		body = setJSONRaw(body, "tools", joinJSONRaw(toolsRaw))
	}

	effort := jsonStringValue(gjson.GetBytes(body, "reasoning.effort"))
	if strings.HasPrefix(modelName, "gpt-5.6") {
		if effort == "" {
			effort = "xhigh"
		}
		switch effort {
		case "none", "low", "medium", "high", "xhigh":
		default:
			return nil, fmt.Errorf("finalize Zed Responses request: unsupported GPT-5.6 reasoning effort %q", effort)
		}
	} else if strings.HasPrefix(modelName, "gpt-5.5") {
		effort = "xhigh"
	}
	if effort != "" {
		body = setJSONValue(body, "reasoning.effort", effort)
		if effort != "none" && !gjson.GetBytes(body, "reasoning.summary").Exists() {
			body = setJSONValue(body, "reasoning.summary", "detailed")
		}
	}
	requestedBudget := zedOutputBudgetBytes(body)
	body = deleteJSONPath(body, "max_completion_tokens")
	body = deleteJSONPath(body, "max_tokens")
	if effort == "xhigh" && requestedBudget < 32768 {
		requestedBudget = 32768
	}
	if requestedBudget > 0 {
		body = setJSONValue(body, "max_output_tokens", requestedBudget)
	}
	return body, nil
}

func normalizeZedOpenAIToolCallHistory(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	var patches []struct {
		path, value string
	}
	for index, item := range input.Array() {
		if !item.IsObject() {
			continue
		}
		itemType := jsonStringValue(item.Get("type"))
		if itemType != "function_call" && itemType != "custom_tool_call" {
			continue
		}
		name := jsonStringValue(item.Get("name"))
		namespace := jsonStringValue(item.Get("namespace"))
		if name == "" || namespace == "" {
			continue
		}
		prefix := "input." + strconv.Itoa(index)
		qualified := cliproxyutil.QualifyResponsesNamespaceToolName(namespace, name)
		patches = append(patches,
			struct{ path, value string }{prefix + ".name", qualified},
		)
	}
	for _, patch := range patches {
		body = setJSONValue(body, patch.path, patch.value)
		// Delete namespace after setting name.
		namespaceIdx := strings.LastIndex(patch.path, ".name")
		if namespaceIdx >= 0 {
			body = deleteJSONPath(body, patch.path[:namespaceIdx]+".namespace")
		}
	}
	return body
}

func finalizeZedGoogleProviderRequest(body []byte, modelName string) ([]byte, error) {
	if !isMutableJSONObject(body) {
		return nil, errors.New("decode translated Google request")
	}
	// Reject trailing JSON.
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&json.RawMessage{}); err != nil {
		return nil, errors.New("decode translated Google request")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode translated Google request")
	}

	body = setJSONValue(body, "model", "models/"+modelName)
	if !gjson.GetBytes(body, "generationConfig").IsObject() {
		body = setJSONRaw(body, "generationConfig", `{}`)
	}
	if !gjson.GetBytes(body, "generationConfig.candidateCount").Exists() {
		body = setJSONValue(body, "generationConfig.candidateCount", 1)
	}
	if !gjson.GetBytes(body, "generationConfig.stopSequences").Exists() {
		body = setJSONRaw(body, "generationConfig.stopSequences", `[]`)
	}
	if !gjson.GetBytes(body, "generationConfig.temperature").Exists() {
		body = setJSONValue(body, "generationConfig.temperature", 1.0)
	}
	// Promote parametersJsonSchema → parameters in function declarations.
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for toolIndex, tool := range tools.Array() {
			decls := tool.Get("functionDeclarations")
			if !decls.IsArray() {
				continue
			}
			for declIndex, decl := range decls.Array() {
				if decl.Get("parametersJsonSchema").Exists() && !decl.Get("parameters").Exists() {
					path := fmt.Sprintf("tools.%d.functionDeclarations.%d", toolIndex, declIndex)
					body = setJSONRaw(body, path+".parameters", decl.Get("parametersJsonSchema").Raw)
					body = deleteJSONPath(body, path+".parametersJsonSchema")
				}
			}
		}
	}
	return body, nil
}

func finalizeZedAnthropicProviderRequest(body, originalRequest, originalAnthropicRequest []byte) ([]byte, error) {
	if !isMutableJSONObject(body) {
		return nil, errors.New("decode translated Anthropic request")
	}
	// Reject trailing JSON.
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&json.RawMessage{}); err != nil {
		return nil, errors.New("decode translated Anthropic request")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode translated Anthropic request")
	}

	// Zed owns the /completions stream. Its native Anthropic provider_request
	// does not carry the Anthropic HTTP transport's stream flag.
	body = deleteJSONPath(body, "stream")
	body = normalizeZedAnthropicContentBlocks(body)
	body = restoreZedAnthropicCacheControls(body, originalAnthropicRequest)
	if zedOutputBudgetBytes(originalRequest) == 0 {
		// The shared converter defaults to 32000, but Zed's native request and
		// Claude Sonnet 4.5 both require the conservative 8192 default.
		body = setJSONValue(body, "max_tokens", 8192)
	}
	return body, nil
}

func restoreZedAnthropicCacheControls(body, originalRequest []byte) []byte {
	if len(originalRequest) == 0 || !gjson.ValidBytes(originalRequest) {
		return body
	}
	original := gjson.ParseBytes(originalRequest)
	if !original.IsObject() {
		return body
	}

	// Top-level cache_control.
	if cc := original.Get("cache_control"); cc.Exists() {
		body = setJSONRaw(body, "cache_control", cc.Raw)
	}

	// System blocks: restore by index.
	origSystem := original.Get("system")
	bodySystem := gjson.GetBytes(body, "system")
	if origSystem.IsArray() && bodySystem.IsArray() {
		origBlocks := origSystem.Array()
		bodyBlocks := bodySystem.Array()
		for i := 0; i < len(origBlocks) && i < len(bodyBlocks); i++ {
			if cc := origBlocks[i].Get("cache_control"); cc.Exists() {
				body = setJSONRaw(body, "system."+strconv.Itoa(i)+".cache_control", cc.Raw)
			}
		}
	}

	// Tools: restore by name.
	origTools := original.Get("tools")
	if origTools.IsArray() {
		toolCacheControls := make(map[string]string)
		for _, tool := range origTools.Array() {
			name := jsonStringValue(tool.Get("name"))
			if name == "" {
				continue
			}
			if cc := tool.Get("cache_control"); cc.Exists() {
				toolCacheControls[name] = cc.Raw
			}
		}
		if len(toolCacheControls) > 0 {
			bodyTools := gjson.GetBytes(body, "tools")
			if bodyTools.IsArray() {
				for index, tool := range bodyTools.Array() {
					name := jsonStringValue(tool.Get("name"))
					if ccRaw, ok := toolCacheControls[name]; ok {
						body = setJSONRaw(body, "tools."+strconv.Itoa(index)+".cache_control", ccRaw)
					}
				}
			}
		}
	}
	return body
}

func normalizeZedAnthropicContentBlocks(body []byte) []byte {
	textBlockRaw := func(text string) string {
		block, err := sjson.Set(`{"type":"text"}`, "text", text)
		if err != nil {
			return `{"type":"text","text":""}`
		}
		return block
	}
	// system string → array of text blocks.
	if system := gjson.GetBytes(body, "system"); system.Type == gjson.String {
		body = setJSONRaw(body, "system", "["+textBlockRaw(system.String())+"]")
	}
	// messages: string content → array, tool_result missing is_error → false.
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body
	}
	for msgIndex, message := range messages.Array() {
		if !message.IsObject() {
			continue
		}
		content := message.Get("content")
		msgPrefix := "messages." + strconv.Itoa(msgIndex)
		if content.Type == gjson.String {
			body = setJSONRaw(body, msgPrefix+".content", "["+textBlockRaw(content.String())+"]")
			content = gjson.GetBytes(body, msgPrefix+".content")
		}
		if !content.IsArray() {
			continue
		}
		for blockIndex, block := range content.Array() {
			if !block.IsObject() || jsonStringValue(block.Get("type")) != "tool_result" {
				continue
			}
			if !block.Get("is_error").Exists() {
				body = setJSONValue(body, msgPrefix+".content."+strconv.Itoa(blockIndex)+".is_error", false)
			}
		}
	}
	return body
}

func zedMessageRaw(role, text string) string {
	msg := setJSONValue(setJSONValue([]byte(`{"type":"message"}`), "role", role), "content", "")
	block := setJSONValue(setJSONValue([]byte(`{}`), "type", "input_text"), "text", text)
	msg = setJSONRaw(msg, "content", "["+string(block)+"]")
	return string(msg)
}

func zedInputTextBlockRaw(typeName, text string) string {
	return string(setJSONValue(setJSONValue([]byte(`{}`), "type", typeName), "text", text))
}

func zedSelectedToolName(choice gjson.Result) string {
	if !choice.IsObject() {
		return ""
	}
	if name := jsonStringValue(choice.Get("name")); name != "" {
		return qualifyZedToolName(choice, name)
	}
	name := jsonStringValue(choice.Get("function.name"))
	return qualifyZedToolName(choice, name)
}

func qualifyZedToolName(choice gjson.Result, name string) string {
	if name == "" {
		return name
	}
	namespace := jsonStringValue(choice.Get("namespace"))
	if namespace == "" {
		namespace = jsonStringValue(choice.Get("function.namespace"))
	}
	if namespace == "" {
		namespace = jsonStringValue(choice.Get("custom.namespace"))
	}
	return cliproxyutil.QualifyResponsesNamespaceToolName(namespace, name)
}

func zedOutputBudgetBytes(body []byte) int64 {
	for _, key := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		value := gjson.GetBytes(body, key)
		if value.Type == gjson.Number {
			if n, ok := jsonIntegerValue(value); ok {
				return n
			}
		}
	}
	return 0
}

func injectZedResponsesHeaders(request *http.Request, accessToken string) {
	if request == nil {
		return
	}
	for name := range request.Header {
		delete(request.Header, name)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	request.Header.Set("User-Agent", zedauth.UserAgent())
	request.Header.Set("x-zed-version", zedauth.ZedVersion)
	request.Header.Set("x-zed-client-supports-status-messages", "true")
	request.Header.Set("x-zed-client-supports-stream-ended-request-completion-status", "true")
}

func prepareZedResponsesResponse(response *http.Response, plan *zedWirePlan, registry *protocol.Registry) error {
	if response == nil || response.Body == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil
	}
	if plan == nil {
		return errors.New("prepare Zed Responses response: wire plan is missing")
	}
	if plan.providerProtocol != protocol.Codex && registry == nil {
		return errors.New("prepare Zed Responses response: protocol registry is unavailable")
	}
	ctx := context.Background()
	if response.Request != nil {
		ctx = response.Request.Context()
	}
	original := response.Body
	reader, writer := io.Pipe()
	go relayZedResponsesEvents(ctx, original, writer, plan, registry)
	response.Body = &zedResponsesBody{PipeReader: reader, upstream: original}
	response.Header.Set("Content-Type", "text/event-stream")
	response.Header.Set("Cache-Control", "no-cache")
	response.Header.Del("Content-Length")
	response.ContentLength = -1
	return nil
}

type zedResponsesBody struct {
	*io.PipeReader
	upstream io.ReadCloser
}

func (b *zedResponsesBody) Close() error {
	readerErr := b.PipeReader.Close()
	upstreamErr := b.upstream.Close()
	if readerErr != nil {
		return readerErr
	}
	return upstreamErr
}

func relayZedResponsesEvents(ctx context.Context, upstream io.ReadCloser, output *io.PipeWriter, plan *zedWirePlan, registry *protocol.Registry) {
	defer func() { _ = upstream.Close() }()
	reader := bufio.NewReader(upstream)
	state := &zedRelayState{}
	for {
		line, readErr := reader.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			if bytes.HasPrefix(line, []byte("data:")) {
				line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			}
			ended, writeErr := writeZedResponsesEvent(ctx, output, line, plan, registry, state)
			if writeErr != nil {
				_ = output.CloseWithError(writeErr)
				return
			}
			if ended {
				_ = output.Close()
				return
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				_ = output.Close()
			} else {
				_ = output.CloseWithError(readErr)
			}
			return
		}
	}
}

type zedRelayState struct {
	transform          any
	failed             bool
	anthropicStarted   bool
	anthropicStopped   bool
	anthropicToolUse   bool
	anthropicOpenIndex *int
}

func writeZedResponsesEvent(
	ctx context.Context,
	output io.Writer,
	line []byte,
	plan *zedWirePlan,
	registry *protocol.Registry,
	state *zedRelayState,
) (bool, error) {
	var envelope struct {
		Status string          `json:"status"`
		Event  json.RawMessage `json:"event"`
		Type   string          `json:"type"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return false, fmt.Errorf("decode Zed Responses event: %w", err)
	}
	if envelope.Status != "" && len(envelope.Event) == 0 && envelope.Type == "" {
		if envelope.Status != "stream_ended" {
			if envelope.Status == "error" || envelope.Status == "failed" {
				return false, fmt.Errorf("zed stream ended with status %q", envelope.Status)
			}
			return false, nil
		}
		if plan.providerProtocol == protocol.Anthropic {
			if err := finishZedAnthropicStream(ctx, output, plan, registry, state); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	event := json.RawMessage(line)
	if len(envelope.Event) > 0 && !bytes.Equal(bytes.TrimSpace(envelope.Event), []byte("null")) {
		event = envelope.Event
	}
	if plan.providerProtocol != protocol.Codex {
		return false, writeZedProviderEvent(ctx, output, event, plan, registry, state)
	}
	event = restoreZedOpenAIToolIdentities(event, plan.toolIdentities)
	var eventType struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(event, &eventType); err != nil || strings.TrimSpace(eventType.Type) == "" {
		return false, errors.New("zed Responses event is missing type")
	}
	_, err := fmt.Fprintf(output, "event: %s\ndata: %s\n\n", eventType.Type, bytes.TrimSpace(event))
	return false, err
}

func restoreZedOpenAIToolIdentities(event []byte, identities map[string]cliproxyutil.ResponsesToolIdentity) []byte {
	if len(identities) == 0 {
		return event
	}
	restore := func(path string) {
		item := gjson.GetBytes(event, path)
		if itemType := item.Get("type").String(); itemType != "function_call" && itemType != "custom_tool_call" {
			return
		}
		identity, ok := identities[item.Get("name").String()]
		if !ok || identity.Name == "" {
			return
		}
		event = cliproxycommon.SetResponsesToolCallIdentity(event, identity.Name, identity.Namespace, path)
	}
	restore("item")
	for index := range gjson.GetBytes(event, "response.output").Array() {
		restore(fmt.Sprintf("response.output.%d", index))
	}
	return event
}

func writeZedProviderEvent(
	ctx context.Context,
	output io.Writer,
	event []byte,
	plan *zedWirePlan,
	registry *protocol.Registry,
	state *zedRelayState,
) error {
	if plan.providerProtocol == protocol.Anthropic {
		var metadata struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(event, &metadata); err != nil {
			return fmt.Errorf("decode Zed Anthropic event: %w", err)
		}
		if metadata.Type == "error" {
			state.failed = true
			_, err := fmt.Fprintf(output, "event: error\ndata: %s\n\n", bytes.TrimSpace(event))
			return err
		}
		if !state.anthropicStarted && metadata.Type != "message_start" && metadata.Type != "ping" {
			start := []byte(fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_zed","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"usage":{"input_tokens":0,"output_tokens":0}}}`, plan.model))
			if err := translateZedProviderEvent(ctx, output, start, plan, registry, state); err != nil {
				return err
			}
			state.anthropicStarted = true
		}
		switch metadata.Type {
		case "message_start":
			state.anthropicStarted = true
		case "content_block_start":
			index := metadata.Index
			state.anthropicOpenIndex = &index
			state.anthropicToolUse = state.anthropicToolUse || metadata.ContentBlock.Type == "tool_use"
		case "content_block_stop":
			state.anthropicOpenIndex = nil
		case "message_stop":
			state.anthropicStopped = true
			state.anthropicOpenIndex = nil
		}
	}
	return translateZedProviderEvent(ctx, output, event, plan, registry, state)
}

func finishZedAnthropicStream(
	ctx context.Context,
	output io.Writer,
	plan *zedWirePlan,
	registry *protocol.Registry,
	state *zedRelayState,
) error {
	if state.failed || !state.anthropicStarted || state.anthropicStopped {
		return nil
	}
	if state.anthropicOpenIndex != nil {
		stop := []byte(fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, *state.anthropicOpenIndex))
		if err := translateZedProviderEvent(ctx, output, stop, plan, registry, state); err != nil {
			return err
		}
		state.anthropicOpenIndex = nil
	}
	stopReason := "end_turn"
	if state.anthropicToolUse {
		stopReason = "tool_use"
	}
	delta := []byte(fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":0}}`, stopReason))
	if err := translateZedProviderEvent(ctx, output, delta, plan, registry, state); err != nil {
		return err
	}
	if err := translateZedProviderEvent(ctx, output, []byte(`{"type":"message_stop"}`), plan, registry, state); err != nil {
		return err
	}
	state.anthropicStopped = true
	return nil
}

func translateZedProviderEvent(
	ctx context.Context,
	output io.Writer,
	event []byte,
	plan *zedWirePlan,
	registry *protocol.Registry,
	state *zedRelayState,
) error {
	framed, err := frameZedProviderEvent(plan.providerProtocol, event)
	if err != nil {
		return err
	}
	chunks, err := registry.TranslateResponseStream(
		ctx, plan.providerProtocol, protocol.Codex, plan.model,
		plan.originalRequest, plan.translatedRequest, framed, &state.transform,
	)
	if err != nil {
		return fmt.Errorf("translate Zed %s response: %w", plan.provider, err)
	}
	for _, chunk := range chunks {
		if _, err := output.Write(chunk); err != nil {
			return err
		}
	}
	return nil
}

func frameZedProviderEvent(providerProtocol protocol.Protocol, event []byte) ([]byte, error) {
	event = bytes.TrimSpace(event)
	switch providerProtocol {
	case protocol.Anthropic:
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(event, &payload); err != nil || strings.TrimSpace(payload.Type) == "" {
			return nil, errors.New("zed Anthropic event is missing type")
		}
		return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", payload.Type, event)), nil
	case protocol.Gemini:
		// Gemini 分支只做字节拼接，没有后续解码兜底，所以这里是唯一一道语法防线：
		// 不校验就会把上游的残帧原样封进 data: 发给下游。Anthropic 分支的 Unmarshal
		// 已经等价地拒绝非法 JSON，不必再扫一遍。
		if !json.Valid(event) {
			return nil, errors.New("zed provider event is invalid JSON")
		}
		return append(append([]byte("data: "), event...), '\n', '\n'), nil
	default:
		return nil, fmt.Errorf("unsupported Zed response provider protocol %q", providerProtocol)
	}
}

func zedCredentialRejected(status int, body []byte) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	return status == http.StatusForbidden && !zedModelPlanRejected(status, body)
}

func zedModelPlanRejected(status int, body []byte) bool {
	return status == http.StatusForbidden && bytes.Contains(bytes.ToLower(body), []byte("plan"))
}
