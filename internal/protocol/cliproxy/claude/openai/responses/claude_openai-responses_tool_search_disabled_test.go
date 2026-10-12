package responses

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// The dev golden was produced by running renderToolSearchGolden against the
// translator at origin/dev d739485df, before Codex tool_search support. With
// upstream.claude.tool-search off the translator must reproduce it exactly.
type toolSearchGoldenCase struct {
	Name    string          `json:"name"`
	Request json.RawMessage `json:"request"`
}

type toolSearchGoldenFixtures struct {
	Stream []string               `json:"stream"`
	Cases  []toolSearchGoldenCase `json:"cases"`
}

type toolSearchGoldenOutput struct {
	Request      string   `json:"request"`
	RequestError string   `json:"request_error,omitempty"`
	Stream       []string `json:"stream"`
	NonStream    string   `json:"non_stream"`
}

var toolSearchGoldenCreatedAt = regexp.MustCompile(`"created_at":\d+`)

// renderToolSearchGolden runs one fixture through the request translator and
// both response translators, masking the wall-clock created_at field.
func renderToolSearchGolden(fixtures toolSearchGoldenFixtures, c toolSearchGoldenCase) toolSearchGoldenOutput {
	normalize := func(raw []byte) string {
		return toolSearchGoldenCreatedAt.ReplaceAllString(string(raw), `"created_at":0`)
	}
	translated, errConvert := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-5", c.Request, true)
	result := toolSearchGoldenOutput{Request: string(translated)}
	if errConvert != nil {
		result.RequestError = errConvert.Error()
	}
	var param any
	for _, chunk := range fixtures.Stream {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", c.Request, translated, []byte(chunk), &param) {
			result.Stream = append(result.Stream, normalize(output))
		}
	}
	result.NonStream = normalize(ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", c.Request, translated, []byte(strings.Join(fixtures.Stream, "\n")), nil))
	return result
}

func loadToolSearchGolden(t *testing.T) (toolSearchGoldenFixtures, map[string]toolSearchGoldenOutput) {
	t.Helper()
	dir := filepath.Join("testdata", "tool_search_disabled")
	var fixtures toolSearchGoldenFixtures
	var golden map[string]toolSearchGoldenOutput
	for name, target := range map[string]any{"cases.json": &fixtures, "dev_golden.json": &golden} {
		raw, errRead := os.ReadFile(filepath.Join(dir, name))
		if errRead != nil {
			t.Fatalf("read %s: %v", name, errRead)
		}
		if errUnmarshal := json.Unmarshal(raw, target); errUnmarshal != nil {
			t.Fatalf("decode %s: %v", name, errUnmarshal)
		}
	}
	if len(fixtures.Cases) == 0 || len(golden) != len(fixtures.Cases) {
		t.Fatalf("golden has %d entries for %d cases", len(golden), len(fixtures.Cases))
	}
	return fixtures, golden
}

func TestToolSearchDisabledMatchesDevGolden(t *testing.T) {
	setClientToolSearchEnabledForTest(t, false)
	fixtures, golden := loadToolSearchGolden(t)
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			want, ok := golden[c.Name]
			if !ok {
				t.Fatalf("no dev golden for %s", c.Name)
			}
			got := renderToolSearchGolden(fixtures, c)
			if got.Request != want.Request || got.RequestError != want.RequestError {
				t.Fatalf("request differs from dev\n got: %s %s\nwant: %s %s", got.Request, got.RequestError, want.Request, want.RequestError)
			}
			if strings.Join(got.Stream, "\n") != strings.Join(want.Stream, "\n") {
				t.Fatalf("stream differs from dev\n got: %v\nwant: %v", got.Stream, want.Stream)
			}
			if got.NonStream != want.NonStream {
				t.Fatalf("non-stream differs from dev\n got: %s\nwant: %s", got.NonStream, want.NonStream)
			}
		})
	}
}

// With the switch on, the same fixtures must take the tool_search paths, so
// the golden comparison above cannot pass by accident.
func TestToolSearchEnabledDiffersFromDevGolden(t *testing.T) {
	setClientToolSearchEnabledForTest(t, true)
	fixtures, golden := loadToolSearchGolden(t)
	for _, c := range fixtures.Cases {
		if c.Name != "codex_session" {
			continue
		}
		if got := renderToolSearchGolden(fixtures, c); got.Request == golden[c.Name].Request {
			t.Fatal("codex_session translated as dev with tool search enabled")
		}
		return
	}
	t.Fatal("codex_session fixture missing")
}

// The compatibility entry point used for API-key compat models follows the
// pinned value too.
func TestToolSearchCompatContextFollowsPin(t *testing.T) {
	request := codexToolSearchRequest(codexUserTurn, codexSearchCall1, codexSearchOutput1)
	for _, tc := range []struct {
		global, pinned bool
	}{{global: false, pinned: true}, {global: true, pinned: false}} {
		setClientToolSearchEnabledForTest(t, tc.global)
		out, errConvert := ConvertOpenAIResponsesRequestToClaudeWithCompatContext(WithClientToolSearch(context.Background(), tc.pinned), "claude-sonnet-4-5", request, true)
		if errConvert != nil {
			t.Fatalf("convert: %v", errConvert)
		}
		if got := gjson.GetBytes(out, `tools.#(name=="tool_search")`).Exists(); got != tc.pinned {
			t.Fatalf("global=%v pinned=%v: tool_search declared = %v", tc.global, tc.pinned, got)
		}
	}
}
