package signature

import (
	"strings"
	"testing"

	"ccLoad/internal/protocol/cliproxy/signature/signaturetest"

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

func TestSanitizeGeminiRequestThoughtSignaturesCallAfterTextGetsBypass(t *testing.T) {
	t.Run("unsigned_calls_separated_by_text", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}}},{"functionCall":{"name":"second","args":{}}},{"text":"more"},{"functionCall":{"name":"third","args":{}}}]}]}`)

		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("first call signature = %q, want bypass sentinel; output=%s", got, out)
		}
		if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
			t.Fatalf("second parallel call should remain unsigned; output=%s", out)
		}
		if got := gjson.GetBytes(out, "contents.0.parts.3.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("call after text signature = %q, want bypass sentinel; output=%s", got, out)
		}
	})

	t.Run("bypass_signed_calls_separated_by_text", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[` +
			`{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"},` +
			`{"functionCall":{"name":"second","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"},` +
			`{"text":"more"},` +
			`{"functionCall":{"name":"third","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"}]}]}`)

		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("first call signature = %q, want bypass sentinel; output=%s", got, out)
		}
		if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
			t.Fatalf("second parallel call should have bypass stripped; output=%s", out)
		}
		if got := gjson.GetBytes(out, "contents.0.parts.3.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("call after text signature = %q, want bypass sentinel preserved; output=%s", got, out)
		}
	})

	t.Run("thought_part_does_not_end_parallel_group", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[` +
			`{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"},` +
			`{"text":"hidden","thought":true},` +
			`{"functionCall":{"name":"second","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"}]}]}`)

		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("first call signature = %q, want bypass sentinel; output=%s", got, out)
		}
		if signature := gjson.GetBytes(out, "contents.0.parts.2.thoughtSignature"); signature.Exists() {
			t.Fatalf("call after thought part should remain unsigned sibling; output=%s", out)
		}
	})
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

func bareTinkReplayPayload(sig string) []byte {
	return []byte(`{"contents":[` +
		`{"role":"user","parts":[{"text":"list"}]},` +
		`{"role":"model","parts":[` +
		`{"text":"thinking","thought":true},` +
		`{"functionCall":{"name":"list_dir","args":{"path":"."}},"thoughtSignature":"` + sig + `"}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"list_dir","response":{"files":[]}}}]},` +
		`{"role":"model","parts":[{"text":"done","thoughtSignature":"` + sig + `"}]}]}`)
}

func TestSanitizeGeminiRequestThoughtSignatures_PreservesVertexBareTink(t *testing.T) {
	sig := testGeminiBareTinkSignature(405)
	out := SanitizeGeminiRequestThoughtSignatures(bareTinkReplayPayload(sig), "contents")

	if got := gjson.GetBytes(out, "contents.1.parts.1.thoughtSignature").String(); got != sig {
		t.Fatalf("functionCall signature = %q, want original bare-Tink value", got)
	}
	if got := gjson.GetBytes(out, "contents.3.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("text signature = %q, want original bare-Tink value", got)
	}
}

func TestSanitizeNonVertexGeminiRequestThoughtSignatures_ReplacesVertexBareTink(t *testing.T) {
	sig := testGeminiBareTinkSignature(405)
	out := SanitizeNonVertexGeminiRequestThoughtSignatures(bareTinkReplayPayload(sig), "contents")

	if got := gjson.GetBytes(out, "contents.1.parts.1.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("functionCall signature = %q, want bypass sentinel", got)
	}
	if gjson.GetBytes(out, "contents.3.parts.0.thoughtSignature").Exists() {
		t.Fatalf("text signature should be dropped: %s", out)
	}
	if got := gjson.GetBytes(out, "contents.1.parts.0.thought").Bool(); !got {
		t.Fatalf("thought part must stay untouched: %s", out)
	}
}

func TestSanitizeNonVertexGeminiRequestThoughtSignatures_PreservesWrappedEnvelope(t *testing.T) {
	sig := testGeminiThoughtSignatureEnvelope()
	out := SanitizeNonVertexGeminiRequestThoughtSignatures(bareTinkReplayPayload(sig), "contents")

	if got := gjson.GetBytes(out, "contents.1.parts.1.thoughtSignature").String(); got != sig {
		t.Fatalf("functionCall signature = %q, want wrapped envelope preserved", got)
	}
	if got := gjson.GetBytes(out, "contents.3.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("text signature = %q, want wrapped envelope preserved", got)
	}
}

func TestSanitizeVertexSignaturesForNonVertexOnlyChangesBareTink(t *testing.T) {
	sig := testGeminiBareTinkSignature(405)
	wrapped := testGeminiThoughtSignatureEnvelope()
	input := []byte(`{"request":{"contents":[{"role":"model","parts":[` +
		`{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + sig + `"},` +
		`{"functionCall":{"name":"second","args":{}},"thought_signature":"gemini#` + sig + `"},` +
		`{"text":"thought","thought":true,"thoughtSignature":"` + sig + `"},` +
		`{"functionCall":{"name":"third","args":{}},"thoughtSignature":"` + wrapped + `"},` +
		`{"text":"more","thoughtSignature":"opaque-other-provider"},` +
		`{"functionCall":{"name":"fourth","args":{},"thought_signature":"` + sig + `"}},` +
		`{"text":"unsigned"},` +
		`{"functionCall":{"name":"unsigned","args":{}}},` +
		`{"functionCall":{"name":"fifth","args":{}},"thoughtSignature":"` + sig + `"}` +
		`]}, {"role":"user","parts":[{"functionResponse":{"name":"first","response":{}},"thoughtSignature":"` + sig + `"}]}]}}`)
	out := SanitizeVertexSignaturesForNonVertex(input, "request.contents")
	for _, index := range []int{0, 5} {
		if got := gjson.GetBytes(out, "request.contents.0.parts").Array()[index].Get("thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("part %d signature = %q, want bypass: %s", index, got, out)
		}
	}
	parts := gjson.GetBytes(out, "request.contents.0.parts").Array()
	for _, index := range []int{1, 2, 8} {
		if parts[index].Get("thoughtSignature").Exists() || parts[index].Get("thought_signature").Exists() {
			t.Fatalf("part %d kept bare-Tink signature: %s", index, out)
		}
	}
	if parts[5].Get("functionCall.thought_signature").Exists() {
		t.Fatalf("nested alias survived normalization: %s", out)
	}
	originalParts := gjson.GetBytes(input, "request.contents.0.parts").Array()
	for _, index := range []int{3, 4, 6, 7} {
		if parts[index].Raw != originalParts[index].Raw {
			t.Fatalf("unrelated part %d changed: got %s, want %s", index, parts[index].Raw, originalParts[index].Raw)
		}
	}
	if gjson.GetBytes(out, "request.contents.1.parts.0.thoughtSignature").Exists() {
		t.Fatalf("functionResponse kept bare-Tink signature: %s", out)
	}
}

func TestSanitizeVertexSignaturesForNonVertexPreservesOtherSignatureContracts(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[` +
		`{"functionCall":{"name":"unsigned","args":{}}},` +
		`{"text":"more"},` +
		`{"functionCall":{"name":"signed","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"},` +
		`{"text":"reason","thought":true,"thought_signature":"` + signaturetest.AntigravityCAQSWithChannel(19) + `"}` +
		`]}, {"role":"user","parts":[{"functionResponse":{"name":"signed","response":{}},"thoughtSignature":"opaque"}]}]}`)
	out := SanitizeVertexSignaturesForNonVertex(input, "")
	if &out[0] != &input[0] {
		t.Fatalf("payload without bare-Tink signatures changed: %s", out)
	}
}
