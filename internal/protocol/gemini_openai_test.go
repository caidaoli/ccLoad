package protocol_test

import (
	"context"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
)

func TestRegistry_TranslateRequest_GeminiToOpenAI(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"systemInstruction":{"parts":[{"text":"be careful"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	got, err := reg.TranslateRequest(protocol.Gemini, protocol.OpenAI, "gpt-4o", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if !strings.Contains(string(got), `"role":"system"`) || !strings.Contains(string(got), `"be careful"`) {
		t.Fatalf("expected openai system message, got %s", got)
	}
	if !strings.Contains(string(got), `"role":"user"`) || !strings.Contains(string(got), `"content":"hello"`) {
		t.Fatalf("expected openai user message, got %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_OpenAIToGemini(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	translatedReq := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	rawResp := []byte(`{"id":"chatcmpl_1","object":"chat.completion","created":0,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gpt-4o", rawReq, translatedReq, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	if !strings.Contains(string(got), `"role":"model"`) || !strings.Contains(string(got), `"text":"world"`) {
		t.Fatalf("unexpected gemini response: %s", got)
	}
	if !strings.Contains(string(got), `"promptTokenCount":3`) || !strings.Contains(string(got), `"candidatesTokenCount":5`) {
		t.Fatalf("expected gemini usage metadata, got %s", got)
	}
}

func TestRegistry_TranslateResponseStream_OpenAIToGemini(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	translatedReq := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"stream":true}`)

	var state any
	chunks, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gpt-4o", rawReq, translatedReq, []byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream failed: %v", err)
	}
	if len(chunks) != 1 || !strings.Contains(string(chunks[0]), `"text":"hello"`) {
		t.Fatalf("unexpected gemini stream chunk: %#v", chunks)
	}

	done, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gpt-4o", rawReq, translatedReq, []byte("data: [DONE]\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream done failed: %v", err)
	}
	if len(done) != 1 || !strings.Contains(string(done[0]), `"finishReason":"STOP"`) {
		t.Fatalf("unexpected gemini done chunk: %#v", done)
	}
}

func TestBuildTransformPlan_SupportsGeminiToOpenAI(t *testing.T) {
	plan, err := protocol.BuildTransformPlan(
		protocol.Gemini,
		protocol.OpenAI,
		"/v1beta/models/gpt-4o:generateContent",
		"",
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
		nil,
		"gpt-4o",
		"",
		false,
	)
	if err != nil {
		t.Fatalf("BuildTransformPlan failed: %v", err)
	}
	if !plan.NeedsTransform || plan.RequestFamily != protocol.RequestFamilyGenerateContent {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestRegistry_SameProtocolPassthrough(t *testing.T) {
	reg := protocol.NewRegistry()
	raw := []byte(`{"hello":"world"}`)

	gotReq, err := reg.TranslateRequest(protocol.Gemini, protocol.Gemini, "gemini-2.5-pro", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest passthrough failed: %v", err)
	}
	if string(gotReq) != string(raw) {
		t.Fatalf("expected request passthrough, got %s", gotReq)
	}

	gotResp, err := reg.TranslateResponseNonStream(context.Background(), protocol.Gemini, protocol.Gemini, "gemini-2.5-pro", raw, raw, raw)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream passthrough failed: %v", err)
	}
	if string(gotResp) != string(raw) {
		t.Fatalf("expected response passthrough, got %s", gotResp)
	}

	gotChunks, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Gemini, "gemini-2.5-pro", raw, raw, raw, nil)
	if err != nil {
		t.Fatalf("TranslateResponseStream passthrough failed: %v", err)
	}
	if len(gotChunks) != 1 || string(gotChunks[0]) != string(raw) {
		t.Fatalf("expected stream passthrough, got %#v", gotChunks)
	}
}

func TestBuildTransformPlan_SameProtocolPassthrough(t *testing.T) {
	plan, err := protocol.BuildTransformPlan(
		protocol.Gemini,
		protocol.Gemini,
		"/v1beta/models/gemini-2.5-pro:generateContent",
		"",
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
		nil,
		"gemini-2.5-pro",
		"",
		false,
	)
	if err != nil {
		t.Fatalf("BuildTransformPlan failed: %v", err)
	}
	if plan.NeedsTransform {
		t.Fatalf("same protocol should not require transform: %+v", plan)
	}
	if got := plan.UpstreamPath; got != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("expected upstream path passthrough, got %s", got)
	}
}
