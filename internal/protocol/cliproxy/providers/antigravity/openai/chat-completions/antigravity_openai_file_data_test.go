package chat_completions

import (
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestConvertOpenAIRequestToAntigravityNormalizesFileDataURL(t *testing.T) {
	input := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":[{"type":"file","file":{"filename":"test.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}}]}]}`)

	out, _ := ConvertOpenAIRequestToAntigravity("gemini-2.5-pro", input, false)
	inlineData := ojson.GetBytes(out, "request.contents.0.parts.0.inlineData")
	if got := inlineData.Get("mimeType").String(); got != "application/pdf" {
		t.Fatalf("inlineData.mimeType = %q, want application/pdf. Output: %s", got, out)
	}
	if got := inlineData.Get("data").String(); got != "JVBERi0xLjQK" {
		t.Fatalf("inlineData.data = %q, want raw base64 payload. Output: %s", got, out)
	}
}

func TestConvertOpenAIRequestToAntigravityNormalizesAssistantImageDataURL(t *testing.T) {
	input := []byte(`{"messages":[
		{"role":"assistant","content":[
			{"type":"image_url","image_url":{"url":"data:image/png ;name=diagram.png; BASE64 ,iVBORw0KGgo="}},
			{"type":"image_url","image_url":{"url":"data:image/png;name=diagram.png,invalid"}}
		]},
		{"role":"user","content":"Describe the diagram"}
	]}`)
	out, err := ConvertOpenAIRequestToAntigravity("gemini-3-pro-preview", input, false)
	if err != nil {
		t.Fatal(err)
	}
	parts := ojson.GetBytes(out, "request.contents.0.parts").Array()
	if len(parts) != 1 || parts[0].Get("inlineData.mimeType").String() != "image/png" ||
		parts[0].Get("inlineData.data").String() != "iVBORw0KGgo=" {
		t.Fatalf("unexpected assistant image parts: %s", out)
	}
}
