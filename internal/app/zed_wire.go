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
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"
	"ccLoad/internal/zedauth"
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
}

func finalizeZedResponsesBody(registry *protocol.Registry, body, originalAnthropicRequest []byte) ([]byte, *zedWirePlan, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var providerRequest map[string]any
	if err := decoder.Decode(&providerRequest); err != nil || providerRequest == nil {
		return nil, nil, errors.New("finalize Zed Responses request: invalid JSON object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("finalize Zed Responses request: trailing JSON")
	}
	modelName, _ := providerRequest["model"].(string)
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return nil, nil, errors.New("finalize Zed Responses request: model is required")
	}
	if modelName == "gpt-5.6" {
		modelName = "gpt-5.6-sol"
		providerRequest["model"] = modelName
	}
	provider, providerProtocol, err := zedProviderProtocol(modelName)
	if err != nil {
		return nil, nil, err
	}
	providerRequest["stream"] = true
	normalizeZedCodexInput(providerRequest)
	originalRequest, err := json.Marshal(providerRequest)
	if err != nil {
		return nil, nil, errors.New("finalize Zed Responses request: encode normalized request failed")
	}

	translatedRequest := originalRequest
	if provider == zedauth.ProviderOpenAI {
		if err := normalizeZedOpenAIProviderRequest(providerRequest, modelName); err != nil {
			return nil, nil, err
		}
		translatedRequest, err = json.Marshal(providerRequest)
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
	return encoded, &zedWirePlan{
		model: modelName, provider: provider, providerProtocol: providerProtocol,
		originalRequest: originalRequest, translatedRequest: translatedRequest,
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

func normalizeZedCodexInput(request map[string]any) {
	switch input := request["input"].(type) {
	case string:
		request["input"] = []any{zedMessage("user", input)}
	case []any:
		normalized := make([]any, 0, len(input))
		for _, rawItem := range input {
			item, ok := rawItem.(map[string]any)
			if !ok {
				normalized = append(normalized, rawItem)
				continue
			}
			for key, value := range item {
				if value == nil {
					delete(item, key)
				}
			}
			role, _ := item["role"].(string)
			if role == "developer" {
				role = "system"
				item["role"] = role
			}
			if role != "" && item["type"] == nil {
				item["type"] = "message"
			}
			if content, ok := item["content"].(string); ok {
				contentType := "input_text"
				if role == "assistant" {
					contentType = "output_text"
				}
				item["content"] = []any{map[string]any{"type": contentType, "text": content}}
			}
			normalized = append(normalized, item)
		}
		request["input"] = normalized
	}
}

func normalizeZedOpenAIProviderRequest(request map[string]any, modelName string) error {
	additionalTools := make([]any, 0)
	if input, ok := request["input"].([]any); ok {
		normalized := make([]any, 0, len(input))
		for _, rawItem := range input {
			item, ok := rawItem.(map[string]any)
			if ok && item["type"] == "additional_tools" {
				if tools, toolsOK := item["tools"].([]any); toolsOK {
					additionalTools = append(additionalTools, tools...)
				}
				continue
			}
			normalized = append(normalized, rawItem)
		}
		request["input"] = normalized
	}

	selectedTool := zedSelectedToolName(request["tool_choice"])
	if selectedTool != "" {
		request["tool_choice"] = "required"
	}
	tools := make([]any, 0)
	seen := make(map[string]struct{})
	appendTools := func(raw any) {
		list, _ := raw.([]any)
		for _, candidate := range list {
			tool, ok := candidate.(map[string]any)
			if !ok {
				continue
			}
			toolType, _ := tool["type"].(string)
			name, _ := tool["name"].(string)
			if (toolType != "function" && toolType != "custom") || name == "" || (selectedTool != "" && name != selectedTool) {
				continue
			}
			if _, duplicate := seen[name]; duplicate {
				continue
			}
			seen[name] = struct{}{}
			tools = append(tools, tool)
		}
	}
	appendTools(request["tools"])
	appendTools(additionalTools)
	if request["tools"] != nil || len(additionalTools) > 0 {
		request["tools"] = tools
	}

	effort := ""
	reasoning, _ := request["reasoning"].(map[string]any)
	if reasoning != nil {
		effort, _ = reasoning["effort"].(string)
	}
	if strings.HasPrefix(modelName, "gpt-5.6") {
		if effort == "" {
			effort = "xhigh"
		}
		switch effort {
		case "none", "low", "medium", "high", "xhigh":
		default:
			return fmt.Errorf("finalize Zed Responses request: unsupported GPT-5.6 reasoning effort %q", effort)
		}
	} else if strings.HasPrefix(modelName, "gpt-5.5") {
		effort = "xhigh"
	}
	if effort != "" {
		if reasoning == nil {
			reasoning = make(map[string]any)
		}
		reasoning["effort"] = effort
		if effort != "none" && reasoning["summary"] == nil {
			reasoning["summary"] = "detailed"
		}
		request["reasoning"] = reasoning
	}
	requestedBudget := zedOutputBudget(request)
	delete(request, "max_completion_tokens")
	delete(request, "max_tokens")
	if effort == "xhigh" && requestedBudget < 32768 {
		requestedBudget = 32768
	}
	if requestedBudget > 0 {
		request["max_output_tokens"] = requestedBudget
	}
	return nil
}

func finalizeZedGoogleProviderRequest(body []byte, modelName string) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil || request == nil {
		return nil, errors.New("decode translated Google request")
	}
	request["model"] = "models/" + modelName
	config, _ := request["generationConfig"].(map[string]any)
	if config == nil {
		config = make(map[string]any)
		request["generationConfig"] = config
	}
	if config["candidateCount"] == nil {
		config["candidateCount"] = 1
	}
	if config["stopSequences"] == nil {
		config["stopSequences"] = []any{}
	}
	if config["temperature"] == nil {
		config["temperature"] = 1.0
	}
	if tools, ok := request["tools"].([]any); ok {
		for _, rawTool := range tools {
			tool, _ := rawTool.(map[string]any)
			declarations, _ := tool["functionDeclarations"].([]any)
			for _, rawDeclaration := range declarations {
				declaration, _ := rawDeclaration.(map[string]any)
				if schema, exists := declaration["parametersJsonSchema"]; exists && declaration["parameters"] == nil {
					declaration["parameters"] = schema
					delete(declaration, "parametersJsonSchema")
				}
			}
		}
	}
	return json.Marshal(request)
}

func finalizeZedAnthropicProviderRequest(body, originalRequest, originalAnthropicRequest []byte) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil || request == nil {
		return nil, errors.New("decode translated Anthropic request")
	}
	// Zed owns the /completions stream. Its native Anthropic provider_request
	// does not carry the Anthropic HTTP transport's stream flag.
	delete(request, "stream")
	normalizeZedAnthropicContentBlocks(request)
	restoreZedAnthropicCacheControls(request, originalAnthropicRequest)
	var original map[string]any
	if json.Unmarshal(originalRequest, &original) == nil && zedOutputBudget(original) == 0 {
		// The shared converter defaults to 32000, but Zed's native request and
		// Claude Sonnet 4.5 both require the conservative 8192 default.
		request["max_tokens"] = 8192
	}
	return json.Marshal(request)
}

func restoreZedAnthropicCacheControls(request map[string]any, originalRequest []byte) {
	var original map[string]any
	if len(originalRequest) == 0 || json.Unmarshal(originalRequest, &original) != nil {
		return
	}
	copyCacheControl := func(target, source map[string]any) {
		if cacheControl, exists := source["cache_control"]; exists {
			target["cache_control"] = cacheControl
		}
	}
	copyCacheControl(request, original)

	originalSystem, _ := original["system"].([]any)
	system, _ := request["system"].([]any)
	for i := 0; i < len(originalSystem) && i < len(system); i++ {
		source, sourceOK := originalSystem[i].(map[string]any)
		target, targetOK := system[i].(map[string]any)
		if sourceOK && targetOK {
			copyCacheControl(target, source)
		}
	}

	originalTools, _ := original["tools"].([]any)
	toolCacheControls := make(map[string]any, len(originalTools))
	for _, rawTool := range originalTools {
		tool, ok := rawTool.(map[string]any)
		name, _ := tool["name"].(string)
		if !ok || name == "" {
			continue
		}
		if cacheControl, exists := tool["cache_control"]; exists {
			toolCacheControls[name] = cacheControl
		}
	}
	tools, _ := request["tools"].([]any)
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		name, _ := tool["name"].(string)
		if !ok || name == "" {
			continue
		}
		if cacheControl, exists := toolCacheControls[name]; exists {
			tool["cache_control"] = cacheControl
		}
	}
}

