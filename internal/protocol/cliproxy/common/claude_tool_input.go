package common

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// ClaudeToolInput joins Gemini function-call argument objects into one Claude
// input stream. The zero value represents a tool block with no arguments yet.
type ClaudeToolInput struct {
	emitted      bool
	openObject   bool
	pendingEmpty bool
}

// Append returns the next input_json_delta fragment for a cleaned args value.
// An empty result means the value did not add input to the block.
func (input *ClaudeToolInput) Append(args string, parameterless bool) string {
	if parameterless {
		if input.emitted {
			return ""
		}
		input.emitted = true
		return "{}"
	}

	trimmed := strings.TrimSpace(args)
	body := ""
	if ojson.Parse(trimmed).IsObject() {
		body = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "{"), "}"))
	}
	if body == "" {
		if !input.emitted {
			input.pendingEmpty = true
		}
		return ""
	}

	input.emitted = true
	input.pendingEmpty = false
	if input.openObject {
		return "," + body
	}
	input.openObject = true
	return "{" + body
}

// Finish returns the last fragment and resets the input state for the next tool block.
func (input *ClaudeToolInput) Finish() string {
	var fragment string
	if input.openObject {
		fragment = "}"
	} else if input.pendingEmpty {
		fragment = "{}"
	}
	*input = ClaudeToolInput{}
	return fragment
}
