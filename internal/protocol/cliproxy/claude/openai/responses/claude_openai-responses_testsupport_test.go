package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func claudeAssistantBlockTypes(t *testing.T, claudeReq []byte) []string {
	t.Helper()
	var kinds []string
	gjson.GetBytes(claudeReq, "messages").ForEach(func(_, message gjson.Result) bool {
		if message.Get("role").String() != "assistant" {
			return true
		}
		kinds = kinds[:0]
		message.Get("content").ForEach(func(_, block gjson.Result) bool {
			kinds = append(kinds, block.Get("type").String())
			return true
		})
		return true
	})
	return kinds
}

func responsesRequestFromItems(items ...string) []byte {
	raw := `{"model":"claude-test","input":[`
	for i, item := range items {
		if i > 0 {
			raw += ","
		}
		raw += item
	}
	return []byte(raw + `]}`)
}

func mustTestSignature(t *testing.T) string {
	t.Helper()
	raw, _ := testClaudeResponsesThinkingSignature(t)
	return raw
}
