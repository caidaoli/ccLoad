package cursorauth

import (
	"errors"
	"fmt"
	"strings"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
)

type sdkRunState struct {
	agentID       string
	runID         string
	text          string
	resultSeen    bool
	doneSeen      bool
	status        sdkv1.RunLifecycleStatus
	errorCode     string
	statusMessage string
	usage         *Usage
	onRunID       func(string)
	onTerminal    func(string)
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
		return s.consumeResult(envelope.Result)
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
	if message.GetType() == "status" {
		if text := firstString(payload, "message"); text != "" {
			s.statusMessage = text
		}
		if strings.EqualFold(firstString(payload, "status"), "RUNNING") {
			return []Event{{Ping: true}}, nil
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
		s.text += delta
		events = append(events, Event{Delta: delta, Text: s.text})
	}
	return events, nil
}

func (s *sdkRunState) consumeResult(result *sdkv1.RunStreamResult) ([]Event, error) {
	if result == nil {
		return nil, errors.New("cursor run returned an empty result envelope")
	}
	if s.resultSeen {
		return nil, errors.New("cursor run returned more than one result envelope")
	}
	if _, err := s.recordRunID(result.GetRunId()); err != nil {
		return nil, err
	}
	if err := s.setAgentID(result.GetAgentId()); err != nil {
		return nil, err
	}
	s.resultSeen = true
	s.status = result.GetStatus()
	s.errorCode = result.GetErrorCode()
	final := result.GetResult()
	if final == nil {
		return nil, errors.New("cursor run terminal result is missing RunResult")
	}
	if _, err := s.recordRunID(final.GetRunId()); err != nil {
		return nil, err
	}
	if err := s.setAgentID(final.GetAgentId()); err != nil {
		return nil, err
	}
	if innerStatus := final.GetStatus(); innerStatus != sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_UNSPECIFIED {
		if s.status != sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_UNSPECIFIED && s.status != innerStatus {
			return nil, errors.New("cursor run result statuses disagree")
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
		return nil, fmt.Errorf("cursor run result has non-terminal status %s", s.status.String())
	}
	if s.onTerminal != nil {
		s.onTerminal(s.runID)
	}
	finalText := final.GetResult()
	switch {
	case finalText == s.text:
		return nil, nil
	case strings.HasPrefix(finalText, s.text):
		delta := finalText[len(s.text):]
		s.text = finalText
		return []Event{{Delta: delta, Text: s.text}}, nil
	case finalText != "" && strings.HasSuffix(s.text, finalText):
		// One run may emit an interim assistant message before its final
		// assistant message. RunResult.result contains only the latter, while
		// Text is the complete append-only stream already delivered downstream.
		return nil, nil
	default:
		return nil, errors.New("cursor run final text diverged from its assistant stream")
	}
}

func usageFromSDK(usage *sdkv1.TokenUsage) *Usage {
	if usage == nil {
		return nil
	}
	return &Usage{
		InputTokens:      safeSDKTokenCount(usage.GetInputTokens()),
		OutputTokens:     safeSDKTokenCount(usage.GetOutputTokens()),
		CacheReadTokens:  safeSDKTokenCount(usage.GetCacheReadTokens()),
		CacheWriteTokens: safeSDKTokenCount(usage.GetCacheWriteTokens()),
		TotalTokens:      safeSDKTokenCount(usage.GetTotalTokens()),
		ReasoningTokens:  safeSDKTokenCount(usage.GetReasoningTokens()),
	}
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

func (s *sdkRunState) finalError(streamErr error) error {
	if streamErr != nil {
		return classifyBridgeError(streamErr)
	}
	if !s.resultSeen {
		return errors.New("cursor run stream closed before result")
	}
	if !s.doneSeen {
		return errors.New("cursor run stream closed before done")
	}
	switch s.status {
	case sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED:
		return nil
	case sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR,
		sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_CANCELLED,
		sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_EXPIRED:
		message := strings.TrimSpace(s.statusMessage)
		if message == "" {
			message = strings.TrimSpace(s.errorCode)
		}
		if message == "" {
			message = s.status.String()
		}
		if cursorRunCredentialRejected(s.errorCode, message) {
			return &BridgeError{
				SDKCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED,
				Message: message,
			}
		}
		return errors.New("cursor run failed: " + message)
	default:
		return fmt.Errorf("cursor run ended with non-terminal status %s", s.status.String())
	}
}

func cursorRunCredentialRejected(errorCode, message string) bool {
	code := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(errorCode)))
	switch code {
	case "unauthorized", "unauthenticated", "authentication_error", "auth_error", "sdk_error_code_unauthorized":
		return true
	}
	// Bridge v1.0.28 sometimes exposes a rejected Cursor session only through
	// the terminal Run status text. Keep this exact remediation phrase scoped
	// to Run results; bare Connect authentication errors may belong to the
	// loopback bridge itself and must not rotate a channel credential.
	message = strings.ToLower(strings.TrimSpace(message))
	return strings.HasPrefix(message, "authentication error") &&
		strings.Contains(message, "try logging out and back in")
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok {
			return value
		}
	}
	return ""
}
