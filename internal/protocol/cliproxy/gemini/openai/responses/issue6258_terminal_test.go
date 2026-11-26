package responses

import (
	"context"
	"fmt"
	"testing"

	"github.com/MoeForge/moego/ojson"

	"ccLoad/internal/protocol/cliproxy/util"
)

const issue6258Usage = `{"promptTokenCount":100,"candidatesTokenCount":10,"thoughtsTokenCount":50,"totalTokenCount":160,"cachedContentTokenCount":7}`

func issue6258Translate(t *testing.T, param *any, frame string) []ojson.Result {
	t.Helper()
	var events []ojson.Result
	for _, chunk := range ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-3.7-flash", nil, nil, []byte(frame), param) {
		_, event := parseSSEEvent(t, chunk)
		events = append(events, event)
	}
	return events
}

func issue6258Terminal(t *testing.T, events []ojson.Result) ojson.Result {
	t.Helper()
	var terminal ojson.Result
	terminals, created, inProgress := 0, 0, 0
	previous := int64(-1)
	added := map[string]ojson.Result{}
	done := map[string]int{}
	responseID := ""
	for _, event := range events {
		seq := event.Get("sequence_number").Int()
		if seq <= previous {
			t.Errorf("sequence_number=%d after %d", seq, previous)
		}
		previous = seq
		if id := event.Get("response.id").String(); id != "" {
			if responseID != "" && id != responseID {
				t.Errorf("response ID changed: %q -> %q", responseID, id)
			}
			responseID = id
		}
		switch event.Get("type").String() {
		case "response.created":
			created++
		case "response.in_progress":
			inProgress++
		case "response.output_item.added":
			added[event.Get("item.id").String()] = event
		case "response.output_item.done":
			id := event.Get("item.id").String()
			done[id]++
			start, ok := added[id]
			if !ok || start.Get("output_index").Int() != event.Get("output_index").Int() {
				t.Errorf("item identity/index changed: %s", event.Raw)
			}
		case "response.completed", "response.incomplete", "response.failed":
			terminals++
			terminal = event
		}
	}
	if terminals != 1 {
		t.Errorf("terminal count=%d, want exactly 1", terminals)
	}
	if created != 1 || inProgress != 1 {
		t.Errorf("created/in_progress counts=%d/%d, want 1/1", created, inProgress)
	}
	for id := range added {
		if done[id] != 1 {
			t.Errorf("item %s done count=%d, want exactly 1", id, done[id])
		}
	}
	for index, item := range terminal.Get("response.output").Array() {
		start, ok := added[item.Get("id").String()]
		if !ok || start.Get("output_index").Int() != int64(index) {
			t.Errorf("terminal output identity/index changed: %s", item.Raw)
		}
	}
	return terminal.Get("response")
}

func issue6258AssertUsage(t *testing.T, response ojson.Result, input, output, reasoning, total, cached int64) {
	t.Helper()
	for path, want := range map[string]int64{
		"input_tokens": input, "output_tokens": output, "output_tokens_details.reasoning_tokens": reasoning,
		"total_tokens": total, "input_tokens_details.cached_tokens": cached,
	} {
		got := response.Get("usage." + path)
		if !got.Exists() || got.Int() != want {
			t.Errorf("usage.%s=%s, want %d; usage=%s", path, got.Raw, want, response.Get("usage").Raw)
		}
	}
}

func TestIssue6258GeminiResponsesSplitStopUsage(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		for _, usageKey := range []string{"usageMetadata", "cpaUsageMetadata"} {
			t.Run(fmt.Sprintf("wrapped=%t/%s", wrapped, usageKey), func(t *testing.T) {
				frame := func(body string) string {
					if wrapped {
						return `data: {"response":` + body + `}`
					}
					return "data: " + body
				}
				var param any
				events := issue6258Translate(t, &param, frame(`{"responseId":"split-stop","candidates":[{"content":{"parts":[{"text":"answer"}]}}]}`))
				finish := frame(`{"candidates":[{"content":{"parts":[{"text":" tail"}]},"finishReason":"STOP"}]}`)
				beforeUsage := issue6258Translate(t, &param, finish)
				for _, event := range beforeUsage {
					if event.Get("type").String() == "response.completed" {
						t.Error("STOP without usage emitted a terminal before the usage tail")
					}
				}
				events = append(events, beforeUsage...)
				repeatedFinish := issue6258Translate(t, &param, frame(`{"candidates":[{"finishReason":"STOP"}]}`))
				for _, event := range repeatedFinish {
					if event.Get("type").String() == "response.completed" {
						t.Error("repeated pending STOP emitted a terminal before usage")
					}
				}
				events = append(events, repeatedFinish...)
				afterUsage := issue6258Translate(t, &param, frame(`{"`+usageKey+`":`+issue6258Usage+`}`))
				tailTerminals := 0
				for _, event := range afterUsage {
					if event.Get("type").String() == "response.completed" {
						tailTerminals++
					}
				}
				if tailTerminals != 1 {
					t.Errorf("usage tail emitted %d terminals, want 1", tailTerminals)
				}
				events = append(events, afterUsage...)
				for _, repeated := range []string{finish, "data: [DONE]", "data: [DONE]"} {
					events = append(events, issue6258Translate(t, &param, repeated)...)
				}
				response := issue6258Terminal(t, events)
				if response.Get("status").String() != "completed" || response.Get("output.0.content.0.text").String() != "answer tail" {
					t.Errorf("lost finish-frame content or wrong status: %s", response.Raw)
				}
				issue6258AssertUsage(t, response, 100, 60, 50, 160, 7)
			})
		}
	}
}

