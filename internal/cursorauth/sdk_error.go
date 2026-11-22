package cursorauth

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"ccLoad/internal/util"

	"connectrpc.com/connect"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
)

// RateLimit preserves optional Cursor rate-limit fields, including zero values.
type RateLimit struct {
	Limit             *uint64
	Remaining         *uint64
	ResetEpochSeconds *uint64
}

// BridgeError preserves Cursor's structured error contract without exposing
// generated protobuf types to the application layer.
type BridgeError struct {
	ConnectCode           connect.Code
	SDKCode               sdkv1.SdkErrorCode
	Message               string
	RequestID             string
	HelpURL               string
	Provider              string
	RetryAfter            time.Duration
	RateLimit             *RateLimit
	contextLengthExceeded bool ///< Run 终态信封或结果含明确的上下文超限码。
	cause                 error
}

/**
 * @brief 表示网关已确认问题出在客户端请求参数。
 */
var ErrInvalidRequest = errors.New("invalid Cursor request")

/**
 * @brief 格式化 SDK 拒绝，保留业务分类与错误说明。
 * @return 可显示的错误说明，nil 接收者返回空字符串。
 */
func (e *BridgeError) Error() string {
	if e == nil {
		return ""
	}
	message := e.Message
	if message == "" && e.cause != nil {
		message = e.cause.Error()
	}
	if e.SDKCode == sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED {
		if message == "" {
			return "cursor is not authenticated"
		}
		return "cursor is not authenticated: " + message
	}
	if e.SDKCode != sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNSPECIFIED {
		return fmt.Sprintf("cursor %s: %s", e.SDKCode.String(), message)
	}
	if e.ConnectCode == 0 {
		return "cursor run failed: " + message
	}
	if e.ConnectCode != connect.CodeUnknown {
		return fmt.Sprintf("cursor-sdk-bridge %s: %s", e.ConnectCode.String(), message)
	}
	return "cursor-sdk-bridge: " + message
}

