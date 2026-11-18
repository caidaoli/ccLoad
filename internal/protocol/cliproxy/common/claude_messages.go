package common

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ClaudeMessageAccumulator groups consecutive Claude messages by role.
type ClaudeMessageAccumulator struct {
	messages     [][]byte
	role         string
	content      [][]byte
	toolUseParts [][]byte
}

// NewClaudeMessageAccumulator creates an accumulator sized for the expected messages.
func NewClaudeMessageAccumulator(capacity int) *ClaudeMessageAccumulator {
	return &ClaudeMessageAccumulator{
		messages: NewRawArrayItems(int64(capacity)),
	}
}

// Append adds one Claude-shaped message to the current role turn.
func (a *ClaudeMessageAccumulator) Append(message []byte) {
	if len(message) == 0 {
		return
	}
	root := gjson.ParseBytes(message)
	role := root.Get("role").String()
	if role != "user" && role != "assistant" {
		return
	}
	parts := claudeMessageContentParts(root.Get("content"))
	if len(parts) == 0 {
		return
	}
	if a.role != "" && a.role != role {
		a.Flush()
	}
	a.role = role
	for _, part := range parts {
		if role == "assistant" && gjson.GetBytes(part, "type").String() == "tool_use" {
			a.toolUseParts = append(a.toolUseParts, part)
			continue
		}
		a.content = append(a.content, part)
	}
}

// Flush closes the current role turn while keeping accumulated messages.
func (a *ClaudeMessageAccumulator) Flush() {
	if a.role == "" {
		return
	}
	parts := a.content
	if len(a.toolUseParts) > 0 {
		combined := make([][]byte, 0, len(a.content)+len(a.toolUseParts))
		combined = append(combined, a.content...)
		combined = append(combined, a.toolUseParts...)
		parts = combined
	}
	if len(parts) > 0 {
		message := []byte(`{"role":"","content":[]}`)
		message, _ = sjson.SetBytes(message, "role", a.role)
		message, _ = sjson.SetRawBytes(message, "content", JoinRawArray(parts))
		a.messages = append(a.messages, message)
	}
	a.role = ""
	a.content = nil
	a.toolUseParts = nil
}

// Messages flushes the final turn and returns all accumulated messages.
func (a *ClaudeMessageAccumulator) Messages() [][]byte {
	a.Flush()
	return a.messages
}

// AlignClaudeToolResults orders tool_result blocks by the preceding tool_use IDs,
// preserving non-result content blocks at their existing indexes. If a complete
// one-to-one match is unavailable, the original content is returned.
func AlignClaudeToolResults(content gjson.Result, toolUseIDs []string) gjson.Result {
	if !content.IsArray() || len(toolUseIDs) == 0 {
		return content
	}

	parts := content.Array()
	toolResults := make([]gjson.Result, 0, len(toolUseIDs))
	toolResultIndices := make([]int, 0, len(toolUseIDs))
	for i, part := range parts {
		if part.Get("type").String() == "tool_result" {
			toolResults = append(toolResults, part)
			toolResultIndices = append(toolResultIndices, i)
		}
	}
	if len(toolResults) != len(toolUseIDs) {
		return content
	}

	reorderedResults := make([]gjson.Result, 0, len(toolUseIDs))
	used := make([]bool, len(toolResults))
	for _, toolUseID := range toolUseIDs {
		matched := -1
		for resultIndex, toolResult := range toolResults {
			if !used[resultIndex] && toolUseID != "" && toolResult.Get("tool_use_id").String() == toolUseID {
				matched = resultIndex
				break
			}
		}
		if matched < 0 {
			return content
		}
		used[matched] = true
		reorderedResults = append(reorderedResults, toolResults[matched])
	}

	ordered := make([][]byte, len(parts))
	for i, part := range parts {
		ordered[i] = []byte(part.Raw)
	}
	for i, slotIndex := range toolResultIndices {
		ordered[slotIndex] = []byte(reorderedResults[i].Raw)
	}
	return gjson.ParseBytes(JoinRawArray(ordered))
}

