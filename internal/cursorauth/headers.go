package cursorauth

import (
	"net/http"
	"regexp"
	"strings"
)

var printableHeaderValue = regexp.MustCompile(`^[\x20-\x7e]+$`)

// SourceHeaders returns the CLI identity headers cursor-agent sends on
// control-plane RPCs.
func SourceHeaders() [][2]string {
	return [][2]string{
		{"User-Agent", "cursor-agent/" + ClientVersion},
		{"x-cursor-client-version", "cli-" + ClientVersion},
		{"x-cursor-client-type", ClientType},
		{"x-ghost-mode", GhostMode},
	}
}

// ApplySourceHeaders stamps the CLI identity on a control-plane request.
func ApplySourceHeaders(header http.Header) {
	if header == nil {
		return
	}
	for _, entry := range SourceHeaders() {
		if value := headerValue(entry[1]); value != "" {
			header.Set(entry[0], value)
		}
	}
}

func headerValue(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" || !printableHeaderValue.MatchString(value) {
		return ""
	}
	return value
}
