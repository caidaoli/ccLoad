package signature

import (
	"fmt"
	"strings"
	"testing"

	"ccLoad/internal/protocol/cliproxy/signature/signaturetest"

	"github.com/MoeForge/moego/ojson"
)

func TestSanitizeGeminiRequestThoughtSignaturesPreservesGeminiSignature(t *testing.T) {
	sig := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + sig + `"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("thoughtSignature = %q, want %q. Output: %s", got, sig, string(out))
	}
	if &out[0] != &input[0] {
		t.Fatal("compatible canonical signature payload was copied")
	}
}

func TestSanitizeGeminiRequestThoughtSignatures_PromotesNonEmptyFallback(t *testing.T) {
	sig := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	for _, empty := range []string{`""`, `"  "`, `null`} {
		t.Run(empty, func(t *testing.T) {
			body := []byte(`{"contents":[{"role":"model","parts":[
				{"function_call":{"name":"first","thought_signature":"` + sig + `"},"thoughtSignature":` + empty + `},
				{"functionCall":{"name":"second"}},
				{"text":"visible","thoughtSignature":"","thought_signature":null}
			]},{"role":"user","parts":[
				{"function_response":{"name":"first","thoughtSignature":"` + sig + `","response":{"thought_signature":"tool data"}},"thought_signature":""}
			]}]}`)
			out := SanitizeGeminiRequestThoughtSignatures(body, "contents")
			if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != sig {
				t.Fatalf("thoughtSignature=%q, want %q; output=%s", got, sig, out)
			}
			for _, path := range []string{
				"contents.0.parts.0.function_call.thought_signature",
				"contents.0.parts.1.thoughtSignature",
				"contents.0.parts.2.thoughtSignature",
				"contents.0.parts.2.thought_signature",
				"contents.1.parts.0.function_response.thoughtSignature",
				"contents.1.parts.0.thought_signature",
			} {
				if ojson.GetBytes(out, path).Exists() {
					t.Errorf("unexpected signature at %s; output=%s", path, out)
				}
			}
			if got := ojson.GetBytes(out, "contents.1.parts.0.function_response.response.thought_signature").String(); got != "tool data" {
				t.Fatalf("tool data was changed: %s", out)
			}
		})
	}
}

func TestSanitizeVertexSignaturesForNonVertex_SnakeCaseCalls(t *testing.T) {
	sig := testGeminiBareTinkSignature(405)
	body := []byte(`{"contents":[{"role":"model","parts":[
		{"function_call":{"name":"first","thought_signature":"` + sig + `"}},
		{"functionCall":{"name":"second"},"thoughtSignature":"` + sig + `"},
		{"text":"keep","thoughtSignature":"existing"}
	]}]}`)
	out := SanitizeVertexSignaturesForNonVertex(body, "contents")
	if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Errorf("first function_call did not gain its required signature: %s", out)
	}
	if ojson.GetBytes(out, "contents.0.parts.0.function_call.thought_signature").Exists() ||
		ojson.GetBytes(out, "contents.0.parts.1.thoughtSignature").Exists() ||
		ojson.GetBytes(out, "contents.0.parts.2.thoughtSignature").String() != "existing" {
		t.Fatalf("expected only incompatible Vertex signatures to change: %s", out)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesNormalizesDuplicateCanonicalField(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `","thoughtSignature":"bad","thoughtSignature":"worse"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("thoughtSignature = %q, want bypass sentinel. Output: %s", got, out)
	}
	if count := strings.Count(string(out), `"thoughtSignature"`); count != 1 {
		t.Fatalf("thoughtSignature field count = %d, want 1. Output: %s", count, out)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesParallelSyntheticOnlyFirstGetsBypass(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}}},{"functionCall":{"name":"second","args":{}}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("first call signature = %q, want bypass sentinel; output=%s", got, out)
	}
	if signature := ojson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
		t.Fatalf("second parallel call should remain unsigned; output=%s", out)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesCallAfterTextGetsBypass(t *testing.T) {
	t.Run("unsigned_calls_separated_by_text", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}}},{"functionCall":{"name":"second","args":{}}},{"text":"more"},{"functionCall":{"name":"third","args":{}}}]}]}`)

		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("first call signature = %q, want bypass sentinel; output=%s", got, out)
		}
		if signature := ojson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
			t.Fatalf("second parallel call should remain unsigned; output=%s", out)
		}
		if got := ojson.GetBytes(out, "contents.0.parts.3.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
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

		if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("first call signature = %q, want bypass sentinel; output=%s", got, out)
		}
		if signature := ojson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
			t.Fatalf("second parallel call should have bypass stripped; output=%s", out)
		}
		if got := ojson.GetBytes(out, "contents.0.parts.3.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("call after text signature = %q, want bypass sentinel preserved; output=%s", got, out)
		}
	})

	t.Run("thought_part_does_not_end_parallel_group", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[` +
			`{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"},` +
			`{"text":"hidden","thought":true},` +
			`{"functionCall":{"name":"second","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"}]}]}`)

		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("first call signature = %q, want bypass sentinel; output=%s", got, out)
		}
		if signature := ojson.GetBytes(out, "contents.0.parts.2.thoughtSignature"); signature.Exists() {
			t.Fatalf("call after thought part should remain unsigned sibling; output=%s", out)
		}
	})
}

