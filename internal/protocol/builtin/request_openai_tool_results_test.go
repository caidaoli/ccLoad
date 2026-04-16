package builtin

import "testing"

func TestRequestOpenAIToolResults(t *testing.T) {
	parts := []conversationPart{
		{Kind: partKindText, Text: "tool ok"},
		{Kind: partKindImage, Media: &conversationMedia{URL: "https://example.com/tool.png", Detail: "high"}},
		{Kind: partKindFile, Media: &conversationMedia{Data: "cGRm", MIMEType: "application/pdf", Filename: "doc.pdf"}},
	}

	content, err := encodeOpenAIToolResultContent(parts)
	if err != nil {
		t.Fatalf("encodeOpenAIToolResultContent failed: %v", err)
	}
	items, ok := content.([]map[string]any)
	if !ok || len(items) != 3 {
		t.Fatalf("expected structured tool result content, got %#v", content)
	}
	if items[0]["type"] != "text" || items[1]["type"] != "image_url" || items[2]["type"] != "file" {
		t.Fatalf("unexpected content items: %#v", items)
	}
}
