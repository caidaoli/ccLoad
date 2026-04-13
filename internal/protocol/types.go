package protocol

import (
	"context"
	"fmt"
	"slices"
	"strings"
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

// RequestFamily identifies the client request surface that is being transformed.
type RequestFamily string

// RequestFamily values enumerate the supported client request surfaces.
const (
	RequestFamilyUnknown         RequestFamily = ""
	RequestFamilyChatCompletions RequestFamily = "chat_completions"
	RequestFamilyResponses       RequestFamily = "responses"
	RequestFamilyMessages        RequestFamily = "messages"
	RequestFamilyGenerateContent RequestFamily = "generate_content"
	RequestFamilyCompletions     RequestFamily = "completions"
	RequestFamilyEmbeddings      RequestFamily = "embeddings"
	RequestFamilyImages          RequestFamily = "images"
)

// TransformPlan captures the chosen transform metadata for one proxy attempt.
type TransformPlan struct {
	ClientProtocol   Protocol
	UpstreamProtocol Protocol
	RequestFamily    RequestFamily
	OriginalPath     string
	UpstreamPath     string
	OriginalBody     []byte
	TranslatedBody   []byte
	OriginalModel    string
	ActualModel      string
	Streaming        bool
	NeedsTransform   bool
}

var supportedTransformFamiliesByClientAndUpstream = map[Protocol]map[Protocol][]RequestFamily{
	OpenAI: {
		Gemini:    {RequestFamilyChatCompletions},
		Anthropic: {RequestFamilyChatCompletions},
		Codex:     {RequestFamilyChatCompletions},
	},
	Anthropic: {
		OpenAI: {RequestFamilyMessages},
		Gemini: {RequestFamilyMessages},
		Codex:  {RequestFamilyMessages},
	},
	Codex: {
		OpenAI:    {RequestFamilyResponses},
		Gemini:    {RequestFamilyResponses},
		Anthropic: {RequestFamilyResponses},
	},
	Gemini: {
		OpenAI:    {RequestFamilyGenerateContent},
		Anthropic: {RequestFamilyGenerateContent},
		Codex:     {RequestFamilyGenerateContent},
	},
}

// SupportedClientProtocolsForUpstream returns the documented client-facing protocols
// that can be translated into the given upstream protocol.
func SupportedClientProtocolsForUpstream(upstream Protocol) []Protocol {
	supported := make([]Protocol, 0, len(supportedTransformFamiliesByClientAndUpstream))
	for client, upstreams := range supportedTransformFamiliesByClientAndUpstream {
		if len(upstreams[upstream]) == 0 {
			continue
		}
		supported = append(supported, client)
	}
	if len(supported) == 0 {
		return nil
	}
	slices.Sort(supported)
	return supported
}

// SupportsTransform reports whether the runtime has a documented transform path for
// the given client/upstream protocol pair.
func SupportsTransform(client, upstream Protocol) bool {
	return len(supportedTransformFamiliesByClientAndUpstream[client][upstream]) > 0
}

// SupportsTransformFamily reports whether the runtime has a documented transform path for
// the given client/upstream protocol pair on the current request family.
func SupportsTransformFamily(client, upstream Protocol, family RequestFamily) bool {
	for _, supportedFamily := range supportedTransformFamiliesByClientAndUpstream[client][upstream] {
		if supportedFamily == family {
			return true
		}
	}
	return false
}

// DetectRequestFamily infers the client request surface from the request path.
func DetectRequestFamily(path string) RequestFamily {
	path = strings.TrimSpace(path)
	switch {
	case strings.HasPrefix(path, "/v1/chat/completions"):
		return RequestFamilyChatCompletions
	case strings.HasPrefix(path, "/v1/responses"):
		return RequestFamilyResponses
	case strings.HasPrefix(path, "/v1/messages"):
		return RequestFamilyMessages
	case strings.Contains(path, ":generateContent"), strings.Contains(path, ":streamGenerateContent"):
		return RequestFamilyGenerateContent
	case strings.HasPrefix(path, "/v1/completions"):
		return RequestFamilyCompletions
	case strings.HasPrefix(path, "/v1/embeddings"):
		return RequestFamilyEmbeddings
	case strings.HasPrefix(path, "/v1/images/"):
		return RequestFamilyImages
	default:
		return RequestFamilyUnknown
	}
}

// BuildTransformPlan turns request metadata into a concrete runtime plan that can
// travel through request preparation, forwarding, and response translation.
func BuildTransformPlan(client, upstream Protocol, originalPath, upstreamPath string, originalBody, preparedBody []byte, originalModel, actualModel string, streaming bool) (TransformPlan, error) {
	plan := TransformPlan{
		ClientProtocol:   client,
		UpstreamProtocol: upstream,
		RequestFamily:    DetectRequestFamily(originalPath),
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
	if !SupportsTransformFamily(client, upstream, plan.RequestFamily) {
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
