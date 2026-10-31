package claude

import (
	"errors"
	"fmt"
	"testing"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"

	"github.com/tidwall/gjson"
)

const userTurnUpload = `{"type":"container_upload","file_id":"file-1"}`

func TestConvertClaudeRequestToAntigravity_RefusesAnyEmptiedUserTurn(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantType string
	}{
		{
			name:     "history then attachment only",
			input:    `{"model":"m","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"hi"}]},{"role":"user","content":[` + userTurnUpload + `]}]}`,
			wantType: "container_upload",
		},
		{
			name:     "system prompt and system reminder",
			input:    `{"model":"m","system":"sys","messages":[{"role":"system","content":"reminder"},{"role":"user","content":[` + userTurnUpload + `]}]}`,
			wantType: "container_upload",
		},
		{
			name:     "emptied turn before a later text turn",
			input:    `{"model":"m","messages":[{"role":"user","content":[` + userTurnUpload + `]},{"role":"assistant","content":[{"type":"text","text":"ok"}]},{"role":"user","content":"next"}]}`,
			wantType: "container_upload",
		},
		{
			name:     "document without bytes",
			input:    `{"model":"m","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file-1"}}]}]}`,
			wantType: "document",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := convertClaudeRequestToAntigravity("m", []byte(tc.input), false)
			var unsupported *translatorcommon.UnsupportedPartError
			if !errors.As(err, &unsupported) || unsupported.Type != tc.wantType || unsupported.StatusCode() != 400 {
				t.Fatalf("err = %v, want unsupported content part: %s; body = %s", err, tc.wantType, body)
			}
			if got, want := err.Error(), "unsupported content part: "+tc.wantType; got != want {
				t.Fatalf("error text = %q, want %q", got, want)
			}
			if !gjson.ValidBytes(body) {
				t.Fatalf("body is not JSON: %q", body)
			}
		})
	}
}

func TestConvertClaudeRequestToAntigravity_KeepsTurnWithTextBesideAttachment(t *testing.T) {
	input := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":[{"type":"text","text":"keep me"},` + userTurnUpload + `]}]}`)
	body, err := convertClaudeRequestToAntigravity("m", input, false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := gjson.GetBytes(body, "request.contents.2.parts.0.text").String(); got != "keep me" {
		t.Fatalf("text was lost: %s", body)
	}
}

func TestConvertClaudeRequestToAntigravity_Base64DocumentBecomesInlineData(t *testing.T) {
	input := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQK"}}]}]}`)
	body, err := convertClaudeRequestToAntigravity("m", input, false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	inlineData := gjson.GetBytes(body, "request.contents.2.parts.0.inlineData")
	if inlineData.Get("mimeType").String() != "application/pdf" || inlineData.Get("data").String() != "JVBERi0xLjQK" {
		t.Fatalf("inlineData = %s, body = %s", inlineData.Raw, body)
	}
}

func TestConvertClaudeRequestToAntigravity_ExportedWrapperKeepsAJSONBody(t *testing.T) {
	input := []byte(`{"model":"m","messages":[{"role":"user","content":[` + userTurnUpload + `]}]}`)
	if body, _ := ConvertClaudeRequestToAntigravity("m", input, false); !gjson.ValidBytes(body) {
		t.Fatalf("body is not JSON: %q", body)
	}
}

func TestClaudeToAntigravityRegistryCarriesTheRefusal(t *testing.T) {
	input := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":[` + userTurnUpload + `]}]}`)
	envelope := newRequestEnvelope(ConvertClaudeRequestToAntigravity("m", input, false))
	var unsupported *translatorcommon.UnsupportedPartError
	if !errors.As(envelope.Err, &unsupported) || unsupported.Type != "container_upload" {
		t.Fatalf("envelope.Err = %v, body = %s", envelope.Err, envelope.Body)
	}
}

// claudeImageTurnToAntigravity translates a conversation of two history turns followed by a final user
// turn holding the given content parts.
func claudeImageTurnToAntigravity(finalContent string) requestEnvelope {
	input := `{"model":"m","messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":[` + finalContent + `]}]}`
	return newRequestEnvelope(ConvertClaudeRequestToAntigravity("m", []byte(input), false))
}

func requireImageRefusal(t *testing.T, envelope requestEnvelope) {
	t.Helper()
	var unsupported *translatorcommon.UnsupportedPartError
	if !errors.As(envelope.Err, &unsupported) || unsupported.Type != "image" || unsupported.StatusCode() != 400 {
		t.Fatalf("envelope.Err = %v, want unsupported content part: image; body = %s", envelope.Err, envelope.Body)
	}
	if got, want := envelope.Err.Error(), "unsupported content part: image"; got != want {
		t.Fatalf("error text = %q, want %q", got, want)
	}
}

func TestClaudeToAntigravityUnrepresentableImageOnlyTurnIsRefused(t *testing.T) {
	cases := map[string]string{
		"url source":  `{"type":"url","url":"https://example.test/a.png"}`,
		"file source": `{"type":"file","file_id":"file-1"}`,
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			requireImageRefusal(t, claudeImageTurnToAntigravity(`{"type":"image","source":`+source+`}`))
		})
	}
}