func claudeMessageContentParts(content gjson.Result) [][]byte {
	if !content.Exists() || content.Type == gjson.Null {
		return nil
	}
	if content.Type == gjson.String {
		if content.String() == "" {
			return nil
		}
		part := []byte(`{"type":"text","text":""}`)
		part, _ = sjson.SetBytes(part, "text", content.String())
		return [][]byte{part}
	}
	if !content.IsArray() {
		return nil
	}
	parts := make([][]byte, 0, len(content.Array()))
	content.ForEach(func(_, part gjson.Result) bool {
		if part.IsObject() {
			parts = append(parts, []byte(part.Raw))
		}
		return true
	})
	return parts
}

// ClaudeDocumentHeader returns a formatted metadata header for optional title
// and context fields on a Claude document block, or "" when neither is set.
func ClaudeDocumentHeader(part gjson.Result) string {
	title := strings.TrimSpace(part.Get("title").String())
	contextText := strings.TrimSpace(part.Get("context").String())
	if title == "" && contextText == "" {
		return ""
	}
	var b strings.Builder
	if title != "" {
		b.WriteString("Document: ")
		b.WriteString(title)
	}
	if contextText != "" {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("Context: ")
		b.WriteString(contextText)
	}
	return b.String()
}

// ClaudeDocumentTextBlock formats a Claude plain-text document block
// (source.type == "text") into text preserving optional title and context.
// Returns ("", false) when the source is not a text source or carries no text.
func ClaudeDocumentTextBlock(part gjson.Result) (string, bool) {
	source := part.Get("source")
	if source.Get("type").String() != "text" {
		return "", false
	}
	data := source.Get("data").String()
	if strings.TrimSpace(data) == "" {
		return "", false
	}
	header := ClaudeDocumentHeader(part)
	if header == "" {
		return data, true
	}
	return header + "\n" + data, true
}

// ExpandClaudeContentDocument walks a Claude document block with
// source.type == "content", converting nested text and image blocks. An
// optional title/context header is emitted before the first converted item only
// if at least one item succeeds. Returns true if any item was emitted.
func ExpandClaudeContentDocument(
	part gjson.Result,
	emitText func(string),
	convertImage func(gjson.Result) ([]byte, bool),
	emitImage func([]byte),
	onImageDrop func(),
) bool {
	source := part.Get("source")
	if source.Get("type").String() != "content" {
		return false
	}
	content := source.Get("content")
	if !content.Exists() {
		return false
	}
	header := ClaudeDocumentHeader(part)
	headerEmitted := false
	emitHeader := func() {
		if header != "" && !headerEmitted {
			emitText(header)
			headerEmitted = true
		}
	}
	emitted := false
	if content.Type == gjson.String {
		if text := content.String(); strings.TrimSpace(text) != "" {
			emitHeader()
			emitText(text)
			emitted = true
		}
		return emitted
	}
	if !content.IsArray() {
		return false
	}
	content.ForEach(func(_, item gjson.Result) bool {
		if item.Type == gjson.String {
			if text := item.String(); strings.TrimSpace(text) != "" {
				emitHeader()
				emitText(text)
				emitted = true
			}
			return true
		}
		if !item.IsObject() {
			return true
		}
		switch item.Get("type").String() {
		case "text":
			if text := item.Get("text").String(); strings.TrimSpace(text) != "" {
				emitHeader()
				emitText(text)
				emitted = true
			}
		case "image":
			if convertImage != nil && emitImage != nil {
				if img, ok := convertImage(item); ok {
					emitHeader()
					emitImage(img)
					emitted = true
				} else if onImageDrop != nil {
					onImageDrop()
				}
			} else if onImageDrop != nil {
				onImageDrop()
			}
		}
		return true
	})
	return emitted
}