func TestSanitizeGeminiRequestThoughtSignaturesNativeParallelPreservesUnsignedSibling(t *testing.T) {
	nativeSignature := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"},{"functionCall":{"name":"second","args":{}}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != nativeSignature {
		t.Fatalf("first call signature = %q, want native signature; output=%s", got, out)
	}
	if signature := ojson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
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

	if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != nativeSignature {
		t.Fatalf("first call signature = %q, want native signature; output=%s", got, out)
	}
	if signature := ojson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
		t.Fatalf("polluted sibling bypass should be removed; output=%s", out)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesRemovesPrefixedSiblingBypass(t *testing.T) {
	nativeSignature := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	for _, prefix := range []string{"gemini", "google"} {
		t.Run(prefix, func(t *testing.T) {
			input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"},{"functionCall":{"name":"second","args":{}},"thoughtSignature":"` + prefix + `#` + GeminiSkipThoughtSignatureValidator + `"}]}]}`)

			out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

			if signature := ojson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
				t.Fatalf("prefixed sibling bypass should be removed; output=%s", out)
			}
		})
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesLeavesUnsignedThoughtUnsigned(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"text":"hidden","thought":true}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if signature := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature"); signature.Exists() {
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

	if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("thoughtSignature = %q, want bypass sentinel. Output: %s", got, string(out))
	}
	if ojson.GetBytes(out, "contents.0.parts.0.functionCall.thoughtSignature").Exists() {
		t.Fatalf("nested functionCall thoughtSignature should be removed. Output: %s", string(out))
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesPreservesField2WrappedUUIDFunctionCall(t *testing.T) {
	sig := testGemini3ThoughtSignature([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
	input := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + sig + `"}]}]}}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "request.contents")

	if got := ojson.GetBytes(out, "request.contents.0.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("thoughtSignature = %q, want wrapped UUID signature preserved. Output: %s", got, string(out))
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesRemovesFunctionResponseSignature(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"ok"},"thoughtSignature":"bad","thoughtSignature":"worse"},"thoughtSignature":"bad"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").Exists() {
		t.Fatalf("functionResponse top-level thoughtSignature should be removed. Output: %s", string(out))
	}
	if ojson.GetBytes(out, "contents.0.parts.0.functionResponse.thoughtSignature").Exists() {
		t.Fatalf("functionResponse nested thoughtSignature should be removed. Output: %s", string(out))
	}
}

func TestSanitizeGeminiRequestThoughtSignatures_PreservesToolCallAndResponseSignatures(t *testing.T) {
	const liveCapturedToolCallSig = "ErUDCrIDCAISrQMBEU0yD9ECvDhSY1DQJNUGafArdfd2mDfO8VQq7XjLx/91zESuo0QPSdkRFWkLeVIocSQmQULonYMOJcs6XDLV2LTRC9myb3MCCP9CUoWbEeqhAvXKTScyS3nwBDDVJYuDDbY3YvR4V86T/DnU3qufpaVZ3wQOiJVyBVZ515dYTN+XGq7SuUc3RpfAqVU06jgxaCM0WKV4Df5mGMJWb25e/aFG2Jc7upSqpf3n6aElj+4c/eWr4GdKd0TUIElXBZ0HEN/vNcWzD3F0S4MeVbk1LDakL6HG6oyaSS2gocxYNYxqm9mdMHaXYa4mIYqWqmqBEnbgcHp8H4fgqBxc3Cx8C3otV8IarO5OALaVDA3NaXB1zjLet1587kEpkCNr9OvrYOES2nCl/i4EgbPK01nlXo+Wwm5jsZU5nEG4/Z0bErzqC5TKwOsqpJ7afL2sPWI0IGrXhXL+QCumWCS5iUtwybSkL7CYSk9GC+iY+ev6FAmC4V5JEc4OaWOc9+m/29LniN/iPTSxtUQSZT94pUa3/irIIdH7ReAS3cpeM6OTvumR1PwNxXx3XM1mEGc="
	sigModel := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})

	input := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"toolCall":{"toolType":"GOOGLE_SEARCH_WEB","id":"1"},"thoughtSignature":"` + liveCapturedToolCallSig + `"},{"toolResponse":{"toolType":"GOOGLE_SEARCH_WEB","id":"1"},"thoughtSignature":"` + liveCapturedToolCallSig + `"},{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + sigModel + `"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := ojson.GetBytes(out, "contents.1.parts.0.thoughtSignature").String(); got != liveCapturedToolCallSig {
		t.Fatalf("toolCall thoughtSignature = %q, want %q. Output: %s", got, liveCapturedToolCallSig, string(out))
	}
	if got := ojson.GetBytes(out, "contents.1.parts.1.thoughtSignature").String(); got != liveCapturedToolCallSig {
		t.Fatalf("toolResponse thoughtSignature = %q, want %q. Output: %s", got, liveCapturedToolCallSig, string(out))
	}
	if got := ojson.GetBytes(out, "contents.1.parts.2.thoughtSignature").String(); got != sigModel {
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

	if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != arbitrarySig {
		t.Fatalf("toolCall thoughtSignature = %q, want preserved %q", got, arbitrarySig)
	}
	if got := ojson.GetBytes(out, "contents.0.parts.1.thoughtSignature").String(); got != arbitrarySig {
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

	if got := ojson.GetBytes(out, "contents.0.parts.0.thought_signature").String(); got != arbitrarySig {
		t.Fatalf("tool_call thought_signature = %q, want preserved %q", got, arbitrarySig)
	}
	if got := ojson.GetBytes(out, "contents.0.parts.1.thought_signature").String(); got != arbitrarySig {
		t.Fatalf("tool_response thought_signature = %q, want preserved %q", got, arbitrarySig)
	}
}

func TestSanitizeGeminiRequestThoughtSignatures_DropsForeignSignatureOnTextPart(t *testing.T) {
	// An arbitrary unknown or foreign signature on a model text part must be dropped
	// to prevent upstream Gemini 400 "Corrupted thought signature" errors.
	const foreignSig = "claude_or_invalid_signature"
	input := []byte(`{"contents":[{"role":"model","parts":[{"text":"answer","thoughtSignature":"` + foreignSig + `"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").Exists() {
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

	if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != toolSig {
		t.Fatalf("toolCall signature = %q, want preserved %q", got, toolSig)
	}
	if got := ojson.GetBytes(out, "contents.0.parts.1.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
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

	if got := ojson.GetBytes(out, "contents.1.parts.1.thoughtSignature").String(); got != sig {
		t.Fatalf("functionCall signature = %q, want original bare-Tink value", got)
	}
	if got := ojson.GetBytes(out, "contents.3.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("text signature = %q, want original bare-Tink value", got)
	}
}

func TestSanitizeNonVertexGeminiRequestThoughtSignatures_ReplacesVertexBareTink(t *testing.T) {
	sig := testGeminiBareTinkSignature(405)
	out := SanitizeNonVertexGeminiRequestThoughtSignatures(bareTinkReplayPayload(sig), "contents")

	if got := ojson.GetBytes(out, "contents.1.parts.1.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("functionCall signature = %q, want bypass sentinel", got)
	}
	if ojson.GetBytes(out, "contents.3.parts.0.thoughtSignature").Exists() {
		t.Fatalf("text signature should be dropped: %s", out)
	}
	if got := ojson.GetBytes(out, "contents.1.parts.0.thought").Bool(); !got {
		t.Fatalf("thought part must stay untouched: %s", out)
	}
}

func TestSanitizeNonVertexGeminiRequestThoughtSignatures_PreservesWrappedEnvelope(t *testing.T) {
	sig := testGeminiThoughtSignatureEnvelope()
	out := SanitizeNonVertexGeminiRequestThoughtSignatures(bareTinkReplayPayload(sig), "contents")

	if got := ojson.GetBytes(out, "contents.1.parts.1.thoughtSignature").String(); got != sig {
		t.Fatalf("functionCall signature = %q, want wrapped envelope preserved", got)
	}
	if got := ojson.GetBytes(out, "contents.3.parts.0.thoughtSignature").String(); got != sig {
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
		if got := ojson.GetBytes(out, "request.contents.0.parts").Array()[index].Get("thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("part %d signature = %q, want bypass: %s", index, got, out)
		}
	}
	parts := ojson.GetBytes(out, "request.contents.0.parts").Array()
	for _, index := range []int{1, 2, 8} {
		if parts[index].Get("thoughtSignature").Exists() || parts[index].Get("thought_signature").Exists() {
			t.Fatalf("part %d kept bare-Tink signature: %s", index, out)
		}
	}
	if parts[5].Get("functionCall.thought_signature").Exists() {
		t.Fatalf("nested alias survived normalization: %s", out)
	}
	originalParts := ojson.GetBytes(input, "request.contents.0.parts").Array()
	for _, index := range []int{3, 4, 6, 7} {
		if parts[index].Raw != originalParts[index].Raw {
			t.Fatalf("unrelated part %d changed: got %s, want %s", index, parts[index].Raw, originalParts[index].Raw)
		}
	}
	if ojson.GetBytes(out, "request.contents.1.parts.0.thoughtSignature").Exists() {
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

func TestSanitizeGeminiRequestThoughtSignatures_StripsEmptySystemInstructionParts(t *testing.T) {
	t.Run("drops_systemInstruction_when_all_parts_are_empty_objects", func(t *testing.T) {
		input := []byte(`{"request":{"systemInstruction":{"parts":[{}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "request.contents")
		if ojson.GetBytes(out, "request.systemInstruction").Exists() {
			t.Fatalf("expected request.systemInstruction to be removed, got %s", out)
		}
	})

	t.Run("retains_non_empty_systemInstruction_parts", func(t *testing.T) {
		input := []byte(`{"systemInstruction":{"parts":[{},{"text":"keep rule"},{}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")
		parts := ojson.GetBytes(out, "systemInstruction.parts").Array()
		if len(parts) != 1 || parts[0].Get("text").String() != "keep rule" {
			t.Fatalf("unexpected systemInstruction.parts: %s", out)
		}
	})

	t.Run("handles_snake_case_without_contents", func(t *testing.T) {
		input := []byte(`{"request":{"system_instruction":{"role":"system","parts":[{}, {"text":"keep"}, {}]}}}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "request.contents")
		sys := ojson.GetBytes(out, "request.system_instruction")
		parts := sys.Get("parts").Array()
		if sys.Get("role").String() != "system" || len(parts) != 1 || parts[0].Get("text").String() != "keep" {
			t.Fatalf("unexpected request.system_instruction: %s", out)
		}
	})

	t.Run("keeps_empty_text_field_and_valid_part_order", func(t *testing.T) {
		input := []byte(`{"systemInstruction":{"parts":[{"text":"first"},{},{"text":""},{},{"text":"last"}]}}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "")
		parts := ojson.GetBytes(out, "systemInstruction.parts").Array()
		if len(parts) != 3 || parts[0].Get("text").String() != "first" || !parts[1].Get("text").Exists() || parts[1].Get("text").String() != "" || parts[2].Get("text").String() != "last" {
			t.Fatalf("non-empty Part objects changed or reordered: %s", out)
		}
	})

}

func TestSanitizeNonVertexGeminiRequestThoughtSignaturesDoc_ReplacesVertexBareTink(t *testing.T) {
	sig := testGeminiBareTinkSignature(405)
	doc := ojson.NewDoc(bareTinkReplayPayload(sig))
	SanitizeNonVertexGeminiRequestThoughtSignaturesDoc(doc, "contents")

	if got := doc.Get("contents.1.parts.1.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("functionCall signature = %q, want bypass sentinel", got)
	}
	if doc.Get("contents.3.parts.0.thoughtSignature").Exists() {
		t.Fatalf("text signature should be dropped: %s", doc.String())
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesDoc_SnakeCaseFunctionCallAndResponse(t *testing.T) {
	input := []byte(`{"contents":[` +
		`{"role":"model","parts":[{"function_call":{"name":"get_weather"},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"}]},` +
		`{"role":"user","parts":[{"function_response":{"name":"get_weather","response":{}},"thoughtSignature":"abc"}]}` +
		`]}`)
	doc := ojson.NewDoc(input)
	SanitizeGeminiRequestThoughtSignaturesDoc(doc, "contents")

	if got := doc.Get("contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("snake_case function_call sentinel = %q, want %q; out=%s", got, GeminiSkipThoughtSignatureValidator, doc.String())
	}
	if doc.Get("contents.1.parts.0.thoughtSignature").Exists() {
		t.Fatalf("snake_case function_response thoughtSignature should be stripped; out=%s", doc.String())
	}
}

func TestSanitizeGeminiRequestThoughtSignatures_SnakeCaseFunctionCallBypassAndParallel(t *testing.T) {
	t.Run("unsigned_first_function_call_gets_bypass", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[{"function_call":{"name":"first","args":{}}},{"function_call":{"name":"second","args":{}}}]}]}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("first snake_case function_call signature = %q, want bypass sentinel; out=%s", got, out)
		}
		if ojson.GetBytes(out, "contents.0.parts.1.thoughtSignature").Exists() {
			t.Fatalf("second parallel snake_case function_call should stay unsigned; out=%s", out)
		}
	})

	t.Run("parallel_bypass_stripped_on_sibling_only", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[` +
			`{"function_call":{"name":"first","args":{}},"thought_signature":"` + GeminiSkipThoughtSignatureValidator + `"},` +
			`{"function_call":{"name":"second","args":{}},"thought_signature":"` + GeminiSkipThoughtSignatureValidator + `"}` +
			`]}]}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("first snake_case function_call signature = %q, want bypass sentinel; out=%s", got, out)
		}
		if ojson.GetBytes(out, "contents.0.parts.0.thought_signature").Exists() {
			t.Fatalf("first snake_case function_call should normalize thought_signature to thoughtSignature; out=%s", out)
		}
		if ojson.GetBytes(out, "contents.0.parts.1.thoughtSignature").Exists() || ojson.GetBytes(out, "contents.0.parts.1.thought_signature").Exists() {
			t.Fatalf("sibling snake_case function_call bypass should be stripped; out=%s", out)
		}
	})

	t.Run("mixed_snake_and_camel_parallel_calls", func(t *testing.T) {
		nativeSig := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
		input := []byte(`{"contents":[{"role":"model","parts":[` +
			`{"function_call":{"name":"first","args":{}},"thoughtSignature":"` + nativeSig + `"},` +
			`{"functionCall":{"name":"second","args":{}}}` +
			`]}]}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != nativeSig {
			t.Fatalf("first snake_case function_call signature = %q, want %q; out=%s", got, nativeSig, out)
		}
		if ojson.GetBytes(out, "contents.0.parts.1.thoughtSignature").Exists() {
			t.Fatalf("camelCase sibling after snake_case function_call should stay unsigned; out=%s", out)
		}
	})
}

func TestSanitizeGeminiRequestThoughtSignatures_SnakeCaseNestedSignatures(t *testing.T) {
	t.Run("replaces_nested_function_call_signature", func(t *testing.T) {
		sig := testGeminiThoughtSignature([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
		input := []byte(`{"contents":[{"role":"model","parts":[{"function_call":{"name":"f","args":{},"thought_signature":"` + sig + `","thoughtSignature":"` + sig + `"}}]}]}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("thoughtSignature = %q, want bypass sentinel; out=%s", got, out)
		}
		if ojson.GetBytes(out, "contents.0.parts.0.function_call.thought_signature").Exists() ||
			ojson.GetBytes(out, "contents.0.parts.0.function_call.thoughtSignature").Exists() {
			t.Fatalf("nested function_call signature fields should be removed; out=%s", out)
		}
	})

	t.Run("strips_top_level_and_nested_function_response_signatures", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"user","parts":[{"function_response":{"name":"f","response":{"result":"ok"},"thoughtSignature":"bad1","thought_signature":"bad2"},"thought_signature":"bad3"}]}]}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		for _, path := range []string{
			"contents.0.parts.0.thoughtSignature",
			"contents.0.parts.0.thought_signature",
			"contents.0.parts.0.function_response.thoughtSignature",
			"contents.0.parts.0.function_response.thought_signature",
		} {
			if ojson.GetBytes(out, path).Exists() {
				t.Fatalf("expected %s to be stripped; out=%s", path, out)
			}
		}
	})

	t.Run("reuses_unsigned_snake_case_function_response_payload", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"user","parts":[{"function_response":{"name":"f","response":{"result":"ok"}}}]}]}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if &out[0] != &input[0] {
			t.Fatal("unsigned snake_case function_response payload was copied")
		}
	})
}