func TestIssue6258GeminiResponsesMaxTokens(t *testing.T) {
	for _, mode := range []string{"same_frame", "split_usage", "no_usage_clean_done"} {
		t.Run("stream/"+mode, func(t *testing.T) {
			var param any
			events := issue6258Translate(t, &param, `{"responseId":"max","candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`)
			finish := `{"candidates":[{"finishReason":"MAX_TOKENS"}]`
			if mode == "same_frame" {
				finish += `,"usageMetadata":` + issue6258Usage
			}
			events = append(events, issue6258Translate(t, &param, finish+`}`)...)
			if mode == "split_usage" {
				events = append(events, issue6258Translate(t, &param, `{"usageMetadata":`+issue6258Usage+`}`)...)
			}
			for _, frame := range []string{"[DONE]", "[DONE]", finish + `}`} {
				events = append(events, issue6258Translate(t, &param, frame)...)
			}
			response := issue6258Terminal(t, events)
			for _, event := range events {
				kind := event.Get("type").String()
				if kind == "response.completed" || kind == "response.failed" {
					t.Errorf("MAX_TOKENS emitted %s, want response.incomplete", kind)
				}
			}
			issue6258AssertIncomplete(t, response)
			if mode != "no_usage_clean_done" {
				issue6258AssertUsage(t, response, 100, 60, 50, 160, 7)
			}
		})
	}
	t.Run("nonstream", func(t *testing.T) {
		raw := `{"responseId":"max","candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":` + issue6258Usage + `}`
		response := util.ParseGJSONBytesNoCopy(ConvertGeminiResponseToOpenAIResponsesNonStream(context.Background(), "gemini-3.7-flash", nil, nil, []byte(raw), nil))
		issue6258AssertIncomplete(t, response)
		issue6258AssertUsage(t, response, 100, 60, 50, 160, 7)
	})
}

func issue6258AssertIncomplete(t *testing.T, response ojson.Result) {
	t.Helper()
	if response.Get("status").String() != "incomplete" || response.Get("incomplete_details.reason").String() != "max_output_tokens" {
		t.Errorf("MAX_TOKENS status=%q details=%s, want incomplete/max_output_tokens", response.Get("status").String(), response.Get("incomplete_details").Raw)
	}
	if response.Get("output.0.status").String() != "incomplete" || response.Get("output.0.content.0.text").String() != "partial" {
		t.Errorf("partial message must be preserved and incomplete: %s", response.Get("output").Raw)
	}
}

func TestIssue6258GeminiResponsesUsageSnapshots(t *testing.T) {
	for _, test := range []struct {
		name, finishUsage        string
		output, reasoning, total int64
	}{
		{"absent_preserves", `{"candidatesTokenCount":12,"totalTokenCount":182}`, 62, 50, 182},
		{"explicit_zero_replaces", `{"candidatesTokenCount":12,"thoughtsTokenCount":0,"totalTokenCount":0}`, 12, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var param any
			var events []ojson.Result
			for _, frame := range []string{
				`{"responseId":"snapshots","candidates":[{"content":{"parts":[{"text":"answer"}]}}],"usageMetadata":` + issue6258Usage + `}`,
				`{"usageMetadata":` + issue6258Usage + `}`,
				`{"usageMetadata":{"promptTokenCount":120,"cachedContentTokenCount":0},"cpaUsageMetadata":{"promptTokenCount":999}}`,
				`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":` + test.finishUsage + `}`,
				`[DONE]`,
			} {
				events = append(events, issue6258Translate(t, &param, frame)...)
			}
			issue6258AssertUsage(t, issue6258Terminal(t, events), 120, test.output, test.reasoning, test.total, 0)
		})
	}
}

