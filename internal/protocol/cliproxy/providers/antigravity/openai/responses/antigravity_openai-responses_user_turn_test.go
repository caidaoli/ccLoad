package responses

import (
	"errors"
	"strings"
	"testing"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"

	"github.com/tidwall/gjson"
)

const (
	antigravityTurnHello     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`
	antigravityTurnAssistant = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}`
	antigravityTurnNext      = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	antigravityTurnDeveloper = `{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev"}]}`
	antigravityTurnFileID    = `{"type":"input_file","file_id":"file-1"}`
	antigravityTurnFileData  = `{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}`
	antigravityTurnNoFile    = `{"type":"input_file","filename":"a.pdf"}`
	antigravityTurnText      = `{"type":"input_text","text":"keep me"}`
)

func antigravityUserTurn(parts ...string) string {
	return `{"type":"message","role":"user","content":[` + strings.Join(parts, ",") + `]}`
}

func antigravityTurnPayload(extra string, items ...string) []byte {
	return []byte(`{"model":"gemini-3-flash",` + extra + `"input":[` + strings.Join(items, ",") + `]}`)
}

func TestOpenAIResponsesToAntigravityRegistryCarriesTheRefusal(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
	}{
		{
			name:  "history then file id only",
			input: antigravityTurnPayload("", antigravityTurnHello, antigravityTurnAssistant, antigravityUserTurn(antigravityTurnNoFile)),
		},
		{
			name:  "instructions and developer prompt do not hide the empty turn",
			input: antigravityTurnPayload(`"instructions":"sys",`, antigravityTurnDeveloper, antigravityUserTurn(antigravityTurnNoFile)),
		},
		{
			name:  "emptied turn before a later text turn",
			input: antigravityTurnPayload("", antigravityUserTurn(antigravityTurnNoFile), antigravityTurnAssistant, antigravityTurnNext),
		},
		{
			name:  "native web search request",
			input: antigravityTurnPayload(`"tools":[{"type":"web_search"}],`, antigravityTurnHello, antigravityTurnAssistant, antigravityUserTurn(antigravityTurnNoFile)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := ConvertOpenAIResponsesRequestToAntigravity("gemini-3-flash", tc.input, false)
			var unsupported *translatorcommon.UnsupportedPartError
			if !errors.As(err, &unsupported) || unsupported.Type != "input_file" || unsupported.StatusCode() != 400 {
				t.Fatalf("err = %v, want unsupported content part: input_file; body = %s", err, body)
			}
			if !gjson.ValidBytes(body) {
				t.Fatalf("body is not JSON: %q", body)
			}
		})
	}
}

func TestOpenAIResponsesToAntigravityKeepsTurnWithTextBesideFileID(t *testing.T) {
	input := antigravityTurnPayload("", antigravityTurnHello, antigravityTurnAssistant, antigravityUserTurn(antigravityTurnText, antigravityTurnFileID))
	envelope := newRequestEnvelope(ConvertOpenAIResponsesRequestToAntigravity("gemini-3-flash", input, false))
	if envelope.Err != nil {
		t.Fatalf("envelope.Err = %v", envelope.Err)
	}
	if got := gjson.GetBytes(envelope.Body, "request.contents.2.parts.0.text").String(); got != "keep me" {
		t.Fatalf("text was lost: %s", envelope.Body)
	}
}

func TestOpenAIResponsesToAntigravityInlineFileStaysInlineData(t *testing.T) {
	input := antigravityTurnPayload("", antigravityTurnHello, antigravityTurnAssistant, antigravityUserTurn(antigravityTurnFileData))
	envelope := newRequestEnvelope(ConvertOpenAIResponsesRequestToAntigravity("gemini-3-flash", input, false))
	if envelope.Err != nil {
		t.Fatalf("envelope.Err = %v", envelope.Err)
	}
	if got := gjson.GetBytes(envelope.Body, "request.contents.2.parts.0.inline_data.mime_type").String(); got != "application/pdf" {
		t.Fatalf("inline file was not kept: %s", envelope.Body)
	}
}

func TestConvertOpenAIResponsesRequestToAntigravity_ExportedWrapperKeepsAJSONBody(t *testing.T) {
	input := antigravityTurnPayload("", antigravityUserTurn(antigravityTurnFileID))
	if body, _ := ConvertOpenAIResponsesRequestToAntigravity("gemini-3-flash", input, false); !gjson.ValidBytes(body) {
		t.Fatalf("body is not JSON: %q", body)
	}
}

// requestEnvelope pairs a converted body with its conversion error, standing in
// for the upstream SDK envelope these tests were written against.
type requestEnvelope struct {
	Body []byte
	Err  error
}

func newRequestEnvelope(body []byte, err error) requestEnvelope {
	return requestEnvelope{Body: body, Err: err}
}
