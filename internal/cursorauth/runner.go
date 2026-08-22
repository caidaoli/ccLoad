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
// only text newly appended by this event. Usage is present on the final event
// when the SDK runtime reports it. RawResponse contains exactly one received
// RunStreamMessage encoded as standard protobuf JSON when capture is enabled.
type Event struct {
	Delta       string
	Text        string
	Done        bool
	Err         error
	Usage       *Usage
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
	Run(ctx context.Context, credential *Credential, model, prompt string) (<-chan Event, error)
}

// ModelLister returns the exact model IDs accepted by the Cursor SDK.
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
)
