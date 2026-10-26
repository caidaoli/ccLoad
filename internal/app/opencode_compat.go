package app

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"net/http"
	neturl "net/url"
	"regexp"
	"sort"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
)

const (
	opencodeSessionHeader = "x-opencode-session"

	// OpenCode Go's documented OpenAI-compatible base endpoint. The header is
	// enabled only for this exact HTTPS host and path boundary.
	opencodeHost = "opencode.ai"
	opencodePath = "/zen/go"
)

func isOpenCodeChannel(cfg *model.Config) bool {
	if cfg == nil {
		return false
	}
	for _, endpoint := range cfg.URLs {
		if isOpenCodeEndpoint(endpoint.URL) {
			return true
		}
	}
	return false
}

// isOpenCodeEndpoint identifies the documented OpenCode Go endpoint
// from a configured channel URL. URL matching is structural: the host is exact
// and the path must be /zen/go or a descendant such as /zen/go/v1/responses.
func isOpenCodeEndpoint(raw string) bool {
	return isOpenCodePath(raw, opencodePath)
}

// Zen and Go share the inference wire; quota and muse compatibility remain
// Go-only.
func isOpenCodeOfficialEndpoint(raw string) bool {
	return isOpenCodePath(raw, "/zen")
}

func isOpenCodeAPIKeyEndpoint(cfg *model.Config, raw string) bool {
	return cfg != nil && model.NormalizeAuthType(cfg.AuthType) == model.AuthTypeAPIKey && isOpenCodeOfficialEndpoint(raw)
}

// OpenCode exposes different protocols for individual models. A rejected
// protocol is capability evidence, not a malformed prompt or model outage.
func isOpenCodeModelProtocolUnsupported(cfg *model.Config, target string, status int, body []byte) bool {
	if status != http.StatusBadRequest || !isOpenCodeAPIKeyEndpoint(cfg, target) {
		return false
	}
	var payload struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	return sonic.Unmarshal(body, &payload) == nil && payload.Error.Type == "ModelProtocolUnsupported"
}

func isOpenCodePath(raw, prefix string) bool {
	raw = strings.TrimSpace(model.StripExactUpstreamURLMarker(raw))
	if raw == "" {
		return false
	}
	parsed, err := neturl.Parse(raw)
	if err != nil || parsed == nil {
		return false
	}
	if !strings.EqualFold(parsed.Scheme, "https") ||
		!strings.EqualFold(parsed.Hostname(), opencodeHost) ||
		parsed.Port() != "" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = parsed.Path
	}
	// Reject dot segments before applying the prefix check. Otherwise a URL
	// such as /zen/go/../other would satisfy the textual boundary while
	// resolving to a different upstream resource.
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// ensureOpenCodeSessionHeader forwards one stable session identifier
// to OpenCode. Existing explicit values win; otherwise use a stable source
// header or execution identity, with a request-scoped UUID as the final
// fallback. Unrelated headers remain subject to normal forwarding rules.
func ensureOpenCodeSessionHeader(dst, src http.Header, executionIdentity string) {
	if dst == nil {
		return
	}
	for _, name := range []string{
		opencodeSessionHeader,
		"x-opencode-session-id",
		"x-session-id",
		"x-session-affinity",
		"x-litellm-session-id",
		"x-litellm-trace-id",
		"Session_id",
		"Session-Id",
		"x-client-request-id",
	} {
		if value := opencodeHeaderValue(dst, name); value != "" {
			dst.Set(opencodeSessionHeader, value)
			return
		}
		if value := opencodeHeaderValue(src, name); value != "" {
			dst.Set(opencodeSessionHeader, value)
			return
		}
	}
	for _, headers := range []http.Header{dst, src} {
		names := make([]string, 0, len(headers))
		for name := range headers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			values := headers[name]
			lower := strings.ToLower(strings.TrimSpace(name))
			if !strings.HasPrefix(lower, "x-") || !strings.HasSuffix(lower, "-session-id") || lower == "x-parent-session-id" {
				continue
			}
			for _, value := range values {
				if value = strings.TrimSpace(value); value != "" {
					dst.Set(opencodeSessionHeader, value)
					return
				}
			}
		}
	}
	if value := strings.TrimSpace(executionIdentity); value != "" {
		dst.Set(opencodeSessionHeader, value)
		return
	}
	// OpenCode requires presence even when a client does not expose a session
	// identity. This fallback is request-scoped and never persisted.
	dst.Set(opencodeSessionHeader, util.NewUUIDv4())
}

