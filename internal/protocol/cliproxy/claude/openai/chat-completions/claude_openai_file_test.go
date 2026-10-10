package chat_completions

import (
	"errors"
	"testing"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToClaude_FileParts(t *testing.T) {
	const (
		text    = `{"type":"text","text":"read it"}`
		audio   = `{"type":"input_audio","input_audio":{"format":"wav","data":"UklGRg=="}}`
		missing = `{"type":"file","file":{"file_id":"file-absent"}}`
		inline  = `{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}}`
		system  = `{"role":"system","content":"be brief"},`
	)
	cases := []struct {
		name, prefix, content string
		wantErr               string
		wantTypes             string
	}{
		{name: "unknown file id alone", content: missing, wantErr: "file"},
		{name: "audio alone", content: audio, wantErr: "input_audio"},
		{name: "a system prompt does not hide the empty turn", prefix: system, content: missing, wantErr: "file"},
		{name: "text beside an unknown file id", content: text + "," + missing, wantTypes: `["text"]`},
		{name: "text beside audio", content: text + "," + audio, wantTypes: `["text"]`},
		{name: "inline file data becomes a document", content: inline, wantTypes: `["document"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"model":"claude-sonnet-4","messages":[` + tc.prefix + `{"role":"user","content":[` + tc.content + `]}]}`
			got := newRequestEnvelope(ConvertOpenAIRequestToClaude("claude-sonnet-4", []byte(payload), false))

			var part *translatorcommon.UnsupportedPartError
			if tc.wantErr != "" {
				if !errors.As(got.Err, &part) || part.Type != tc.wantErr {
					t.Fatalf("err = %v, want unsupported %s", got.Err, tc.wantErr)
				}
				return
			}
			if got.Err != nil {
				t.Fatalf("err = %v", got.Err)
			}
			if types := gjson.GetBytes(got.Body, "messages.0.content.#.type").Raw; types != tc.wantTypes {
				t.Fatalf("content types = %s, want %s. Output: %s", types, tc.wantTypes, got.Body)
			}
		})
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
