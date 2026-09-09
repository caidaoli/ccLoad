package responses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Chat Completions has no tool-search item type. A client-executed Responses
// tool_search is represented as one ordinary function so that the model can
// still ask the host to perform the search and the original call_id can be
// paired with the following tool output.
const responsesChatToolSearchFunctionName = "tool_search"

type responsesToolSearchDeclaration struct {
	tool gjson.Result
}

func walkResponsesToolSearchDeclarations(root gjson.Result, visit func(responsesToolSearchDeclaration) bool) {
	proceed := true
	scan := func(tools gjson.Result) {
		if !proceed || !tools.Exists() || !tools.IsArray() {
			return
		}
		tools.ForEach(func(_, tool gjson.Result) bool {
			if strings.TrimSpace(tool.Get("type").String()) == "tool_search" {
				proceed = visit(responsesToolSearchDeclaration{tool: tool})
			}
			return proceed
		})
	}

	scan(root.Get("tools"))
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			switch strings.TrimSpace(item.Get("type").String()) {
			case "additional_tools":
				scan(item.Get("tools"))
			case "tool_search_output":
				// The output carries the tools loaded by the client. Those
				// declarations are regular function/custom/namespace tools;
				// a nested tool_search is not meaningful and is rejected by
				// validation rather than being treated as a new search tool.
			}
			return proceed
		})
	}
}

func responsesToolSearchNames(requestRawJSON []byte) map[string]struct{} {
	names := make(map[string]struct{})
	root := gjson.ParseBytes(requestRawJSON)
	walkResponsesToolSearchDeclarations(root, func(responsesToolSearchDeclaration) bool {
		names[responsesChatToolSearchFunctionName] = struct{}{}
		return true
	})
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			switch strings.TrimSpace(item.Get("type").String()) {
			case "tool_search_call", "tool_search_output":
				names[responsesChatToolSearchFunctionName] = struct{}{}
			}
			return true
		})
	}
	return names
}

// normalizeResponsesToolSearchArguments returns the object JSON required by
// Responses tool_search_call. The Responses API normally sends an object, but
// accepting a JSON object string here keeps replayed history compatible with
// clients that serialize all tool arguments as strings.
func normalizeResponsesToolSearchArguments(value gjson.Result) (string, error) {
	if !value.Exists() {
		return "", fmt.Errorf("tool_search_call.arguments is required")
	}
	candidate := value
	if value.Type == gjson.String {
		if !gjson.Valid(value.String()) {
			return "", fmt.Errorf("tool_search_call.arguments must be an object")
		}
		candidate = gjson.Parse(value.String())
	}
	if !candidate.IsObject() {
		return "", fmt.Errorf("tool_search_call.arguments must be an object")
	}
	if !json.Valid([]byte(candidate.Raw)) {
		return "", fmt.Errorf("tool_search_call.arguments must be valid JSON")
	}
	return candidate.Raw, nil
}

func responsesToolSearchArgumentsObject(arguments string) []byte {
	if raw, err := normalizeResponsesToolSearchArguments(gjson.Parse(arguments)); err == nil {
		return []byte(raw)
	}
	// An invalid search call must never be represented as an executable
	// Responses item. Error-aware response conversion reports the validation
	// error to its caller; this helper returns nil for the legacy best-effort
	// converter so it cannot manufacture a successful call payload.
	return nil
}

func setToolSearchOutputContent(toolMessage []byte, tools gjson.Result) []byte {
	if tools.Exists() && tools.IsArray() {
		// Chat tool messages have string content. Keeping the loaded tool
		// declarations as JSON preserves namespace/function metadata for the
		// next Responses request and is also readable by ordinary providers.
		toolMessage, _ = sjson.SetBytes(toolMessage, "content", tools.Raw)
		return toolMessage
	}
	toolMessage, _ = sjson.SetBytes(toolMessage, "content", "[]")
	return toolMessage
}

func responsesRequestHasToolSearch(root gjson.Result) bool {
	found := false
	scan := func(tools gjson.Result) {
		if found || !tools.Exists() || !tools.IsArray() {
			return
		}
		tools.ForEach(func(_, tool gjson.Result) bool {
			if strings.TrimSpace(tool.Get("type").String()) == "tool_search" {
				found = true
				return false
			}
			return true
		})
	}
	scan(root.Get("tools"))
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			typeName := strings.TrimSpace(item.Get("type").String())
			if typeName == "tool_search_call" || typeName == "tool_search_output" {
				found = true
				return false
			}
			if typeName == "additional_tools" {
				scan(item.Get("tools"))
			}
			return !found
		})
	}
	return found
}

