package common

import "github.com/tidwall/gjson"

// IsGeminiThoughtPart reports whether a Gemini part contains hidden model thought.
func IsGeminiThoughtPart(part gjson.Result) bool {
	return part.Get("thought").Bool()
}

// GeminiMessageRole maps Gemini content roles to roles accepted by the other
// supported wire protocols. Missing roles are user content, never empty output.
func GeminiMessageRole(role string) string {
	switch role {
	case "model":
		return "assistant"
	case "", "function", "tool":
		return "user"
	default:
		return role
	}
}
