package util

import "regexp"

var functionNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_.:-]`)

// SanitizeFunctionName converts a tool name to Gemini's accepted shape.
func SanitizeFunctionName(name string) string {
	if name == "" {
		return ""
	}
	sanitized := functionNameSanitizer.ReplaceAllString(name, "_")
	if len(sanitized) == 0 {
		return "_"
	}
	first := sanitized[0]
	if (first < 'a' || first > 'z') && (first < 'A' || first > 'Z') && first != '_' {
		if len(sanitized) >= 64 {
			sanitized = sanitized[:63]
		}
		sanitized = "_" + sanitized
	}
	if len(sanitized) > 64 {
		sanitized = sanitized[:64]
	}
	return sanitized
}
