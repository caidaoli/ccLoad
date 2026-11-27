package cursorauth

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"ccLoad/internal/util"

	"google.golang.org/protobuf/encoding/protojson"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
)

/**
 * @brief 单条 Send 流的状态及已发出用量，仅由流读取协程访问。
 */
type sdkRunState struct {
	agentID               string
	runID                 string
	finalText             string ///< SDK 终态快照；是否为正文取决于最终运行状态。
	resultSeen            bool
	doneSeen              bool
	status                sdkv1.RunLifecycleStatus
	errorCode             string
	errorMessage          string ///< 来自状态事件的诊断，终态提供具体原因时替换。
	contextLengthExceeded bool   ///< 终态错误字段明确报告单次请求超过上下文窗口。
	usage                 *Usage
	reportedUsage         Usage ///< 已通过逐轮事件发出的真实用量，用于扣除终态重复累计。
	onRunID               func(string)
	onTerminal            func(string)
}

func (s *sdkRunState) consume(message *sdkv1.RunStreamMessage) ([]Event, error) {
	if message == nil {
		return nil, nil
	}
	if s.doneSeen {
		return nil, errors.New("cursor run returned data after done")
	}
	switch envelope := message.GetEnvelope().(type) {
	case *sdkv1.RunStreamMessage_SdkMessage:
		if s.resultSeen {
			return nil, errors.New("cursor run returned an sdk_message after result")
		}
		return s.consumeSDKMessage(envelope.SdkMessage)
	case *sdkv1.RunStreamMessage_Result:
		return nil, s.consumeResult(envelope.Result)
	case *sdkv1.RunStreamMessage_Done:
		return nil, s.consumeDone(envelope.Done)
	case *sdkv1.RunStreamMessage_InteractionUpdate, *sdkv1.RunStreamMessage_Step:
		if s.resultSeen {
			return nil, errors.New("cursor run returned data after result")
		}
		return nil, nil
	default:
		// Empty keepalives, raw deltas, steps, and future envelopes are not
		// assistant text. The typed assistant stream is the sole text source.
		return nil, nil
	}
}

/**
 * @brief 将 SDK 消息转换为独立思考、正文、运行状态和逐轮真实用量事件。
 * @param message 上游 SDK 消息。
 * @return 本条消息产生的事件；无客户端事件时为 nil。
 * @return 会话身份改变或 usage 结构非法时返回错误。
 */
func (s *sdkRunState) consumeSDKMessage(message *sdkv1.SdkMessage) ([]Event, error) {
	if message == nil {
		return nil, nil
	}
	if message.GetMessage() == nil {
		return nil, nil
	}
	payload := message.GetMessage().AsMap()
	if err := s.setLiveRunID(firstString(payload, "run_id", "runId")); err != nil {
		return nil, err
	}
	if err := s.setAgentID(firstString(payload, "agent_id", "agentId")); err != nil {
		return nil, err
	}
	if message.GetType() == "usage" {
		value := message.GetMessage().GetFields()["usage"].GetStructValue()
		if value == nil {
			return nil, errors.New("cursor usage message is missing token counts")
		}
		data, err := protojson.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode cursor usage message: %w", err)
		}
		var tokens sdkv1.TokenUsage
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, &tokens); err != nil {
			return nil, fmt.Errorf("decode cursor usage message: %w", err)
		}
		usage := usageFromSDK(&tokens)
		if usage == nil {
			return nil, nil
		}
		s.reportedUsage.Add(usage)
		return []Event{{Usage: usage}}, nil
	}
	if message.GetType() == "status" {
		if strings.EqualFold(firstString(payload, "status"), "RUNNING") {
			return []Event{{Ping: true}}, nil
		}
		if text := strings.TrimSpace(firstString(payload, "message")); text != "" {
			s.errorMessage = text
		}
		return nil, nil
	}
	if message.GetType() == "thinking" {
		if delta := firstString(payload, "text"); delta != "" {
			return []Event{{ThinkingDelta: delta}}, nil
		}
		return nil, nil
	}
	if message.GetType() != "assistant" {
		return nil, nil
	}
	inner, _ := payload["message"].(map[string]any)
	content, _ := inner["content"].([]any)
	events := make([]Event, 0, len(content))
	for _, value := range content {
		block, ok := value.(map[string]any)
		if !ok || firstString(block, "type") != "text" {
			continue
		}
		delta := firstString(block, "text")
		if delta == "" {
			continue
		}
		events = append(events, Event{Delta: delta})
	}
	return events, nil
}