func validateOpenAIResponsesRequestForChat(raw []byte) error {
	if !gjson.ValidBytes(raw) {
		return fmt.Errorf("invalid OpenAI Responses request JSON")
	}
	root := gjson.ParseBytes(raw)
	if !root.IsObject() {
		return fmt.Errorf("OpenAI Responses request must be a JSON object")
	}
	// Keep the historical best-effort behavior for ordinary Responses tools.
	// This validator is only strict when a request actually uses client tool
	// search or replays one of its input/output items.
	if !responsesRequestHasToolSearch(root) {
		return nil
	}

	searchDeclarationCount := 0
	searchCallIDs := make(map[string]struct{})
	searchOutputIDs := make(map[string]struct{})

	var validateToolArray func(gjson.Result, string, bool, bool) error
	validateToolArray = func(tools gjson.Result, path string, allowSearch, strict bool) error {
		if !tools.Exists() {
			return nil
		}
		if !tools.IsArray() {
			return fmt.Errorf("%s must be an array", path)
		}
		for index, tool := range tools.Array() {
			if !tool.IsObject() {
				return fmt.Errorf("%s[%d] must be an object", path, index)
			}
			typeName := strings.TrimSpace(tool.Get("type").String())
			switch typeName {
			case "", "function", "custom":
				if strict && responsesToolName(tool) == "" {
					return fmt.Errorf("%s[%d] %s tool name is required", path, index, typeName)
				}
			case "namespace":
				if !strict {
					continue
				}
				if strings.TrimSpace(tool.Get("name").String()) == "" {
					return fmt.Errorf("%s[%d] namespace name is required", path, index)
				}
				children := tool.Get("tools")
				if !children.Exists() || !children.IsArray() {
					return fmt.Errorf("%s[%d].tools must be an array", path, index)
				}
				for childIndex, child := range children.Array() {
					childType := strings.TrimSpace(child.Get("type").String())
					if childType == "namespace" {
						return fmt.Errorf("%s[%d].tools[%d] nested namespace tools are not convertible to Chat Completions", path, index, childIndex)
					}
				}
				if err := validateToolArray(children, fmt.Sprintf("%s[%d].tools", path, index), false, true); err != nil {
					return err
				}
			case "tool_search":
				if !allowSearch {
					return fmt.Errorf("%s[%d] tool_search declarations are only valid at request level", path, index)
				}
				if strings.TrimSpace(tool.Get("execution").String()) != "client" {
					return fmt.Errorf("%s[%d] tool_search execution must be \"client\" for Chat Completions conversion", path, index)
				}
				searchDeclarationCount++
				if parameters := tool.Get("parameters"); parameters.Exists() && !parameters.IsObject() {
					return fmt.Errorf("%s[%d].parameters must be an object", path, index)
				}
			case "web_search", "web_search_preview", "web_search_preview_2025_03_11":
				// The existing adapter maps Responses web_search tools to
				// Chat's web_search_options rather than a function tool.
				if strict {
					return fmt.Errorf("%s[%d] Responses tool type %q is not convertible to a loaded Chat Completions function", path, index, typeName)
				}
			default:
				if strings.HasPrefix(typeName, "web_search") {
					continue
				}
				if strict {
					return fmt.Errorf("%s[%d] Responses tool type %q is not convertible to Chat Completions", path, index, typeName)
				}
			}
		}
		return nil
	}

	if err := validateToolArray(root.Get("tools"), "tools", true, false); err != nil {
		return err
	}
	checkReservedToolCollision := func() error {
		if !responsesRequestHasToolSearch(root) {
			return nil
		}
		collision := false
		walkResponsesToolDeclarations(root, func(declaration responsesToolDeclaration) bool {
			if declaration.chatName == responsesChatToolSearchFunctionName {
				collision = true
				return false
			}
			return true
		})
		if collision {
			return fmt.Errorf("a regular Responses tool already uses reserved Chat function name %q", responsesChatToolSearchFunctionName)
		}
		return nil
	}
	input := root.Get("input")
	if input.Exists() && !input.IsArray() && input.Type != gjson.String {
		return fmt.Errorf("input must be a string or array")
	}

	// Validate additional tool declarations and collect search calls before
	// checking outputs, so pairing remains correct even for replay histories
	// whose items were reordered by a client.
	if input.IsArray() {
		for index, item := range input.Array() {
			if strings.TrimSpace(item.Get("type").String()) == "additional_tools" {
				if err := validateToolArray(item.Get("tools"), fmt.Sprintf("input[%d].tools", index), true, false); err != nil {
					return err
				}
			}
		}
	}
	if searchDeclarationCount > 1 {
		return fmt.Errorf("multiple client tool_search declarations cannot be mapped to one Chat Completions function")
	}
	if err := checkReservedToolCollision(); err != nil {
		return err
	}
	if !input.IsArray() {
		return nil
	}
	for index, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "tool_search_call" {
			continue
		}
		if strings.TrimSpace(item.Get("execution").String()) != "client" {
			return fmt.Errorf("input[%d] tool_search_call execution must be \"client\" for Chat Completions conversion", index)
		}
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID == "" {
			return fmt.Errorf("input[%d] tool_search_call.call_id is required", index)
		}
		if _, duplicate := searchCallIDs[callID]; duplicate {
			return fmt.Errorf("input[%d] duplicate tool_search_call.call_id %q", index, callID)
		}
		if _, err := normalizeResponsesToolSearchArguments(item.Get("arguments")); err != nil {
			return fmt.Errorf("input[%d]: %w", index, err)
		}
		searchCallIDs[callID] = struct{}{}
	}
	for index, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "tool_search_output" {
			continue
		}
		if strings.TrimSpace(item.Get("execution").String()) != "client" {
			return fmt.Errorf("input[%d] tool_search_output execution must be \"client\" for Chat Completions conversion", index)
		}
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID == "" {
			return fmt.Errorf("input[%d] tool_search_output.call_id is required", index)
		}
		if _, duplicate := searchOutputIDs[callID]; duplicate {
			return fmt.Errorf("input[%d] duplicate tool_search_output.call_id %q", index, callID)
		}
		tools := item.Get("tools")
		if !tools.Exists() || !tools.IsArray() {
			return fmt.Errorf("input[%d] tool_search_output.tools must be an array", index)
		}
		if err := validateToolArray(tools, fmt.Sprintf("input[%d].tools", index), false, true); err != nil {
			return err
		}
		if _, paired := searchCallIDs[callID]; !paired {
			return fmt.Errorf("input[%d] tool_search_output.call_id %q has no matching tool_search_call", index, callID)
		}
		searchOutputIDs[callID] = struct{}{}
	}
	for callID := range searchCallIDs {
		if _, paired := searchOutputIDs[callID]; !paired {
			return fmt.Errorf("tool_search_call.call_id %q has no matching tool_search_output; replay the complete search history", callID)
		}
	}
	return nil
}

