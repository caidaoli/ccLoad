package common

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
