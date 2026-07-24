package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type responsesWebsocketSession struct {
	lastRequest        []byte
	lastResponseOutput []byte
	lastResponseID     string
	pendingToolCallIDs []string
}

func newResponsesWebsocketSession() *responsesWebsocketSession {
	return &responsesWebsocketSession{lastResponseOutput: []byte("[]")}
}

func (s *responsesWebsocketSession) normalizeRequest(payload []byte) ([]byte, error) {
	if !gjson.ValidBytes(payload) {
		return nil, errors.New("invalid websocket request JSON")
	}
	requestType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if requestType != responsesWebsocketRequestCreate && requestType != responsesWebsocketRequestAppend {
		return nil, fmt.Errorf("unsupported websocket request type %q", requestType)
	}
	if len(s.lastRequest) == 0 {
		if requestType != responsesWebsocketRequestCreate {
			return nil, errors.New("response.append received before response.create")
		}
		return normalizeInitialResponsesWebsocketRequest(payload)
	}

	nextInput := gjson.GetBytes(payload, "input")
	if !nextInput.Exists() || !nextInput.IsArray() {
		return nil, errors.New("websocket request requires array field: input")
	}
	previousID := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String())
	if previousID != "" && s.lastResponseID != "" && previousID != s.lastResponseID {
		return nil, fmt.Errorf("previous response %q is not available on this websocket", previousID)
	}
	if len(s.pendingToolCallIDs) > 0 && !inputSatisfiesResponsesWebsocketToolCalls(nextInput, s.pendingToolCallIDs) {
		if previousID != "" || requestType == responsesWebsocketRequestAppend {
			return nil, errors.New("incremental websocket request is missing output for a pending tool call")
		}
		normalized, err := normalizeReplacementResponsesWebsocketRequest(payload, s.lastRequest)
		if err != nil {
			return nil, err
		}
		return enforceResponsesWebsocketTranscriptLimit(normalized)
	}

	if previousID == "" && inputContainsCompletedTranscript(nextInput) {
		normalized, err := normalizeReplacementResponsesWebsocketRequest(payload, s.lastRequest)
		if err != nil {
			return nil, err
		}
		return enforceResponsesWebsocketTranscriptLimit(normalized)
	}

	merged, err := mergeResponsesWebsocketInput(
		gjson.GetBytes(s.lastRequest, "input"),
		gjson.ParseBytes(s.lastResponseOutput),
		nextInput,
	)
	if err != nil {
		return nil, err
	}

	normalized, err := normalizeReplacementResponsesWebsocketRequest(payload, s.lastRequest)
	if err != nil {
		return nil, err
	}
	normalized, err = sjson.SetRawBytes(normalized, "input", merged)
	if err != nil {
		return nil, fmt.Errorf("set merged websocket input: %w", err)
	}
	return enforceResponsesWebsocketTranscriptLimit(normalized)
}

func (s *responsesWebsocketSession) commit(request []byte, result responsesWebsocketTurnResult) {
	if s == nil {
		return
	}
	s.lastRequest = bytes.Clone(request)
	if len(result.completedOutput) == 0 {
		s.lastResponseOutput = []byte("[]")
	} else {
		s.lastResponseOutput = bytes.Clone(result.completedOutput)
	}
	s.lastResponseID = strings.TrimSpace(result.completedResponseID)
	s.pendingToolCallIDs = append([]string(nil), result.pendingToolCallIDs...)
}