/**
 * @brief 从 run 累计用量扣除已发出的逐轮用量，防止终态重复计费。
 * @return 尚未发出的真实用量；上游没有累计量时为 nil。
 * @note 累计快照可能滞后于流事件，各分项差额最低为零。
 */
func (s *sdkRunState) unreportedUsage() *Usage {
	if s.usage == nil {
		return nil
	}
	return &Usage{
		InputTokens:      max(0, s.usage.InputTokens-s.reportedUsage.InputTokens),
		OutputTokens:     max(0, s.usage.OutputTokens-s.reportedUsage.OutputTokens),
		CacheReadTokens:  max(0, s.usage.CacheReadTokens-s.reportedUsage.CacheReadTokens),
		CacheWriteTokens: max(0, s.usage.CacheWriteTokens-s.reportedUsage.CacheWriteTokens),
		TotalTokens:      max(0, s.usage.TotalTokens-s.reportedUsage.TotalTokens),
		ReasoningTokens:  max(0, s.usage.ReasoningTokens-s.reportedUsage.ReasoningTokens),
	}
}

/**
 * @brief 识别 SDK 没有提供具体原因的错误文案。
 * @param message 已去除首尾空白的诊断文本。
 * @return 空文案或通用失败说明返回 true。
 */
func genericCursorRunError(message string) bool {
	switch strings.TrimSuffix(strings.ToLower(message), ".") {
	case "", "error", "unknown error", "run failed", "cursor sdk run failed":
		return true
	}
	return false
}

/**
 * @brief 判定终态正文是否为已流出的末段，保留原始输出字节。
 * @param streamed 当前 HTTP 轮已发出的正文。
 * @param final SDK 终态携带的正文。
 * @return 原样后缀或仅首尾空白不同的完整末段返回 true。
 */
func cursorFinalTextAlreadyEmitted(streamed, final string) bool {
	tail := strings.TrimSpace(final)
	if tail == "" {
		return final == "" || strings.HasSuffix(streamed, final)
	}
	prefix, matched := strings.CutSuffix(strings.TrimSpace(streamed), tail)
	if !matched || prefix == "" {
		return matched
	}
	before, _ := utf8.DecodeLastRuneInString(prefix)
	after, _ := utf8.DecodeRuneInString(tail)
	isWord := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }
	return !isWord(before) || !isWord(after)
}

/**
 * @brief 比对成功终态和当前 HTTP 轮的正文，保留已经交付的字节。
 * @param streamed 本轮已经交付的正文，不含任何工具结果之前的轮次。
 * @param final 通过 SDK 成功终态确认的正文快照。
 * @return 终态新增的正文；没有增量时为空。
 * @return 非空快照与流出的正文冲突时返回错误。
 */
func cursorFinalTextDelta(streamed, final string) (string, error) {
	if cursorFinalTextAlreadyEmitted(streamed, final) {
		return "", nil
	}
	if delta, ok := strings.CutPrefix(final, streamed); ok {
		return delta, nil
	}
	return "", errors.New("cursor run final text diverged from its assistant stream")
}

/**
 * @brief 校验终态并暂存快照和用量，等待完整终态序列确认。
 * @param result 上游终态信封。
 * @return 终态结构、会话身份或状态不一致时返回错误。
 */
