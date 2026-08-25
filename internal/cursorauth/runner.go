package cursorauth

import (
	"context"
	"errors"
)

// Usage is the cumulative token usage reported for one Cursor SDK run.
// Cursor reports uncached input, cache reads, and cache writes as disjoint
// counters; TotalTokens is their sum with OutputTokens.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	TotalTokens      int
	ReasoningTokens  int
}

// Event is one Cursor inference update. Text is cumulative; Delta contains
// only text newly appended by this event. Usage may be an estimated context
// signal on a tool-call event or the final runtime usage. RawResponse contains
// exactly one received RunStreamMessage encoded as standard protobuf JSON when
// capture is enabled.
type Event struct {
	Delta    string
	Text     string
	ToolCall *ToolCall
	Usage    *Usage
	// UsageEstimated marks a local context estimate. Estimated usage is exposed
	// to the client for context management, but must not be billed or logged.
	UsageEstimated bool
	// Replayed marks a completed native run returned for a duplicate tool-result
	// request. The wire layer must not charge or log that run a second time.
	Replayed    bool
	Done        bool
	Err         error
	RawResponse []byte
}

type rawResponseCaptureContextKey struct{}

// WithRawResponseCapture asks the SDK runner to emit every RunStreamMessage
// as standard protobuf JSON before emitting its parsed inference events.
func WithRawResponseCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, rawResponseCaptureContextKey{}, true)
}

func rawResponseCaptureEnabled(ctx context.Context) bool {
	enabled, _ := ctx.Value(rawResponseCaptureContextKey{}).(bool)
	return enabled
}

// Runner runs one Cursor inference.
type Runner interface {
	Run(ctx context.Context, credential *Credential, request Request) (<-chan Event, error)
}

// ModelLister returns the model IDs accepted by the Cursor SDK.
type ModelLister interface {
	ListModels(ctx context.Context, apiKey string) ([]string, error)
}

var (
	// ErrAgentMissing reports that the pinned bridge companion is unavailable.
	ErrAgentMissing = errors.New("cursor-sdk-bridge is not installed")
	// ErrMissingAPIKey reports a channel imported without its User API Key.
	ErrMissingAPIKey = errors.New("cursor credential is missing api_key")
	// ErrBridgeClosed reports use after the process owner began shutting down.
	ErrBridgeClosed = errors.New("cursor-sdk-bridge is closed")
	// ErrToolSessionNotFound reports a result for a suspended run that is no
	// longer owned by this ccLoad process.
	ErrToolSessionNotFound = errors.New("cursor tool session was not found")
)