func normalizeInitialResponsesWebsocketRequest(payload []byte) ([]byte, error) {
	modelName := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	if modelName == "" {
		return nil, errors.New("missing model in response.create request")
	}
	if !gjson.GetBytes(payload, "input").Exists() {
		return nil, errors.New("missing input in response.create request")
	}
	normalized, err := sjson.DeleteBytes(payload, "type")
	if err != nil {
		return nil, fmt.Errorf("remove websocket event type: %w", err)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	normalized, err = sjson.SetBytes(normalized, "stream", true)
	if err != nil {
		return nil, fmt.Errorf("force streaming request: %w", err)
	}
	return enforceResponsesWebsocketTranscriptLimit(normalized)
}

func normalizeReplacementResponsesWebsocketRequest(payload []byte, lastRequest []byte) ([]byte, error) {
	normalized, err := sjson.DeleteBytes(payload, "type")
	if err != nil {
		return nil, fmt.Errorf("remove websocket event type: %w", err)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	if strings.TrimSpace(gjson.GetBytes(normalized, "model").String()) == "" {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, err = sjson.SetBytes(normalized, "stream", true)
	if err != nil {
		return nil, fmt.Errorf("force streaming request: %w", err)
	}
	return normalized, nil
}

func mergeResponsesWebsocketInput(parts ...gjson.Result) ([]byte, error) {
	items := make([]json.RawMessage, 0)
	seenItemIDs := make(map[string]struct{})
	seenCallIDs := make(map[string]struct{})
	for _, part := range parts {
		if !part.Exists() {
			continue
		}
		if !part.IsArray() {
			return nil, errors.New("websocket transcript input must be an array")
		}
		for _, item := range part.Array() {
			raw := bytes.TrimSpace([]byte(item.Raw))
			if len(raw) == 0 || !json.Valid(raw) {
				return nil, errors.New("websocket transcript contains invalid item JSON")
			}
			itemID := strings.TrimSpace(item.Get("id").String())
			if itemID != "" {
				if _, exists := seenItemIDs[itemID]; exists {
					continue
				}
				seenItemIDs[itemID] = struct{}{}
			}
			itemType := strings.TrimSpace(item.Get("type").String())
			if itemType == "function_call" || itemType == "custom_tool_call" {
				callID := strings.TrimSpace(item.Get("call_id").String())
				if callID != "" {
					if _, exists := seenCallIDs[callID]; exists {
						continue
					}
					seenCallIDs[callID] = struct{}{}
				}
			}
			items = append(items, bytes.Clone(raw))
		}
	}
	merged, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("marshal websocket transcript: %w", err)
	}
	return merged, nil
}

func inputContainsCompletedTranscript(input gjson.Result) bool {
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "function_call", "custom_tool_call":
			return true
		case "message":
			if strings.TrimSpace(item.Get("role").String()) == "assistant" {
				return true
			}
		}
		if strings.TrimSpace(item.Get("role").String()) == "assistant" {
			return true
		}
		if strings.TrimSpace(item.Get("role").String()) == "user" && strings.HasPrefix(
			responsesWebsocketMessageText(item),
			codexLocalCompactionSummaryPrefix+"\n",
		) {
			return true
		}
	}
	return false
}

const codexLocalCompactionSummaryPrefix = "Another language model started to solve this problem and produced a summary of its thinking process. You also have access to the state of the tools that were used by that language model. Use this to build on the work that has already been done and avoid duplicating work. Here is the summary produced by the other language model, use the information in this summary to assist with your own analysis:"

func responsesWebsocketMessageText(message gjson.Result) string {
	content := message.Get("content")
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	var text strings.Builder
	for _, part := range content.Array() {
		if part.Get("type").String() == "input_text" || part.Get("type").String() == "text" {
			text.WriteString(part.Get("text").String())
		}
	}
	return text.String()
}

func inputSatisfiesResponsesWebsocketToolCalls(input gjson.Result, pending []string) bool {
	outputs := make(map[string]struct{}, len(pending))
	for _, item := range input.Array() {
		itemType := strings.TrimSpace(item.Get("type").String())
		if itemType != "function_call_output" && itemType != "custom_tool_call_output" {
			continue
		}
		if callID := strings.TrimSpace(item.Get("call_id").String()); callID != "" {
			outputs[callID] = struct{}{}
		}
	}
	for _, callID := range pending {
		if _, ok := outputs[callID]; !ok {
			return false
		}
	}
	return true
}

func enforceResponsesWebsocketTranscriptLimit(payload []byte) ([]byte, error) {
	maxBytes := maxProxyBodyBytes("/v1/responses")
	if int64(len(payload)) > maxBytes {
		return nil, fmt.Errorf("websocket transcript exceeds %d byte limit; compact and replay the conversation", maxBytes)
	}
	return payload, nil
}