func (s *sdkRunState) consumeResult(result *sdkv1.RunStreamResult) error {
	if result == nil {
		return errors.New("cursor run returned an empty result envelope")
	}
	if s.resultSeen {
		return errors.New("cursor run returned more than one result envelope")
	}
	if _, err := s.recordRunID(result.GetRunId()); err != nil {
		return err
	}
	if err := s.setAgentID(result.GetAgentId()); err != nil {
		return err
	}
	s.resultSeen = true
	s.status = result.GetStatus()
	final := result.GetResult()
	if final == nil {
		return errors.New("cursor run terminal result is missing RunResult")
	}
	outerCode, innerCode := strings.TrimSpace(result.GetErrorCode()), strings.TrimSpace(final.GetErrorCode())
	s.errorCode = cmp.Or(outerCode, innerCode)
	s.contextLengthExceeded = util.IsExplicitContextLengthExceeded(normalizeCursorRunErrorCode(outerCode), "") ||
		util.IsExplicitContextLengthExceeded(normalizeCursorRunErrorCode(innerCode), "")
	finalMessage := strings.TrimSpace(final.GetErrorMessage())
	if genericCursorRunError(finalMessage) {
		s.errorMessage = cmp.Or(s.errorMessage, finalMessage)
	} else {
		s.errorMessage = finalMessage
	}
	if _, err := s.recordRunID(final.GetRunId()); err != nil {
		return err
	}
	if err := s.setAgentID(final.GetAgentId()); err != nil {
		return err
	}
	if innerStatus := final.GetStatus(); innerStatus != sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_UNSPECIFIED {
		if s.status != sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_UNSPECIFIED && s.status != innerStatus {
			return errors.New("cursor run result statuses disagree")
		}
		s.status = innerStatus
	}
	s.usage = usageFromSDK(final.GetUsage())
	switch s.status {
	case sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED,
		sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR,
		sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_CANCELLED,
		sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_EXPIRED:
	default:
		return fmt.Errorf("cursor run result has non-terminal status %s", s.status.String())
	}
	if s.onTerminal != nil {
		s.onTerminal(s.runID)
	}
	s.finalText = final.GetResult()
	return nil
}

/**
 * @brief 将 SDK 用量转换为计费所需的互斥输入与缓存计数。
 * @param usage 上游用量；nil 表示未报告。
 * @return 归一化用量；未报告或含缓存的总输入小于缓存读写合计时返回 nil。
 * @details v1.2.0 本地运行时的 total=input+output，input 已包含缓存；
 * 仅在上游总量明确采用该口径时扣除缓存，保留采用分项总和口径的上游数据。
 */
func usageFromSDK(usage *sdkv1.TokenUsage) *Usage {
	if usage == nil {
		return nil
	}
	result := &Usage{
		InputTokens:      safeSDKTokenCount(usage.GetInputTokens()),
		OutputTokens:     safeSDKTokenCount(usage.GetOutputTokens()),
		CacheReadTokens:  safeSDKTokenCount(usage.GetCacheReadTokens()),
		CacheWriteTokens: safeSDKTokenCount(usage.GetCacheWriteTokens()),
		TotalTokens:      safeSDKTokenCount(usage.GetTotalTokens()),
		ReasoningTokens:  safeSDKTokenCount(usage.GetReasoningTokens()),
	}
	if result.TotalTokens > 0 && result.TotalTokens-result.InputTokens == result.OutputTokens {
		afterCacheRead := result.InputTokens - result.CacheReadTokens
		if result.CacheWriteTokens > afterCacheRead {
			return nil
		}
		result.InputTokens = afterCacheRead - result.CacheWriteTokens
	}
	return result
}

func safeSDKTokenCount(value int64) int {
	if value <= 0 {
		return 0
	}
	maxInt := int64(^uint(0) >> 1)
	if value > maxInt {
		return int(maxInt)
	}
	return int(value)
}

func (s *sdkRunState) consumeDone(done *sdkv1.RunStreamDone) error {
	if done == nil {
		return errors.New("cursor run returned an empty done envelope")
	}
	if !s.resultSeen {
		return errors.New("cursor run returned done before result")
	}
	if s.doneSeen {
		return errors.New("cursor run returned more than one done envelope")
	}
	if _, err := s.recordRunID(done.GetRunId()); err != nil {
		return err
	}
	if err := s.setAgentID(done.GetAgentId()); err != nil {
		return err
	}
	s.doneSeen = true
	return nil
}

func (s *sdkRunState) setLiveRunID(runID string) error {
	added, err := s.recordRunID(runID)
	if err != nil {
		return err
	}
	if added && s.onRunID != nil {
		s.onRunID(s.runID)
	}
	return nil
}

func (s *sdkRunState) recordRunID(runID string) (bool, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return false, nil
	}
	if s.runID != "" && s.runID != runID {
		return false, fmt.Errorf("cursor stream changed run_id from %q to %q", s.runID, runID)
	}
	if s.runID == "" {
		s.runID = runID
		return true, nil
	}
	return false, nil
}