// validateOpenAIChatCompletionsToolSearchArguments checks only calls that are
// known to be the reserved client tool-search function. Ordinary function
// calls continue through the historical best-effort response converter,
// including when their argument text is malformed.
func validateOpenAIChatCompletionsToolSearchArguments(requestForNamespace, responseRawJSON []byte) error {
	if len(requestForNamespace) == 0 || !gjson.ValidBytes(requestForNamespace) {
		return nil
	}
	searchNames := responsesToolSearchNames(requestForNamespace)
	if len(searchNames) == 0 {
		return nil
	}

	root := gjson.ParseBytes(responseRawJSON)
	choices := root.Get("choices")
	if !choices.Exists() || !choices.IsArray() {
		return nil
	}
	var validationErr error
	choices.ForEach(func(_, choice gjson.Result) bool {
		if validationErr != nil {
			return false
		}
		toolCalls := choice.Get("message.tool_calls")
		if !toolCalls.Exists() || !toolCalls.IsArray() {
			return true
		}
		toolCalls.ForEach(func(_, toolCall gjson.Result) bool {
			name := strings.TrimSpace(toolCall.Get("function.name").String())
			if _, isSearchTool := searchNames[name]; !isSearchTool {
				return true
			}
			arguments := toolCall.Get("function.arguments")
			if _, err := normalizeResponsesToolSearchArguments(gjson.Parse(arguments.String())); err != nil {
				callID := strings.TrimSpace(toolCall.Get("id").String())
				if callID == "" {
					returnErr := fmt.Errorf("tool_search_call.arguments: %w", err)
					validationErr = returnErr
				} else {
					validationErr = fmt.Errorf("tool_search_call %q.arguments: %w", callID, err)
				}
				return false
			}
			return true
		})
		return validationErr == nil
	})
	return validationErr
}

// validateOpenAIChatCompletionsToolSearchState checks an aggregated streaming
// call after the provider has declared a terminal finish reason or sent
// [DONE]. A partial argument buffer is therefore treated as invalid rather
// than being exposed as a completed executable search call.
func validateOpenAIChatCompletionsToolSearchState(st *oaiToResponsesState) error {
	if st == nil {
		return nil
	}
	for key, name := range st.FuncNames {
		if _, isSearchTool := st.ToolSearchNames[name]; !isSearchTool {
			continue
		}
		arguments := ""
		if buffer := st.FuncArgsBuf[key]; buffer != nil {
			arguments = buffer.String()
		}
		if _, err := normalizeResponsesToolSearchArguments(gjson.Parse(arguments)); err != nil {
			callID := strings.TrimSpace(st.FuncCallIDs[key])
			if callID == "" {
				return fmt.Errorf("tool_search_call.arguments: %w", err)
			}
			return fmt.Errorf("tool_search_call %q.arguments: %w", callID, err)
		}
	}
	return nil
}

func openAIChatCompletionChunkIsTerminal(rawJSON []byte) bool {
	rawJSON = bytes.TrimSpace(rawJSON)
	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
	}
	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		return true
	}
	root := gjson.ParseBytes(rawJSON)
	choices := root.Get("choices")
	if !choices.Exists() || !choices.IsArray() {
		return false
	}
	terminal := false
	choices.ForEach(func(_, choice gjson.Result) bool {
		if finishReason := choice.Get("finish_reason"); finishReason.Exists() && strings.TrimSpace(finishReason.String()) != "" {
			terminal = true
			return false
		}
		return true
	})
	return terminal
}
