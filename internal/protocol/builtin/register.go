package builtin

import "ccLoad/internal/protocol"

// Register installs the built-in protocol translators used by the proxy.
func Register(reg *protocol.Registry) {
	reg.RegisterRequest(protocol.OpenAI, protocol.Gemini, convertOpenAIRequestToGemini)
	reg.RegisterStreamResponse(protocol.Gemini, protocol.OpenAI, convertGeminiResponseToOpenAIStream)
	reg.RegisterNonStreamResponse(protocol.Gemini, protocol.OpenAI, convertGeminiResponseToOpenAINonStream)
	reg.RegisterRequest(protocol.OpenAI, protocol.Anthropic, convertOpenAIRequestToAnthropic)
	reg.RegisterStreamResponse(protocol.Anthropic, protocol.OpenAI, convertAnthropicResponseToOpenAIStream)
	reg.RegisterNonStreamResponse(protocol.Anthropic, protocol.OpenAI, convertAnthropicResponseToOpenAINonStream)
	reg.RegisterRequest(protocol.OpenAI, protocol.Codex, convertOpenAIRequestToCodex)
	reg.RegisterStreamResponse(protocol.Codex, protocol.OpenAI, convertCodexResponseToOpenAIStream)
	reg.RegisterNonStreamResponse(protocol.Codex, protocol.OpenAI, convertCodexResponseToOpenAINonStream)
	reg.RegisterRequest(protocol.Anthropic, protocol.Gemini, convertAnthropicRequestToGemini)
	reg.RegisterStreamResponse(protocol.Gemini, protocol.Anthropic, convertGeminiResponseToAnthropicStream)
	reg.RegisterNonStreamResponse(protocol.Gemini, protocol.Anthropic, convertGeminiResponseToAnthropicNonStream)
	reg.RegisterRequest(protocol.Codex, protocol.Gemini, convertCodexRequestToGemini)
	reg.RegisterStreamResponse(protocol.Gemini, protocol.Codex, convertGeminiResponseToCodexStream)
	reg.RegisterNonStreamResponse(protocol.Gemini, protocol.Codex, convertGeminiResponseToCodexNonStream)
	reg.RegisterRequest(protocol.Codex, protocol.Anthropic, convertCodexRequestToAnthropic)
	reg.RegisterStreamResponse(protocol.Anthropic, protocol.Codex, convertAnthropicResponseToCodexStream)
	reg.RegisterNonStreamResponse(protocol.Anthropic, protocol.Codex, convertAnthropicResponseToCodexNonStream)
	reg.RegisterRequest(protocol.Codex, protocol.OpenAI, convertCodexRequestToOpenAI)
	reg.RegisterStreamResponse(protocol.OpenAI, protocol.Codex, convertOpenAIResponseToCodexStream)
	reg.RegisterNonStreamResponse(protocol.OpenAI, protocol.Codex, convertOpenAIResponseToCodexNonStream)
}
