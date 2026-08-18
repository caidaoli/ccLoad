package zaiauth

import (
	"net/http"
	"regexp"
	"runtime"
	"strings"
)

// printableHeaderValue mirrors ZCode's own header sanitation: only printable
// ASCII reaches the wire.
var printableHeaderValue = regexp.MustCompile(`^[\x20-\x7e]+$`)

// SourceHeaders returns ZCode's client identity headers in the exact casing the
// official client sends. The proxy path replays them verbatim, so the order and
// spelling are part of the contract rather than cosmetic.
func SourceHeaders() [][2]string {
	headers := [][2]string{
		{"User-Agent", "ZCode/" + AppVersion},
		{"X-ZCode-App-Version", AppVersion},
		{"X-Title", SourceTitle},
		{"X-ZCode-Agent", AgentHeaderValue},
		{"HTTP-Referer", RefererValue},
	}
	return append(headers, PlatformHeaders()...)
}

// PlatformHeaders describes the host ccLoad runs on, the same way ZCode
// describes the machine it runs on.
func PlatformHeaders() [][2]string {
	headers := make([][2]string, 0, 3)
	if platform := headerValue(runtime.GOOS); platform != "" {
		if arch := headerValue(runtime.GOARCH); arch != "" {
			headers = append(headers, [2]string{"X-Platform", platform + "-" + arch})
		}
	}
	headers = append(headers, [2]string{"X-Os-Category", OSCategory()})
	return headers
}

// OSCategory maps the Go runtime to ZCode's operating system vocabulary.
func OSCategory() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	default:
		return "linux"
	}
}

// ApplySourceHeaders stamps the ZCode client identity on a control-plane request.
func ApplySourceHeaders(header http.Header) {
	if header == nil {
		return
	}
	for _, entry := range SourceHeaders() {
		header.Set(entry[0], entry[1])
	}
}

func headerValue(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" || !printableHeaderValue.MatchString(value) {
		return ""
	}
	return value
}
