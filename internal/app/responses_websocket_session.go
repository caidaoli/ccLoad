package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var errResponsesWebsocketPreviousResponseNotFound = errors.New(
	"previous response is not available on this websocket; resend the full conversation input without previous_response_id",
)

/**
 * @brief 标记本地转换没有可核对的 Responses 续接历史。
 */
var errResponsesPreviousResponseUnavailable = errors.New("previous response is not available for protocol conversion; resend the full conversation input without previous_response_id")

/**
 * @brief 跟踪一个执行会话最后一轮的请求、输出和续接状态。
 */
type responsesWebsocketSession struct {
	lastRequest               []byte
	lastResponseOutput        []byte
	lastResponseID            string
	lastResponseIDLocal       bool ///< 上轮终态的 ID 只能由本地会话恢复。
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
	if !sonic.Valid(payload) {
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
	return finalizeResponsesWebsocketRequest(normalized, s.maxBodyBytes)
}

/**
 * @brief 生成 Responses 的完整回放正文和可用于原生 WebSocket 的续接正文。
 * @param payload 本轮 WebSocket 事件。
 * @return 含完整会话历史的正文，失败时为 nil。
 * @return 同连接续接正文；上轮 ID 仅在本地有效时使用完整回放，失败时为 nil。
 * @return 请求无效或恢复失败时返回错误；未知上一轮 ID 可用 errors.Is 匹配 errResponsesWebsocketPreviousResponseNotFound。
 * @note 连接是否可复用由上游会话在确定渠道、Key 和 URL 后决定。
 */
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
	if len(s.lastRequest) == 0 || s.lastResponseIDLocal {
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

/**
 * @brief 恢复当前执行会话可核对的 HTTP Responses 历史。
 * @param payload 客户端原始 HTTP 请求。
 * @return 完整回放请求，保留 HTTP 的流式选择；无法恢复时为 nil。
 * @return 已建立的原生 WebSocket 可用的流式增量请求；无法恢复时为 nil。
 * @return 当前进程可维护本轮历史时为 true。
 * @return 已知会话中的续接无效或请求无法规范化时返回错误。
 */
func (s *responsesWebsocketSession) normalizeHTTPRequests(payload []byte) (
	replayRequest, incrementalRequest []byte, localContinuation bool, err error,
) {
	websocketPayload, err := sjson.SetBytes(payload, "type", responsesWebsocketRequestCreate)
	if err != nil {
		return nil, nil, false, err
	}
	if input := gjson.GetBytes(payload, "input"); input.Type == gjson.String {
		// HTTP accepts a string shorthand; local transcript replay merges items.
		websocketPayload, err = sjson.SetBytes(websocketPayload, "input", []map[string]string{{"role": "user", "content": input.String()}})
		if err != nil {
			return nil, nil, false, fmt.Errorf("normalize HTTP input: %w", err)
		}
	}
	previousID := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String())
	if previousID != "" && previousID != s.lastResponseID {
		return nil, nil, false, nil
	}
	if previousID == "" {
		replayRequest, err = normalizeInitialResponsesWebsocketRequest(websocketPayload, s.maxBodyBytes)
		incrementalRequest = replayRequest
	} else {
		if !gjson.GetBytes(websocketPayload, "instructions").Exists() {
			instructions := gjson.GetBytes(s.lastRequest, "instructions")
			if instructions.Exists() {
				websocketPayload, err = sjson.SetRawBytes(websocketPayload, "instructions", []byte(instructions.Raw))
				if err != nil {
					return nil, nil, false, fmt.Errorf("inherit HTTP instructions: %w", err)
				}
			}
		}
		replayRequest, incrementalRequest, err = s.normalizeRequests(websocketPayload)
	}
	if err != nil {
		return nil, nil, false, err
	}
	if !isStreamingRequest("/v1/responses", payload) {
		replayRequest, err = sjson.SetBytes(replayRequest, "stream", false)
		if err != nil {
			return nil, nil, false, fmt.Errorf("restore HTTP streaming mode: %w", err)
		}
	}
	return replayRequest, incrementalRequest, true, nil
}

/**
 * @brief 保存下一轮可恢复的响应 ID 来源、输入和已交付的输出。
 * @param request 当前轮完整回放正文。
 * @param result 下游已收到的终态或需要保留已交付工具的中断结果。
 * @note 调用方持有当前执行会话的轮次锁。
 */
func (s *responsesWebsocketSession) commit(request []byte, result responsesWebsocketTurnResult) {
	if s == nil {
		return
	}
	s.lastRequest = bytes.Clone(request)
	items := gjson.ParseBytes(result.completedOutput).Array()
	if !result.interrupted && len(items) == 1 && items[0].Get("type").String() == "compaction" &&
		strings.HasPrefix(items[0].Get("encrypted_content").String(), responsesCompactionPrefix) {
		// The capsule replaces the summarized history. Keeping the old input
		// would replay its trigger and compact again on every continuation.
		s.lastRequest, _ = sjson.SetRawBytes(s.lastRequest, "input", []byte("[]"))
	}
	if len(result.completedOutput) == 0 {
		s.lastResponseOutput = []byte("[]")
	} else {
		s.lastResponseOutput = bytes.Clone(result.completedOutput)
	}
	s.lastResponseID = strings.TrimSpace(result.completedResponseID)
	s.lastResponseIDLocal = result.localResponseID
	s.pendingToolCallIDs = append([]string(nil), result.pendingToolCallIDs...)
}

/**
 * @brief 提取供 Responses 会话续轮使用的终态输出和关联 ID。
 * @param response 上游终态的 Responses 对象，可能引用整帧 JSON。
 * @return 含独立响应 ID 的续轮结果。
 */
func responsesWebsocketTurnFromResponse(response gjson.Result) responsesWebsocketTurnResult {
	output := []byte("[]")
	if items := response.Get("output"); items.IsArray() {
		output = []byte(items.Raw)
	}
	return responsesWebsocketTurnResult{
		completedResponseID: strings.Clone(strings.TrimSpace(response.Get("id").String())),
		completedOutput:     output,
		pendingToolCallIDs:  responsesWebsocketPendingToolCallIDs(output),
	}
}

/**
 * @brief 提取 HTTP JSON 中可用于会话续接的 Responses 终态。
 * @param body 已生成或读完的下游 JSON 响应正文。
 * @return 已完成或不完整的响应及输出；其他 JSON 或无有效响应 ID 时为空。
 * @return 捕获到可续接终态时为 true。
 */
func responsesHTTPJSONTurn(body []byte) (responsesWebsocketTurnResult, bool) {
	response := parseJSONBytesNoCopy(body)
	status := response.Get("status").String()
	if (status != "completed" && status != "incomplete") || response.Get("id").Type != gjson.String || !json.Valid(body) {
		return responsesWebsocketTurnResult{}, false
	}
	turn := responsesWebsocketTurnFromResponse(response)
	return turn, turn.completedResponseID != ""
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
		if isNonPortableResponsesReasoning(item) {
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