func TestSanitizeGeminiRequestThoughtSignaturesDoc_EmptyThoughtSignaturePromotesFallback(t *testing.T) {
	sig := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})

	tests := []struct {
		name           string
		partJSON       string
		fallbackRemove string
	}{
		{
			name:           "empty_string_with_extra_content_fallback",
			partJSON:       `{"functionCall":{"name":"get_weather","args":{}},"thoughtSignature":"","extra_content":{"google":{"thought_signature":"` + sig + `"}}}`,
			fallbackRemove: "contents.0.parts.0.extra_content.google.thought_signature",
		},
		{
			name:           "whitespace_with_extra_content_fallback",
			partJSON:       `{"functionCall":{"name":"get_weather","args":{}},"thoughtSignature":"   ","extra_content":{"google":{"thought_signature":"` + sig + `"}}}`,
			fallbackRemove: "contents.0.parts.0.extra_content.google.thought_signature",
		},
		{
			name:           "null_with_extra_content_fallback",
			partJSON:       `{"functionCall":{"name":"get_weather","args":{}},"thoughtSignature":null,"extra_content":{"google":{"thought_signature":"` + sig + `"}}}`,
			fallbackRemove: "contents.0.parts.0.extra_content.google.thought_signature",
		},
		{
			name:           "empty_string_with_snake_case_thought_signature_fallback",
			partJSON:       `{"functionCall":{"name":"get_weather","args":{}},"thoughtSignature":"","thought_signature":"` + sig + `"}`,
			fallbackRemove: "contents.0.parts.0.thought_signature",
		},
		{
			name:           "empty_string_with_nested_functionCall_fallback",
			partJSON:       `{"functionCall":{"name":"get_weather","args":{},"thoughtSignature":"` + sig + `"},"thoughtSignature":""}`,
			fallbackRemove: "contents.0.parts.0.functionCall.thoughtSignature",
		},
		{
			name:           "empty_string_on_text_part_with_extra_content_fallback",
			partJSON:       `{"text":"previous answer","thoughtSignature":"","extra_content":{"google":{"thought_signature":"` + sig + `"}}}`,
			fallbackRemove: "contents.0.parts.0.extra_content.google.thought_signature",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := ojson.NewDoc([]byte(`{"contents":[{"role":"model","parts":[` + tt.partJSON + `]}]}`))
			SanitizeGeminiRequestThoughtSignaturesDoc(doc, "contents")

			if got := doc.Get("contents.0.parts.0.thoughtSignature").String(); got != sig {
				t.Fatalf("thoughtSignature = %q, want %q; out=%s", got, sig, doc.String())
			}
			if doc.Get(tt.fallbackRemove).Exists() {
				t.Fatalf("expected fallback field %s to be removed; out=%s", tt.fallbackRemove, doc.String())
			}
		})
	}
}