func normalizeZedAnthropicContentBlocks(request map[string]any) {
	textBlocks := func(text string) []any {
		return []any{map[string]any{"type": "text", "text": text}}
	}
	if system, ok := request["system"].(string); ok {
		request["system"] = textBlocks(system)
	}
	messages, _ := request["messages"].([]any)
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		if content, ok := message["content"].(string); ok {
			message["content"] = textBlocks(content)
		}
		content, _ := message["content"].([]any)
		for _, rawBlock := range content {
			block, ok := rawBlock.(map[string]any)
			if !ok || block["type"] != "tool_result" {
				continue
			}
			if _, exists := block["is_error"]; !exists {
				block["is_error"] = false
			}
		}
	}
}

func zedMessage(role, text string) map[string]any {
	return map[string]any{
		"type": "message", "role": role,
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}
}

func zedSelectedToolName(raw any) string {
	choice, _ := raw.(map[string]any)
	if choice == nil {
		return ""
	}
	if name, _ := choice["name"].(string); name != "" {
		return name
	}
	function, _ := choice["function"].(map[string]any)
	name, _ := function["name"].(string)
	return name
}

func zedOutputBudget(request map[string]any) int64 {
	for _, key := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		switch value := request[key].(type) {
		case json.Number:
			if number, err := value.Int64(); err == nil {
				return number
			}
		case float64:
			return int64(value)
		case int64:
			return value
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
	var eventType struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(event, &eventType); err != nil || strings.TrimSpace(eventType.Type) == "" {
		return false, errors.New("zed Responses event is missing type")
	}
	_, err := fmt.Fprintf(output, "event: %s\ndata: %s\n\n", eventType.Type, bytes.TrimSpace(event))
	return false, err
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
	if !json.Valid(event) {
		return nil, errors.New("zed provider event is invalid JSON")
	}
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
