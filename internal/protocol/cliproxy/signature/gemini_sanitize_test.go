package signature

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeGeminiRequestThoughtSignaturesPreservesGeminiSignature(t *testing.T) {
	sig := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + sig + `"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("thoughtSignature = %q, want %q. Output: %s", got, sig, string(out))
	}
	if &out[0] != &input[0] {
		t.Fatal("compatible canonical signature payload was copied")
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesNormalizesDuplicateCanonicalField(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `","thoughtSignature":"bad","thoughtSignature":"worse"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("thoughtSignature = %q, want bypass sentinel. Output: %s", got, out)
	}
	if count := strings.Count(string(out), `"thoughtSignature"`); count != 1 {
		t.Fatalf("thoughtSignature field count = %d, want 1. Output: %s", count, out)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesParallelSyntheticOnlyFirstGetsBypass(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}}},{"functionCall":{"name":"second","args":{}}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("first call signature = %q, want bypass sentinel; output=%s", got, out)
	}
	if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
		t.Fatalf("second parallel call should remain unsigned; output=%s", out)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesNativeParallelPreservesUnsignedSibling(t *testing.T) {
	nativeSignature := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"},{"functionCall":{"name":"second","args":{}}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != nativeSignature {
		t.Fatalf("first call signature = %q, want native signature; output=%s", got, out)
	}
	if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
		t.Fatalf("native unsigned sibling should remain unsigned; output=%s", out)
	}
	if &out[0] != &input[0] {
		t.Fatal("already-native parallel history was copied")
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesRemovesPollutedSiblingBypass(t *testing.T) {
	nativeSignature := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"},{"functionCall":{"name":"second","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != nativeSignature {
		t.Fatalf("first call signature = %q, want native signature; output=%s", got, out)
	}
	if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
		t.Fatalf("polluted sibling bypass should be removed; output=%s", out)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesRemovesPrefixedSiblingBypass(t *testing.T) {
	nativeSignature := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	for _, prefix := range []string{"gemini", "google"} {
		t.Run(prefix, func(t *testing.T) {
			input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"},{"functionCall":{"name":"second","args":{}},"thoughtSignature":"` + prefix + `#` + GeminiSkipThoughtSignatureValidator + `"}]}]}`)

			out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

			if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
				t.Fatalf("prefixed sibling bypass should be removed; output=%s", out)
			}
		})
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesLeavesUnsignedThoughtUnsigned(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"text":"hidden","thought":true}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if signature := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature"); signature.Exists() {
		t.Fatalf("unsigned thought should remain unsigned; output=%s", out)
	}
	if &out[0] != &input[0] {
		t.Fatal("unsigned thought payload was copied")
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesReusesUnsignedFunctionResponsePayload(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"ok"}}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if &out[0] != &input[0] {
		t.Fatal("unsigned function response payload was copied")
	}
	if string(out) != string(input) {
		t.Fatalf("payload changed:\n got: %s\nwant: %s", out, input)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesReplacesBase64UUIDFunctionCall(t *testing.T) {
	sig := testGeminiThoughtSignature([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{},"thoughtSignature":"` + sig + `"}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("thoughtSignature = %q, want bypass sentinel. Output: %s", got, string(out))
	}
	if gjson.GetBytes(out, "contents.0.parts.0.functionCall.thoughtSignature").Exists() {
		t.Fatalf("nested functionCall thoughtSignature should be removed. Output: %s", string(out))
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesPreservesField2WrappedUUIDFunctionCall(t *testing.T) {
	sig := testGemini3ThoughtSignature([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
	input := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + sig + `"}]}]}}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "request.contents")

	if got := gjson.GetBytes(out, "request.contents.0.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("thoughtSignature = %q, want wrapped UUID signature preserved. Output: %s", got, string(out))
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesRemovesFunctionResponseSignature(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"ok"},"thoughtSignature":"bad","thoughtSignature":"worse"},"thoughtSignature":"bad"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").Exists() {
		t.Fatalf("functionResponse top-level thoughtSignature should be removed. Output: %s", string(out))
	}
	if gjson.GetBytes(out, "contents.0.parts.0.functionResponse.thoughtSignature").Exists() {
		t.Fatalf("functionResponse nested thoughtSignature should be removed. Output: %s", string(out))
	}
}

func TestSanitizeGeminiRequestThoughtSignatures_PreservesToolCallAndResponseSignatures(t *testing.T) {
	const liveCapturedToolCallSig = "ErUDCrIDCAISrQMBEU0yD9ECvDhSY1DQJNUGafArdfd2mDfO8VQq7XjLx/91zESuo0QPSdkRFWkLeVIocSQmQULonYMOJcs6XDLV2LTRC9myb3MCCP9CUoWbEeqhAvXKTScyS3nwBDDVJYuDDbY3YvR4V86T/DnU3qufpaVZ3wQOiJVyBVZ515dYTN+XGq7SuUc3RpfAqVU06jgxaCM0WKV4Df5mGMJWb25e/aFG2Jc7upSqpf3n6aElj+4c/eWr4GdKd0TUIElXBZ0HEN/vNcWzD3F0S4MeVbk1LDakL6HG6oyaSS2gocxYNYxqm9mdMHaXYa4mIYqWqmqBEnbgcHp8H4fgqBxc3Cx8C3otV8IarO5OALaVDA3NaXB1zjLet1587kEpkCNr9OvrYOES2nCl/i4EgbPK01nlXo+Wwm5jsZU5nEG4/Z0bErzqC5TKwOsqpJ7afL2sPWI0IGrXhXL+QCumWCS5iUtwybSkL7CYSk9GC+iY+ev6FAmC4V5JEc4OaWOc9+m/29LniN/iPTSxtUQSZT94pUa3/irIIdH7ReAS3cpeM6OTvumR1PwNxXx3XM1mEGc="
	sigModel := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})

	input := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"toolCall":{"toolType":"GOOGLE_SEARCH_WEB","id":"1"},"thoughtSignature":"` + liveCapturedToolCallSig + `"},{"toolResponse":{"toolType":"GOOGLE_SEARCH_WEB","id":"1"},"thoughtSignature":"` + liveCapturedToolCallSig + `"},{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + sigModel + `"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.1.parts.0.thoughtSignature").String(); got != liveCapturedToolCallSig {
		t.Fatalf("toolCall thoughtSignature = %q, want %q. Output: %s", got, liveCapturedToolCallSig, string(out))
	}
	if got := gjson.GetBytes(out, "contents.1.parts.1.thoughtSignature").String(); got != liveCapturedToolCallSig {
		t.Fatalf("toolResponse thoughtSignature = %q, want %q. Output: %s", got, liveCapturedToolCallSig, string(out))
	}
	if got := gjson.GetBytes(out, "contents.1.parts.2.thoughtSignature").String(); got != sigModel {
		t.Fatalf("functionCall thoughtSignature = %q, want %q. Output: %s", got, sigModel, string(out))
	}
}

func TestSanitizeGeminiRequestThoughtSignatures_SkipsToolCallAndToolResponseParts(t *testing.T) {
	// Server-side tool blocks (toolCall and toolResponse) carry their own signatures/envelopes
	// that must be echoed back to the API untouched.
	const arbitrarySig = "arbitrary_opaque_tool_sig_value"
	input := []byte(`{"contents":[{"role":"model","parts":[
		{"toolCall":{"toolType":"GOOGLE_SEARCH_WEB","args":{}},"thoughtSignature":"` + arbitrarySig + `"},
		{"toolResponse":{"toolType":"GOOGLE_SEARCH_WEB","response":{}},"thoughtSignature":"` + arbitrarySig + `"}
	]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != arbitrarySig {
		t.Fatalf("toolCall thoughtSignature = %q, want preserved %q", got, arbitrarySig)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature").String(); got != arbitrarySig {
		t.Fatalf("toolResponse thoughtSignature = %q, want preserved %q", got, arbitrarySig)
	}
}

func TestSanitizeGeminiRequestThoughtSignatures_SkipsSnakeCaseToolCallAndResponseParts(t *testing.T) {
	// Defensively ensure snake_case tool_call and tool_response variants are also skipped
	const arbitrarySig = "arbitrary_snake_tool_sig"
	input := []byte(`{"contents":[{"role":"model","parts":[
		{"tool_call":{"tool_type":"GOOGLE_SEARCH_WEB","args":{}},"thought_signature":"` + arbitrarySig + `"},
		{"tool_response":{"tool_type":"GOOGLE_SEARCH_WEB","response":{}},"thought_signature":"` + arbitrarySig + `"}
	]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thought_signature").String(); got != arbitrarySig {
		t.Fatalf("tool_call thought_signature = %q, want preserved %q", got, arbitrarySig)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.1.thought_signature").String(); got != arbitrarySig {
		t.Fatalf("tool_response thought_signature = %q, want preserved %q", got, arbitrarySig)
	}
}

func TestSanitizeGeminiRequestThoughtSignatures_DropsForeignSignatureOnTextPart(t *testing.T) {
	// An arbitrary unknown or foreign signature on a model text part must be dropped
	// to prevent upstream Gemini 400 "Corrupted thought signature" errors.
	const foreignSig = "claude_or_invalid_signature"
	input := []byte(`{"contents":[{"role":"model","parts":[{"text":"answer","thoughtSignature":"` + foreignSig + `"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").Exists() {
		t.Fatalf("expected foreign signature on text part to be dropped, got %s", string(out))
	}
}

func TestSanitizeGeminiRequestThoughtSignatures_MixedToolCallAndUnsignedFunctionCall(t *testing.T) {
	// In a mixed turn with toolCall and unsigned functionCall, the toolCall signature
	// must be preserved, and the first functionCall must receive the bypass sentinel.
	const toolSig = "ErUDCrIDCAISrQMBEU0yD9ECvDhSY1DQJNUGafArdfd2mDfO8VQq7XjLx/91zESuo0QPSdkRFWkLeVIocSQmQULonYMOJcs6XDLV2LTRC9myb3MCCP9CUoWbEeqhAvXKTScyS3nwBDDVJYuDDbY3YvR4V86T/DnU3qufpaVZ3wQOiJVyBVZ515dYTN+XGq7SuUc3RpfAqVU06jgxaCM0WKV4Df5mGMJWb25e/aFG2Jc7upSqpf3n6aElj+4c/eWr4GdKd0TUIElXBZ0HEN/vNcWzD3F0S4MeVbk1LDakL6HG6oyaSS2gocxYNYxqm9mdMHaXYa4mIYqWqmqBEnbgcHp8H4fgqBxc3Cx8C3otV8IarO5OALaVDA3NaXB1zjLet1587kEpkCNr9OvrYOES2nCl/i4EgbPK01nlXo+Wwm5jsZU5nEG4/Z0bErzqC5TKwOsqpJ7afL2sPWI0IGrXhXL+QCumWCS5iUtwybSkL7CYSk9GC+iY+ev6FAmC4V5JEc4OaWOc9+m/29LniN/iPTSxtUQSZT94pUa3/irIIdH7ReAS3cpeM6OTvumR1PwNxXx3XM1mEGc="
	input := []byte(`{"contents":[{"role":"model","parts":[
		{"toolCall":{"toolType":"GOOGLE_SEARCH_WEB","id":"1"},"thoughtSignature":"` + toolSig + `"},
		{"functionCall":{"name":"my_func","args":{}}}
	]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != toolSig {
		t.Fatalf("toolCall signature = %q, want preserved %q", got, toolSig)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("functionCall signature = %q, want bypass %q. Output: %s", got, GeminiSkipThoughtSignatureValidator, string(out))
	}
}
