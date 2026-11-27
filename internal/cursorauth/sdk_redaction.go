package cursorauth

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
)

/**
 * @brief Bridge 诊断中可能由网络库附带的认证数据，覆盖代理地址、请求头和结构化字段。
 */
var (
	diagnosticURLAuth = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/?#@]+@`)                                                                       ///< URL 中的用户信息。
	diagnosticHeader  = regexp.MustCompile(`(?i)\b((?:set-cookie|cookie|(?:proxy-)?authorization)["']?\s*[:=]\s*)[^\r\n]+`)                               ///< 整个认证头包含多种授权格式或多个 Cookie。
	diagnosticBearer  = regexp.MustCompile(`(?i)\b(bearer\s+)[a-z0-9._~+/=-]+`)                                                                           ///< 诊断中的独立 Bearer 值。
	diagnosticField   = regexp.MustCompile(`(?i)\b((?:api[_-]?key|(?:access[_-]?|refresh[_-]?)?token|session[_-]?id)["']?\s*[:=]\s*["']?)[^"'\s,;}\]&]+`) ///< 结构化文本与查询参数中的凭证值。
)

/**
 * @brief 遮蔽 Bridge 诊断里的凭证并限制诊断文本的大小。
 * @param text 上游诊断文本。
 * @param apiKey 当前请求的非空 User API Key。
 * @return 可公开的上游诊断，保留 UTF-8 字符边界。
 */
func redactSDKDiagnostic(text, apiKey string) string {
	if text == "" {
		return ""
	}
	text = diagnosticURLAuth.ReplaceAllString(text, "${1}[redacted]@")
	text = diagnosticHeader.ReplaceAllString(text, "${1}[redacted]")
	text = diagnosticBearer.ReplaceAllString(text, "${1}[redacted]")
	text = diagnosticField.ReplaceAllString(text, "${1}[redacted]")
	text = strings.ReplaceAll(text, apiKey, "[redacted]")
	const limit = 4096
	if len(text) > limit {
		cut := limit - len("…")
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		return text[:cut] + "…"
	}
	return text
}

/**
 * @brief 保留错误链以识别取消或其他哨兵错误，同时提供可输出的错误文本。
 */
type redactedSDKError struct {
	message string ///< 已遮蔽凭证的错误文本。
	cause   error  ///< 用于程序化错误匹配的原始错误。
}

/**
 * @brief 返回可输出的错误文本。
 * @return 已遮蔽凭证的错误文本。
 */
func (err *redactedSDKError) Error() string { return err.message }

/**
 * @brief 继续程序化错误匹配。
 * @return 原始 SDK 错误。
 */
func (err *redactedSDKError) Unwrap() error { return err.cause }

/**
 * @brief 遮蔽 Bridge 错误的诊断文本并保留 SDK 分类和限流信息。
 * @param err 已分类的 Bridge 错误或流故障，允许为 nil。
 * @param apiKey 当前请求的非空 User API Key。
 * @return 已遮蔽凭证的错误；没有回显时保留原错误。
 */
func redactBridgeError(err error, apiKey string) error {
	if err == nil {
		return nil
	}
	if bridgeErr, ok := err.(*BridgeError); ok {
		redacted := *bridgeErr
		redacted.Message = redactSDKDiagnostic(bridgeErr.Message, apiKey)
		if bridgeErr.Message == "" && bridgeErr.cause != nil {
			message := bridgeErr.cause.Error()
			if safe := redactSDKDiagnostic(message, apiKey); safe != message {
				redacted.Message = safe
			}
		}
		redacted.RequestID = redactSDKDiagnostic(bridgeErr.RequestID, apiKey)
		redacted.HelpURL = redactSDKDiagnostic(bridgeErr.HelpURL, apiKey)
		redacted.Provider = redactSDKDiagnostic(bridgeErr.Provider, apiKey)
		if redacted.Message != bridgeErr.Message || redacted.RequestID != bridgeErr.RequestID ||
			redacted.HelpURL != bridgeErr.HelpURL || redacted.Provider != bridgeErr.Provider {
			return &redacted
		}
		return err
	}
	message := err.Error()
	if safe := redactSDKDiagnostic(message, apiKey); safe != message {
		return &redactedSDKError{message: safe, cause: err}
	}
	return err
}

/**
 * @brief 遮蔽原始 SDK 调试帧中的状态与终态错误诊断。
 * @param message 将要保存的上游流帧。
 * @param apiKey 当前请求的非空 User API Key。
 * @return 可用于序列化的流帧；保留原始帧供错误分类和正常内容解析使用。
 */
func redactSDKDebugMessage(message *sdkv1.RunStreamMessage, apiKey string) *sdkv1.RunStreamMessage {
	if outer := message.GetResult(); outer != nil {
		result := outer.GetResult()
		outerCode, innerCode, reason := outer.GetErrorCode(), result.GetErrorCode(), result.GetErrorMessage()
		safeOuterCode := redactSDKDiagnostic(outerCode, apiKey)
		safeInnerCode := redactSDKDiagnostic(innerCode, apiKey)
		safeReason := redactSDKDiagnostic(reason, apiKey)
		text := result.GetResult()
		safeText := text
		if outer.GetStatus() == sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR || result.GetStatus() == sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR {
			safeText = redactSDKDiagnostic(text, apiKey)
		}
		if safeOuterCode != outerCode || safeInnerCode != innerCode || safeReason != reason || safeText != text {
			message = proto.CloneOf(message)
			outer = message.GetResult()
			if safeText != text {
				outer.GetResult().Result = safeText
			}
			if safeOuterCode != outerCode {
				outer.ErrorCode = &safeOuterCode
			}
			if safeInnerCode != innerCode {
				outer.GetResult().ErrorCode = &safeInnerCode
			}
			if safeReason != reason {
				outer.GetResult().ErrorMessage = &safeReason
			}
		}
	} else if sdkMessage := message.GetSdkMessage(); sdkMessage.GetType() == "status" {
		reason := sdkMessage.GetMessage().GetFields()["message"].GetStringValue()
		if safe := redactSDKDiagnostic(reason, apiKey); safe != reason {
			message = proto.CloneOf(message)
			message.GetSdkMessage().GetMessage().GetFields()["message"] = structpb.NewStringValue(safe)
		}
	}
	return message
}
