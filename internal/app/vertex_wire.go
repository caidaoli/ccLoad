package app

import (
	"net/http"
	"net/url"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	cliproxycommon "ccLoad/internal/protocol/cliproxy/common"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// vertexPublisherPathSuffix marks a channel URL that already points at a Vertex
// publisher endpoint: a service account project, an express API key, or a relay.
const vertexPublisherPathSuffix = "/publishers/google"

// isGeminiAPIURL identifies the Google Gemini API, whose signatures differ
// from Vertex. An arbitrary relay URL does not establish its backend dialect.
func isGeminiAPIURL(target *url.URL) bool {
	return target != nil && strings.EqualFold(target.Hostname(), "generativelanguage.googleapis.com")
}

// usesVertexWire identifies Gemini requests that need Vertex body adaptation.
// Exact URLs still need that adaptation; callers skip only their URL remapping.
func usesVertexWire(cfg *model.Config, upstreamProtocol protocol.Protocol, baseURL string) bool {
	if upstreamProtocol != protocol.Gemini {
		return false
	}
	if cfg.UsesVertexOAuth() {
		return true
	}
	parsed, err := url.Parse(strings.TrimSpace(model.StripExactUpstreamURLMarker(baseURL)))
	if err != nil {
		return false
	}
	path := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(path, vertexPublisherPathSuffix) {
		return true
	}
	if !model.HasExactUpstreamURLMarker(baseURL) {
		return false
	}
	// Exact API-key URLs can name the complete publisher model endpoint.
	// They still need Vertex body/signature policy even though no URL is built.
	_, modelAction, publisherModel := strings.Cut(path, vertexPublisherPathSuffix+"/models/")
	if !publisherModel || strings.Contains(modelAction, "/") {
		return false
	}
	modelName, action, hasAction := strings.Cut(modelAction, ":")
	return hasAction && modelName != "" && (action == "generateContent" || action == "streamGenerateContent" || action == "countTokens")
}

// buildVertexPublisherURL maps a Gemini API path onto a Vertex publisher base:
// /v1beta/models/{model}:{action} becomes {base}/models/{model}:{action}.
// Without alt=sse Vertex streams a JSON array. ccLoad-built streams (translated
// requests, channel tests) need SSE; a Gemini passthrough keeps the caller's query.
func buildVertexPublisherURL(baseURL, requestPath, rawQuery string, defaultSSE bool) string {
	path := requestPath
	for _, version := range []string{"/v1beta/", "/v1alpha/", "/v1/"} {
		if rest, ok := strings.CutPrefix(path, version+"models/"); ok {
			path = "/models/" + rest
			break
		}
	}
	if defaultSSE && strings.HasSuffix(path, ":streamGenerateContent") {
		query, _ := url.ParseQuery(rawQuery)
		if query.Get("alt") == "" {
			query.Set("alt", "sse")
			rawQuery = query.Encode()
		}
	}
	return buildUpstreamURL(baseURL, path, rawQuery)
}

// injectVertexAuthHeaders authenticates with the service account access token.
// A Gemini API key header must not reach Vertex next to it.
func injectVertexAuthHeaders(req *http.Request, accessToken string) {
	req.Header.Del("x-goog-api-key")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
}

// prepareVertexRequestBody adapts a Gemini body to what Vertex requires but the
// Gemini API tolerates. Only Responses (Codex) clients carry tool call IDs.
func prepareVertexRequestBody(body []byte, responsesClient bool) []byte {
	body = normalizeVertexContentRoles(body)
	if responsesClient {
		body = stripVertexResponsesToolCallIDs(body)
	}
	return body
}

// normalizeVertexContentRoles fills a missing or invalid content role: the
// Gemini API defaults it to user, Vertex rejects it ("Please use a valid role:
// user, model."). Same rule as CLIProxyAPI ConvertGeminiRequestToGemini: a
// functionResponse turn is user, any other turn alternates starting from user.
func normalizeVertexContentRoles(body []byte) []byte {
	contents := gjson.GetBytes(body, "contents")
	if !contents.IsArray() {
		return body
	}
	type roleFix struct{ path, role string }
	var fixes []roleFix
	previous := ""
	contents.ForEach(func(index, content gjson.Result) bool {
		role := content.Get("role").String()
		if role != "user" && role != "model" {
			role = "user"
			if previous == "user" && !cliproxycommon.ContentHasGeminiFunctionResponse([]byte(content.Raw)) {
				role = "model"
			}
			fixes = append(fixes, roleFix{path: "contents." + index.String() + ".role", role: role})
		}
		previous = role
		return true
	})
	for _, fix := range fixes {
		if updated, err := sjson.SetBytes(body, fix.path, fix.role); err == nil {
			body = updated
		}
	}
	return body
}

// stripVertexResponsesToolCallIDs removes the OpenAI Responses call IDs the
// Responses-to-Gemini translator keeps on functionCall/functionResponse parts;
// Vertex rejects them (CLIProxyAPI StripVertexOpenAIResponsesToolCallIDs).
func stripVertexResponsesToolCallIDs(body []byte) []byte {
	contents := gjson.GetBytes(body, "contents")
	if !contents.IsArray() {
		return body
	}
	var paths []string
	contents.ForEach(func(contentIndex, content gjson.Result) bool {
		content.Get("parts").ForEach(func(partIndex, part gjson.Result) bool {
			for _, field := range []string{"functionCall", "functionResponse"} {
				if part.Get(field + ".id").Exists() {
					paths = append(paths, "contents."+contentIndex.String()+".parts."+partIndex.String()+"."+field+".id")
				}
			}
			return true
		})
		return true
	})
	for _, path := range paths {
		if updated, err := sjson.DeleteBytes(body, path); err == nil {
			body = updated
		}
	}
	return body
}
