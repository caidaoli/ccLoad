package responses

import (
	"context"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func convertGeminiImageStream(t *testing.T, lines ...string) [][]byte {
	t.Helper()
	var param any
	var events [][]byte
	for _, line := range append(lines, "[DONE]") {
		events = append(events, ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-3.1-flash-image", nil, nil, []byte(line), &param)...)
	}
	return events
}

func TestConvertGeminiInlineDataImageStream(t *testing.T) {
	events := convertGeminiImageStream(t,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"thought":true,"inlineData":{"mimeType":"image/png","data":"RFJBRlQ="}}]}}],"responseId":"resp_img"}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Here it is"}]}}],"responseId":"resp_img"}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/webp","data":"RklOQUw="}}]}}],"responseId":"resp_img"}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/webp","data":"RklOQUw="}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1120,"totalTokenCount":1123},"responseId":"resp_img"}`,
	)

	var added, done []ojson.Result
	var completed ojson.Result
	for _, ev := range events {
		event, data := parseSSEEvent(t, ev)
		switch {
		case event == "response.output_item.added" && data.Get("item.type").String() == "image_generation_call":
			added = append(added, data)
		case event == "response.output_item.done" && data.Get("item.type").String() == "image_generation_call":
			done = append(done, data)
		case event == "response.completed":
			completed = data.Get("response")
		}
	}
	if len(added) != 1 || len(done) != 1 {
		t.Fatalf("image added=%d done=%d, want 1 each (thought image skipped, repeat deduped): %s", len(added), len(done), eventsToString(events))
	}
	if got := added[0].Get("item.output_format").String(); got != "webp" {
		t.Fatalf("added output_format = %q, want webp", got)
	}
	if got := done[0].Get("item.result").String(); got != "RklOQUw=" {
		t.Fatalf("done result = %q, want RklOQUw=", got)
	}
	if got := done[0].Get("item.status").String(); got != "completed" {
		t.Fatalf("done status = %q, want completed", got)
	}

	output := completed.Get("output").Array()
	if len(output) != 2 || output[0].Get("type").String() != "message" || output[1].Get("type").String() != "image_generation_call" {
		t.Fatalf("completed output = %s, want message then image_generation_call", completed.Get("output").Raw)
	}
	if output[1].Get("id").String() != done[0].Get("item.id").String() || output[1].Get("result").String() != "RklOQUw=" || output[1].Get("output_format").String() != "webp" {
		t.Fatalf("completed image item = %s, want streamed done item", output[1].Raw)
	}
	if got := output[0].Get("content.0.text").String(); got != "Here it is" {
		t.Fatalf("message text = %q, want Here it is", got)
	}
}

func TestConvertGeminiInlineDataImageStreamPreservesSignature(t *testing.T) {
	events := convertGeminiImageStream(t,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/jpeg","data":"Ymll"},"thoughtSignature":"EjQKDg=="}]},"finishReason":"STOP"}],"responseId":"resp_img_sig"}`,
	)
	var completed ojson.Result
	for _, ev := range events {
		if event, data := parseSSEEvent(t, ev); event == "response.completed" {
			completed = data.Get("response")
		}
	}
	output := completed.Get("output").Array()
	if len(output) != 2 || output[0].Get("type").String() != "reasoning" || output[1].Get("type").String() != "image_generation_call" {
		t.Fatalf("completed output = %s, want signature carrier then image", completed.Get("output").Raw)
	}
	signature, direction, _, _, ok := decodeGeminiResponsesCarrier(output[0].Get("encrypted_content").String())
	if !ok || signature != "EjQKDg==" || direction != geminiResponsesCarrierStandalone {
		t.Fatalf("carrier = %q (signature=%q direction=%q ok=%v), want standalone EjQKDg==", output[0].Get("encrypted_content").String(), signature, direction, ok)
	}
	if got := output[1].Get("output_format").String(); got != "jpeg" {
		t.Fatalf("output_format = %q, want jpeg", got)
	}
}

func TestConvertGeminiInlineDataImageNonStream(t *testing.T) {
	raw := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"inlineData":{"mimeType":"image/png","data":"RFJBRlQ="}},{"text":"Here it is"},{"inline_data":{"mime_type":"image/png","data":"RklOQUw="}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1120,"totalTokenCount":1123},"responseId":"resp_img"}`)
	result := ConvertGeminiResponseToOpenAIResponsesNonStream(context.Background(), "gemini-3.1-flash-image", nil, nil, raw, nil)
	output := ojson.GetBytes(result, "output").Array()
	if len(output) != 2 || output[0].Get("type").String() != "message" || output[1].Get("type").String() != "image_generation_call" {
		t.Fatalf("output = %s, want message then image_generation_call", ojson.GetBytes(result, "output").Raw)
	}
	image := output[1]
	if image.Get("result").String() != "RklOQUw=" || image.Get("output_format").String() != "png" || image.Get("status").String() != "completed" {
		t.Fatalf("image item = %s", image.Raw)
	}
}

func TestConvertGeminiInlineDataImageNonStreamPreservesSignature(t *testing.T) {
	raw := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Here it is"},{"inlineData":{"mimeType":"image/jpeg","data":"Ymll"},"thoughtSignature":"EjQKDg=="}]},"finishReason":"STOP"}],"responseId":"resp_img_sig"}`)
	result := ConvertGeminiResponseToOpenAIResponsesNonStream(context.Background(), "gemini-3-pro-image", nil, nil, raw, nil)
	output := ojson.GetBytes(result, "output").Array()
	if len(output) != 3 || output[0].Get("type").String() != "message" || output[1].Get("type").String() != "reasoning" || output[2].Get("type").String() != "image_generation_call" {
		t.Fatalf("output = %s, want message, signature carrier, image", ojson.GetBytes(result, "output").Raw)
	}
	if output[0].Get("content.0.encrypted_content").Exists() || output[0].Get("encrypted_content").Exists() {
		t.Fatalf("image signature must not bind to the preceding message: %s", output[0].Raw)
	}
	if signature, direction, _, _, ok := decodeGeminiResponsesCarrier(output[1].Get("encrypted_content").String()); !ok || signature != "EjQKDg==" || direction != geminiResponsesCarrierStandalone {
		t.Fatalf("carrier = %s, want standalone EjQKDg==", output[1].Raw)
	}
	if got := output[2].Get("output_format").String(); got != "jpeg" {
		t.Fatalf("output_format = %q, want jpeg", got)
	}
}

func TestConvertGeminiInlineDataNonImageIsNotReportedAsImage(t *testing.T) {
	raw := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"audio/pcm","data":"AAAA"}}]},"finishReason":"STOP"}],"responseId":"resp_audio"}`)
	result := ConvertGeminiResponseToOpenAIResponsesNonStream(context.Background(), "gemini-test", nil, nil, raw, nil)
	if image := ojson.GetBytes(result, `output.#(type=="image_generation_call")`); image.Exists() {
		t.Fatalf("audio/pcm must not be emitted as image_generation_call: %s", result)
	}

	events := convertGeminiImageStream(t, `data: {"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"audio/pcm","data":"AAAA"}}]},"finishReason":"STOP"}],"responseId":"resp_audio"}`)
	for _, ev := range events {
		if _, data := parseSSEEvent(t, ev); data.Get("item.type").String() == "image_generation_call" {
			t.Fatalf("audio/pcm must not be streamed as image_generation_call: %s", eventsToString(events))
		}
	}
}

func eventsToString(events [][]byte) []byte {
	var out []byte
	for _, e := range events {
		out = append(out, e...)
		out = append(out, '\n')
	}
	return out
}