func TestSanitizeGeminiRequestThoughtSignatures_AllEmptySignaturePaths(t *testing.T) {
	t.Run("first_function_call_gets_bypass_and_clears_empty_fallback", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"","extra_content":{"google":{"thought_signature":"   "}}}]}]}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("thoughtSignature = %q, want bypass sentinel; out=%s", got, out)
		}
		if ojson.GetBytes(out, "contents.0.parts.0.extra_content.google.thought_signature").Exists() {
			t.Fatalf("empty fallback field should be removed; out=%s", out)
		}
	})

	t.Run("text_part_strips_all_empty_signature_fields", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[{"text":"answer","thoughtSignature":"","thought_signature":null}]}]}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").Exists() ||
			ojson.GetBytes(out, "contents.0.parts.0.thought_signature").Exists() {
			t.Fatalf("empty signature fields on text part should be stripped; out=%s", out)
		}
	})

	t.Run("function_response_strips_all_empty_signature_fields", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"ok"}},"thoughtSignature":"","extra_content":{"google":{"thought_signature":""}}}]}]}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		if ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").Exists() ||
			ojson.GetBytes(out, "contents.0.parts.0.extra_content.google.thought_signature").Exists() {
			t.Fatalf("empty signature fields on functionResponse should be stripped; out=%s", out)
		}
	})
}

