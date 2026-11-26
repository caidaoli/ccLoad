package protocol

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Protocol identifies a client-facing or upstream request/response protocol.
type Protocol string

// SystemOne is the native System One decision protocol. It is excluded from
// chat conversions: its client endpoint only passes through to System One channels.
const SystemOne Protocol = "system_one"

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

// AllProtocols returns every supported protocol in canonical display order.
// This is the single enumeration source: adding a protocol means updating
// this list (plus the transform table below) and nothing else.
func AllProtocols() []Protocol {
	return []Protocol{Anthropic, Codex, OpenAI, Gemini}
}

// IsValid reports whether p is one of the supported protocols.
func IsValid(p Protocol) bool {
	switch p {
	case Anthropic, Codex, OpenAI, Gemini:
		return true
	default:
		return false
	}
}

// RequestFamily identifies the client request surface that is being transformed.
type RequestFamily string

// RequestFamily values enumerate the supported client request surfaces.
const (
	RequestFamilyUnknown         RequestFamily = ""
	RequestFamilyChatCompletions RequestFamily = "chat_completions"
	RequestFamilyResponses       RequestFamily = "responses"
	RequestFamilyMessages        RequestFamily = "messages"
	RequestFamilyCountTokens     RequestFamily = "count_tokens"
	RequestFamilyGenerateContent RequestFamily = "generate_content"
	RequestFamilyCompletions     RequestFamily = "completions"
	RequestFamilyEmbeddings      RequestFamily = "embeddings"
	RequestFamilyImages          RequestFamily = "images"
	// RequestFamilyAlphaSearch is the Codex /v1/alpha/search surface.
	// It is native passthrough only — not a protocol-transform family.
	RequestFamilyAlphaSearch RequestFamily = "alpha_search"
	// RequestFamilySystemOne is the native /v1/systemone decision surface.
	RequestFamilySystemOne RequestFamily = "system_one"
)

/**
 * @brief 记录一次代理尝试选定的模型、协议和请求正文，供请求及响应转换共享。
 * @note 转发、写响应和调试捕获可跨 goroutine 读取此计划。
 * @details TODO(perf)：原始和转换后的正文会在长流期间同时驻留。分阶段释放前需要先收敛各阶段读点。
 */
type TransformPlan struct {
	ClientProtocol   Protocol
	UpstreamProtocol Protocol
	RequestFamily    RequestFamily
	OriginalPath     string
	UpstreamPath     string
	OriginalBody     []byte
	TranslatedBody   []byte
	OriginalModel    string
	RequestedModel   string ///< Request model before routing, including an explicit thinking suffix.
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

// SupportsTransform reports whether the runtime has a documented transform path for
// the given client/upstream protocol pair.
func SupportsTransform(client, upstream Protocol) bool {
	return len(supportedTransformFamiliesByClientAndUpstream[client][upstream]) > 0
}

// SupportsTransformFamily reports whether the runtime has a documented transform path for
// the given client/upstream protocol pair on the current request family.
func SupportsTransformFamily(client, upstream Protocol, family RequestFamily) bool {
	return slices.Contains(supportedTransformFamiliesByClientAndUpstream[client][upstream], family)
}

func matchesCanonicalEndpoint(path, endpoint string) bool {
	_, ok := replaceCanonicalEndpoint(path, endpoint, endpoint)
	return ok
}

func replaceCanonicalEndpoint(path, from, to string) (string, bool) {
	idx := strings.Index(path, from)
	if idx < 0 {
		return "", false
	}
	after := idx + len(from)
	if after != len(path) && path[after] != '?' && path[after] != '/' {
		return "", false
	}
	return path[:idx] + to + path[after:], true
}

// RewriteResponsesPathToAlphaSearch maps a Codex Responses endpoint to the
// sibling alpha/search path. ok is false when path is not a Responses endpoint.
func RewriteResponsesPathToAlphaSearch(path string) (string, bool) {
	path = strings.TrimSpace(path)
	replacements := [][2]string{
		{"/backend-api/codex/responses", "/backend-api/codex/alpha/search"},
		{"/v1/codex/responses", "/v1/codex/alpha/search"},
		{"/v1/responses", "/v1/alpha/search"},
	}
	for _, pair := range replacements {
		if rewritten, ok := replaceCanonicalEndpoint(path, pair[0], pair[1]); ok {
			return rewritten, true
		}
	}
	return "", false
}

// DetectRequestFamily infers the client request surface from the request path.
func DetectRequestFamily(path string) RequestFamily {
	path = strings.TrimSpace(path)
	switch {
	case matchesCanonicalEndpoint(path, "/v1/chat/completions"):
		return RequestFamilyChatCompletions
	case matchesCanonicalEndpoint(path, "/v1/responses"),
		matchesCanonicalEndpoint(path, "/v1/codex/responses"),
		matchesCanonicalEndpoint(path, "/backend-api/codex/responses"):
		return RequestFamilyResponses
	case matchesCanonicalEndpoint(path, "/v1/messages/count_tokens"):
		return RequestFamilyCountTokens
	case matchesCanonicalEndpoint(path, "/v1/messages"):
		return RequestFamilyMessages
	case matchesCanonicalEndpoint(path, "/v1/alpha/search"),
		matchesCanonicalEndpoint(path, "/v1/codex/alpha/search"),
		matchesCanonicalEndpoint(path, "/backend-api/codex/alpha/search"):
		return RequestFamilyAlphaSearch
	case strings.Contains(path, ":generateContent"), strings.Contains(path, ":streamGenerateContent"),
		strings.Contains(path, ":countTokens"):
		return RequestFamilyGenerateContent
	case matchesCanonicalEndpoint(path, "/v1/completions"):
		return RequestFamilyCompletions
	case matchesCanonicalEndpoint(path, "/v1/embeddings"):
		return RequestFamilyEmbeddings
	case strings.Contains(path, "/v1/images/"):
		return RequestFamilyImages
	case matchesCanonicalEndpoint(path, "/v1/systemone"):
		return RequestFamilySystemOne
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