func opencodeHeaderValue(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	names := make([]string, 0, len(headers))
	for headerName := range headers {
		if !strings.EqualFold(strings.TrimSpace(headerName), name) {
			continue
		}
		names = append(names, headerName)
	}
	sort.Strings(names)
	for _, headerName := range names {
		values := headers[headerName]
		for _, value := range values {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

// OpenCode validates the descending session layout, including the counter bits.
var openCodeSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{9}f[c-f][0-9a-f][0-9A-Za-z]{14}$`)

func openCodeSessionID(signal string) string {
	if openCodeSessionPattern.MatchString(signal) {
		return signal
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	// Invert the 36-bit time signal and encode counter 1 as its complement.
	timeBytes := []byte{^sum[0], ^sum[1], ^sum[2], ^sum[3], (^sum[4] & 0xf0) | 0xf, 0xfe}
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	n := new(big.Int).SetBytes(sum[6:16])
	base, remainder := big.NewInt(62), new(big.Int)
	suffix := make([]byte, 14)
	for i := len(suffix) - 1; i >= 0; i-- {
		n.DivMod(n, base, remainder)
		suffix[i] = alphabet[remainder.Int64()]
	}
	return "ses_" + hex.EncodeToString(timeBytes) + string(suffix)
}

// Only the selected official endpoint gets this wire, never another URL in the channel.
func injectOpenCodeHeaders(req *http.Request, cfg *model.Config, upstream protocol.Protocol, key string, source http.Header, executionIdentity string) {
	if !isOpenCodeAPIKeyEndpoint(cfg, req.URL.String()) {
		return
	}
	switch upstream {
	case protocol.OpenAI, protocol.Codex, protocol.Anthropic, protocol.Gemini:
	default:
		return
	}
	identity := cloneHeaders(req.Header)
	ensureOpenCodeSessionHeader(identity, source, executionIdentity)
	session := openCodeSessionID(identity.Get(opencodeSessionHeader))
	project := opencodeHeaderValue(source, "x-opencode-project")
	if project == "" {
		project = "ccload:default-project"
	}
	projectHash := sha256.Sum256([]byte("opencode-project\x00" + project))
	req.Header = make(http.Header)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "opencode/latest/2.0.29/cli")
	req.Header.Set("X-Opencode-Client", "cli")
	for _, name := range []string{opencodeSessionHeader, "x-opencode-session-id", "x-session-affinity", "X-Session-Id"} {
		req.Header.Set(name, session)
	}
	req.Header.Set("X-Opencode-Project", hex.EncodeToString(projectHash[:20]))
	if parent := opencodeHeaderValue(source, "x-parent-session-id"); parent != "" {
		req.Header.Set("X-Parent-Session-Id", parent)
	}
	switch upstream {
	case protocol.Anthropic:
		req.Header.Set("X-Api-Key", key)
		req.Header.Set("Anthropic-Version", "2023-06-01")
		req.Header.Set("Anthropic-Beta", "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14")
	case protocol.Gemini:
		// Keyed Gemini routes read only x-goog-api-key.
		req.Header.Set("x-goog-api-key", key)
	default:
		req.Header.Set("Authorization", "Bearer "+key)
	}
	applyHeaderRules(req.Header, cfg.HeaderRules())
	// This obsolete correlation header is explicitly rejected by OpenCode.
	deleteRawHeader(req.Header, "x-opencode-request")
}

// big-pickle is the zero-price model without a -free suffix in the official
// OpenCode catalog (models.opencode.ai, 2026-10-15). Never infer free access
// from ccLoad's administrator-overridden billing prices.
func isOpenCodeFreeModel(name string) bool {
	name = strings.ToLower(model.RoutingModelName(name))
	return strings.Contains(name, "free") || name == "big-pickle"
}

// TypeSafe Jev decision models are listed in the OpenAI catalog but answer
// only POST /v1/systemone; every inference protocol rejects them.
func isOpenCodeSystemOneModel(name string) bool {
	return strings.HasPrefix(strings.ToLower(model.RoutingModelName(name)), "jev-")
}

func isOpenCodeFreeRequest(cfg *model.Config, target string, upstream protocol.Protocol, path, name string) bool {
	return isOpenCodeAPIKeyEndpoint(cfg, target) && isOpenCodeInferencePath(upstream, path) && isOpenCodeFreeModel(name)
}

// isOpenCodeInferencePath reports the per-protocol inference routes OpenCode
// serves; Gemini countTokens is not an inference request.
func isOpenCodeInferencePath(upstream protocol.Protocol, path string) bool {
	switch protocol.DetectRequestFamily(path) {
	case protocol.RequestFamilyChatCompletions:
		return upstream == protocol.OpenAI
	case protocol.RequestFamilyResponses:
		return upstream == protocol.Codex
	case protocol.RequestFamilyMessages:
		return upstream == protocol.Anthropic
	case protocol.RequestFamilyGenerateContent:
		return upstream == protocol.Gemini && !strings.Contains(path, ":countTokens")
	default:
		return false
	}
}

// openCodeGeminiURL maps a Gemini API path onto OpenCode's route
// {base}/v1/models/{model}:{action}; OpenCode has no /v1beta prefix. The free
// gate only accepts the SSE stream action, so free requests always stream and
// non-stream callers are served by collectOpenCodeCompletion. ccLoad-built
// streams (translation or free shaping) request SSE explicitly.
func openCodeGeminiURL(baseURL, requestPath, rawQuery string, defaultSSE, free bool) string {
	path := requestPath
	for _, version := range []string{"/v1beta/", "/v1alpha/"} {
		if rest, ok := strings.CutPrefix(path, version+"models/"); ok {
			path = "/v1/models/" + rest
			break
		}
	}
	if head, ok := strings.CutSuffix(path, ":generateContent"); ok && free {
		path = head + ":streamGenerateContent"
	}
	if (defaultSSE || free) && strings.HasSuffix(path, ":streamGenerateContent") {
		query, _ := neturl.ParseQuery(rawQuery)
		if query.Get("alt") == "" {
			query.Set("alt", "sse")
			rawQuery = query.Encode()
		}
	}
	return buildUpstreamURL(baseURL, path, rawQuery)
}