func (e *BridgeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

/**
 * @brief 判断 Cursor 是否拒绝了渠道的会话或 User API Key。
 * @param err Run 或 Bridge 调用返回的错误。
 * @return SDK 确认凭证无效或找不到对应 API Key 时返回 true。
 */
func IsCredentialRejected(err error) bool {
	bridgeErr, ok := errors.AsType[*BridgeError](err)
	return ok && (bridgeErr.SDKCode == sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED ||
		bridgeErr.SDKCode == sdkv1.SdkErrorCode_SDK_ERROR_CODE_API_KEY_NOT_FOUND)
}

/**
 * @brief 判断当前 Cursor 账户是否明确缺少套餐、角色或功能权限。
 * @param err Run 或 Bridge 调用返回的错误。
 * @return SDK 确认账户无法使用该能力时返回 true。
 */
func IsAccountCapabilityUnavailable(err error) bool {
	bridgeErr, ok := errors.AsType[*BridgeError](err)
	if !ok {
		return false
	}
	switch bridgeErr.SDKCode {
	case sdkv1.SdkErrorCode_SDK_ERROR_CODE_PLAN_REQUIRED,
		sdkv1.SdkErrorCode_SDK_ERROR_CODE_ROLE_FORBIDDEN,
		sdkv1.SdkErrorCode_SDK_ERROR_CODE_FEATURE_UNAVAILABLE:
		return true
	default:
		return false
	}
}

/**
 * @brief 判断 Cursor 是否明确报告本次请求超过模型上下文窗口。
 * @param err Run 或 Bridge 调用返回的错误。
 * @return 已知上下文超限码或明确的超限说明返回 true。
 */
func IsContextLengthExceeded(err error) bool {
	bridgeErr, ok := errors.AsType[*BridgeError](err)
	if !ok {
		return false
	}
	switch bridgeErr.SDKCode {
	case sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNSPECIFIED,
		sdkv1.SdkErrorCode_SDK_ERROR_CODE_VALIDATION_ERROR,
		sdkv1.SdkErrorCode_SDK_ERROR_CODE_UPSTREAM_ERROR:
		return bridgeErr.contextLengthExceeded || util.IsExplicitContextLengthExceeded("", bridgeErr.Message)
	default:
		return false
	}
}

/**
 * @brief 判断 Cursor 是否因账户用量上限拒绝了当前模型。
 * @param err Run 或 Bridge 调用返回的错误。
 * @return 错误链中含 USAGE_LIMIT_EXCEEDED 的 BridgeError 时为 true。
 */
func IsUsageLimitExceeded(err error) bool {
	var bridgeErr *BridgeError
	return errors.As(err, &bridgeErr) &&
		bridgeErr.SDKCode == sdkv1.SdkErrorCode_SDK_ERROR_CODE_USAGE_LIMIT_EXCEEDED
}

/**
 * @brief 识别 Cursor SDK 的临时限流并取出可用的恢复时间。
 * @param err Run 或 Bridge 调用返回的错误。
 * @param now 识别本次限流的时间。
 * @return 可信的恢复时间，优先 RetryAfter，其次限流窗口重置时间；缺失时为零。
 * @return 错误链包含 SDK 的 RATE_LIMIT_EXCEEDED 时为 true。
 */
func RateLimitRetryTime(err error, now time.Time) (time.Time, bool) {
	bridgeErr, ok := errors.AsType[*BridgeError](err)
	if !ok || bridgeErr.SDKCode != sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED {
		return time.Time{}, false
	}
	if bridgeErr.RetryAfter > 0 {
		return now.Add(bridgeErr.RetryAfter), true
	}
	if limit := bridgeErr.RateLimit; limit != nil && limit.ResetEpochSeconds != nil {
		reset := *limit.ResetEpochSeconds
		if reset <= uint64(now.Add(math.MaxInt64).Unix()) {
			if until := time.Unix(int64(reset), 0); until.After(now) {
				return until, true
			}
		}
	}
	return time.Time{}, true
}

/**
 * @brief 判断 Cursor 是否明确拒绝当前模型。
 * @param err Run 或 Bridge 调用返回的错误。
 * @return 错误链包含 SDK 的 INVALID_MODEL 时为 true。
 */
func IsInvalidModel(err error) bool {
	bridgeErr, ok := errors.AsType[*BridgeError](err)
	return ok && bridgeErr.SDKCode == sdkv1.SdkErrorCode_SDK_ERROR_CODE_INVALID_MODEL
}

/**
 * @brief 判断 SDK 是否明确报告请求中的 Agent 不存在。
 * @param err 已分类的 Send 启动错误。
 * @return SDK 确认 Agent 已不存在时为 true。
 */
func isBridgeAgentNotFound(err error) bool {
	bridgeErr, ok := errors.AsType[*BridgeError](err)
	return ok && bridgeErr.SDKCode == sdkv1.SdkErrorCode_SDK_ERROR_CODE_AGENT_NOT_FOUND
}

/**
 * @brief 把 Connect 错误转换为应用可据以分流的 SDK 错误。
 * @param err RPC 或流返回的错误。
 * @return 带原始错误链的 BridgeError；非 Connect 错误原样返回，nil 仍为 nil。
 */
func classifyBridgeError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*BridgeError](err); ok {
		return err
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return err
	}
	bridgeErr := &BridgeError{
		ConnectCode: connectErr.Code(),
		Message:     connectErr.Message(),
		cause:       err,
	}
	for _, detail := range connectErr.Details() {
		value, valueErr := detail.Value()
		if valueErr != nil {
			continue
		}
		sdkDetail, ok := value.(*sdkv1.SdkErrorDetails)
		if !ok {
			continue
		}
		bridgeErr.SDKCode = sdkDetail.GetSdkErrorCode()
		if sdkDetail.GetMessage() != "" {
			bridgeErr.Message = sdkDetail.GetMessage()
		}
		bridgeErr.RequestID = sdkDetail.GetRequestId()
		bridgeErr.HelpURL = sdkDetail.GetHelpUrl()
		bridgeErr.Provider = sdkDetail.GetProvider()
		if retryAfter := sdkDetail.GetRetryAfter(); retryAfter != nil && retryAfter.IsValid() {
			delay := retryAfter.AsDuration()
			// AsDuration clamps protobuf durations beyond Go's representable range.
			if retryAfter.GetSeconds() == int64(delay/time.Second) && retryAfter.GetNanos() == int32(delay%time.Second) {
				bridgeErr.RetryAfter = delay
			}
		}
		if limit := sdkDetail.GetRateLimit(); limit != nil {
			bridgeErr.RateLimit = &RateLimit{
				Limit:             limit.Limit,
				Remaining:         limit.Remaining,
				ResetEpochSeconds: limit.ResetEpochSeconds,
			}
		}
		break
	}
	return bridgeErr
}

