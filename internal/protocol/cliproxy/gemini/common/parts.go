package common

import "github.com/tidwall/gjson"

// ThoughtSignature returns the first nonempty signature across Gemini's field aliases.
func ThoughtSignature(part gjson.Result) string {
	for _, field := range [...]string{"thoughtSignature", "thought_signature"} {
		if signature := part.Get(field).Str; signature != "" {
			return signature
		}
	}
	return ""
}

// HasUnsupportedPartPayload keeps omitted payloads from being mistaken for
// detached signatures belonging to a neighboring supported part.
func HasUnsupportedPartPayload(part gjson.Result, supportedFields ...string) bool {
	unsupported := false
	part.ForEach(func(key, _ gjson.Result) bool {
		name := key.String()
		switch name {
		case "text", "functionCall", "function_call", "functionResponse", "function_response",
			"inlineData", "inline_data", "fileData", "file_data",
			"executableCode", "executable_code", "codeExecutionResult", "code_execution_result",
			"toolCall", "tool_call", "toolResponse", "tool_response",
			"audioTranscription", "audio_transcription":
			// Only semantic payload fields can create an unsupported boundary.
		default:
			// Metadata such as partMetadata must not hide supported content.
			return true
		}
		for _, field := range supportedFields {
			if name == field {
				return true
			}
		}
		unsupported = true
		return false
	})
	return unsupported
}
