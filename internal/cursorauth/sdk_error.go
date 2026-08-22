package cursorauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

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
	ConnectCode connect.Code
	SDKCode     sdkv1.SdkErrorCode
	Message     string
	RequestID   string
	HelpURL     string
	Provider    string
	RetryAfter  time.Duration
	RateLimit   *RateLimit
	cause       error
}

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

// IsCredentialRejected reports whether Cursor rejected the channel's User API Key.
func IsCredentialRejected(err error) bool {
	var bridgeErr *BridgeError
	return errors.As(err, &bridgeErr) &&
		bridgeErr.SDKCode == sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED
}

func classifyBridgeError(err error) error {
	if err == nil {
		return nil
	}
	var existing *BridgeError
	if errors.As(err, &existing) {
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
			bridgeErr.RetryAfter = retryAfter.AsDuration()
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

func classifyBridgeOperationError(
	operation string,
	operationCtx context.Context,
	timeout time.Duration,
	started time.Time,
	err error,
) error {
	classified := classifyBridgeError(err)
	var connectErr *connect.Error
	deadlineExceeded := errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &connectErr) && connectErr.Code() == connect.CodeDeadlineExceeded)
	if !deadlineExceeded {
		return classified
	}

	elapsed := time.Since(started)
	if elapsed < 0 {
		elapsed = 0
	}
	summary := fmt.Sprintf("cursor SDK %s returned deadline_exceeded after %s", operation, elapsed.Round(time.Millisecond))
	if operationCtx != nil && errors.Is(context.Cause(operationCtx), context.DeadlineExceeded) {
		summary = fmt.Sprintf("cursor SDK %s exceeded its local deadline after %s", operation, elapsed.Round(time.Millisecond))
		if timeout > 0 {
			summary += fmt.Sprintf(" (operation limit=%s)", timeout)
		}
	}
	if bridgeDeadlineHasDiagnostic(classified) {
		return fmt.Errorf("%s: %w", summary, classified)
	}

	diagnostic := "cursor-sdk-bridge returned no detail beyond deadline_exceeded"
	if bridgeProxyEnvironmentConfigured() {
		diagnostic = "cursor-sdk-bridge inherited HTTP_PROXY/HTTPS_PROXY/ALL_PROXY and returned no detail beyond deadline_exceeded"
	}
	return fmt.Errorf("%s; %s: %w", summary, diagnostic, classified)
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