func TestGeminiResponsesWaitsForValidFinishAndUsage(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapped=%t", wrapped), func(t *testing.T) {
			var state any
			var events []ojson.Result
			for i, frame := range []string{
				`{"responseId":"nullable-usage","candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":" "}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":7,"thoughtsTokenCount":2}}`,
				`{"candidates":[{"finishReason":false}],"usageMetadata":{"cachedContentTokenCount":null,"thoughtsTokenCount":"9"}}`,
				`{"candidates":[{"finishReason":"MAX_TOKENS"}],"usageMetadata":null}`,
				`{"usageMetadata":{},"cpaUsageMetadata":{"candidatesTokenCount":4,"totalTokenCount":16}}`,
				"[DONE]", "[DONE]",
			} {
				if wrapped && frame != "[DONE]" {
					frame = `{"response":` + frame + `}`
				}
				tail := issue6258Translate(t, &state, "data: "+frame)
				for _, event := range tail {
					if event.Get("response.status").Str != "" && event.Get("response.status").Str != "in_progress" && i != 3 {
						t.Fatalf("terminal at frame %d: %s", i, event.Raw)
					}
				}
				events = append(events, tail...)
			}
			response := issue6258Terminal(t, events)
			issue6258AssertIncomplete(t, response)
			issue6258AssertUsage(t, response, 10, 6, 2, 16, 7)
		})
	}
}

func TestGeminiResponsesNonStreamNullableUsage(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		wantUsage   bool
	}{
		{"null", `"usageMetadata":null`, false},
		{"no counts", `"usageMetadata":{"totalTokenCount":null}`, false},
		{"cpa fallback", `"usageMetadata":null,"cpaUsageMetadata":{"totalTokenCount":0}`, true},
		{"zero", `"usageMetadata":{"totalTokenCount":0}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],` + tc.usage + `}`
			response := ojson.ParseBytes(ConvertGeminiResponseToOpenAIResponsesNonStream(t.Context(), "gemini-3.7-flash", nil, nil, []byte(raw), nil))
			total := response.Get("usage.total_tokens")
			if tc.wantUsage {
				if total.Type != ojson.Number || total.Int() != 0 {
					t.Errorf("expected numeric zero: %s", response.Raw)
				}
			} else if response.Get("usage").Exists() {
				t.Errorf("unexpected usage: %s", response.Raw)
			}
		})
	}
}

func TestIssue6258GeminiResponsesMaxTokensOnlyActiveMessageIncomplete(t *testing.T) {
	var param any
	var events []ojson.Result
	for _, frame := range []string{
		`{"responseId":"mixed","candidates":[{"content":{"parts":[{"text":"preface"}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call-1","name":"lookup","args":{"key":"value"}}}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":` + issue6258Usage + `}`,
		`[DONE]`,
	} {
		events = append(events, issue6258Translate(t, &param, frame)...)
	}
	response := issue6258Terminal(t, events)
	items := response.Get("output").Array()
	if len(items) != 3 {
		t.Fatalf("output count=%d, want preface/tool/active message: %s", len(items), response.Raw)
	}
	for index, want := range []string{"completed", "completed", "incomplete"} {
		if got := items[index].Get("status").String(); got != want {
			t.Errorf("output[%d].status=%q, want %q", index, got, want)
		}
		for _, event := range events {
			if event.Get("type").String() == "response.output_item.done" && event.Get("output_index").Int() == int64(index) && event.Get("item.status").String() != want {
				t.Errorf("output_item.done[%d].status=%q, want %q", index, event.Get("item.status").String(), want)
			}
		}
	}
}

func TestIssue6258GeminiResponsesDoneUsesUsageSnapshot(t *testing.T) {
	for _, usageKey := range []string{"usageMetadata", "cpaUsageMetadata"} {
		t.Run(usageKey, func(t *testing.T) {
			var param any
			var events []ojson.Result
			for _, frame := range []string{
				`{"responseId":"snapshot-done","candidates":[{"content":{"parts":[{"text":"answer"}]}}],"` + usageKey + `":` + issue6258Usage + `}`,
				`{"` + usageKey + `":` + issue6258Usage + `}`,
				`{"candidates":[{"finishReason":"STOP"}]}`,
				`[DONE]`, `[DONE]`,
			} {
				events = append(events, issue6258Translate(t, &param, frame)...)
			}
			issue6258AssertUsage(t, issue6258Terminal(t, events), 100, 60, 50, 160, 7)
		})
	}
}

func TestIssue6258GeminiResponsesSameFrameControl(t *testing.T) {
	var param any
	events := issue6258Translate(t, &param, `{"responseId":"control","candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":`+issue6258Usage+`}`)
	events = append(events, issue6258Translate(t, &param, "[DONE]")...)
	response := issue6258Terminal(t, events)
	issue6258AssertUsage(t, response, 100, 60, 50, 160, 7)
}
