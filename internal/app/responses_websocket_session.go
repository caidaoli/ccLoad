package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var errResponsesWebsocketPreviousResponseNotFound = errors.New(
	"previous response is not available on this websocket; resend the full conversation input without previous_response_id",
)

type responsesWebsocketSession struct {
	lastRequest               []byte
	lastResponseOutput        []byte
	lastResponseID            string
	pendingToolCallIDs        []string
	replacementReplayRequired bool
	maxBodyBytes              int64
}

func newResponsesWebsocketSession(maxBodyBytes int64) *responsesWebsocketSession {
	if maxBodyBytes <= 0 {
		maxBodyBytes = normalizeMaxBodyBytes(maxBodyBytes)
	}
	return &responsesWebsocketSession{lastResponseOutput: []byte("[]"), maxBodyBytes: maxBodyBytes}
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
		if strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String()) != "" {
			return nil, errResponsesWebsocketPreviousResponseNotFound
		}
		return normalizeInitialResponsesWebsocketRequest(payload, s.maxBodyBytes)
	}

	nextInput := gjson.GetBytes(payload, "input")
	if !nextInput.Exists() || !nextInput.IsArray() {
		return nil, errors.New("websocket request requires array field: input")
	}
	previousID := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String())
	if previousID != "" && previousID != s.lastResponseID {
		return nil, fmt.Errorf("%w: %q", errResponsesWebsocketPreviousResponseNotFound, previousID)
	}
	if s.replacementReplayRequired && requestType == responsesWebsocketRequestCreate && previousID == "" {
		return s.normalizeFullReplacement(payload)
	}
	if len(s.pendingToolCallIDs) > 0 && !inputSatisfiesResponsesWebsocketToolCalls(nextInput, s.pendingToolCallIDs) {
		if previousID != "" || requestType == responsesWebsocketRequestAppend {
			return nil, errors.New("incremental websocket request is missing output for a pending tool call")
		}
		return s.normalizeFullReplacement(payload)
	}

	if previousID == "" && inputContainsCompletedTranscript(nextInput) {
		return s.normalizeFullReplacement(payload)
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
	return finalizeResponsesWebsocketRequest(normalized, s.maxBodyBytes)
}

func (s *responsesWebsocketSession) normalizeFullReplacement(payload []byte) ([]byte, error) {
	normalized, err := normalizeReplacementResponsesWebsocketRequest(payload, s.lastRequest)
	if err != nil {
		return nil, err
	}
	normalized, err = preserveGatewayOwnedResponsesInput(normalized, s.lastRequest)
	if err != nil {
		return nil, err
	}
	return finalizeResponsesWebsocketRequest(normalized, s.maxBodyBytes)
}

// normalizeRequests 同时生成两份请求：
//   - replayRequest 始终包含完整 transcript，可安全发送到任意新 transport；
//   - incrementalRequest 仅包含本轮 input，并用 previous_response_id 续接当前原生 WS。
//
// 连接是否仍可复用由上游 session 在选定渠道、Key 和 URL 后决定。这里不猜 transport 状态。
func (s *responsesWebsocketSession) normalizeRequests(payload []byte) (replayRequest, incrementalRequest []byte, err error) {
	requestType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	previousID := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String())
	replacementReplay := s.replacementReplayRequired &&
		requestType == responsesWebsocketRequestCreate && previousID == ""
	replayRequest, err = s.normalizeRequest(payload)
	if err != nil {
		return nil, nil, err
	}
	if len(s.lastRequest) > 0 {
		// replayRequest 会作为新上游 transport 的第一次请求发送。只清理
		// execution session 已积累的不可移植 reasoning；真正的客户端首请求
		// 必须原样保留，握手失败后的同渠道 HTTP fallback 也依赖这个契约。
		replayRequest = responsesReplayWithoutNonPortableReasoning(replayRequest)
	}
	if replacementReplay {
		s.replacementReplayRequired = false
		return replayRequest, bytes.Clone(replayRequest), nil
	}
	if len(s.lastRequest) == 0 {
		return replayRequest, bytes.Clone(replayRequest), nil
	}

	nextInput := gjson.GetBytes(payload, "input")
	if previousID == "" && inputContainsCompletedTranscript(nextInput) {
		return replayRequest, bytes.Clone(replayRequest), nil
	}
	if len(s.pendingToolCallIDs) > 0 && !inputSatisfiesResponsesWebsocketToolCalls(nextInput, s.pendingToolCallIDs) &&
		previousID == "" && requestType == responsesWebsocketRequestCreate {
		return replayRequest, bytes.Clone(replayRequest), nil
	}

	incrementalRequest, err = normalizeReplacementResponsesWebsocketRequest(payload, s.lastRequest)
	if err != nil {
		return nil, nil, err
	}
	if s.lastResponseID != "" {
		incrementalRequest, err = sjson.SetBytes(incrementalRequest, "previous_response_id", s.lastResponseID)
		if err != nil {
			return nil, nil, fmt.Errorf("set websocket previous response id: %w", err)
		}
	}
	return replayRequest, incrementalRequest, nil
}

