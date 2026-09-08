package app

import (
	"net/http"
	neturl "net/url"
	"sort"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
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
	return path == opencodePath || strings.HasPrefix(path, opencodePath+"/")
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
