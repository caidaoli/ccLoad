package protocol

import "context"

// Protocol identifies a client-facing or upstream request/response protocol.
type Protocol string

const (
	// Anthropic is the Anthropic Messages protocol surface.
	Anthropic Protocol = "anthropic"
	// Codex is the Codex Responses protocol surface.
	Codex Protocol = "codex"
	// OpenAI is the OpenAI-compatible protocol surface.
	OpenAI Protocol = "openai"
	// Gemini is the Gemini generateContent protocol surface.
	Gemini Protocol = "gemini"
)

// TransformPlan captures the chosen transform metadata for one proxy attempt.
type TransformPlan struct {
	ClientProtocol   Protocol
	UpstreamProtocol Protocol
	OriginalPath     string
	UpstreamPath     string
	OriginalBody     []byte
	TranslatedBody   []byte
	OriginalModel    string
	ActualModel      string
	Streaming        bool
	NeedsTransform   bool
}

// RequestTransform rewrites one client request body into the upstream protocol shape.
type RequestTransform func(model string, rawJSON []byte, stream bool) ([]byte, error)

// ResponseStreamTransform rewrites one upstream streaming event into client-facing chunks.
type ResponseStreamTransform func(ctx context.Context, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) ([][]byte, error)

// ResponseNonStreamTransform rewrites one upstream non-stream response into the client-facing shape.
type ResponseNonStreamTransform func(ctx context.Context, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte) ([]byte, error)
