package protocol

import (
	"context"
	"fmt"
	"slices"
)

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

var supportedTransformSourcesByUpstream = map[Protocol][]Protocol{
	Gemini:    {Anthropic, Codex, OpenAI},
	Anthropic: {Codex, OpenAI},
}

// SupportedClientProtocolsForUpstream returns the documented client-facing protocols
// that can be translated into the given upstream protocol.
func SupportedClientProtocolsForUpstream(upstream Protocol) []Protocol {
	supported := supportedTransformSourcesByUpstream[upstream]
	if len(supported) == 0 {
		return nil
	}
	return slices.Clone(supported)
}

// SupportsTransform reports whether the runtime has a documented transform path for
// the given client/upstream protocol pair.
func SupportsTransform(client, upstream Protocol) bool {
	for _, candidate := range supportedTransformSourcesByUpstream[upstream] {
		if candidate == client {
			return true
		}
	}
	return false
}

// BuildTransformPlan turns request metadata into a concrete runtime plan that can
// travel through request preparation, forwarding, and response translation.
func BuildTransformPlan(client, upstream Protocol, originalPath, upstreamPath string, originalBody, preparedBody []byte, originalModel, actualModel string, streaming bool) (TransformPlan, error) {
	plan := TransformPlan{
		ClientProtocol:   client,
		UpstreamProtocol: upstream,
		OriginalPath:     originalPath,
		UpstreamPath:     upstreamPath,
		OriginalBody:     originalBody,
		TranslatedBody:   preparedBody,
		OriginalModel:    originalModel,
		ActualModel:      actualModel,
		Streaming:        streaming,
	}

	if plan.UpstreamPath == "" {
		plan.UpstreamPath = plan.OriginalPath
	}
	if plan.TranslatedBody == nil {
		plan.TranslatedBody = plan.OriginalBody
	}
	if plan.ActualModel == "" {
		plan.ActualModel = plan.OriginalModel
	}

	if client == "" || upstream == "" || client == upstream {
		return plan, nil
	}
	if !SupportsTransform(client, upstream) {
		return TransformPlan{}, fmt.Errorf("unsupported protocol transform: %s -> %s", client, upstream)
	}

	plan.NeedsTransform = true
	return plan, nil
}

// RequestModel returns the model name that should be sent upstream.
func (p TransformPlan) RequestModel() string {
	if p.ActualModel != "" {
		return p.ActualModel
	}
	return p.OriginalModel
}

// ResponseModel returns the client-visible model name to use in translated
// responses so redirects remain transparent to callers.
func (p TransformPlan) ResponseModel() string {
	if p.OriginalModel != "" {
		return p.OriginalModel
	}
	return p.ActualModel
}

// RequestTransform rewrites one client request body into the upstream protocol shape.
type RequestTransform func(model string, rawJSON []byte, stream bool) ([]byte, error)

// ResponseStreamTransform rewrites one upstream streaming event into client-facing chunks.
type ResponseStreamTransform func(ctx context.Context, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) ([][]byte, error)

// ResponseNonStreamTransform rewrites one upstream non-stream response into the client-facing shape.
type ResponseNonStreamTransform func(ctx context.Context, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte) ([]byte, error)