/**
 * @brief 转换 Bridge RPC 错误，并保留超时来源及可安全输出的诊断。
 * @param operation Bridge RPC 名称。
 * @param operationCtx 本次 RPC 的上下文。
 * @param timeout 本地 RPC 时间预算；未单独限制时为零。
 * @param started RPC 开始时间。
 * @param err 上游返回的错误。
 * @param apiKey 当前请求的 User API Key。
 * @return 可安全输出的已分类错误，保留 SDK 详情和错误链。
 */
func classifyBridgeOperationError(
	operation string,
	operationCtx context.Context,
	timeout time.Duration,
	started time.Time,
	err error,
	apiKey string,
) error {
	classified := classifyBridgeError(err)
	safe := redactBridgeError(classified, apiKey)
	var connectErr *connect.Error
	deadlineExceeded := errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &connectErr) && connectErr.Code() == connect.CodeDeadlineExceeded)
	if !deadlineExceeded {
		return safe
	}

	elapsed := max(time.Since(started), 0)
	summary := fmt.Sprintf("cursor SDK %s returned deadline_exceeded after %s", operation, elapsed.Round(time.Millisecond))
	if operationCtx != nil && errors.Is(context.Cause(operationCtx), context.DeadlineExceeded) {
		summary = fmt.Sprintf("cursor SDK %s exceeded its local deadline after %s", operation, elapsed.Round(time.Millisecond))
		if timeout > 0 {
			summary += fmt.Sprintf(" (operation limit=%s)", timeout)
		}
	}
	if bridgeDeadlineHasDiagnostic(classified) {
		return fmt.Errorf("%s: %w", summary, safe)
	}

	diagnostic := "cursor-sdk-bridge returned no detail beyond deadline_exceeded"
	if bridgeProxyEnvironmentConfigured() {
		diagnostic = "cursor-sdk-bridge inherited HTTP_PROXY/HTTPS_PROXY/ALL_PROXY and returned no detail beyond deadline_exceeded"
	}
	return fmt.Errorf("%s; %s: %w", summary, diagnostic, safe)
}

func bridgeDeadlineHasDiagnostic(err error) bool {
	var bridgeErr *BridgeError
	if !errors.As(err, &bridgeErr) {
		return false
	}
	if bridgeErr.SDKCode != sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNSPECIFIED ||
		bridgeErr.RequestID != "" || bridgeErr.HelpURL != "" || bridgeErr.Provider != "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(bridgeErr.Message)) {
	case "", "deadline exceeded", "context deadline exceeded":
		return false
	default:
		return true
	}
}

func bridgeProxyEnvironmentConfigured() bool {
	for _, key := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
		"http_proxy", "https_proxy", "all_proxy",
	} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return true
		}
	}
	return false
}

func isBridgeTransportFailure(err error) bool {
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeUnavailable {
		return false
	}
	for _, detail := range connectErr.Details() {
		value, valueErr := detail.Value()
		if valueErr == nil {
			if _, ok := value.(*sdkv1.SdkErrorDetails); ok {
				return false
			}
		}
	}
	return true
}
