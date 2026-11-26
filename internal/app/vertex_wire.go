package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
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
	modelName, action, hasAction := strings.CutLast(modelAction, ":")
	return hasAction && modelName != "" && (action == "generateContent" || action == "streamGenerateContent" || action == "countTokens")
}

/**
 * @brief 匹配明确指出 topP 固定范围的完整错误文本。
 */
var vertexTopPRange = regexp.MustCompile(`(?i)^(?:generation_?config\.)?top_?p must be (?:between 0(?:\.0+)? and 1(?:\.0+)?|in (?:the range |range )?\[0(?:\.0+)?, *1(?:\.0+)?\])\.?$`)

/**
 * @brief 确认 Vertex 400 响应明确指出客户端给定的非法 topP。
 * @param client 原始客户端协议。
 * @param original 应用渠道请求规则前的客户端正文。
 * @param body Vertex HTTP 错误正文或 SSE 错误事件 JSON。
 * @return 原始 topP 超出 0–1 且错误只指向该字段时返回 true。
 */
func isVertexTopPRangeError(client protocol.Protocol, original, body []byte) bool {
	var payload struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				Type       string `json:"@type"`
				Violations []struct {
					Field string `json:"field"`
				} `json:"fieldViolations"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Error.Code != http.StatusBadRequest || payload.Error.Status != "INVALID_ARGUMENT" {
		return false
	}
	if len(payload.Error.Details) == 0 {
		return vertexTopPRange.MatchString(strings.TrimSpace(payload.Error.Message)) && vertexClientTopPOutOfRange(client, original)
	}
	hasTopPViolation := false
	for _, detail := range payload.Error.Details {
		if detail.Type == "type.googleapis.com/google.rpc.ErrorInfo" {
			continue
		}
		if detail.Type != "type.googleapis.com/google.rpc.BadRequest" || len(detail.Violations) == 0 {
			return false
		}
		for _, violation := range detail.Violations {
			switch violation.Field {
			case "generationConfig.topP", "generationConfig.top_p", "generation_config.topP", "generation_config.top_p":
			default:
				return false
			}
		}
		hasTopPViolation = true
	}
	return hasTopPViolation && vertexClientTopPOutOfRange(client, original)
}

/**
 * @brief 判断请求方显式声明的 topP 是否超出 Vertex 的 0–1 范围。
 * @param client 原始客户端协议。
 * @param body 应用渠道请求规则前的客户端正文。
 * @return 至少有一个数值且所有同义字段都超出范围时返回 true。
 */
func vertexClientTopPOutOfRange(client protocol.Protocol, body []byte) bool {
	if client != protocol.Gemini {
		var request struct {
			TopP *float64 `json:"top_p"`
		}
		return json.Unmarshal(body, &request) == nil && request.TopP != nil && (*request.TopP < 0 || *request.TopP > 1)
	}
	type generationConfig struct {
		TopPCamel *float64 `json:"topP"`
		TopPSnake *float64 `json:"top_p"`
	}
	var request struct {
		ConfigCamel generationConfig `json:"generationConfig"`
		ConfigSnake generationConfig `json:"generation_config"`
	}
	if json.Unmarshal(body, &request) != nil {
		return false
	}
	found := false
	for _, topP := range []*float64{request.ConfigCamel.TopPCamel, request.ConfigCamel.TopPSnake, request.ConfigSnake.TopPCamel, request.ConfigSnake.TopPSnake} {
		if topP == nil {
			continue
		}
		if *topP >= 0 && *topP <= 1 {
			return false
		}
		found = true
	}
	return found
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