func TestSanitizeGeminiRequestThoughtSignatures_PrunesEmptyExtraContentShell(t *testing.T) {
	sig := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})

	t.Run("function_call_prunes_empty_extra_content_shell", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{}},"extra_content":{"google":{"thought_signature":"` + sig + `"}}}]}]}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")
		part := ojson.GetBytesNoCopy(out, "contents.0.parts.0")
		if got := part.Get("thoughtSignature").String(); got != sig {
			t.Fatalf("thoughtSignature = %q, want %q; part=%s", got, sig, part.Raw)
		}
		if part.Get("extra_content").Exists() {
			t.Fatalf("empty extra_content shell should be pruned; part=%s", part.Raw)
		}
	})

	t.Run("function_response_prunes_empty_extra_content_shell", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"ok"}},"extra_content":{"google":{"thought_signature":"` + sig + `"}}}]}]}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")
		part := ojson.GetBytesNoCopy(out, "contents.0.parts.0")
		if part.Get("extra_content").Exists() {
			t.Fatalf("empty extra_content shell on functionResponse should be pruned; part=%s", part.Raw)
		}
	})

	t.Run("preserves_non_empty_sibling_fields", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[{"text":"a","extra_content":{"google":{"thought_signature":"` + sig + `","keep":"yes"}}},{"text":"b","extra_content":{"google":{"thought_signature":"` + sig + `"},"keep":"yes"}}]}]}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		p0 := ojson.GetBytesNoCopy(out, "contents.0.parts.0")
		if p0.Get("extra_content.google.thought_signature").Exists() || p0.Get("extra_content.google.keep").String() != "yes" {
			t.Fatalf("p0 should strip signature but preserve non-empty extra_content.google sibling: %s", p0.Raw)
		}
		p1 := ojson.GetBytesNoCopy(out, "contents.0.parts.1")
		if p1.Get("extra_content.google").Exists() || p1.Get("extra_content.keep").String() != "yes" {
			t.Fatalf("p1 should prune empty google but preserve extra_content sibling: %s", p1.Raw)
		}
	})

	t.Run("duplicate_google_and_extra_content_wrappers_pruned_and_siblings_preserved", func(t *testing.T) {
		input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{}},"extra_content":{"google":{"thought_signature":"` + sig + `"},"google":{}}},{"text":"two-google-sigs","extra_content":{"google":{"thought_signature":"` + sig + `"},"google":{"thought_signature":"` + sig + `"}}},{"text":"google-keep-and-sig","extra_content":{"google":{"keep":"yes"},"google":{"thought_signature":"` + sig + `"}}},{"text":"two-extra-sigs","extra_content":{"google":{"thought_signature":"` + sig + `"}},"extra_content":{"google":{"thought_signature":"` + sig + `"}}},{"text":"extra-keep-and-sig","extra_content":{"google":{"thought_signature":"` + sig + `"},"keep":"yes"},"extra_content":{"google":{"thought_signature":"` + sig + `"}}}]}]}`)
		out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

		p0 := ojson.GetBytesNoCopy(out, "contents.0.parts.0")
		if p0.Get("thoughtSignature").String() != sig || p0.Get("extra_content").Exists() || strings.Contains(p0.Raw, "thought_signature") {
			t.Fatalf("p0 should normalize signature and prune duplicate empty google wrapper: %s", p0.Raw)
		}
		for _, idx := range []int{1, 3} {
			p := ojson.GetBytesNoCopy(out, fmt.Sprintf("contents.0.parts.%d", idx))
			if p.Get("thoughtSignature").String() != sig || p.Get("extra_content").Exists() || strings.Contains(p.Raw, "thought_signature") {
				t.Fatalf("p%d left extra_content or thought_signature: %s", idx, p.Raw)
			}
		}
		p2 := ojson.GetBytesNoCopy(out, "contents.0.parts.2")
		if p2.Get("thoughtSignature").String() != sig || strings.Contains(p2.Raw, "thought_signature") || p2.Get("extra_content.google.keep").String() != "yes" || strings.Count(p2.Raw, `"google"`) != 1 {
			t.Fatalf("p2 should strip duplicate google sig and keep non-empty google sibling: %s", p2.Raw)
		}
		p4 := ojson.GetBytesNoCopy(out, "contents.0.parts.4")
		if p4.Get("thoughtSignature").String() != sig || strings.Contains(p4.Raw, "thought_signature") || strings.Contains(p4.Raw, `"google"`) || p4.Get("extra_content.keep").String() != "yes" || strings.Count(p4.Raw, `"extra_content"`) != 1 {
			t.Fatalf("p4 should prune empty duplicate extra_content and keep sibling: %s", p4.Raw)
		}
	})
}