func TestClaudeToAntigravityUnrepresentableImageBesideTextStillSucceeds(t *testing.T) {
	for _, source := range []string{`{"type":"url","url":"https://example.test/a.png"}`, `{"type":"file","file_id":"file-1"}`} {
		envelope := claudeImageTurnToAntigravity(`{"type":"text","text":"keep me"},{"type":"image","source":` + source + `}`)
		if envelope.Err != nil {
			t.Fatalf("envelope.Err = %v, body = %s", envelope.Err, envelope.Body)
		}
		parts := gjson.GetBytes(envelope.Body, "request.contents.2.parts").Array()
		if len(parts) != 1 || parts[0].Get("text").String() != "keep me" {
			t.Fatalf("text was lost or an image leaked: %s", envelope.Body)
		}
	}
}

func TestClaudeToAntigravityEmptyTextDoesNotHideAnUnrepresentableImage(t *testing.T) {
	requireImageRefusal(t, claudeImageTurnToAntigravity(`{"type":"text","text":""},{"type":"image","source":{"type":"url","url":"https://example.test/a.png"}}`))
}

func TestClaudeToAntigravityBase64ImageOnlyTurnStaysInlineData(t *testing.T) {
	envelope := claudeImageTurnToAntigravity(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}`)
	if envelope.Err != nil {
		t.Fatalf("envelope.Err = %v, body = %s", envelope.Err, envelope.Body)
	}
	inlineData := gjson.GetBytes(envelope.Body, "request.contents.2.parts.0.inlineData")
	if inlineData.Get("mimeType").String() != "image/png" || inlineData.Get("data").String() != "aGVsbG8=" {
		t.Fatalf("inlineData = %s, body = %s", inlineData.Raw, envelope.Body)
	}
}

func TestClaudeToAntigravityWhitespaceTextDoesNotHideAnUnrepresentableImage(t *testing.T) {
	for _, text := range []string{"  ", ` \n\t`, `\u00a0`} {
		requireImageRefusal(t, claudeImageTurnToAntigravity(`{"type":"text","text":"`+text+`"},{"type":"image","source":{"type":"file","file_id":"file-1"}}`))
	}
	requireImageRefusal(t, claudeImageTurnToAntigravity(`{"type":"image","source":{"type":"url","url":"https://example.test/a.png"}},{"type":"text","text":"  "}`))
}

func TestClaudeToAntigravityRealTextBesideWhitespaceAndAnImageStillSucceeds(t *testing.T) {
	envelope := claudeImageTurnToAntigravity(`{"type":"text","text":"  "},{"type":"text","text":"keep me"},{"type":"image","source":{"type":"file","file_id":"file-1"}}`)
	if envelope.Err != nil {
		t.Fatalf("envelope.Err = %v, body = %s", envelope.Err, envelope.Body)
	}
	found := false
	for _, part := range gjson.GetBytes(envelope.Body, "request.contents.2.parts").Array() {
		if part.Get("text").String() == "keep me" {
			found = true
		}
	}
	if !found {
		t.Fatalf("text was lost: %s", envelope.Body)
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

func TestConvertClaudeRequestToAntigravity_PreservesToolResultErrors(t *testing.T) {
	tests := []struct {
		name         string
		fields       string
		wantResponse string
		wantImage    bool
	}{
		{
			name:         "success_without_flag",
			fields:       `"content":"permission denied"`,
			wantResponse: `{"result":"permission denied"}`,
		},
		{
			name:         "explicit_success",
			fields:       `"is_error":false,"content":{"ok":true}`,
			wantResponse: `{"result":{"ok":true}}`,
		},
		{
			name:         "non_bool_is_error_string_treated_as_success",
			fields:       `"is_error":"true","content":"permission denied"`,
			wantResponse: `{"result":"permission denied"}`,
		},
		{
			name:         "non_bool_is_error_number_treated_as_success",
			fields:       `"is_error":1,"content":"permission denied"`,
			wantResponse: `{"result":"permission denied"}`,
		},
		{
			name:         "failed_string",
			fields:       `"is_error":true,"content":"permission denied"`,
			wantResponse: `{"error":"permission denied"}`,
		},
		{
			name:         "failed_structured_result",
			fields:       `"is_error":true,"content":{"code":"EACCES","retryable":false}`,
			wantResponse: `{"error":{"code":"EACCES","retryable":false}}`,
		},
		{
			name:         "failed_text_and_image",
			fields:       `"is_error":true,"content":[{"type":"text","text":"permission denied"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]`,
			wantResponse: `{"error":{"type":"text","text":"permission denied"}}`,
			wantImage:    true,
		},
		{
			name:         "failed_image_only_array",
			fields:       `"is_error":true,"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]`,
			wantResponse: `{"error":""}`,
			wantImage:    true,
		},
		{
			name:         "failed_single_image_object",
			fields:       `"is_error":true,"content":{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}`,
			wantResponse: `{"error":""}`,
			wantImage:    true,
		},
		{
			name:         "failed_multiple_blocks",
			fields:       `"is_error":true,"content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]`,
			wantResponse: `{"error":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}`,
		},
		{
			name:         "failed_without_content",
			fields:       `"is_error":true`,
			wantResponse: `{"error":""}`,
		},
		{
			name:         "failed_schema_ref_remains_opaque",
			fields:       `"is_error":true,"content":{"$ref":"#/definitions/Error"}`,
			wantResponse: `{"error":"{\"$ref\":\"#/definitions/Error\"}"}`,
		},
	}

	for _, stream := range []bool{false, true} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tt.name), func(t *testing.T) {
				input := []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-5","messages":[
					{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Read","input":{}}]},
					{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1",%s}]}
				]}`, tt.fields))
				output, errTranslate := ConvertClaudeRequestToAntigravity("claude-sonnet-4-5", input, stream)
				if errTranslate != nil {
					t.Fatal(errTranslate)
				}
				if !gjson.ValidBytes(output) {
					t.Fatalf("invalid translated JSON: %s", output)
				}
				response := gjson.GetBytes(output, "request.contents.1.parts.0.functionResponse")
				if response.Get("id").String() != "call_1" || response.Get("name").String() != "Read" {
					t.Fatalf("tool result lost its call identity: %s", response.Raw)
				}
				if got := response.Get("response").Raw; got != tt.wantResponse {
					t.Fatalf("functionResponse.response = %s, want %s", got, tt.wantResponse)
				}
				if tt.wantImage {
					if got := response.Get("parts.0.inlineData").Raw; got != `{"mimeType":"image/png","data":"aGVsbG8="}` {
						t.Fatalf("functionResponse.parts[0].inlineData = %s", got)
					}
				} else if response.Get("parts").Exists() {
					t.Fatalf("unexpected functionResponse.parts: %s", response.Get("parts").Raw)
				}
			})
		}
	}
}
