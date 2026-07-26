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