func (s *responsesWebsocketSession) requireReplacementReplay() {
	if s != nil {
		s.replacementReplayRequired = true
	}
}

// normalizeHTTPRequests keeps ordinary Responses HTTP semantics intact:
// no previous_response_id starts a replacement response, while a continuation
// may use native WS only when this process owns the referenced transcript.
func (s *responsesWebsocketSession) normalizeHTTPRequests(payload []byte) (
	replayRequest, incrementalRequest []byte, localContinuation bool, err error,
) {
	websocketPayload, err := sjson.SetBytes(payload, "type", responsesWebsocketRequestCreate)
	if err != nil {
		return nil, nil, false, err
	}
	previousID := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String())
	if previousID != "" && len(s.lastRequest) == 0 {
		return nil, nil, false, nil
	}
	if previousID == "" {
		replayRequest, err = normalizeInitialResponsesWebsocketRequest(websocketPayload, s.maxBodyBytes)
		if err != nil {
			return nil, nil, false, err
		}
		return replayRequest, bytes.Clone(replayRequest), true, nil
	}
	replayRequest, incrementalRequest, err = s.normalizeRequests(websocketPayload)
	return replayRequest, incrementalRequest, err == nil, err
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

func normalizeInitialResponsesWebsocketRequest(payload []byte, maxBodyBytes int64) ([]byte, error) {
	modelName := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	if modelName == "" {
		return nil, errors.New("missing model in response.create request")
	}
	normalized, err := sjson.DeleteBytes(payload, "type")
	if err != nil {
		return nil, fmt.Errorf("remove websocket event type: %w", err)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	if !gjson.GetBytes(normalized, "input").Exists() {
		normalized, err = sjson.SetRawBytes(normalized, "input", []byte("[]"))
		if err != nil {
			return nil, fmt.Errorf("set empty websocket input: %w", err)
		}
	}
	normalized, err = sjson.SetBytes(normalized, "stream", true)
	if err != nil {
		return nil, fmt.Errorf("force streaming request: %w", err)
	}
	return finalizeResponsesWebsocketRequest(normalized, maxBodyBytes)
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

// preserveGatewayOwnedResponsesInput keeps internal completed tool pairs across
// a client-driven full replay. The client cannot echo items it never received.
// We only restore them when the client's visible input exactly extends the last
// committed visible prefix; otherwise replacement semantics win and we do not
// guess where hidden state belongs.
func preserveGatewayOwnedResponsesInput(payload, lastRequest []byte) ([]byte, error) {
	lastInput := gjson.GetBytes(lastRequest, "input")
	nextInput := gjson.GetBytes(payload, "input")
	if !lastInput.IsArray() || !nextInput.IsArray() {
		return payload, nil
	}

	lastItems := lastInput.Array()
	visibleItems := make([]gjson.Result, 0, len(lastItems))
	gatewayCallIDs := make(map[string]struct{})
	for index := 0; index < len(lastItems); index++ {
		if index+1 < len(lastItems) {
			if callID, ok := codexQuotaOverdraftGatewayPair(lastItems[index], lastItems[index+1]); ok {
				gatewayCallIDs[callID] = struct{}{}
				index++
				continue
			}
		}
		visibleItems = append(visibleItems, lastItems[index])
	}
	if len(gatewayCallIDs) == 0 {
		return payload, nil
	}

	nextItems := nextInput.Array()
	if len(nextItems) < len(visibleItems) {
		return payload, nil
	}
	for index := range visibleItems {
		if !responsesWebsocketJSONEqual(visibleItems[index], nextItems[index]) {
			return payload, nil
		}
	}
	for _, item := range nextItems {
		if _, exists := gatewayCallIDs[strings.TrimSpace(item.Get("call_id").String())]; exists {
			return payload, nil
		}
	}

	merged := make([]json.RawMessage, 0, len(lastItems)+len(nextItems)-len(visibleItems))
	for _, item := range lastItems {
		merged = append(merged, json.RawMessage(bytes.Clone([]byte(item.Raw))))
	}
	for _, item := range nextItems[len(visibleItems):] {
		merged = append(merged, json.RawMessage(bytes.Clone([]byte(item.Raw))))
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal websocket gateway-owned input: %w", err)
	}
	updated, err := sjson.SetRawBytes(payload, "input", encoded)
	if err != nil {
		return nil, fmt.Errorf("restore websocket gateway-owned input: %w", err)
	}
	return updated, nil
}

func codexQuotaOverdraftGatewayPair(call, output gjson.Result) (string, bool) {
	if strings.TrimSpace(call.Get("type").String()) != "custom_tool_call" ||
		strings.TrimSpace(call.Get("name").String()) != "exec" ||
		strings.TrimSpace(output.Get("type").String()) != "custom_tool_call_output" {
		return "", false
	}
	callID := strings.TrimSpace(call.Get("call_id").String())
	return callID, strings.HasPrefix(callID, "call_ccload_overdraft_") &&
		strings.TrimSpace(output.Get("call_id").String()) == callID
}

func responsesWebsocketJSONEqual(left, right gjson.Result) bool {
	var leftValue any
	var rightValue any
	if json.Unmarshal([]byte(left.Raw), &leftValue) != nil || json.Unmarshal([]byte(right.Raw), &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
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
			if isResponsesWebsocketToolCallType(itemType) {
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
		case "compaction", "compaction_summary":
			return true
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
		if !isResponsesWebsocketToolCallOutputType(item.Get("type").String()) {
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

// validateResponsesWebsocketToolCallPairing rejects a transcript containing a
// function_call_output/custom_tool_call_output whose call_id has no matching
// function_call/custom_tool_call anywhere in the same input array.
//
// Upstream hard-rejects such transcripts. Without this check, that upstream
// 400 would reach classifier.go, which treats HTTP 400 as a model-level
// failure and cools down + retries an unrelated channel for what is actually
// a malformed client request — burning the candidate list for nothing.
func validateResponsesWebsocketToolCallPairing(payload []byte) error {
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return nil
	}
	calls := make(map[string]struct{})
	for _, item := range input.Array() {
		if !isResponsesWebsocketToolCallType(item.Get("type").String()) {
			continue
		}
		if callID := strings.TrimSpace(item.Get("call_id").String()); callID != "" {
			calls[callID] = struct{}{}
		}
	}
	for _, item := range input.Array() {
		if !isResponsesWebsocketToolCallOutputType(item.Get("type").String()) {
			continue
		}
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID == "" {
			continue
		}
		if _, ok := calls[callID]; !ok {
			return fmt.Errorf("websocket transcript has tool call output for unknown call_id %q", callID)
		}
	}
	return nil
}

func enforceResponsesWebsocketTranscriptLimit(payload []byte, maxBodyBytes int64) ([]byte, error) {
	if int64(len(payload)) > maxBodyBytes {
		return nil, fmt.Errorf("websocket transcript exceeds %d byte limit; compact and replay the conversation", maxBodyBytes)
	}
	return payload, nil
}

// finalizeResponsesWebsocketRequest is the single funnel every normalizeRequest
// branch returns through: validate tool call pairing, then enforce the
// transcript byte limit.
func finalizeResponsesWebsocketRequest(payload []byte, maxBodyBytes int64) ([]byte, error) {
	if err := validateResponsesWebsocketToolCallPairing(payload); err != nil {
		return nil, err
	}
	return enforceResponsesWebsocketTranscriptLimit(payload, maxBodyBytes)
}

// responsesReplayWithoutNonPortableReasoning removes stored reasoning
// references that cannot survive store=false or a transport/account boundary.
// encrypted_content is the self-contained representation for stateless replay.
// Message and tool items are wire-visible transcript and retain their required IDs.
func responsesReplayWithoutNonPortableReasoning(payload []byte) []byte {
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return payload
	}
	items := input.Array()
	filtered := make([]json.RawMessage, 0, len(items))
	changed := false
	for _, item := range items {
		itemID := strings.TrimSpace(item.Get("id").String())
		encrypted := item.Get("encrypted_content")
		if item.Get("type").String() == "reasoning" && itemID != "" &&
			(encrypted.Type != gjson.String || strings.TrimSpace(encrypted.String()) == "") {
			changed = true
			continue
		}
		filtered = append(filtered, json.RawMessage(item.Raw))
	}
	if !changed {
		return payload
	}
	encoded, err := json.Marshal(filtered)
	if err != nil {
		return payload
	}
	updated, err := sjson.SetRawBytes(payload, "input", encoded)
	if err != nil {
		return payload
	}
	return updated
}