func (s *sdkRunState) setAgentID(agentID string) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil
	}
	if s.agentID != "" && s.agentID != agentID {
		return fmt.Errorf("cursor stream changed agent_id from %q to %q", s.agentID, agentID)
	}
	s.agentID = agentID
	return nil
}

/**
 * @brief 在 SDK 流结束时识别传输错误和结构化业务拒绝。
 * @param streamErr Connect 流错误；正常读完流时为 nil。
 * @param streamed 已经交付的当前轮正文。
 * @return 成功终态返回 nil，缺少终态帧时包装 io.ErrUnexpectedEOF，已识别的 SDK 拒绝返回 BridgeError。
 */
func (s *sdkRunState) finalError(streamErr error, streamed string) error {
	if streamErr != nil {
		return classifyBridgeError(streamErr)
	}
	if !s.resultSeen {
		return fmt.Errorf("cursor run stream closed before result: %w", io.ErrUnexpectedEOF)
	}
	if !s.doneSeen {
		return fmt.Errorf("cursor run stream closed before done: %w", io.ErrUnexpectedEOF)
	}
	switch s.status {
	case sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED:
		return nil
	case sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR,
		sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_CANCELLED,
		sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_EXPIRED:
		message := s.errorMessage
		if s.status == sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR && streamed == "" && genericCursorRunError(message) {
			if detail := strings.TrimSpace(s.finalText); !genericCursorRunError(detail) {
				message = detail
			}
		}
		message = cmp.Or(message, s.errorCode, s.status.String())
		bridgeErr := &BridgeError{
			SDKCode: cursorRunErrorCode(s.errorCode, message), Message: message,
			contextLengthExceeded: s.contextLengthExceeded,
		}
		if bridgeErr.SDKCode != sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNSPECIFIED || IsContextLengthExceeded(bridgeErr) {
			return bridgeErr
		}
		return errors.New("cursor run failed: " + message)
	default:
		return fmt.Errorf("cursor run ended with non-terminal status %s", s.status.String())
	}
}

/**
 * @brief 规范化 Run 终态错误码以匹配 SDK 和上游模型提供商使用的稳定名称。
 * @param code 已去掉首尾空白的 Run 终态错误码。
 * @return 小写名称，去除可选的 SDK_ERROR_CODE_ 前缀。
 */
func normalizeCursorRunErrorCode(code string) string {
	code = strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(code))
	return strings.TrimPrefix(code, "sdk_error_code_")
}

/**
 * @brief 将 Run 终态的稳定错误码和已有认证、用量文案映射为 SDK 分类。
 * @param errorCode 已去掉首尾空白的 Run 终态错误码，可能为空。
 * @param message 已去掉首尾空白的 Run 终态说明。
 * @return 已识别的 SDK 错误码；无法分类时返回 UNSPECIFIED。
 */
func cursorRunErrorCode(errorCode, message string) sdkv1.SdkErrorCode {
	codeName := normalizeCursorRunErrorCode(errorCode)
	if code := sdkv1.SdkErrorCode_value["SDK_ERROR_CODE_"+strings.ToUpper(codeName)]; code != 0 {
		return sdkv1.SdkErrorCode(code)
	}
	switch codeName {
	case "unauthenticated", "authentication_error", "auth_error":
		return sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED
	}
	// Bridge v1.0.28 sometimes exposes a rejected Cursor session only through
	// the terminal Run status text. Keep this exact remediation phrase scoped
	// to Run results; bare Connect authentication errors may belong to the
	// loopback bridge itself and must not rotate a channel credential.
	message = strings.ToLower(message)
	if strings.HasPrefix(message, "authentication error") && strings.Contains(message, "try logging out and back in") {
		return sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED
	}
	// Cursor sometimes reports only "You've hit your usage limit ..."; the
	// phrase after the apostrophe supports both straight and curly quotes.
	if strings.Contains(message, "hit your usage limit") {
		return sdkv1.SdkErrorCode_SDK_ERROR_CODE_USAGE_LIMIT_EXCEEDED
	}
	return sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNSPECIFIED
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok {
			return value
		}
	}
	return ""
}
