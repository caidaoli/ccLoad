package builtin

import "ccLoad/internal/protocol"

// Register installs the built-in protocol translators used by the proxy.
func Register(reg *protocol.Registry) {
	// Requests go from client to upstream; responses travel in the opposite direction.
	register := func(client, upstream protocol.Protocol, request protocol.RequestTransform, stream protocol.ResponseStreamTransform, nonStream protocol.ResponseNonStreamTransform) {
		reg.RegisterRequest(client, upstream, request)
		reg.RegisterStreamResponse(upstream, client, stream)
		reg.RegisterNonStreamResponse(upstream, client, nonStream)
	}

	register(protocol.OpenAI, protocol.Gemini, cliproxyOpenAIRequestToGemini,
		cliproxyGeminiResponseToOpenAIStream, cliproxyGeminiResponseToOpenAINonStream)
	register(protocol.Gemini, protocol.OpenAI, cliproxyGeminiRequestToOpenAI,
		cliproxyOpenAIResponseToGeminiStream, cliproxyOpenAIResponseToGeminiNonStream)
	register(protocol.OpenAI, protocol.Anthropic, cliproxyOpenAIRequestToAnthropic,
		cliproxyAnthropicResponseToOpenAIStream, cliproxyAnthropicResponseToOpenAINonStream)
	register(protocol.Anthropic, protocol.OpenAI, cliproxyAnthropicRequestToOpenAI,
		cliproxyOpenAIResponseToAnthropicStream, cliproxyOpenAIResponseToAnthropicNonStream)
	register(protocol.OpenAI, protocol.Codex, cliproxyOpenAIRequestToCodex,
		cliproxyCodexResponseToOpenAIStream, cliproxyCodexResponseToOpenAINonStream)
	register(protocol.Anthropic, protocol.Gemini, cliproxyAnthropicRequestToGemini,
		cliproxyGeminiResponseToAnthropicStream, cliproxyGeminiResponseToAnthropicNonStream)
	register(protocol.Gemini, protocol.Anthropic, cliproxyGeminiRequestToAnthropic,
		cliproxyAnthropicResponseToGeminiStream, cliproxyAnthropicResponseToGeminiNonStream)
	register(protocol.Codex, protocol.Gemini, cliproxyCodexRequestToGemini,
		cliproxyGeminiResponseToCodexStream, cliproxyGeminiResponseToCodexNonStream)
	register(protocol.Gemini, protocol.Codex, cliproxyGeminiRequestToCodex,
		cliproxyCodexResponseToGeminiStream, cliproxyCodexResponseToGeminiNonStream)
	register(protocol.Codex, protocol.Anthropic, cliproxyCodexRequestToAnthropic,
		cliproxyAnthropicResponseToCodexStream, cliproxyAnthropicResponseToCodexNonStream)
	register(protocol.Anthropic, protocol.Codex, cliproxyAnthropicRequestToCodex,
		cliproxyCodexResponseToAnthropicStream, cliproxyCodexResponseToAnthropicNonStream)
	register(protocol.Codex, protocol.OpenAI, cliproxyCodexRequestToOpenAI,
		cliproxyOpenAIResponseToCodexStream, cliproxyOpenAIResponseToCodexNonStream)
}
