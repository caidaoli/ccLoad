package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/cursorauth"
	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/cursorauth/sdkgen/sdk/v1/sdkv1connect"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"connectrpc.com/connect"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
)

/**
 * @brief 在 HTTP 代理边界重放逐轮用量和终态事件。
 */
type cursorUsageEventsRunner []cursorauth.Event

/**
 * @brief 为需要观察请求取消的 HTTP 回归提供可控 Runner 边界。
 */
type cursorRunnerFunc func(context.Context, *cursorauth.Credential, cursorauth.Request) (<-chan cursorauth.Event, error)

/**
 * @brief 执行回归场景定义的事件生产流程。
 * @param ctx 当前附着请求上下文。
 * @param credential 当前渠道凭证。
 * @param request 当前推理请求。
 * @return 场景生成的推理事件通道。
 * @return 场景定义的启动错误；nil 表示已开始生成事件。
 */
func (r cursorRunnerFunc) Run(ctx context.Context, credential *cursorauth.Credential, request cursorauth.Request) (<-chan cursorauth.Event, error) {
	return r(ctx, credential, request)
}

/**
 * @brief 返回预设事件，模拟 SDK 已解析的真实用量与工具回调。
 * @param ctx 当前请求上下文。
 * @param credential 当前渠道凭证。
 * @param request 当前请求。
 * @return 顺序包含全部预设事件的已关闭通道。
 * @return 始终为 nil，故障通过预设终态事件报告。
 */
func (r cursorUsageEventsRunner) Run(
	_ context.Context,
	_ *cursorauth.Credential,
	_ cursorauth.Request,
) (<-chan cursorauth.Event, error) {
	events := make(chan cursorauth.Event, len(r))
	for _, event := range r {
		events <- event
	}
	close(events)
	return events, nil
}

/**
 * @brief 核对协议展示用量与日志估算一致，并且真实用量字段保持为零。
 * @param t 测试上下文。
 * @param body HTTP JSON 响应或 SSE 事件流。
 * @param clientProtocol 客户端协议。
 * @param streaming 是否以 SSE 返回。
 * @param entry 当前请求的日志。
 * @param wantOutput 预期估算输出量。
 */
func assertCursorDisplayEstimate(t *testing.T, body []byte, clientProtocol string, streaming bool, entry *model.LogEntry, wantOutput int) {
	t.Helper()
	path, inputKey, outputKey := "usage", "input_tokens", "output_tokens"
	switch clientProtocol {
	case "openai":
		inputKey, outputKey = "prompt_tokens", "completion_tokens"
	case "responses":
		if streaming {
			path = "response.usage"
		}
	case "gemini":
		path, inputKey, outputKey = "usageMetadata", "promptTokenCount", "candidatesTokenCount"
	}
	var usage gjson.Result
	if streaming {
		for block := range bytes.SplitSeq(body, []byte("\n\n")) {
			_, data := parseSSEEventChunk(block)
			if value := gjson.GetBytes(data, path); value.IsObject() {
				usage = value
			}
		}
	} else {
		usage = gjson.GetBytes(body, path)
	}
	input, output := usage.Get(inputKey), usage.Get(outputKey)
	estimate := entry.UsageEstimate
	if estimate == nil || input.Type != gjson.Number || output.Type != gjson.Number || input.Int() <= 0 ||
		int(input.Int()) != estimate.InputTokens || int(output.Int()) != estimate.OutputTokens || estimate.OutputTokens != wantOutput {
		t.Fatalf("client usage=%s logged estimate=%+v want output=%d body=%s", usage.Raw, estimate, wantOutput, body)
	}
	if entry.InputTokens != 0 || entry.OutputTokens != 0 || entry.ReasoningTokens != 0 ||
		entry.CacheReadInputTokens != 0 || entry.CacheCreationInputTokens != 0 || entry.Cost != 0 {
		t.Fatalf("display estimate was billed: %+v", entry)
	}
}

/**
 * @brief Cursor 思考增量经四种 HTTP 协议返回时保留空白与 Unicode，并独立于正文计入展示估算。
 * @param t 测试上下文。
 */
func TestProxy_CursorOAuthThinkingAcrossProtocols(t *testing.T) {
	t.Parallel()
	for _, clientProtocol := range []string{"anthropic", "openai", "responses", "gemini"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", clientProtocol, streaming), func(t *testing.T) {
				t.Parallel()
				credentialJSON, err := (&cursorauth.Credential{APIKey: "test-key", AccessToken: "test-session"}).JSON()
				if err != nil {
					t.Fatalf("encode Cursor credential: %v", err)
				}
				env := setupProxyTestEnv(t, []testChannel{{
					name: "cursor-thinking", upstreamProtocol: "openai", models: "composer-2.5",
					authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
				}}, map[int]string{0: "https://unused.invalid"})
				env.server.cursorRunner = cursorUsageEventsRunner{
					{Ping: true}, {ThinkingDelta: "先"}, {ThinkingDelta: "\n  "}, {ThinkingDelta: "想🧠"},
					{Delta: "答", Text: "答"}, {Delta: "案", Text: "答案"}, {Done: true, Text: "答案", Usage: &cursorauth.Usage{}},
				}
				path := "/v1/chat/completions"
				body := map[string]any{
					"model": "composer-2.5", "stream": streaming, "max_tokens": 256,
					"messages": []any{map[string]any{"role": "user", "content": "hello"}},
				}
				switch clientProtocol {
				case "anthropic":
					path = "/v1/messages"
				case "responses":
					path = "/v1/responses"
					body = map[string]any{"model": "composer-2.5", "stream": streaming, "input": "hello"}
				case "gemini":
					path = "/v1beta/models/composer-2.5:generateContent"
					if streaming {
						path = "/v1beta/models/composer-2.5:streamGenerateContent?alt=sse"
					}
					body = map[string]any{"contents": []any{map[string]any{
						"role": "user", "parts": []any{map[string]any{"text": "hello"}},
					}}}
				}
				response := doProxyRequest(t, env.engine, path, body, nil)
				if response.Code != http.StatusOK {
					t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
				}
				var thinking, text strings.Builder
				if streaming {
					for block := range bytes.SplitSeq(response.Body.Bytes(), []byte("\n\n")) {
						eventType, data := parseSSEEventChunk(block)
						if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
							continue
						}
						if !gjson.ValidBytes(data) {
							t.Fatalf("invalid SSE JSON: %s", data)
						}
						payload := gjson.ParseBytes(data)
						switch clientProtocol {
						case "anthropic":
							if payload.Get("delta.type").String() == "thinking_delta" {
								thinking.WriteString(payload.Get("delta.thinking").String())
							} else if payload.Get("delta.type").String() == "text_delta" {
								text.WriteString(payload.Get("delta.text").String())
							}
						case "openai":
							thinking.WriteString(payload.Get("choices.0.delta.reasoning_content").String())
							text.WriteString(payload.Get("choices.0.delta.content").String())
						case "responses":
							switch eventType {
							case "response.reasoning_summary_text.delta":
								thinking.WriteString(payload.Get("delta").String())
							case "response.output_text.delta":
								text.WriteString(payload.Get("delta").String())
							}
						case "gemini":
							for _, part := range payload.Get("candidates.0.content.parts").Array() {
								if part.Get("thought").Bool() {
									thinking.WriteString(part.Get("text").String())
								} else {
									text.WriteString(part.Get("text").String())
								}
							}
						}
					}
				} else {
					if !gjson.ValidBytes(response.Body.Bytes()) {
						t.Fatalf("invalid response JSON: %s", response.Body.String())
					}
					payload := gjson.ParseBytes(response.Body.Bytes())
					switch clientProtocol {
					case "anthropic":
						for _, block := range payload.Get("content").Array() {
							if block.Get("type").String() == "thinking" {
								thinking.WriteString(block.Get("thinking").String())
							} else if block.Get("type").String() == "text" {
								text.WriteString(block.Get("text").String())
							}
						}
					case "openai":
						thinking.WriteString(payload.Get("choices.0.message.reasoning_content").String())
						text.WriteString(payload.Get("choices.0.message.content").String())
					case "responses":
						for _, item := range payload.Get("output").Array() {
							if item.Get("type").String() == "reasoning" {
								for _, summary := range item.Get("summary").Array() {
									thinking.WriteString(summary.Get("text").String())
								}
							} else if item.Get("type").String() == "message" {
								for _, content := range item.Get("content").Array() {
									text.WriteString(content.Get("text").String())
								}
							}
						}
					case "gemini":
						for _, part := range payload.Get("candidates.0.content.parts").Array() {
							if part.Get("thought").Bool() {
								thinking.WriteString(part.Get("text").String())
							} else {
								text.WriteString(part.Get("text").String())
							}
						}
					}
				}
				if thinking.String() != "先\n  想🧠" || text.String() != "答案" {
					t.Fatalf("thinking=%q text=%q body=%s", thinking.String(), text.String(), response.Body.String())
				}
				assertCursorDisplayEstimate(t, response.Body.Bytes(), clientProtocol, streaming, waitForProxyLog(t, env, "composer-2.5"), 2)
			})
		}
	}
}

/**
 * @brief Anthropic 思考、正文和工具块保持 SDK 事件顺序，SSE 索引与块生命周期合法。
 * @param t 测试上下文。
 */
func TestProxy_CursorOAuthAnthropicThinkingBlockOrder(t *testing.T) {
	t.Parallel()
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			t.Parallel()
			credentialJSON, err := (&cursorauth.Credential{APIKey: "test-key", AccessToken: "test-session"}).JSON()
			if err != nil {
				t.Fatalf("encode Cursor credential: %v", err)
			}
			env := setupProxyTestEnv(t, []testChannel{{
				name: "cursor-thinking-order", upstreamProtocol: "anthropic", models: "composer-2.5",
				authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
			}}, map[int]string{0: "https://unused.invalid"})
			env.server.cursorRunner = cursorUsageEventsRunner{
				{Ping: true}, {ThinkingDelta: "想1"}, {Delta: "答1", Text: "答1"},
				{ThinkingDelta: "想2"}, {Delta: "答2", Text: "答1答2"},
				{ToolCall: &cursorauth.ToolCall{ID: "call_lookup", Name: "lookup", Arguments: json.RawMessage(`{"q":"中文"}`)}},
			}
			response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
				"model": "composer-2.5", "stream": streaming, "max_tokens": 256,
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
				"tools":    []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}},
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
			}
			assertCursorDisplayEstimate(t, response.Body.Bytes(), "anthropic", streaming, waitForProxyLog(t, env, "composer-2.5"), 16)
			wantTypes := []string{"thinking", "text", "thinking", "text", "tool_use"}
			wantText := []string{"想1", "答1", "想2", "答2"}
			if !streaming {
				payload := gjson.ParseBytes(response.Body.Bytes())
				blocks := payload.Get("content").Array()
				if len(blocks) != len(wantTypes) || payload.Get("stop_reason").String() != "tool_use" {
					t.Fatalf("invalid Anthropic message: %s", response.Body.String())
				}
				for index, block := range blocks {
					if block.Get("type").String() != wantTypes[index] || block.Get("signature").Exists() {
						t.Fatalf("block %d type/signature: %s", index, block.Raw)
					}
					if index < len(wantText) && block.Get(wantTypes[index]).String() != wantText[index] {
						t.Fatalf("block %d content: %s", index, block.Raw)
					}
				}
				if blocks[4].Get("id").String() != "call_lookup" || blocks[4].Get("name").String() != "lookup" ||
					blocks[4].Get("input.q").String() != "中文" {
					t.Fatalf("tool block=%s", blocks[4].Raw)
				}
				return
			}
			active := -1
			var blockTypes, blockText []string
			var started, stopped bool
			for chunk := range bytes.SplitSeq(response.Body.Bytes(), []byte("\n\n")) {
				eventType, data := parseSSEEventChunk(chunk)
				if len(data) == 0 {
					continue
				}
				if !gjson.ValidBytes(data) {
					t.Fatalf("invalid SSE JSON: %s", data)
				}
				payload := gjson.ParseBytes(data)
				if stopped || payload.Get("type").String() != eventType {
					t.Fatalf("invalid event after stop or mismatched event type: %s", chunk)
				}
				switch eventType {
				case "message_start":
					if started || len(blockTypes) != 0 {
						t.Fatalf("late/duplicate message_start: %s", chunk)
					}
					started = true
				case "content_block_start", "content_block_delta", "content_block_stop":
					index := payload.Get("index")
					if !started || index.Type != gjson.Number {
						t.Fatalf("content event without message/index: %s", chunk)
					}
					if eventType == "content_block_start" {
						blockType := payload.Get("content_block.type").String()
						if active != -1 || int(index.Int()) != len(blockTypes) || len(blockTypes) >= len(wantTypes) ||
							blockType != wantTypes[len(blockTypes)] || payload.Get("content_block.signature").Exists() {
							t.Fatalf("invalid block start: %s", chunk)
						}
						active = int(index.Int())
						blockTypes = append(blockTypes, blockType)
						blockText = append(blockText, payload.Get("content_block."+blockType).String())
						if blockType == "tool_use" && (payload.Get("content_block.id").String() != "call_lookup" ||
							payload.Get("content_block.name").String() != "lookup") {
							t.Fatalf("invalid tool block: %s", chunk)
						}
						continue
					}
					if active == -1 || int(index.Int()) != active {
						t.Fatalf("delta/stop outside active block: %s", chunk)
					}
					if eventType == "content_block_stop" {
						active = -1
						continue
					}
					key, deltaType := blockTypes[active], blockTypes[active]+"_delta"
					if key == "tool_use" {
						key, deltaType = "partial_json", "input_json_delta"
					}
					if payload.Get("delta.type").String() != deltaType {
						t.Fatalf("delta type does not match active block: %s", chunk)
					}
					blockText[active] += payload.Get("delta." + key).String()
				case "message_delta":
					if active != -1 || payload.Get("delta.stop_reason").String() != "tool_use" {
						t.Fatalf("invalid message_delta: %s", chunk)
					}
				case "message_stop":
					if active != -1 {
						t.Fatalf("message stopped with open block: %s", chunk)
					}
					stopped = true
				}
			}
			if !started || !stopped || active != -1 || len(blockTypes) != len(wantTypes) {
				t.Fatalf("incomplete block lifecycle: types=%v active=%d body=%s", blockTypes, active, response.Body.String())
			}
			for index, want := range wantText {
				if blockText[index] != want {
					t.Fatalf("block %d text=%q want=%q", index, blockText[index], want)
				}
			}
			if !gjson.Valid(blockText[4]) || gjson.Get(blockText[4], "q").String() != "中文" {
				t.Fatalf("tool input=%s", blockText[4])
			}
		})
	}
}

/**
 * @brief 思考已输出后取消或失败必须保留思考，且不能生成成功的流终态。
 * @param t 测试上下文。
 */
func TestProxy_CursorOAuthThinkingInterrupted(t *testing.T) {
	t.Parallel()
	for _, clientProtocol := range []string{"anthropic", "responses"} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/canceled=%t", clientProtocol, canceled), func(t *testing.T) {
				t.Parallel()
				credentialJSON, err := (&cursorauth.Credential{APIKey: "test-key", AccessToken: "test-session"}).JSON()
				if err != nil {
					t.Fatalf("encode Cursor credential: %v", err)
				}
				env := setupProxyTestEnv(t, []testChannel{{
					name: "cursor-thinking-interrupted", upstreamProtocol: "openai", models: "composer-2.5",
					authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
				}}, map[int]string{0: "https://unused.invalid"})
				runErr := errors.New("thinking run failed")
				if canceled {
					runErr = context.Canceled
				}
				env.server.cursorRunner = cursorUsageEventsRunner{
					{Ping: true}, {ThinkingDelta: "中"}, {ThinkingDelta: "断前 "}, {Done: true, Err: runErr},
				}
				path := "/v1/messages"
				body := map[string]any{
					"model": "composer-2.5", "stream": true, "max_tokens": 256,
					"messages": []any{map[string]any{"role": "user", "content": "hello"}},
				}
				if clientProtocol == "responses" {
					path = "/v1/responses"
					body = map[string]any{"model": "composer-2.5", "stream": true, "input": "hello"}
				}
				response := doProxyRequest(t, env.engine, path, body, nil)
				if response.Code != http.StatusOK {
					t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
				}
				var thinking strings.Builder
				var failed bool
				for chunk := range bytes.SplitSeq(response.Body.Bytes(), []byte("\n\n")) {
					eventType, data := parseSSEEventChunk(chunk)
					if len(data) == 0 {
						continue
					}
					if bytes.Equal(data, []byte("[DONE]")) || !gjson.ValidBytes(data) {
						t.Fatalf("unexpected success terminator or invalid SSE JSON: %s", chunk)
					}
					payload := gjson.ParseBytes(data)
					if eventType == "message_stop" || eventType == "response.completed" ||
						payload.Get("delta.stop_reason").String() != "" || payload.Get("response.status").String() == "completed" {
						t.Fatalf("interrupted thinking emitted successful terminal event: %s", chunk)
					}
					if payload.Get("delta.type").String() == "thinking_delta" {
						thinking.WriteString(payload.Get("delta.thinking").String())
					} else if eventType == "response.reasoning_summary_text.delta" {
						thinking.WriteString(payload.Get("delta").String())
					}
					failed = failed || payload.Get("error").Exists() || payload.Get("response.error").Exists()
				}
				if thinking.String() != "中断前 " || (!canceled && !failed) {
					t.Fatalf("thinking=%q failed=%t body=%s", thinking.String(), failed, response.Body.String())
				}
			})
		}
	}
}

type cursorRefreshOnceRunner struct {
	calls            int
	accessTokens     []string
	outputThenReject bool
}

func (r *cursorRefreshOnceRunner) Run(
	_ context.Context,
	credential *cursorauth.Credential,
	_ cursorauth.Request,
) (<-chan cursorauth.Event, error) {
	r.calls++
	r.accessTokens = append(r.accessTokens, credential.AccessToken)
	events := make(chan cursorauth.Event, 2)
	if r.calls == 1 {
		if r.outputThenReject {
			events <- cursorauth.Event{Delta: "partial", Text: "partial"}
		}
		events <- cursorauth.Event{Done: true, Err: &cursorauth.BridgeError{
			SDKCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED,
			Message: "expired Cursor session",
		}}
	} else {
		events <- cursorauth.Event{Delta: "ok after refresh", Text: "ok after refresh", Done: true}
	}
	close(events)
	return events, nil
}

/**
 * @brief 通过实际请求日志核对 Cursor 启动失败与 SDK 已收到业务请求时的思考等级。
 * @param t 测试上下文。
 */
func TestProxy_CursorReasoningCommitsOnlySentEffort(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t, "high", "high", "high", "high")
	credential, err := (&cursorauth.Credential{APIKey: "key", AccessToken: "session"}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "cursor-reasoning", upstreamProtocol: "openai", models: "gpt-5.6-sol",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credential,
	}}, []string{"https://unused.invalid"})
	headers := map[string]string{"Session-Id": "cursor-reasoning"}
	modelLog := func(applied string, status int) *model.LogEntry {
		return waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
			return entry.Model == "gpt-5.6-sol" && entry.StatusCode == status &&
				entry.ReasoningDecision != nil && entry.ReasoningDecision.AppliedEffort == applied
		})
	}

	env.server.cursorRunner = &fakeCursorRunner{err: cursorauth.ErrAgentMissing}
	failed := doProxyRequest(t, env.engine, "/v1/chat/completions", reasoningAutoChatBody("First task"), headers)
	if failed.Code != http.StatusServiceUnavailable || modelLog("", http.StatusServiceUnavailable) == nil {
		t.Fatalf("unsent request recorded an applied effort or returned HTTP %d", failed.Code)
	}
	for _, actual := range []string{"", "low", "high"} {
		env.server.cursorRunner = cursorUsageEventsRunner{
			{SentEffort: &actual}, {Text: "answer", Done: true},
		}
		reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Next task "+actual), headers)
		entry := modelLog(actual, http.StatusOK)
		if entry == nil || entry.ReasoningDecision.SuggestedEffort != "high" || entry.ReasoningDecision.PreviousEffort != "" {
			t.Fatalf("SDK effort %q produced log %+v", actual, entry)
		}
	}
}

func TestProxy_CursorOAuthUsesCLIInsteadOfHTTP(t *testing.T) {
	t.Parallel()

	upstreamHits := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()

	credentialJSON, err := (&cursorauth.Credential{AccessToken: "tok", Email: "user@example.com"}).JSON()
	if err != nil {
		t.Fatalf("encode cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-cli", upstreamProtocol: "anthropic", models: "claude-sonnet-5",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: upstream.URL})
	runner := &fakeCursorRunner{text: "ok from cli", usage: &cursorauth.Usage{
		InputTokens: 11, OutputTokens: 7, CacheReadTokens: 5, CacheWriteTokens: 3,
		TotalTokens: 26, ReasoningTokens: 2,
	}}
	env.server.cursorRunner = runner

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model":      "claude-sonnet-5",
		"max_tokens": 16,
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{
		"anthropic-version": "2023-06-01", "Session-Id": "client-session", "Thread-Id": "client-thread",
		"X-Claude-Code-Session-Id": "claude-session", "X-Claude-Code-Agent-Id": "child",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	if upstreamHits != 0 {
		t.Fatalf("Cursor OAuth must not HTTP-forward, hits = %d", upstreamHits)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	content, _ := payload["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("body = %s", response.Body.String())
	}
	block, _ := content[0].(map[string]any)
	if block["text"] != "ok from cli" {
		t.Fatalf("text = %#v", block["text"])
	}
	if runner.model != "claude-sonnet-5" {
		t.Fatalf("model = %q", runner.model)
	}
	entry := waitForProxyLog(t, env, "claude-sonnet-5")
	wantScope := cursorauth.SessionScope{
		ChannelID: entry.ChannelID, TokenID: 1, SessionID: "client-session", ThreadID: "client-thread",
		ClaudeSessionID: "claude-session", ClaudeAgentID: "child",
	}
	if runner.request.SessionScope != wantScope {
		t.Fatalf("Cursor runner scope = %+v, want %+v", runner.request.SessionScope, wantScope)
	}
	if entry.InputTokens != 11 || entry.OutputTokens != 7 || entry.CacheReadInputTokens != 5 ||
		entry.CacheCreationInputTokens != 3 || entry.Cache5mInputTokens != 3 || entry.ReasoningTokens != 2 {
		t.Fatalf("logged usage = in:%d out:%d cache_read:%d cache_write:%d cache_5m:%d reasoning:%d",
			entry.InputTokens, entry.OutputTokens, entry.CacheReadInputTokens,
			entry.CacheCreationInputTokens, entry.Cache5mInputTokens, entry.ReasoningTokens)
	}
}

func TestProxy_CursorOAuthRemintsRejectedCredentialAndRetries(t *testing.T) {
	t.Parallel()
	credentialJSON, err := (&cursorauth.Credential{
		APIKey: "cursor-user-api-key", AccessToken: "cursor-access-old", RefreshToken: "cursor-refresh-old",
	}).JSON()
	if err != nil {
		t.Fatalf("encode Cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-refresh", upstreamProtocol: "openai", models: "grok-4.6",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	exchangeCalls := 0
	env.server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		exchangeCalls++
		if request.URL.Path != cursorauth.ExchangeAPIKeyPath {
			t.Fatalf("refresh path = %s", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer cursor-user-api-key" {
			t.Fatalf("refresh authorization = %q", got)
		}
		return cursorTestHTTPResponse(
			request, http.StatusOK,
			`{"accessToken":"cursor-access-new","refreshToken":"cursor-refresh-new"}`,
		), nil
	})}
	env.server.cursorCredentials = newCursorCredentialManager(
		env.store, env.server.getClientForChannel, env.server.invalidateChannelRelatedCache,
	)
	runner := &cursorRefreshOnceRunner{}
	env.server.cursorRunner = runner

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "grok-4.6", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "ok after refresh") {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	if exchangeCalls != 1 || runner.calls != 2 ||
		len(runner.accessTokens) != 2 || runner.accessTokens[0] != "cursor-access-old" ||
		runner.accessTokens[1] != "cursor-access-new" {
		t.Fatalf("refresh calls=%d runner calls=%d access tokens=%v", exchangeCalls, runner.calls, runner.accessTokens)
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs() = (%d, %v)", len(configs), err)
	}
	persisted, err := cursorauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || persisted.AccessToken != "cursor-access-new" || persisted.RefreshToken != "cursor-refresh-new" {
		t.Fatalf("persisted credential = (%+v, %v)", persisted, err)
	}
}

/**
 * @brief 客户端在 re-mint 期间断开不冷却渠道，后台换取的新凭证照常落库。
 * @param t 测试上下文。
 */
func TestProxy_CursorOAuthCancelDuringRemintDoesNotCooldown(t *testing.T) {
	t.Parallel()
	credentialJSON, err := (&cursorauth.Credential{
		APIKey: "cursor-user-api-key", AccessToken: "cursor-access-old", RefreshToken: "cursor-refresh-old",
	}).JSON()
	if err != nil {
		t.Fatalf("encode Cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-cancel", upstreamProtocol: "openai", models: "grok-4.6",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	release := make(chan struct{})
	exchanged := make(chan struct{})
	env.server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		cancel()
		<-release
		defer close(exchanged)
		return cursorTestHTTPResponse(
			request, http.StatusOK,
			`{"accessToken":"cursor-access-new","refreshToken":"cursor-refresh-new"}`,
		), nil
	})}
	env.server.cursorCredentials = newCursorCredentialManager(
		env.store, env.server.getClientForChannel, env.server.invalidateChannelRelatedCache,
	)
	env.server.cursorRunner = &cursorRefreshOnceRunner{}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"grok-4.6","messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key")
	env.engine.ServeHTTP(httptest.NewRecorder(), req)
	close(release)
	<-exchanged

	cooldowns, err := env.store.GetAllChannelCooldowns(t.Context())
	if err != nil || len(cooldowns) != 0 {
		t.Fatalf("client cancellation cooled the channel: %v, %v", cooldowns, err)
	}
}

func TestProxy_CursorOAuthDoesNotReplayStreamAfterOutput(t *testing.T) {
	t.Parallel()
	credentialJSON, err := (&cursorauth.Credential{
		APIKey: "cursor-user-api-key", AccessToken: "cursor-access-old",
	}).JSON()
	if err != nil {
		t.Fatalf("encode Cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-no-stream-replay", upstreamProtocol: "openai", models: "grok-4.6",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	exchangeCalls := 0
	env.server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		exchangeCalls++
		return cursorTestHTTPResponse(
			request, http.StatusOK,
			`{"accessToken":"cursor-access-new","refreshToken":"cursor-refresh-new"}`,
		), nil
	})}
	env.server.cursorCredentials = newCursorCredentialManager(
		env.store, env.server.getClientForChannel, env.server.invalidateChannelRelatedCache,
	)
	runner := &cursorRefreshOnceRunner{outputThenReject: true}
	env.server.cursorRunner = runner

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "grok-4.6", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "partial") ||
		!strings.Contains(response.Body.String(), "expired Cursor session") {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	if exchangeCalls != 1 || runner.calls != 1 {
		t.Fatalf("stream refresh/replay mismatch: refresh calls=%d runner calls=%d", exchangeCalls, runner.calls)
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs() = (%d, %v)", len(configs), err)
	}
	persisted, err := cursorauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || persisted.AccessToken != "cursor-access-new" {
		t.Fatalf("persisted credential = (%+v, %v)", persisted, err)
	}
}

/**
 * @brief 验证 SDK 限流和无效模型按响应提交状态切换渠道，只影响当前模型。
 * @param t 测试上下文。
 */
func TestProxy_CursorSDKModelRejection(t *testing.T) {
	t.Parallel()
	const limit = sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED
	const invalid = sdkv1.SdkErrorCode_SDK_ERROR_CODE_INVALID_MODEL
	const wait = 23 * time.Second
	for _, tt := range []struct {
		name          string
		code          sdkv1.SdkErrorCode
		backup        bool
		committed     bool
		wantHTTP      int
		wantBackupRun int
	}{
		{"start rate limit fails over", limit, true, false, http.StatusOK, 1},
		{"committed rate limit does not fail over", limit, true, true, http.StatusOK, 0},
		{"rate limit returned with retry hint", limit, false, false, http.StatusTooManyRequests, 0},
		{"invalid model fails over", invalid, true, false, http.StatusOK, 1},
		{"committed invalid model does not fail over", invalid, true, true, http.StatusOK, 0},
		{"invalid model returned", invalid, false, false, http.StatusBadRequest, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			channels := []testChannel{{
				name: "cursor-primary", upstreamProtocol: "openai", models: "grok-4.6,composer-2.5",
				authType: model.AuthTypeCursorOAuth, oauthCredential: `{"access_token":"test-session","api_key":"primary-key"}`,
			}}
			urls := map[int]string{0: "https://unused.invalid"}
			if tt.backup {
				channels = append(channels, testChannel{
					name: "cursor-backup", upstreamProtocol: "openai", models: "grok-4.6",
					authType: model.AuthTypeCursorOAuth, oauthCredential: `{"access_token":"test-session","api_key":"backup-key"}`,
				})
				urls[1] = "https://unused.invalid"
			}
			env := setupProxyTestEnv(t, channels, urls)
			primaryCalls, backupCalls := 0, 0
			const failureMessage = "SDK explicitly rejected the requested model"
			failure := &cursorauth.BridgeError{SDKCode: tt.code, Message: failureMessage, RetryAfter: wait}
			env.server.cursorRunner = cursorRunnerFunc(func(ctx context.Context, credential *cursorauth.Credential, request cursorauth.Request) (<-chan cursorauth.Event, error) {
				if credential.APIKey == "backup-key" {
					backupCalls++
					return cursorUsageEventsRunner{{Done: true, Text: "backup"}}.Run(ctx, credential, request)
				}
				primaryCalls++
				if request.Model == "composer-2.5" {
					return cursorUsageEventsRunner{{Done: true, Text: "another model"}}.Run(ctx, credential, request)
				}
				if tt.committed {
					return cursorUsageEventsRunner{{Delta: "partial"}, {Done: true, Err: failure}}.Run(ctx, credential, request)
				}
				return nil, failure
			})
			messages := []any{map[string]any{"role": "user", "content": "hello"}}
			before := time.Now()
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "grok-4.6", "stream": tt.committed, "messages": messages,
			}, nil)
			if response.Code != tt.wantHTTP || primaryCalls != 1 || backupCalls != tt.wantBackupRun {
				t.Fatalf("HTTP=%d primary=%d backup=%d body=%s", response.Code, primaryCalls, backupCalls, response.Body.String())
			}
			switch {
			case tt.committed:
				var errorMessage string
				for block := range bytes.SplitSeq(response.Body.Bytes(), []byte("\n\n")) {
					_, data := parseSSEEventChunk(block)
					if field := gjson.GetBytes(data, "error.message"); field.Exists() {
						errorMessage = field.String()
					}
				}
				if !strings.Contains(errorMessage, failureMessage) || strings.Contains(response.Body.String(), "[DONE]") {
					t.Fatalf("committed stream body=%s", response.Body.String())
				}
			case tt.backup:
				if got := gjson.GetBytes(response.Body.Bytes(), "choices.0.message.content").String(); got != "backup" {
					t.Fatalf("fallback response=%s", response.Body.String())
				}
			default:
				if !strings.Contains(gjson.GetBytes(response.Body.Bytes(), "error.message").String(), failureMessage) {
					t.Fatalf("failure response=%s", response.Body.String())
				}
				hint := response.Header().Get("Retry-After")
				if tt.code == limit {
					seconds, err := strconv.Atoi(hint)
					if err != nil || seconds < 22 || seconds > 24 {
						t.Fatalf("Retry-After=%q, want about %s", hint, wait)
					}
				} else if hint != "" {
					t.Fatalf("invalid model returned Retry-After=%q", hint)
				}
			}
			modelCooldowns, err := env.store.GetAllModelCooldowns(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if tt.code == limit {
				if len(modelCooldowns) != 1 {
					t.Fatalf("model cooldowns=%v", modelCooldowns)
				}
				for _, models := range modelCooldowns {
					until := models["grok-4.6"]
					if len(models) != 1 || until.Before(before.Add(wait)) || until.After(time.Now().Add(wait+time.Second)) {
						t.Fatalf("model cooldowns=%v, want current model held until Retry-After", modelCooldowns)
					}
				}
			} else if len(modelCooldowns) != 0 {
				t.Fatalf("invalid model persisted cooldowns=%v", modelCooldowns)
			}
			channelCooldowns, err := env.store.GetAllChannelCooldowns(t.Context())
			if err != nil || len(channelCooldowns) != 0 {
				t.Fatalf("unexpected channel cooldowns=%v err=%v", channelCooldowns, err)
			}
			other := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "composer-2.5", "messages": messages,
			}, nil)
			if other.Code != http.StatusOK || gjson.GetBytes(other.Body.Bytes(), "choices.0.message.content").String() != "another model" {
				t.Fatalf("other model HTTP=%d body=%s", other.Code, other.Body.String())
			}
		})
	}
}

/**
 * @brief 验证账户拒绝可以在提交响应前换渠，已确认的请求错误直接返回。
 * @param t 测试上下文。
 */
func TestProxy_CursorSDKAccountAndRequestFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		err        error
		terminal   bool
		committed  bool
		failsOver  bool
		wantStatus int
		wantType   string
	}{
		{"plan", &cursorauth.BridgeError{SDKCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_PLAN_REQUIRED, Message: "plan required"}, false, false, true, 200, ""},
		{"role", &cursorauth.BridgeError{SDKCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_ROLE_FORBIDDEN, Message: "role forbidden"}, true, false, true, 200, ""},
		{"feature after commit", &cursorauth.BridgeError{SDKCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_FEATURE_UNAVAILABLE, Message: "feature unavailable"}, true, true, false, 200, "permission_error"},
		{"ambiguous validation", &cursorauth.BridgeError{SDKCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_VALIDATION_ERROR, Message: "invalid bridge option"}, false, false, true, 200, ""},
		{"token throttle", &cursorauth.BridgeError{SDKCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_UPSTREAM_ERROR, Message: "Too many tokens per minute; rate limit exceeded for this context window"}, true, false, true, 200, ""},
		{"local request validation", fmt.Errorf("%w: tool_choice references an undeclared tool", cursorauth.ErrInvalidRequest), false, false, false, 400, "invalid_request_error"},
		{"tool turn already running", cursorauth.ErrToolTurnInProgress, false, false, false, http.StatusConflict, "invalid_request_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := setupProxyTestEnv(t, []testChannel{
				{name: "cursor-primary", upstreamProtocol: "openai", models: "composer-2.5", authType: model.AuthTypeCursorOAuth, oauthCredential: `{"access_token":"test-session","api_key":"primary-key"}`},
				{name: "cursor-backup", upstreamProtocol: "openai", models: "composer-2.5", authType: model.AuthTypeCursorOAuth, oauthCredential: `{"access_token":"test-session","api_key":"backup-key"}`},
			}, map[int]string{0: "https://unused.invalid", 1: "https://unused.invalid"})
			primary, backup := 0, 0
			env.server.cursorRunner = cursorRunnerFunc(func(ctx context.Context, credential *cursorauth.Credential, req cursorauth.Request) (<-chan cursorauth.Event, error) {
				if credential.APIKey == "backup-key" {
					backup++
					return cursorUsageEventsRunner{{Done: true, Text: "backup"}}.Run(ctx, credential, req)
				}
				primary++
				if !tc.terminal {
					return nil, tc.err
				}
				events := cursorUsageEventsRunner{}
				if tc.committed {
					events = append(events, cursorauth.Event{Delta: "partial"})
				}
				events = append(events, cursorauth.Event{Done: true, Err: tc.err})
				return events.Run(ctx, credential, req)
			})
			resp := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "composer-2.5", "stream": tc.committed,
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			}, nil)
			if resp.Code != tc.wantStatus || primary != 1 || backup > 1 || (backup == 1) != tc.failsOver {
				t.Fatalf("HTTP=%d primary=%d backup=%d body=%s", resp.Code, primary, backup, resp.Body)
			}
			if tc.failsOver {
				if got := gjson.GetBytes(resp.Body.Bytes(), "choices.0.message.content").String(); got != "backup" {
					t.Fatalf("response=%s", resp.Body)
				}
			} else {
				payload := resp.Body.Bytes()
				if tc.committed {
					for block := range bytes.SplitSeq(payload, []byte("\n\n")) {
						if _, data := parseSSEEventChunk(block); gjson.GetBytes(data, "error").Exists() {
							payload = data
						}
					}
				}
				if got := gjson.GetBytes(payload, "error.type").String(); got != tc.wantType {
					t.Fatalf("error.type=%q, response=%s", got, resp.Body)
				}
			}
			channelCooldowns, err := env.store.GetAllChannelCooldowns(t.Context())
			if err != nil || len(channelCooldowns) != 0 {
				t.Fatalf("channel cooldowns=%v err=%v", channelCooldowns, err)
			}
			modelCooldowns, err := env.store.GetAllModelCooldowns(t.Context())
			if err != nil || len(modelCooldowns) != 0 {
				t.Fatalf("model cooldowns=%v err=%v", modelCooldowns, err)
			}
		})
	}
}

/**
 * @brief 上下文超限在已提交的协议流中保留错误码，其余请求以 400 停止换渠。
 * @param t 测试上下文。
 */
func TestProxy_CursorContextOverflowAcrossProtocols(t *testing.T) {
	t.Parallel()
	for _, wire := range []struct{ path, request string }{
		{"/v1/chat/completions", `{"model":"composer-2.5","messages":[{"role":"user","content":"hello"}]}`},
		{"/v1/responses", `{"model":"composer-2.5","input":"hello"}`},
		{"/v1/messages", `{"model":"composer-2.5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`},
	} {
		for _, stage := range []string{"start", "terminal", "committed"} {
			t.Run(wire.path+"/"+stage, func(t *testing.T) {
				t.Parallel()
				env := setupProxyTestEnv(t, []testChannel{
					{name: "cursor-primary", upstreamProtocol: "openai", models: "composer-2.5", authType: model.AuthTypeCursorOAuth, oauthCredential: `{"access_token":"test-session","api_key":"primary-key"}`},
					{name: "cursor-backup", upstreamProtocol: "openai", models: "composer-2.5", authType: model.AuthTypeCursorOAuth, oauthCredential: `{"access_token":"test-session","api_key":"backup-key"}`},
				}, map[int]string{0: "https://unused.invalid", 1: "https://unused.invalid"})
				message := "Input exceeds the context window"
				if wire.path == "/v1/messages" && stage == "terminal" {
					message = "context_too_large: request rejected"
				}
				failure := &cursorauth.BridgeError{SDKCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_VALIDATION_ERROR, Message: message}
				calls := 0
				env.server.cursorRunner = cursorRunnerFunc(func(ctx context.Context, credential *cursorauth.Credential, req cursorauth.Request) (<-chan cursorauth.Event, error) {
					calls++
					if credential.APIKey == "backup-key" {
						return cursorUsageEventsRunner{{Done: true, Text: "unexpected fallback"}}.Run(ctx, credential, req)
					}
					switch stage {
					case "start":
						return nil, failure
					case "terminal":
						return cursorUsageEventsRunner{{Done: true, Err: failure}}.Run(ctx, credential, req)
					default:
						return cursorUsageEventsRunner{{Delta: "partial"}, {Done: true, Err: failure}}.Run(ctx, credential, req)
					}
				})
				var req map[string]any
				if err := json.Unmarshal([]byte(wire.request), &req); err != nil {
					t.Fatal(err)
				}
				req["stream"] = stage != "start"
				resp := doProxyRequest(t, env.engine, wire.path, req, nil)
				wantHTTP := http.StatusBadRequest
				payload := resp.Body.Bytes()
				if stage == "committed" {
					wantHTTP = http.StatusOK
					var lastSequence int64 = -1
					for block := range bytes.SplitSeq(payload, []byte("\n\n")) {
						event, data := parseSSEEventChunk(block)
						message := gjson.ParseBytes(data)
						if event == "error" || message.Get("error").Exists() {
							if wire.path == "/v1/responses" && (lastSequence < 0 || message.Get("sequence_number").Int() != lastSequence+1) {
								t.Fatalf("error sequence=%s follows %d", data, lastSequence)
							}
							payload = data
						}
						if seq := message.Get("sequence_number"); seq.Exists() {
							lastSequence = seq.Int()
						}
						if event == "response.completed" || event == "message_stop" || bytes.Equal(data, []byte("[DONE]")) {
							t.Fatalf("unexpected successful terminator: %s", data)
						}
					}
				}
				if resp.Code != wantHTTP || calls != 1 {
					t.Fatalf("HTTP=%d calls=%d body=%s", resp.Code, calls, resp.Body)
				}
				detail := gjson.GetBytes(payload, "error")
				switch {
				case wire.path == "/v1/messages":
					if gjson.GetBytes(payload, "type").String() != "error" || detail.Get("type").String() != "invalid_request_error" ||
						!strings.Contains(detail.Get("message").String(), "context_length_exceeded") {
						t.Fatalf("Anthropic error=%s", payload)
					}
				case wire.path == "/v1/responses" && stage == "committed":
					if gjson.GetBytes(payload, "type").String() != "error" || gjson.GetBytes(payload, "code").String() != "context_length_exceeded" {
						t.Fatalf("Responses SSE error=%s", payload)
					}
				default:
					if detail.Get("type").String() != "invalid_request_error" || detail.Get("code").String() != "context_length_exceeded" {
						t.Fatalf("OpenAI error=%s", payload)
					}
				}
				channelCooldowns, err := env.store.GetAllChannelCooldowns(t.Context())
				if err != nil || len(channelCooldowns) != 0 {
					t.Fatalf("channel cooldowns=%v err=%v", channelCooldowns, err)
				}
				modelCooldowns, err := env.store.GetAllModelCooldowns(t.Context())
				if err != nil || len(modelCooldowns) != 0 {
					t.Fatalf("model cooldowns=%v err=%v", modelCooldowns, err)
				}
			})
		}
	}
}

/**
 * @brief 重放一次已处理的 SDK 限流时保留原有退避截止时间。
 * @param t 测试上下文。
 */
func TestProxy_CursorReplayKeepsOriginalCooldown(t *testing.T) {
	t.Parallel()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor", upstreamProtocol: "openai", models: "grok-4.6,composer-2.5",
		authType: model.AuthTypeCursorOAuth, oauthCredential: `{"access_token":"test-session","api_key":"test-key"}`,
	}}, map[int]string{0: "https://unused.invalid"})
	const modelID = "grok-4.6"
	failure := &cursorauth.BridgeError{SDKCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED, Message: "SDK rejected the model"}
	calls := 0
	env.server.cursorRunner = cursorRunnerFunc(func(ctx context.Context, credential *cursorauth.Credential, request cursorauth.Request) (<-chan cursorauth.Event, error) {
		calls++
		return cursorUsageEventsRunner{{Done: true, Err: failure, Replayed: calls > 1}}.Run(ctx, credential, request)
	})
	var firstUntil time.Time
	for attempt := range 2 {
		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": modelID, "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}, nil)
		if response.Code != http.StatusTooManyRequests || calls != attempt+1 ||
			!strings.Contains(gjson.GetBytes(response.Body.Bytes(), "error.message").String(), failure.Message) {
			t.Fatalf("attempt=%d calls=%d HTTP=%d body=%s", attempt, calls, response.Code, response.Body.String())
		}
		cooldowns, err := env.store.GetAllModelCooldowns(t.Context())
		if err != nil || len(cooldowns) != 1 {
			t.Fatalf("model cooldowns=%v err=%v", cooldowns, err)
		}
		for _, models := range cooldowns {
			until := models[modelID]
			if attempt == 0 {
				firstUntil = until
				if !firstUntil.After(time.Now()) {
					t.Fatalf("initial rate limit did not cool %s: %v", modelID, firstUntil)
				}
			} else if !until.Equal(firstUntil) {
				t.Fatalf("replay moved cooldown from %v to %v", firstUntil, until)
			}
		}
	}
}

/**
 * @brief 重放凭证拒绝时保留客户端错误，避免凭证重铸或备用渠道推理。
 * @param t 测试上下文。
 */
func TestProxy_CursorReplayDoesNotRefreshOrFailOver(t *testing.T) {
	t.Parallel()
	env := setupProxyTestEnv(t, []testChannel{
		{name: "cursor-primary", upstreamProtocol: "openai", models: "grok-4.6",
			authType: model.AuthTypeCursorOAuth, oauthCredential: `{"access_token":"test-session","api_key":"primary-key"}`},
		{name: "cursor-backup", upstreamProtocol: "openai", models: "grok-4.6",
			authType: model.AuthTypeCursorOAuth, oauthCredential: `{"access_token":"test-session","api_key":"backup-key"}`},
	}, map[int]string{0: "https://unused.invalid", 1: "https://unused.invalid"})
	primaryCalls, backupCalls, refreshCalls := 0, 0, 0
	env.server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		refreshCalls++
		return cursorTestHTTPResponse(request, http.StatusOK, `{"accessToken":"new-session","refreshToken":"new-refresh"}`), nil
	})}
	env.server.cursorCredentials = newCursorCredentialManager(env.store, env.server.getClientForChannel, env.server.invalidateChannelRelatedCache)
	const message = "the original run used a rejected credential"
	failure := &cursorauth.BridgeError{SDKCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED, Message: message}
	env.server.cursorRunner = cursorRunnerFunc(func(ctx context.Context, credential *cursorauth.Credential, request cursorauth.Request) (<-chan cursorauth.Event, error) {
		if credential.APIKey == "backup-key" {
			backupCalls++
			return cursorUsageEventsRunner{{Done: true, Text: "backup"}}.Run(ctx, credential, request)
		}
		primaryCalls++
		events := cursorUsageEventsRunner{}
		if primaryCalls != 2 {
			events = append(events, cursorauth.Event{Delta: "partial"})
		}
		return append(events, cursorauth.Event{Done: true, Err: failure, Replayed: primaryCalls > 1}).Run(ctx, credential, request)
	})
	for attempt, want := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusOK} {
		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": "grok-4.6", "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}, nil)
		if response.Code != want || primaryCalls != attempt+1 || backupCalls != 0 || refreshCalls != 1 {
			t.Fatalf("attempt=%d HTTP=%d primary=%d backup=%d refresh=%d body=%s", attempt, response.Code, primaryCalls, backupCalls, refreshCalls, response.Body.String())
		}
		var got string
		if want == http.StatusUnauthorized {
			got = gjson.GetBytes(response.Body.Bytes(), "error.message").String()
		} else {
			for block := range bytes.SplitSeq(response.Body.Bytes(), []byte("\n\n")) {
				_, data := parseSSEEventChunk(block)
				if field := gjson.GetBytes(data, "error.message"); field.Exists() {
					got = field.String()
				}
			}
		}
		if !strings.Contains(got, message) {
			t.Fatalf("attempt=%d original failure missing from %s", attempt, response.Body.String())
		}
	}
}

/**
 * @brief 重放的实时和终态用量均不重复写入日志、令牌统计或成本。
 * @param t 测试上下文。
 */
func TestProxy_CursorOAuthReplayDoesNotDuplicateUsageLog(t *testing.T) {
	t.Parallel()
	credentialJSON, err := (&cursorauth.Credential{
		APIKey: "cursor-user-api-key", AccessToken: "cursor-access-token", Email: "user@example.com",
	}).JSON()
	if err != nil {
		t.Fatalf("encode cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-replay", upstreamProtocol: "openai", models: "grok-4.6",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	env.server.cursorRunner = cursorUsageEventsRunner{
		{Usage: &cursorauth.Usage{InputTokens: 100, OutputTokens: 20}, Replayed: true},
		{Text: "final", Done: true, Usage: &cursorauth.Usage{InputTokens: 50, OutputTokens: 10}, Replayed: true},
	}
	tokenHash := model.HashToken("test-api-key")
	if err := env.store.CreateAuthToken(context.Background(), &model.AuthToken{Token: tokenHash, IsActive: true}); err != nil {
		t.Fatalf("CreateAuthToken: %v", err)
	}
	token, err := env.store.GetAuthTokenByValue(context.Background(), tokenHash)
	if err != nil {
		t.Fatalf("GetAuthTokenByValue: %v", err)
	}
	injectAPIToken(env.server.authService, "test-api-key", 0, token.ID)

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "grok-4.6",
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "lookup", "arguments": `{}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "value"},
		},
	}, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "final") {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
	logs, err := env.store.ListLogs(
		context.Background(), time.Now().Add(-time.Minute), 10, 0,
		&model.LogFilter{Model: "grok-4.6", LogSource: model.LogSourceProxy},
	)
	if err != nil {
		t.Fatalf("GetLogs() error = %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("replayed request wrote %d duplicate logs", len(logs))
	}
	token, err = env.store.GetAuthTokenByValue(context.Background(), tokenHash)
	if err != nil || token.SuccessCount != 0 || token.FailureCount != 0 || token.PromptTokensTotal != 0 ||
		token.CompletionTokensTotal != 0 || token.TotalCostUSD != 0 {
		t.Fatalf("replayed request changed token statistics: token=%+v err=%v", token, err)
	}
}

/**
 * @brief 逐轮真实用量立即计费，终态只补差，后续错误保留已提交的真实消费。
 * @param t 测试上下文。
 */
func TestProxy_CursorOAuthAccountsIncrementalUsage(t *testing.T) {
	t.Parallel()
	first := &cursorauth.Usage{
		InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 300, CacheWriteTokens: 400, ReasoningTokens: 50,
	}
	second := &cursorauth.Usage{
		InputTokens: 500, OutputTokens: 100, CacheReadTokens: 150, CacheWriteTokens: 200, ReasoningTokens: 25,
	}
	residual := &cursorauth.Usage{
		InputTokens: 100, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 40, ReasoningTokens: 5,
	}
	lowTier := &cursorauth.Usage{InputTokens: 150_000, OutputTokens: 1000, CacheReadTokens: 25_000, CacheWriteTokens: 1_000}
	highTier := &cursorauth.Usage{InputTokens: 190_000, OutputTokens: 2_000, CacheReadTokens: 20_000, CacheWriteTokens: 2_000}
	twoLowTiers := *lowTier
	twoLowTiers.Add(lowTier)
	for _, tt := range []struct {
		name         string
		events       cursorUsageEventsRunner
		resumed      bool
		failed       bool
		uncommitted  bool
		wantEstimate bool
		wantHighTier *cursorauth.Usage
		wantBillable cursorauth.Usage
		wantClient   cursorauth.Usage
	}{
		{
			name: "tool pause preserves reported usage",
			events: cursorUsageEventsRunner{
				{Usage: first},
				{ToolCall: &cursorauth.ToolCall{ID: "call_lookup", Name: "lookup", Arguments: json.RawMessage(`{}`)}},
			},
			wantBillable: *first, wantClient: *first,
		},
		{
			name: "multiple increments and terminal residual",
			events: cursorUsageEventsRunner{
				{Usage: first}, {Usage: second}, {Text: "final", Done: true, Usage: residual},
			},
			wantBillable: cursorauth.Usage{InputTokens: 1600, OutputTokens: 320, CacheReadTokens: 480, CacheWriteTokens: 640, ReasoningTokens: 80},
			wantClient:   cursorauth.Usage{InputTokens: 1500, OutputTokens: 300, CacheReadTokens: 450, CacheWriteTokens: 600, ReasoningTokens: 75},
		},
		{
			name: "terminal residual retains its own context tier",
			events: cursorUsageEventsRunner{
				{Usage: lowTier}, {Usage: lowTier}, {Text: "final", Done: true, Usage: highTier},
			},
			wantBillable: cursorauth.Usage{InputTokens: 490_000, OutputTokens: 4_000, CacheReadTokens: 70_000, CacheWriteTokens: 4_000},
			wantClient:   cursorauth.Usage{InputTokens: 300_000, OutputTokens: 2_000, CacheReadTokens: 50_000, CacheWriteTokens: 2_000},
			wantHighTier: highTier,
		},
		{
			name: "interrupted stream keeps low-tier increments separate",
			events: cursorUsageEventsRunner{
				{Delta: "partial", Text: "partial"},
				{Done: true, Err: errors.New("Cursor stream interrupted"), Usage: &twoLowTiers, UsageIncrements: []cursorauth.Usage{*lowTier, *lowTier}},
			},
			failed:       true,
			wantBillable: twoLowTiers,
		},
		{
			name: "zero usage does not hide real terminal usage",
			events: cursorUsageEventsRunner{
				{Usage: &cursorauth.Usage{}}, {Text: "final", Done: true, Usage: residual},
			},
			wantBillable: *residual, wantClient: *residual,
		},
		{
			name: "zero terminal residual preserves real usage",
			events: cursorUsageEventsRunner{
				{Usage: first}, {Text: "final", Done: true, Usage: &cursorauth.Usage{}},
			},
			wantBillable: *first, wantClient: *first,
		},
		{
			name: "resumed turn estimates context while billing real usage",
			events: cursorUsageEventsRunner{
				{Usage: first}, {Text: "final", Done: true, Usage: residual},
			},
			resumed:      true,
			wantBillable: cursorauth.Usage{InputTokens: 1100, OutputTokens: 220, CacheReadTokens: 330, CacheWriteTokens: 440, ReasoningTokens: 55},
		},
		{
			name: "stream failure retains earlier usage",
			events: cursorUsageEventsRunner{
				{Delta: "partial", Text: "partial"}, {Usage: first}, {Usage: second},
				{Done: true, Err: errors.New("Cursor stream interrupted")},
			},
			failed:       true,
			wantBillable: cursorauth.Usage{InputTokens: 1500, OutputTokens: 300, CacheReadTokens: 450, CacheWriteTokens: 600, ReasoningTokens: 75},
		},
		{
			name: "stream failure only estimates unreported usage",
			events: cursorUsageEventsRunner{
				{Delta: "partial", Text: "partial"},
				{Done: true, Err: errors.New("Cursor stream interrupted")},
			},
			failed: true, wantEstimate: true,
		},
		{
			name: "started run failure before output only estimates input",
			events: cursorUsageEventsRunner{
				{Done: true, Err: errors.New("Cursor stream interrupted")},
			},
			failed: true, uncommitted: true, wantEstimate: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			credentialJSON, err := (&cursorauth.Credential{APIKey: "test-key", AccessToken: "test-session"}).JSON()
			if err != nil {
				t.Fatalf("encode Cursor credential: %v", err)
			}
			inputPrice, outputPrice, cacheReadPrice, cacheWritePrice := 2.0, 3.0, 0.5, 4.0
			highInput, highOutput, highRead, highWrite := 4.0, 6.0, 1.0, 8.0
			env := setupProxyTestEnv(t, []testChannel{{
				name: "cursor-incremental-usage", upstreamProtocol: "openai",
				modelEntries: []model.ModelEntry{{Model: "grok-4.6", Pricing: &util.CustomModelPrice{
					InputPrice: &inputPrice, OutputPrice: &outputPrice, CacheReadPrice: &cacheReadPrice, CacheWritePrice: &cacheWritePrice,
					InputPriceHigh: &highInput, OutputPriceHigh: &highOutput, CacheReadPriceHigh: &highRead, CacheWritePriceHigh: &highWrite,
				}}},
				authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
			}}, map[int]string{0: "https://unused.invalid"})
			env.server.cursorRunner = tt.events
			ctx := context.Background()
			tokenHash := model.HashToken("test-api-key")
			if err := env.store.CreateAuthToken(ctx, &model.AuthToken{Token: tokenHash, IsActive: true}); err != nil {
				t.Fatalf("CreateAuthToken: %v", err)
			}
			token, err := env.store.GetAuthTokenByValue(ctx, tokenHash)
			if err != nil {
				t.Fatalf("GetAuthTokenByValue: %v", err)
			}
			injectAPIToken(env.server.authService, "test-api-key", 0, token.ID)
			messages := []any{map[string]any{"role": "user", "content": "lookup"}}
			if tt.resumed {
				messages = append(messages,
					map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
						"id": "call_lookup", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{}`},
					}}},
					map[string]any{"role": "tool", "tool_call_id": "call_lookup", "content": "result"},
				)
			}
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "grok-4.6", "messages": messages, "stream": tt.failed,
				"tools": []any{map[string]any{"type": "function", "function": map[string]any{
					"name": "lookup", "parameters": map[string]any{"type": "object"},
				}}},
			}, nil)
			wantHTTPStatus := http.StatusOK
			if tt.uncommitted {
				wantHTTPStatus = http.StatusBadGateway
			}
			if response.Code != wantHTTPStatus {
				t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
			}
			if tt.failed {
				var failureMessage string
				if tt.uncommitted {
					var payload struct {
						Error struct {
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
						t.Fatalf("decode failure response: %v body=%s", err, response.Body.String())
					}
					failureMessage = payload.Error.Message
				} else {
					for block := range bytes.SplitSeq(response.Body.Bytes(), []byte("\n\n")) {
						_, data := parseSSEEventChunk(block)
						payload, ok := decodeSSEPayload(data)
						if !ok {
							continue
						}
						if failure, ok := payload["error"].(map[string]any); ok {
							failureMessage, _ = failure["message"].(string)
						}
					}
				}
				if failureMessage != "Cursor stream interrupted" {
					t.Fatalf("stream failure missing from response: %s", response.Body.String())
				}
			} else {
				var payload struct {
					Usage struct {
						PromptTokens     int `json:"prompt_tokens"`
						CompletionTokens int `json:"completion_tokens"`
						TotalTokens      int `json:"total_tokens"`
						PromptDetails    struct {
							CachedTokens         int `json:"cached_tokens"`
							CachedCreationTokens int `json:"cached_creation_tokens"`
						} `json:"prompt_tokens_details"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatalf("decode response: %v body=%s", err, response.Body.String())
				}
				want := tt.wantClient
				wantPrompt := want.InputTokens + want.CacheReadTokens + want.CacheWriteTokens
				if tt.resumed {
					if payload.Usage.PromptTokens <= 0 || payload.Usage.PromptTokens >= 1000 ||
						payload.Usage.CompletionTokens <= 0 || payload.Usage.CompletionTokens >= 100 ||
						payload.Usage.TotalTokens != payload.Usage.PromptTokens+payload.Usage.CompletionTokens ||
						payload.Usage.PromptDetails.CachedTokens != 0 || payload.Usage.PromptDetails.CachedCreationTokens != 0 {
						t.Fatalf("resumed response exposed cumulative usage: %+v", payload.Usage)
					}
				} else if payload.Usage.PromptTokens != wantPrompt || payload.Usage.CompletionTokens != want.OutputTokens ||
					payload.Usage.TotalTokens != wantPrompt+want.OutputTokens || payload.Usage.PromptDetails.CachedTokens != want.CacheReadTokens ||
					payload.Usage.PromptDetails.CachedCreationTokens != want.CacheWriteTokens {
					t.Fatalf("client usage=%+v want=%+v", payload.Usage, want)
				}
			}
			entry := waitForProxyLog(t, env, "grok-4.6")
			want := tt.wantBillable
			wantStatus := http.StatusOK
			wantSuccesses, wantFailures := int64(1), int64(0)
			if tt.failed {
				wantStatus = http.StatusBadGateway
				wantSuccesses, wantFailures = 0, 1
			}
			priced := want
			if tt.wantHighTier != nil {
				// Our high-tier prices are double the low-tier ones.
				priced.Add(tt.wantHighTier)
			}
			wantCost := (float64(priced.InputTokens)*inputPrice + float64(priced.OutputTokens)*outputPrice +
				float64(priced.CacheReadTokens)*cacheReadPrice + float64(priced.CacheWriteTokens)*cacheWritePrice) / 1_000_000
			if entry.StatusCode != wantStatus || entry.InputTokens != want.InputTokens || entry.OutputTokens != want.OutputTokens ||
				entry.CacheReadInputTokens != want.CacheReadTokens || entry.CacheCreationInputTokens != want.CacheWriteTokens ||
				entry.Cache5mInputTokens != want.CacheWriteTokens || entry.ReasoningTokens != want.ReasoningTokens || math.Abs(entry.Cost-wantCost) > 1e-12 {
				t.Fatalf("logged usage=%+v want=%+v cost=%v", entry, want, wantCost)
			}
			if tt.wantEstimate {
				if entry.UsageEstimate == nil || entry.UsageEstimate.InputTokens <= 0 ||
					(tt.uncommitted && entry.UsageEstimate.OutputTokens != 0) || (!tt.uncommitted && entry.UsageEstimate.OutputTokens <= 0) {
					t.Fatalf("missing or incorrect failure estimate: %+v", entry.UsageEstimate)
				}
			} else if entry.UsageEstimate != nil {
				t.Fatalf("real usage must not carry an estimate: %+v", entry.UsageEstimate)
			}
			if (entry.CostBreakdown != nil) != (wantCost > 0) {
				t.Fatalf("cost=%g, cost breakdown=%+v", wantCost, entry.CostBreakdown)
			}
			if breakdown := entry.CostBreakdown; breakdown != nil {
				wantInputCost := float64(priced.InputTokens) * inputPrice / 1_000_000
				if math.Abs(breakdown.Total-wantCost) > 1e-12 || breakdown.Input.Quantity != want.InputTokens ||
					math.Abs(breakdown.Input.Cost-wantInputCost) > 1e-12 ||
					math.Abs(breakdown.Input.PricePerMillion-wantInputCost*1_000_000/float64(want.InputTokens)) > 1e-12 {
					t.Fatalf("cost breakdown=%+v, want total=%v input cost=%v", breakdown, wantCost, wantInputCost)
				}
			}
			for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				token, err = env.store.GetAuthTokenByValue(ctx, tokenHash)
				if err == nil && token.SuccessCount+token.FailureCount > 0 {
					if token.PromptTokensTotal != int64(want.InputTokens) || token.CompletionTokensTotal != int64(want.OutputTokens) ||
						token.CacheReadTokensTotal != int64(want.CacheReadTokens) || token.CacheCreationTokensTotal != int64(want.CacheWriteTokens) ||
						math.Abs(token.TotalCostUSD-wantCost) > 1e-12 || token.SuccessCount != wantSuccesses || token.FailureCount != wantFailures {
						t.Fatalf("token statistics=%+v want=%+v cost=%v", token, want, wantCost)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("token usage was not billed: token=%+v err=%v", token, err)
				}
			}
		})
	}
}

/**
 * @brief 工具轮缺少真实报告时单独展示估算，不累计真实 Token 或费用。
 * @param t 测试上下文。
 */
func TestProxy_CursorOAuthToolTurnReturnsEstimatedUsageWithoutBilling(t *testing.T) {
	t.Parallel()
	credentialJSON, err := (&cursorauth.Credential{
		APIKey: "cursor-user-api-key", AccessToken: "cursor-access-token", Email: "user@example.com",
	}).JSON()
	if err != nil {
		t.Fatalf("encode cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-tool-usage", upstreamProtocol: "openai", models: "grok-4.6",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	runner := &fakeCursorRunner{
		text:      "looking around",
		toolCalls: []cursorauth.ToolCall{{ID: "call_lookup", Name: "lookup", Arguments: json.RawMessage(`{}`)}},
	}
	env.server.cursorRunner = runner
	ctx := context.Background()
	tokenHash := model.HashToken("test-api-key")
	if err := env.store.CreateAuthToken(ctx, &model.AuthToken{Token: tokenHash, IsActive: true}); err != nil {
		t.Fatalf("CreateAuthToken: %v", err)
	}
	token, err := env.store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("GetAuthTokenByValue: %v", err)
	}
	injectAPIToken(env.server.authService, "test-api-key", 0, token.ID)

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "grok-4.6", "messages": []any{map[string]any{"role": "user", "content": "lookup"}},
		"tools": []any{map[string]any{
			"type": "function", "function": map[string]any{
				"name": "lookup", "parameters": map[string]any{"type": "object"},
			},
		}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v body=%s", err, response.Body.String())
	}
	if payload.Usage.TotalTokens != payload.Usage.PromptTokens+payload.Usage.CompletionTokens {
		t.Fatalf("tool response usage = %+v", payload.Usage)
	}

	requestLog := waitForProxyLog(t, env, "grok-4.6")
	assertCursorDisplayEstimate(t, response.Body.Bytes(), "openai", false, requestLog, 15)
	if runner.request.OnDetachedUsage == nil {
		t.Fatal("missing usage receiver for detached tool session")
	}
	runner.request.OnDetachedUsage(cursorauth.Usage{
		InputTokens: 17, OutputTokens: 8, ReasoningTokens: 3, CacheReadTokens: 5, CacheWriteTokens: 4,
	})
	since := requestLog.Time.Add(-time.Second)
	query, responseLog := newTestContext(t, newRequest(http.MethodGet, "/admin/logs?log_source=cursor_detached_usage", nil))
	env.server.HandleErrors(query)
	logs := mustParseAPIResponse[[]model.LogEntry](t, responseLog.Body.Bytes()).Data
	if responseLog.Code != http.StatusOK || len(logs) != 1 {
		t.Fatalf("detached usage log query = %d, %s", responseLog.Code, responseLog.Body.String())
	}
	record := logs[0]
	if record.AuthTokenID != token.ID || record.ChannelID != requestLog.ChannelID || record.Model != requestLog.Model ||
		record.LogSource != model.LogSourceCursorDetachedUsage || record.StatusCode != 0 || record.Cost != 0 || record.UsageEstimate != nil ||
		record.InputTokens != 17 || record.OutputTokens != 8 || record.ReasoningTokens != 3 ||
		record.CacheReadInputTokens != 5 || record.CacheCreationInputTokens != 4 || record.Cache5mInputTokens != 4 {
		t.Fatalf("detached usage attribution or amounts = %+v", record)
	}
	stats, err := env.store.GetStats(ctx, since, time.Now(), &model.LogFilter{LogSource: model.LogSourceAll}, true)
	if err != nil || len(stats) != 1 || stats[0].Total != 1 || stats[0].Success != 1 || stats[0].Error != 0 ||
		stats[0].LastRequestID == nil || *stats[0].LastRequestID != requestLog.ID {
		t.Fatalf("detached usage changed request statistics: %+v, err=%v", stats, err)
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		token, err = env.store.GetAuthTokenByValue(ctx, tokenHash)
		if err == nil && token.SuccessCount == 1 {
			if token.PromptTokensTotal != 0 || token.CompletionTokensTotal != 0 || token.CacheReadTokensTotal != 0 ||
				token.CacheCreationTokensTotal != 0 || token.TotalCostUSD != 0 {
				t.Fatalf("estimated tool usage changed real token statistics: %+v", token)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tool turn statistics were not recorded: token=%+v err=%v", token, err)
		}
	}
}

/**
 * @brief Runner 未启动推理便失败时，不生成消费估算或费用。
 * @param t 测试上下文。
 */
func TestProxy_CursorOAuthRunnerStartFailureHasNoUsageEstimate(t *testing.T) {
	t.Parallel()
	credentialJSON, err := (&cursorauth.Credential{APIKey: "test-key", AccessToken: "test-session"}).JSON()
	if err != nil {
		t.Fatalf("encode Cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-start-failure", upstreamProtocol: "openai", models: "grok-4.6",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	env.server.cursorRunner = &fakeCursorRunner{err: cursorauth.ErrAgentMissing}
	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "grok-4.6", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
	}
	entry := waitForProxyLog(t, env, "grok-4.6")
	if entry.UsageEstimate != nil || entry.InputTokens != 0 || entry.OutputTokens != 0 || entry.Cost != 0 {
		t.Fatalf("runner startup failure fabricated usage: %+v", entry)
	}
}

/**
 * @brief Cursor 缺凭证或 Bridge 暂不可用时切到健康渠道，只冷却缺 Key 的 OAuth 凭证。
 * @param t 测试上下文。
 */
func TestProxy_CursorOAuthUnavailableCandidateFailsOver(t *testing.T) {
	t.Parallel()
	credential := `{"access_token":"test-session","api_key":"test-key"}`
	for _, test := range []struct {
		name         string
		credential   string
		runner       cursorauth.Runner
		status       int
		message      string
		wantCooldown bool
	}{
		{
			name: "legacy access token", credential: `{"access_token":"test-session"}`,
			runner: cursorauth.NewSDKRunner(), status: http.StatusUnauthorized,
			message: cursorauth.ErrMissingAPIKey.Error(), wantCooldown: true,
		},
		{
			name: "runner unavailable", credential: credential, status: http.StatusServiceUnavailable,
			message: "cursor SDK runner is unavailable",
		},
		{
			name: "bridge unavailable", credential: credential, status: http.StatusServiceUnavailable,
			runner: &fakeCursorRunner{err: cursorauth.ErrAgentMissing}, message: cursorauth.ErrAgentMissing.Error(),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backup := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"backup","choices":[{"message":{"role":"assistant","content":"reply from backup"},"finish_reason":"stop"}]}`))
			}))
			defer backup.Close()
			env := setupProxyTestEnv(t, []testChannel{
				{name: "cursor-primary", upstreamProtocol: "openai", models: "grok-4.6",
					authType: model.AuthTypeCursorOAuth, oauthCredential: test.credential},
				{name: "backup", upstreamProtocol: "openai", models: "grok-4.6"},
			}, map[int]string{0: "https://unused.invalid", 1: backup.URL})
			env.server.cursorRunner = test.runner
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "grok-4.6", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
			}, nil)
			if response.Code != http.StatusOK || gjson.GetBytes(response.Body.Bytes(), "choices.0.message.content").String() != "reply from backup" {
				t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
			}
			failure := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
				return entry.ChannelName == "cursor-primary" && entry.StatusCode == test.status && strings.Contains(entry.Message, test.message)
			})
			if failure == nil {
				t.Fatal("missing Cursor failure log")
			}
			cooldowns, err := env.store.GetAllChannelCooldowns(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if cooled := !cooldowns[failure.ChannelID].IsZero(); cooled != test.wantCooldown {
				t.Fatalf("Cursor credential cooldown = %t, want %t: %v", cooled, test.wantCooldown, cooldowns)
			}
			models, err := env.store.GetAllModelCooldowns(t.Context())
			if err != nil || len(models) != 0 {
				t.Fatalf("unexpected model cooldown: %v, %v", models, err)
			}
		})
	}
}

/**
 * @brief URL 图片跳过 Cursor；仅有 Cursor 候选时提示改用内联图片。两种结果均不写冷却。
 * @param t 测试上下文。
 */
func TestProxy_CursorOAuthURLImageCapabilityFallback(t *testing.T) {
	t.Parallel()
	const imageURL = "https://example.com/image.png"
	for _, test := range []struct {
		name      string
		backup    bool
		anthropic bool
	}{
		{name: "compatible backup", backup: true},
		{name: "only Cursor candidates"},
		{name: "Anthropic Messages", anthropic: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			primary := testChannel{
				name: "cursor-primary", upstreamProtocol: "openai", models: "grok-4.6",
				authType: model.AuthTypeCursorOAuth, oauthCredential: `{"access_token":"test-session","api_key":"test-key"}`,
			}
			secondary := primary
			secondary.name = "cursor-secondary"
			upstreams := map[int]string{0: "https://unused.invalid", 1: "https://unused.invalid"}
			if test.backup {
				secondary = testChannel{name: "backup", upstreamProtocol: "openai", models: primary.models}
				backup := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode backup request: %v", err)
					} else if got := gjson.GetBytes(body, "messages.0.content.0.image_url.url").String(); got != imageURL {
						t.Errorf("backup image URL = %q, want %q", got, imageURL)
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"backup","choices":[{"message":{"role":"assistant","content":"reply from backup"},"finish_reason":"stop"}]}`))
				}))
				defer backup.Close()
				upstreams[1] = backup.URL
			}
			env := setupProxyTestEnv(t, []testChannel{primary, secondary}, upstreams)
			env.server.cursorRunner = cursorauth.NewSDKRunner("/unusable-cursor-bridge-path")
			path := "/v1/chat/completions"
			image := map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}}
			if test.anthropic {
				path = "/v1/messages"
				image = map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": imageURL}}
			}
			response := doProxyRequest(t, env.engine, path, map[string]any{
				"model": primary.models, "messages": []any{map[string]any{
					"role": "user", "content": []any{image},
				}},
			}, nil)
			payload := gjson.ParseBytes(response.Body.Bytes())
			if test.backup {
				if response.Code != http.StatusOK || payload.Get("choices.0.message.content").String() != "reply from backup" {
					t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
				}
			} else if response.Code != http.StatusBadRequest || !strings.Contains(payload.Get("error.message").String(), "inline base64 images") {
				t.Fatalf("proxy status=%d body=%s, want inline-image guidance", response.Code, response.Body.String())
			}
			if test.anthropic && (payload.Get("type").String() != "error" || payload.Get("error.type").String() != "invalid_request_error") {
				t.Fatalf("proxy body=%s, want Anthropic invalid_request_error", response.Body.String())
			}
			if cooled, err := env.store.GetAllChannelCooldowns(t.Context()); err != nil || len(cooled) != 0 {
				t.Fatalf("unexpected channel cooldown: %v, %v", cooled, err)
			}
			if cooled, err := env.store.GetAllModelCooldowns(t.Context()); err != nil || len(cooled) != 0 {
				t.Fatalf("unexpected model cooldown: %v, %v", cooled, err)
			}
		})
	}
}

/**
 * @brief 工具恢复轮的客户端上下文不包含 SDK 独立 usage 或终态携带的跨轮累计消费。
 * @param t 测试上下文。
 */
func TestProxy_CursorOAuthResumedTurnDoesNotExposeCumulativeRunUsage(t *testing.T) {
	t.Parallel()
	cumulative := &cursorauth.Usage{
		InputTokens: 42, OutputTokens: 16_177, CacheReadTokens: 1_533_702, CacheWriteTokens: 102_950, TotalTokens: 1_652_871,
	}
	for _, clientProtocol := range []string{"anthropic", "openai"} {
		for _, streaming := range []bool{false, true} {
			for _, scenario := range []struct {
				name        string
				incremental *cursorauth.Usage
				terminal    *cursorauth.Usage
			}{
				{name: "no usage"},
				{name: "zero residual only", terminal: &cursorauth.Usage{}},
				{name: "terminal usage", terminal: cumulative},
				{name: "incremental usage", incremental: cumulative, terminal: &cursorauth.Usage{}},
			} {
				name := fmt.Sprintf("%s/stream=%t/%s", clientProtocol, streaming, scenario.name)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					credentialJSON, err := (&cursorauth.Credential{APIKey: "test-key", AccessToken: "test-session"}).JSON()
					if err != nil {
						t.Fatalf("encode Cursor credential: %v", err)
					}
					env := setupProxyTestEnv(t, []testChannel{{
						name: "cursor-resume-usage", upstreamProtocol: clientProtocol, models: "claude-opus-5-5",
						authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
					}}, map[int]string{0: "https://unused.invalid"})
					events := cursorUsageEventsRunner{{ThinkingDelta: "checking"}}
					if scenario.incremental != nil {
						events = append(events, cursorauth.Event{Usage: scenario.incremental})
					}
					env.server.cursorRunner = append(events, cursorauth.Event{Text: "final answer", Done: true, Usage: scenario.terminal})
					path := "/v1/chat/completions"
					messages := []any{
						map[string]any{"role": "user", "content": "find it"},
						map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
							"id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{}`},
						}}},
						map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "result"},
					}
					if clientProtocol == "anthropic" {
						path = "/v1/messages"
						messages = []any{
							map[string]any{"role": "user", "content": "find it"},
							map[string]any{"role": "assistant", "content": []any{map[string]any{
								"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{},
							}}},
							map[string]any{"role": "user", "content": []any{map[string]any{
								"type": "tool_result", "tool_use_id": "toolu_1", "content": "result",
							}}},
						}
					}
					response := doProxyRequest(t, env.engine, path, map[string]any{
						"model": "claude-opus-5-5", "messages": messages, "stream": streaming, "max_tokens": 256,
					}, nil)
					if response.Code != http.StatusOK {
						t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
					}
					payloads := [][]byte{response.Body.Bytes()}
					if streaming {
						payloads = nil
						for block := range bytes.SplitSeq(response.Body.Bytes(), []byte("\n\n")) {
							_, data := parseSSEEventChunk(block)
							if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
								payloads = append(payloads, data)
							}
						}
					}
					var usage map[string]any
					for _, data := range payloads {
						var payload struct {
							Usage map[string]any `json:"usage"`
						}
						if err := json.Unmarshal(data, &payload); err != nil {
							t.Fatalf("decode response: %v body=%s", err, data)
						}
						if payload.Usage != nil {
							usage = payload.Usage
						}
					}
					inputKey, outputKey := "input_tokens", "output_tokens"
					if clientProtocol == "openai" {
						inputKey, outputKey = "prompt_tokens", "completion_tokens"
					}
					input, inputOK := usage[inputKey].(float64)
					output, outputOK := usage[outputKey].(float64)
					if !inputOK || !outputOK || input <= 0 || input >= 1000 || output <= 0 || output >= 100 {
						t.Fatalf("resumed response exposed invalid context usage: %+v", usage)
					}
					if clientProtocol == "anthropic" {
						if usage["cache_read_input_tokens"] != float64(0) || usage["cache_creation_input_tokens"] != float64(0) {
							t.Fatalf("resumed response exposed cumulative cache usage: %+v", usage)
						}
					} else {
						details, ok := usage["prompt_tokens_details"].(map[string]any)
						if !ok || details["cached_tokens"] != float64(0) || details["cached_creation_tokens"] != float64(0) || usage["total_tokens"] != input+output {
							t.Fatalf("resumed response exposed invalid cache or total usage: %+v", usage)
						}
					}
					entry := waitForProxyLog(t, env, "claude-opus-5-5")
					if scenario.incremental == nil && scenario.terminal != cumulative {
						assertCursorDisplayEstimate(t, response.Body.Bytes(), clientProtocol, streaming, entry, 5)
					} else if entry.InputTokens != cumulative.InputTokens || entry.OutputTokens != cumulative.OutputTokens ||
						entry.CacheReadInputTokens != cumulative.CacheReadTokens || entry.CacheCreationInputTokens != cumulative.CacheWriteTokens || entry.UsageEstimate != nil {
						t.Fatalf("cumulative consumption was not preserved for billing: %+v", entry)
					}
				})
			}
		}
	}
}

func TestProxy_CursorOAuthFirstByteTimeoutPersistsActualDuration(t *testing.T) {
	credentialJSON, err := (&cursorauth.Credential{
		APIKey: "cursor-user-api-key", AccessToken: "cursor-access-token", Email: "user@example.com",
	}).JSON()
	if err != nil {
		t.Fatalf("encode cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-timeout", upstreamProtocol: "openai", models: "composer-2.5",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	env.server.firstByteTimeout = 20 * time.Millisecond
	env.server.streamTimeout = 0
	env.server.cursorRunner = &blockingCursorRunner{started: make(chan struct{}), release: make(chan struct{})}

	started := time.Now()
	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "composer-2.5", "messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
		"stream": true,
	}, nil)
	elapsed := time.Since(started)
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	entry := waitForProxyLog(t, env, "composer-2.5")
	if entry.StatusCode != util.StatusFirstByteTimeout || !entry.IsStreaming {
		t.Fatalf("log status = %d streaming = %v", entry.StatusCode, entry.IsStreaming)
	}
	if entry.FirstByteTime != 0 {
		t.Fatalf("first byte = %.3f, timeout emitted no client content", entry.FirstByteTime)
	}
	if elapsed >= time.Second || entry.Duration <= 0 || entry.Duration >= 1 {
		t.Fatalf("elapsed = %v logged duration = %.3f", elapsed, entry.Duration)
	}
	if !strings.Contains(entry.Message, "upstream first byte timeout") {
		t.Fatalf("log message = %q", entry.Message)
	}
}

/**
 * @brief 网关仅在未输出时切换中断的 Cursor 流，且按失败模型冷却。
 * @param t 测试上下文。
 */
func TestProxy_CursorInterruptedStreamCooldownAndFallback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		idle      bool
		committed bool
	}{
		{name: "heartbeat then idle", idle: true},
		{name: "heartbeat then EOF"},
		{name: "partial output then idle", idle: true, committed: true},
		{name: "partial output then EOF", committed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const modelID = "composer-2.5"
			env := setupProxyTestEnv(t, []testChannel{
				{name: "cursor-primary", upstreamProtocol: "openai", models: modelID + ",grok-4.6",
					authType: model.AuthTypeCursorOAuth, oauthCredential: `{"access_token":"session","api_key":"primary-key"}`},
				{name: "cursor-backup", upstreamProtocol: "openai", models: modelID,
					authType: model.AuthTypeCursorOAuth, oauthCredential: `{"access_token":"session","api_key":"backup-key"}`},
			}, map[int]string{0: "https://unused.invalid", 1: "https://unused.invalid"})
			env.server.streamIdleTimeout = 50 * time.Millisecond
			backupRuns := 0
			env.server.cursorRunner = cursorRunnerFunc(func(ctx context.Context, credential *cursorauth.Credential, _ cursorauth.Request) (<-chan cursorauth.Event, error) {
				events := make(chan cursorauth.Event, 3)
				if credential.APIKey == "backup-key" {
					backupRuns++
					events <- cursorauth.Event{Done: true, Text: "backup reply"}
					close(events)
					return events, nil
				}
				events <- cursorauth.Event{Ping: true}
				if tc.committed {
					events <- cursorauth.Event{Delta: "partial reply"}
				}
				if tc.idle {
					go func() {
						defer close(events)
						<-ctx.Done()
						events <- cursorauth.Event{Done: true, Err: context.Cause(ctx)}
					}()
				} else {
					close(events)
				}
				return events, nil
			})
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": modelID, "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hello"}},
			}, nil)
			wantBackupRuns := 1
			if tc.committed {
				wantBackupRuns = 0
			}
			if response.Code != http.StatusOK || backupRuns != wantBackupRuns {
				t.Fatalf("HTTP=%d backup calls=%d body=%s", response.Code, backupRuns, response.Body)
			}
			var text strings.Builder
			var failureCode string
			var completed bool
			for block := range bytes.SplitSeq(response.Body.Bytes(), []byte("\n\n")) {
				_, data := parseSSEEventChunk(block)
				completed = completed || bytes.Equal(data, []byte("[DONE]"))
				text.WriteString(gjson.GetBytes(data, "choices.0.delta.content").String())
				if code := gjson.GetBytes(data, "error.code").String(); code != "" {
					failureCode = code
				}
			}
			if tc.committed {
				if text.String() != "partial reply" || completed || failureCode != "upstream_stream_interrupted" {
					t.Fatalf("committed stream body=%s", response.Body)
				}
			} else if text.String() != "backup reply" || !completed || failureCode != "" || strings.Contains(response.Body.String(), ": ping") {
				t.Fatalf("failover stream body=%s", response.Body)
			}
			entry := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
				return entry.Model == modelID && entry.StatusCode == util.StatusStreamIncomplete
			})
			wantDiagnostic := "unexpected EOF"
			if tc.idle {
				wantDiagnostic = "upstream stream idle timeout"
			}
			if !strings.Contains(entry.Message, wantDiagnostic) || (entry.FirstByteTime > 0) != tc.committed {
				t.Fatalf("unexpected failure log %+v", entry)
			}
			modelCooldowns, err := env.store.GetAllModelCooldowns(t.Context())
			if err != nil || len(modelCooldowns) != 1 || len(modelCooldowns[entry.ChannelID]) != 1 ||
				!modelCooldowns[entry.ChannelID][modelID].After(time.Now()) {
				t.Fatalf("model cooldowns=%v err=%v", modelCooldowns, err)
			}
			channelCooldowns, err := env.store.GetAllChannelCooldowns(t.Context())
			if err != nil || len(channelCooldowns) != 0 {
				t.Fatalf("channel cooldowns=%v err=%v", channelCooldowns, err)
			}
		})
	}
}

/**
 * @brief 请求路由保留原模型，日志使用 SDK 实际选择的计费速度。
 * @param t 测试上下文。
 */
func TestProxy_CursorOAuthBillsSelectedSpeed(t *testing.T) {
	t.Parallel()
	credentialJSON, err := (&cursorauth.Credential{APIKey: "cursor-key", AccessToken: "cursor-session"}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-speed", upstreamProtocol: "openai",
		modelEntries: []model.ModelEntry{{Model: "composer-2.5"}},
		authType:     model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	env.server.cursorRunner = cursorUsageEventsRunner{{
		Done: true, Text: "ok", BillingModel: "composer-2.5-fast",
		Usage: &cursorauth.Usage{InputTokens: 1_000, OutputTokens: 1_000, CacheReadTokens: 1_000},
	}}
	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "composer-2.5", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, body = %s", response.Code, response.Body.String())
	}
	entry := waitForProxyLog(t, env, "composer-2.5")
	if entry.ServiceTier != "fast" || math.Abs(entry.Cost-0.0185) > 1e-12 || entry.CostBreakdown == nil ||
		math.Abs(entry.CostBreakdown.Input.PricePerMillion-3) > 1e-12 {
		t.Fatalf("logged Cursor cost=%+v, want Fast reference cost of $0.0185", entry)
	}
}

func TestProxy_CursorOAuthSupportsResponses(t *testing.T) {
	t.Parallel()

	for _, streaming := range []bool{false, true} {
		name := "non-stream"
		if streaming {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			upstreamHits := 0
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamHits++
				w.WriteHeader(http.StatusTeapot)
			}))
			defer upstream.Close()

			credentialJSON, err := (&cursorauth.Credential{
				APIKey: "cursor-user-api-key", AccessToken: "cursor-access-token", Email: "user@example.com",
			}).JSON()
			if err != nil {
				t.Fatalf("encode cursor credential: %v", err)
			}
			env := setupProxyTestEnv(t, []testChannel{{
				name: "cursor-responses", upstreamProtocol: "openai", models: "composer-2.5",
				authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
			}}, map[int]string{0: upstream.URL})
			if streaming {
				env.server.configService.cache["debug_log_enabled"] = &model.SystemSetting{
					Key: "debug_log_enabled", Value: "true",
				}
			}
			runner := &fakeCursorRunner{text: "MacBook M5 answer", usage: &cursorauth.Usage{
				InputTokens: 11, OutputTokens: 7, CacheReadTokens: 5, CacheWriteTokens: 3,
			}}
			env.server.cursorRunner = runner

			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": "composer-2.5",
				"input": []any{
					map[string]any{"role": "system", "content": "使用中文回复"},
					map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "input_text", "text": "macbook m5有几款\n\n"},
					}},
				},
				"temperature": "[undefined]", "top_p": "[undefined]",
				"max_output_tokens": "[undefined]", "instructions": "[undefined]",
				"tools": "[undefined]", "tool_choice": "[undefined]", "stream": streaming,
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
			}
			if upstreamHits != 0 {
				t.Fatalf("Cursor OAuth must not HTTP-forward, hits = %d", upstreamHits)
			}
			if !strings.Contains(runner.prompt, "使用中文回复") || !strings.Contains(runner.prompt, "macbook m5有几款") {
				t.Fatalf("translated prompt lost Responses messages: %q", runner.prompt)
			}
			if !strings.Contains(runner.prompt, "[undefined]") {
				t.Fatalf("caller-provided prompt content was rewritten: %q", runner.prompt)
			}
			entry := waitForProxyLog(t, env, "composer-2.5")
			if entry.CacheCreationInputTokens != 3 {
				t.Fatalf("logged cache creation = %d, want 3", entry.CacheCreationInputTokens)
			}
			if streaming {
				debugLog, err := env.store.GetDebugLogByLogID(context.Background(), entry.ID)
				if err != nil || debugLog == nil {
					t.Fatalf("load Cursor Responses debug log: debug=%+v err=%v", debugLog, err)
				}
				if !debugLog.ProtocolTransformed || !bytes.Equal(debugLog.TranslatedRespBody, response.Body.Bytes()) {
					t.Fatalf("translated debug response must match client body:\ndebug=%s\nclient=%s",
						debugLog.TranslatedRespBody, response.Body.Bytes())
				}
			}

			if streaming {
				var delta, completed bool
				for block := range bytes.SplitSeq(response.Body.Bytes(), []byte("\n\n")) {
					eventType, data := parseSSEEventChunk(block)
					payload, ok := decodeSSEPayload(data)
					if !ok {
						continue
					}
					switch eventType {
					case "response.output_text.delta":
						delta = payload["delta"] == "MacBook M5 answer"
					case "response.completed":
						completed = true
						if got := gjson.GetBytes(data, "response.usage.cache_creation_input_tokens").Int(); got != 3 {
							t.Fatalf("stream cache creation = %d, want 3; data=%s", got, data)
						}
					}
				}
				if !delta || !completed {
					t.Fatalf("invalid Responses stream: %s", response.Body.String())
				}
				return
			}

			var payload struct {
				Object string `json:"object"`
				Output []struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode Responses body: %v body=%s", err, response.Body.String())
			}
			if payload.Object != "response" || len(payload.Output) == 0 ||
				len(payload.Output[0].Content) == 0 || payload.Output[0].Content[0].Text != "MacBook M5 answer" {
				t.Fatalf("invalid Responses body: %s", response.Body.String())
			}
			if got := gjson.GetBytes(response.Body.Bytes(), "usage.cache_creation_input_tokens").Int(); got != 3 {
				t.Fatalf("JSON cache creation = %d, want 3", got)
			}
		})
	}
}

/**
 * @brief 验证 HTTP Responses 把工具结果交回原 Agent，并在所属执行会话内原样重放已完成的响应。
 * @param t 测试生命周期。
 */
func TestProxy_CursorResponsesContinuation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Cursor Bridge discovery fixture uses a shell executable")
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			env, bridge := setupCursorRoutingTest(t, []model.ModelEntry{
				{Model: "custom-model", RedirectModel: "upstream-a"},
				{Model: "custom-model", RedirectModel: "upstream-b"},
			})
			if res := batchUpdateSetting(t, env.server, privacySettingKey, `{"enabled":true,"words":["tool-output"]}`); res.Code != http.StatusOK {
				t.Fatalf("enable privacy restoration: HTTP %d %s", res.Code, res.Body)
			}
			const path = "/v1/responses"
			headers := map[string]string{"Session-Id": "responses", "Thread-Id": "original"}
			readTerminal := func(body []byte) gjson.Result {
				if !stream {
					return gjson.ParseBytes(body)
				}
				for block := range bytes.SplitSeq(body, []byte("\n\n")) {
					if event, data := parseSSEEventChunk(block); event == "response.completed" {
						return gjson.GetBytes(data, "response")
					}
				}
				return gjson.Result{}
			}
			first := doProxyRequest(t, env.engine, path, map[string]any{
				"model": "custom-model", "input": "lookup", "stream": stream,
				"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
			}, headers)
			terminal := readTerminal(first.Body.Bytes())
			responseID := terminal.Get("id").String()
			callID := terminal.Get(`output.#(type=="function_call").call_id`).String()
			created := bridge.createdAgents()
			if first.Code != http.StatusOK || responseID == "" || callID == "" || len(created) != 1 {
				t.Fatalf("first HTTP %d; created=%v; body=%s", first.Code, created, first.Body)
			}
			owner := created[0]
			payload := map[string]any{
				"model": "custom-model", "previous_response_id": responseID, "stream": stream,
				"input": []any{map[string]any{"type": "function_call_output", "call_id": callID, "output": "tool-output"}},
			}
			injectAPIToken(env.server.authService, "other-cursor-responses-api-key", 0, 2)
			for _, foreignHeaders := range []map[string]string{
				nil,
				{"Session-Id": "other", "Thread-Id": "original"},
				{"Session-Id": "responses", "Thread-Id": "other"},
				{"Session-Id": "responses", "Thread-Id": "original", "Authorization": "Bearer other-cursor-responses-api-key"},
			} {
				foreign := doProxyRequest(t, env.engine, path, payload, foreignHeaders)
				if foreign.Code != http.StatusBadRequest || !strings.Contains(foreign.Body.String(), "resend the full conversation input without previous_response_id") {
					t.Fatalf("unavailable history HTTP %d; body=%s", foreign.Code, foreign.Body)
				}
			}
			second := doProxyRequest(t, env.engine, path, payload, headers)
			text := readTerminal(second.Body.Bytes()).Get(`output.#(type=="message").content.0.text`).String()
			if created := bridge.createdAgents(); second.Code != http.StatusOK || text != owner+" received tool-output" || !slices.Equal(created, []string{owner}) {
				t.Fatalf("continuation HTTP %d; created=%v; body=%s", second.Code, created, second.Body)
			}
			runner := env.server.swapCursorRunner(cursorRunnerFunc(func(context.Context, *cursorauth.Credential, cursorauth.Request) (<-chan cursorauth.Event, error) {
				t.Error("replayed or rejected tool continuation reached the Cursor Runner")
				return nil, cursorauth.ErrAgentMissing
			}))
			defer env.server.swapCursorRunner(runner)
			replay := doProxyRequest(t, env.engine, path, payload, headers)
			if replay.Code != http.StatusOK || replay.Header().Get("Content-Type") != second.Header().Get("Content-Type") ||
				!bytes.Equal(replay.Body.Bytes(), second.Body.Bytes()) {
				t.Fatalf("replay HTTP %d; original=%s; replay=%s", replay.Code, second.Body, replay.Body)
			}
			alternate := "route-a"
			if owner == alternate {
				alternate = "route-b"
			}
			for _, mismatch := range []struct {
				name    string
				path    string
				payload map[string]any
				headers map[string]string
			}{
				{"path", "/v1/codex/responses", payload, headers},
				{"query", path + "?timeout=5", payload, headers},
				{"channel", path, payload, map[string]string{"Session-Id": "responses", "Thread-Id": "original", "x-ccload-channel": alternate}},
				{"thread", path, payload, map[string]string{"Session-Id": "responses", "Thread-Id": "other"}},
				{"token", path, payload, map[string]string{"Session-Id": "responses", "Thread-Id": "original", "Authorization": "Bearer other-cursor-responses-api-key"}},
				{"stream", path, map[string]any{"model": "custom-model", "previous_response_id": responseID, "input": payload["input"], "stream": !stream}, headers},
				{"stale ordinary input", path, map[string]any{"model": "custom-model", "previous_response_id": responseID, "input": "unrelated"}, headers},
				{"changed tool output", path, map[string]any{
					"model": "custom-model", "previous_response_id": responseID, "stream": stream,
					"input": []any{map[string]any{"type": "function_call_output", "call_id": callID, "output": "changed"}},
				}, headers},
			} {
				t.Run(mismatch.name, func(t *testing.T) {
					res := doProxyRequest(t, env.engine, mismatch.path, mismatch.payload, mismatch.headers)
					if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "resend the full conversation input without previous_response_id") {
						t.Fatalf("mismatched replay HTTP %d; body=%s", res.Code, res.Body)
					}
				})
			}
			ctx := context.Background()
			configs, err := env.store.ListConfigs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			ownerIndex := slices.IndexFunc(configs, func(cfg *model.Config) bool { return cfg.Name == owner })
			if ownerIndex < 0 {
				t.Fatalf("Cursor replay owner %q not found", owner)
			}
			cfg := configs[ownerIndex]
			if stream {
				cfg.ModelEntries = []model.ModelEntry{{Model: "custom-model", RedirectModel: "upstream-replacement"}}
				if _, err := env.store.UpdateConfig(ctx, cfg.ID, cfg); err != nil {
					t.Fatal(err)
				}
			} else {
				updated, err := env.store.CompareAndSwapOAuthCredential(ctx, cfg.ID, model.AuthTypeCursorOAuth, cfg.OAuthCredential,
					`{"api_key":"replacement-account","access_token":"replacement-session"}`)
				if err != nil || !updated {
					t.Fatalf("rotate Cursor key: updated=%t, err=%v", updated, err)
				}
			}
			env.server.InvalidateChannelListCache()
			reconfigured := doProxyRequest(t, env.engine, path, payload, headers)
			if reconfigured.Code != http.StatusBadRequest || !strings.Contains(reconfigured.Body.String(), "resend the full conversation input without previous_response_id") {
				t.Fatalf("changed replay route HTTP %d; body=%s", reconfigured.Code, reconfigured.Body)
			}
			env.server.responsesExecutionSessions.cleanup(time.Now().Add(time.Hour))
			expired := doProxyRequest(t, env.engine, path, payload, headers)
			if expired.Code != http.StatusBadRequest || !strings.Contains(expired.Body.String(), "resend the full conversation input without previous_response_id") {
				t.Fatalf("expired history HTTP %d; body=%s", expired.Code, expired.Body)
			}
			if created := bridge.createdAgents(); !slices.Equal(created, []string{owner}) {
				t.Fatalf("expected one Agent after replays, created = %v", created)
			}
		})
	}
}

/**
 * @brief 验证 Cursor 生成、流式交付和展开本地压缩历史，并拒绝外部压缩历史。
 * @param t 测试生命周期。
 */
func TestProxy_CursorResponsesCompaction(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			const path = "/v1/responses"
			env := setupProxyTestEnv(t, []testChannel{{
				name: "cursor", upstreamProtocol: "openai", models: "composer-2.5",
				authType: model.AuthTypeCursorOAuth, oauthCredential: `{"api_key":"fixture","access_token":"session"}`,
			}}, map[int]string{0: "https://unused.invalid"})
			var requests []cursorauth.Request
			env.server.cursorRunner = cursorRunnerFunc(func(ctx context.Context, credential *cursorauth.Credential, req cursorauth.Request) (<-chan cursorauth.Event, error) {
				requests = append(requests, req)
				text := "continuation reply"
				if strings.Contains(req.Prompt, responsesCompactionPrompt) {
					text = "retained conversation finding"
				}
				return cursorUsageEventsRunner{{Text: text, Usage: &cursorauth.Usage{InputTokens: 9, OutputTokens: 4}, Done: true}}.Run(ctx, credential, req)
			})
			compactPath := path + "/compact"
			input := []any{
				map[string]any{"role": "user", "content": "old task"},
				map[string]any{"type": "function_call", "call_id": "call_old", "name": "lookup", "arguments": "{}"},
				map[string]any{"type": "function_call_output", "call_id": "call_old", "output": "historical result"},
			}
			if stream {
				compactPath = path
				input = append(input, map[string]any{"type": "compaction_trigger"})
			}
			headers := map[string]string{"Session-Id": "cursor-summary"}
			res := doProxyRequest(t, env.engine, compactPath, map[string]any{
				"model": "composer-2.5", "stream": stream, "input": input, "tool_choice": "required",
				"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
			}, headers)
			if res.Code != http.StatusOK || len(requests) != 1 {
				t.Fatalf("compaction HTTP %d; SDK runs=%d; body=%s", res.Code, len(requests), res.Body)
			}
			req := requests[0]
			if len(req.Tools) != 0 || len(req.ToolResults) != 0 || req.SessionScope != (cursorauth.SessionScope{}) || req.ToolChoice != "auto" {
				t.Fatalf("summary inherited runtime state: %+v", req)
			}
			for _, want := range []string{"old task", "historical result", responsesCompactionPrompt} {
				if !strings.Contains(req.Prompt, want) {
					t.Fatalf("summary prompt lost %q: %q", want, req.Prompt)
				}
			}
			terminal := gjson.ParseBytes(res.Body.Bytes())
			object := "response.compaction"
			if stream {
				object = "response"
				var types []string
				for block := range bytes.SplitSeq(res.Body.Bytes(), []byte("\n\n")) {
					event, data := parseSSEEventChunk(block)
					if event != "" {
						types = append(types, event)
					}
					if event == "response.completed" {
						terminal = gjson.GetBytes(data, "response")
					}
				}
				if !slices.Equal(types, []string{"response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.completed"}) {
					t.Fatalf("compaction SSE events: %v", types)
				}
			}
			capsule := terminal.Get("output.0")
			if terminal.Get("object").String() != object || terminal.Get("id").String() == "" ||
				terminal.Get("usage.input_tokens").Int() != 9 || len(terminal.Get("output").Array()) != 1 ||
				capsule.Get("type").String() != "compaction" || !strings.HasPrefix(capsule.Get("encrypted_content").String(), responsesCompactionPrefix) {
				t.Fatalf("compaction terminal: %s", res.Body)
			}
			foreign := doProxyRequest(t, env.engine, path, json.RawMessage(`{"model":"composer-2.5","input":[
				{"type":"compaction","encrypted_content":"foreign"},{"role":"user","content":"continue"}]}`), headers)
			if foreign.Code != http.StatusBadRequest || !strings.Contains(foreign.Body.String(), "cannot restore foreign compaction history") || len(requests) != 1 {
				t.Fatalf("foreign history HTTP %d; SDK runs=%d; body=%s", foreign.Code, len(requests), foreign.Body)
			}
			followups := []map[string]any{
				{"model": "composer-2.5", "previous_response_id": terminal.Get("id").String(), "input": "continue by reference"},
				{"model": "composer-2.5", "input": []any{json.RawMessage(capsule.Raw), map[string]any{"role": "user", "content": "continue by replay"}}},
			}
			for i, followup := range followups {
				continued := doProxyRequest(t, env.engine, path, followup, headers)
				if continued.Code != http.StatusOK || len(requests) != i+2 {
					t.Fatalf("continuation HTTP %d; SDK runs=%d; body=%s", continued.Code, len(requests), continued.Body)
				}
				prompt := requests[i+1].Prompt
				if !strings.Contains(prompt, "Context summary from previous turns:\nretained conversation finding") ||
					!strings.Contains(prompt, "continue by") || strings.Contains(prompt, "old task") || strings.Contains(prompt, responsesCompactionPrompt) {
					t.Fatalf("invalid replayed prompt: %q", prompt)
				}
			}
		})
	}
}

/**
 * @brief 验证 Cursor 合成的 Responses ID 切换渠道后回放历史，且新终态能继续续轮。
 * @param t 测试生命周期。
 */
func TestProxy_CursorResponsesCrossRouteHistory(t *testing.T) {
	for _, tc := range []struct {
		name, protocol                   string
		stream, fallback, websocketFirst bool
	}{
		{name: "native JSON", protocol: "codex"},
		{name: "native SSE fallback", protocol: "codex", stream: true, fallback: true},
		{name: "native HTTP after Cursor WebSocket", protocol: "codex", websocketFirst: true},
		{name: "translated JSON", protocol: "openai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const path = "/v1/responses"
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if !bytes.Contains(body, []byte("first Cursor answer")) || gjson.GetBytes(body, "previous_response_id").Exists() || gjson.GetBytes(body, "stream").Bool() != tc.stream {
					t.Errorf("upstream did not receive a full history with the correct stream mode: %s", body)
				}
				if tc.protocol == "codex" {
					const response = `{"id":"resp_native","object":"response","status":"completed","output":[{"id":"msg_native","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"other route answer"}]}]}`
					if tc.stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, response)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"chat_other","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"other route answer"}}]}`)
			}))
			defer upstream.Close()
			env := setupProxyTestEnv(t, []testChannel{
				{name: "cursor", upstreamProtocol: "openai", models: "composer-2.5", priority: 100, authType: model.AuthTypeCursorOAuth, oauthCredential: `{"api_key":"fixture","access_token":"session"}`},
				{name: "other", upstreamProtocol: tc.protocol, models: "composer-2.5", priority: 50, apiKey: "other-channel-key"},
			}, map[int]string{0: "https://unused.invalid", 1: upstream.URL})
			var prompts []string
			env.server.cursorRunner = cursorRunnerFunc(func(ctx context.Context, credential *cursorauth.Credential, req cursorauth.Request) (<-chan cursorauth.Event, error) {
				prompts = append(prompts, req.Prompt)
				if tc.fallback && len(prompts) == 2 {
					return nil, errors.New("fixture Cursor bridge unavailable before output")
				}
				return cursorUsageEventsRunner{{Text: "first Cursor answer", Done: true}}.Run(ctx, credential, req)
			})
			const sessionID = "cross-route"
			cursorHeaders := map[string]string{"Session-Id": sessionID, "x-ccload-channel": "cursor"}
			getResponseID := func(recorder *httptest.ResponseRecorder) string {
				t.Helper()
				if recorder.Code != http.StatusOK {
					t.Fatalf("HTTP %d; body=%s", recorder.Code, recorder.Body)
				}
				if strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
					parser := newSSEUsageParser("codex")
					if err := parser.Feed(recorder.Body.Bytes()); err != nil {
						t.Fatal(err)
					}
					if turn, ok := parser.GetResponsesTurnResult(); ok && turn.completedResponseID != "" {
						return turn.completedResponseID
					}
				}
				if id := gjson.GetBytes(recorder.Body.Bytes(), "id").String(); id != "" {
					return id
				}
				t.Fatalf("missing terminal response ID: %s", recorder.Body)
				return ""
			}
			var firstID string
			if tc.websocketFirst {
				conn := dialResponsesWebsocketWithTokenAndHeaders(t, env.engine, "test-api-key", http.Header{
					"Session-Id": {sessionID}, "X-Ccload-Channel": {"cursor"},
				})
				if err := conn.WriteJSON(map[string]any{
					"type": "response.create", "model": "composer-2.5", "input": []any{map[string]string{"role": "user", "content": "original task"}},
				}); err != nil {
					t.Fatal(err)
				}
				terminal := readWebsocketUntilType(t, conn, "response.completed")
				response, _ := terminal["response"].(map[string]any)
				firstID, _ = response["id"].(string)
				if firstID == "" {
					t.Fatalf("missing WebSocket terminal response ID: %#v", terminal)
				}
			} else {
				firstID = getResponseID(doProxyRequest(t, env.engine, path, map[string]any{
					"model": "composer-2.5", "input": "original task", "stream": tc.stream,
				}, cursorHeaders))
			}
			secondHeaders := map[string]string{"Session-Id": sessionID}
			if !tc.fallback {
				secondHeaders["x-ccload-channel"] = "other"
			}
			secondID := getResponseID(doProxyRequest(t, env.engine, path, map[string]any{
				"model": "composer-2.5", "input": "other route task", "stream": tc.stream, "previous_response_id": firstID,
			}, secondHeaders))
			third := doProxyRequest(t, env.engine, path, map[string]any{
				"model": "composer-2.5", "previous_response_id": secondID, "input": "back to Cursor",
			}, cursorHeaders)
			wantRuns := 2
			if tc.fallback {
				wantRuns++
			}
			if third.Code != http.StatusOK || len(prompts) != wantRuns {
				t.Fatalf("third HTTP %d; SDK runs=%d; body=%s", third.Code, len(prompts), third.Body)
			}
			if prompt := prompts[len(prompts)-1]; !strings.Contains(prompt, "other route task") || !strings.Contains(prompt, "other route answer") ||
				!strings.Contains(prompt, "back to Cursor") || !strings.Contains(prompt, "first Cursor answer") {
				t.Fatalf("invalid cross-route history: %q", prompt)
			}
		})
	}
}

func TestProxy_CursorOAuthSupportsGemini(t *testing.T) {
	t.Parallel()

	for _, streaming := range []bool{false, true} {
		name := "non-stream"
		path := "/v1beta/models/composer-2.5:generateContent"
		if streaming {
			name = "stream"
			path = "/v1beta/models/composer-2.5:streamGenerateContent?alt=sse"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			credentialJSON, err := (&cursorauth.Credential{
				APIKey: "cursor-user-api-key", AccessToken: "cursor-access-token", Email: "user@example.com",
			}).JSON()
			if err != nil {
				t.Fatalf("encode cursor credential: %v", err)
			}
			env := setupProxyTestEnv(t, []testChannel{{
				name: "cursor-gemini", upstreamProtocol: "openai", models: "composer-2.5",
				authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
			}}, map[int]string{0: "https://unused.invalid"})
			runner := &fakeCursorRunner{text: "Gemini via Cursor", usage: &cursorauth.Usage{
				InputTokens: 11, OutputTokens: 7,
			}}
			env.server.cursorRunner = runner

			response := doProxyRequest(t, env.engine, path, map[string]any{
				"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "使用中文回复"}}},
				"contents": []any{map[string]any{
					"role": "user", "parts": []any{map[string]any{"text": "最新模型是什么"}},
				}},
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
			}
			if runner.model != "composer-2.5" {
				t.Fatalf("SDK model = %q", runner.model)
			}
			if !strings.Contains(runner.prompt, "使用中文回复") || !strings.Contains(runner.prompt, "最新模型是什么") {
				t.Fatalf("translated prompt lost Gemini contents: %q", runner.prompt)
			}

			type geminiResponse struct {
				Candidates []struct {
					Content struct {
						Role  string `json:"role"`
						Parts []struct {
							Text string `json:"text"`
						} `json:"parts"`
					} `json:"content"`
					FinishReason string `json:"finishReason"`
				} `json:"candidates"`
				UsageMetadata *struct {
					PromptTokenCount     int `json:"promptTokenCount"`
					CandidatesTokenCount int `json:"candidatesTokenCount"`
				} `json:"usageMetadata"`
			}
			var payloads []geminiResponse
			if streaming {
				for block := range bytes.SplitSeq(response.Body.Bytes(), []byte("\n\n")) {
					_, data := parseSSEEventChunk(block)
					if len(data) == 0 {
						continue
					}
					var payload geminiResponse
					if err := json.Unmarshal(data, &payload); err != nil {
						t.Fatalf("decode Gemini stream event %q: %v", data, err)
					}
					payloads = append(payloads, payload)
				}
			} else {
				var payload geminiResponse
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatalf("decode Gemini body: %v body=%s", err, response.Body.String())
				}
				payloads = append(payloads, payload)
			}

			var text, finish string
			promptTokens, outputTokens := 0, 0
			for _, payload := range payloads {
				for _, candidate := range payload.Candidates {
					if candidate.Content.Role != "" && candidate.Content.Role != "model" {
						t.Fatalf("candidate role = %q", candidate.Content.Role)
					}
					for _, part := range candidate.Content.Parts {
						text += part.Text
					}
					if candidate.FinishReason != "" {
						finish = candidate.FinishReason
					}
				}
				if payload.UsageMetadata != nil {
					promptTokens = payload.UsageMetadata.PromptTokenCount
					outputTokens = payload.UsageMetadata.CandidatesTokenCount
				}
			}
			if text != "Gemini via Cursor" || finish != "STOP" || promptTokens != 11 || outputTokens != 7 {
				t.Fatalf("text=%q finish=%q usage=%d/%d body=%s",
					text, finish, promptTokens, outputTokens, response.Body.String())
			}
		})
	}
}

/**
 * @brief 响应转译失败取消推理后继续结算晚到用量，同时只输出一次失败。
 * @param t 测试上下文。
 */
func TestProxy_CursorOAuthResponsesTranslationFailureRetainsUsageAfterCancel(t *testing.T) {
	t.Parallel()
	credentialJSON, err := (&cursorauth.Credential{APIKey: "test-key", AccessToken: "test-session"}).JSON()
	if err != nil {
		t.Fatalf("encode Cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-response-translation-failure", upstreamProtocol: "openai",
		modelEntries: []model.ModelEntry{{Model: "grok-4.6", Pricing: channelPrice(1, 2)}},
		authType:     model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	translationErr := errors.New("Cursor response translation failed")
	env.server.protocolRegistry.RegisterStreamResponse(protocol.OpenAI, protocol.Codex,
		func(_ context.Context, _ string, _, _, raw []byte, _ *any) ([][]byte, error) {
			if len(raw) == 0 {
				return nil, nil
			}
			return [][]byte{[]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")}, translationErr
		})
	canceled := make(chan struct{})
	env.server.cursorRunner = cursorRunnerFunc(func(ctx context.Context, _ *cursorauth.Credential, _ cursorauth.Request) (<-chan cursorauth.Event, error) {
		events := make(chan cursorauth.Event, 4)
		go func() {
			defer close(events)
			events <- cursorauth.Event{Delta: "partial", Text: "partial"}
			<-ctx.Done()
			close(canceled)
			events <- cursorauth.Event{Delta: "late output", Ping: true}
			events <- cursorauth.Event{Usage: &cursorauth.Usage{InputTokens: 1000, OutputTokens: 200}}
			events <- cursorauth.Event{Done: true, Usage: &cursorauth.Usage{InputTokens: 500, OutputTokens: 100}, Err: context.Cause(ctx)}
		}()
		return events, nil
	})
	response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "grok-4.6", "input": "hello", "stream": true,
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("response translation failure did not cancel the runner")
	}
	var output strings.Builder
	failures, deltas := 0, 0
	for block := range bytes.SplitSeq(response.Body.Bytes(), []byte("\n\n")) {
		if len(bytes.TrimSpace(block)) == 0 {
			continue
		}
		eventType, data := parseSSEEventChunk(block)
		payload, ok := decodeSSEPayload(data)
		if !ok {
			t.Fatalf("unexpected stream block after translation failure: %s", block)
		}
		switch eventType {
		case "response.output_text.delta":
			deltas++
			delta, _ := payload["delta"].(string)
			output.WriteString(delta)
		case "error":
			failures++
			if payload["message"] != translationErr.Error() {
				t.Fatalf("translation failure was replaced: %s", data)
			}
		default:
			t.Fatalf("unexpected response event after translation failure: %s", data)
		}
	}
	if deltas != 1 || output.String() != "partial" || failures != 1 {
		t.Fatalf("translation failure duplicated output: deltas=%d text=%q failures=%d", deltas, output.String(), failures)
	}
	entry := waitForProxyLog(t, env, "grok-4.6")
	if entry.StatusCode != http.StatusBadGateway || entry.InputTokens != 1500 || entry.OutputTokens != 300 ||
		entry.UsageEstimate != nil || math.Abs(entry.Cost-0.0021) > 1e-12 {
		t.Fatalf("translation failure lost late real usage: %+v", entry)
	}
}

/**
 * @brief Responses SSE 保留原生工具和正文，并显示包含两次工具调用的用量估算。
 * @param t 测试上下文。
 */
func TestProxy_CursorOAuthResponsesEmitsNativeToolCalls(t *testing.T) {
	t.Parallel()

	credentialJSON, err := (&cursorauth.Credential{
		APIKey: "cursor-user-api-key", AccessToken: "cursor-access-token", Email: "user@example.com",
	}).JSON()
	if err != nil {
		t.Fatalf("encode cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-responses-tools", upstreamProtocol: "openai", models: "composer-2.5",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	env.server.cursorRunner = &fakeCursorRunner{
		text: "Checking.",
		toolCalls: []cursorauth.ToolCall{
			{ID: "call_readme", Name: "read", Arguments: json.RawMessage(`{"path":"README.md"}`)},
			{ID: "call_claude", Name: "read", Arguments: json.RawMessage(`{"path":"CLAUDE.md"}`)},
		},
	}

	response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "composer-2.5",
		"input": []any{map[string]any{"role": "user", "content": "read the project overview"}},
		"tools": []any{map[string]any{
			"type": "function", "name": "read", "description": "read a file",
			"parameters": map[string]any{
				"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required": []any{"path"},
			},
		}},
		"tool_choice": "auto", "stream": true,
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	assertCursorDisplayEstimate(t, response.Body.Bytes(), "responses", true, waitForProxyLog(t, env, "composer-2.5"), 32)

	var outputText strings.Builder
	var functionNames, functionArguments []string
	for block := range bytes.SplitSeq(response.Body.Bytes(), []byte("\n\n")) {
		eventType, data := parseSSEEventChunk(block)
		payload, ok := decodeSSEPayload(data)
		if !ok {
			continue
		}
		switch eventType {
		case "response.output_text.delta":
			delta, _ := payload["delta"].(string)
			outputText.WriteString(delta)
		case "response.output_item.done":
			item, _ := payload["item"].(map[string]any)
			if item["type"] == "function_call" {
				name, _ := item["name"].(string)
				arguments, _ := item["arguments"].(string)
				functionNames = append(functionNames, name)
				functionArguments = append(functionArguments, arguments)
			}
		}
	}
	if outputText.String() != "Checking." {
		t.Fatalf("output text leaked tool framing: %q", outputText.String())
	}
	if len(functionNames) != 2 || len(functionArguments) != 2 {
		t.Fatalf("function calls = %q %q", functionNames, functionArguments)
	}
	for i, wantPath := range []string{"README.md", "CLAUDE.md"} {
		var arguments map[string]any
		if functionNames[i] != "read" || json.Unmarshal([]byte(functionArguments[i]), &arguments) != nil ||
			arguments["path"] != wantPath {
			t.Fatalf("function call %d = %q %q", i, functionNames[i], functionArguments[i])
		}
	}
}

/**
 * @brief 验证 Cursor 的上下文超限通过 Gemini JSON 及已开始的 SSE 返回相同的原因。
 * @param t 测试上下文。
 */
func TestProxy_CursorGeminiContextOverflow(t *testing.T) {
	t.Parallel()
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%v", streaming), func(t *testing.T) {
			t.Parallel()
			env := setupProxyTestEnv(t, []testChannel{{
				name: "cursor-gemini", upstreamProtocol: "openai", models: "composer-2.5",
				authType: model.AuthTypeCursorOAuth, oauthCredential: `{"api_key":"test-key","access_token":"test-session"}`,
			}}, map[int]string{0: "https://unused.invalid"})
			failure := &cursorauth.BridgeError{SDKCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_VALIDATION_ERROR, Message: "prompt is too long: 72000 tokens > 64000 maximum"}
			events := cursorUsageEventsRunner{}
			path := "/v1beta/models/composer-2.5:generateContent"
			wantHTTP := http.StatusBadRequest
			if streaming {
				events = append(events, cursorauth.Event{Delta: "partial"})
				path = "/v1beta/models/composer-2.5:streamGenerateContent?alt=sse"
				wantHTTP = http.StatusOK
			}
			env.server.cursorRunner = append(events, cursorauth.Event{Done: true, Err: failure})
			rec := doProxyRequest(t, env.engine, path, map[string]any{
				"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "hello"}}}},
			}, nil)
			if rec.Code != wantHTTP {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			body := rec.Body.Bytes()
			if streaming {
				var terminal []byte
				for block := range bytes.SplitSeq(body, []byte("\n\n")) {
					_, data := parseSSEEventChunk(block)
					if len(data) != 0 {
						terminal = data
					}
				}
				body = terminal
			}
			failureJSON := gjson.GetBytes(body, "error")
			detail := failureJSON.Get("details.0")
			if failureJSON.Get("code").Int() != http.StatusBadRequest || failureJSON.Get("status").String() != "INVALID_ARGUMENT" ||
				!strings.Contains(failureJSON.Get("message").String(), "prompt is too long") ||
				detail.Get("@type").String() != "type.googleapis.com/google.rpc.ErrorInfo" || detail.Get("reason").String() != "CONTEXT_LENGTH_EXCEEDED" {
				t.Fatalf("Gemini error = %s", body)
			}
		})
	}
}

/**
 * @brief 模拟 Bridge 在结构化 RPC 错误、状态说明和 Run 终态中回显当前凭证。
 */
type cursorRedactionBridge struct {
	sdkv1connect.UnimplementedSdkAgentServiceHandler
	sdkv1connect.UnimplementedSdkBridgeControlServiceHandler
	apiKey   string ///< 当前账户的合成 User API Key。
	stopPath string ///< Bridge 子进程用于接收关闭信号的文件。
}

/**
 * @brief 响应 SDKRunner 探活。
 * @param ctx RPC 上下文。
 * @param request 探活请求。
 * @return 成功探活响应。
 * @return 始终为 nil。
 */
func (b *cursorRedactionBridge) Ping(_ context.Context, _ *connect.Request[sdkv1.PingRequest]) (*connect.Response[sdkv1.PingResponse], error) {
	return connect.NewResponse(&sdkv1.PingResponse{}), nil
}

/**
 * @brief 声明测试 Bridge 支持 SDKRunner 需要的线协议和能力。
 * @param ctx RPC 上下文。
 * @param request 版本探测请求。
 * @return SDKRunner 可接受的版本信息。
 * @return 始终为 nil。
 */
func (b *cursorRedactionBridge) GetVersion(_ context.Context, _ *connect.Request[sdkv1.GetVersionRequest]) (*connect.Response[sdkv1.GetVersionResponse], error) {
	return connect.NewResponse(&sdkv1.GetVersionResponse{
		ProtocolVersion: cursorauth.BridgeProtocol,
		Capabilities:    []string{"agent.create", "agent.send", "run.cancel", "agent.management"},
	}), nil
}

/**
 * @brief 允许测试 Bridge 子进程应 SDKRunner 请求正常退出。
 * @param ctx RPC 上下文。
 * @param request Shutdown 请求。
 * @return 成功关闭响应；通知失败时为 nil。
 * @return 写入退出信号文件失败时返回错误。
 */
func (b *cursorRedactionBridge) Shutdown(_ context.Context, _ *connect.Request[sdkv1.ShutdownRequest]) (*connect.Response[sdkv1.ShutdownResponse], error) {
	if err := os.WriteFile(b.stopPath, nil, 0o600); err != nil {
		return nil, err
	}
	return connect.NewResponse(&sdkv1.ShutdownResponse{}), nil
}

/**
 * @brief 为 RPC 限流场景返回带恢复时间的拒绝，为 Run 终态错误场景建立 Agent。
 * @param ctx RPC 上下文。
 * @param request 含当前 User API Key 和模型的请求。
 * @return 成功建立的 Agent；请求被拒绝时为 nil。
 * @return 模拟 Bridge 结构化 RPC 错误。
 */
func (b *cursorRedactionBridge) CreateAgent(_ context.Context, request *connect.Request[sdkv1.CreateAgentRequest]) (*connect.Response[sdkv1.CreateAgentResponse], error) {
	if request.Msg.GetOptions().GetApiKey() != b.apiKey {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unexpected test credential"))
	}
	if modelID := request.Msg.GetOptions().GetModel().GetId(); modelID != "cursor-json" {
		return connect.NewResponse(&sdkv1.CreateAgentResponse{AgentId: modelID}), nil
	}
	message := "RPC rejected " + b.apiKey + " for cursor-other-account"
	detail, err := connect.NewErrorDetail(&sdkv1.SdkErrorDetails{
		SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED,
		Message:      message,
		RetryAfter:   durationpb.New(19 * time.Second),
	})
	if err != nil {
		return nil, err
	}
	rpcErr := connect.NewError(connect.CodeResourceExhausted, errors.New(message))
	rpcErr.AddDetail(detail)
	return nil, rpcErr
}

/**
 * @brief 模拟带凭证回显的运行诊断；首帧额度错误只在终态正文中给出原因。
 * @param ctx RPC 上下文。
 * @param request 已建立的 Agent 所对应的消息请求。
 * @param stream 当前 Bridge 输出流。
 * @return 构造或者发出模拟帧失败时返回错误。
 */
func (b *cursorRedactionBridge) Send(_ context.Context, request *connect.Request[sdkv1.SendRequest], stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
	agentID := request.Msg.GetAgentId()
	if agentID != "cursor-quota-result" {
		status, err := structpb.NewStruct(map[string]any{
			"status": "RUNNING", "run_id": "run-redaction",
			"message": "status rejected " + b.apiKey + " for cursor-other-account",
		})
		if err != nil {
			return err
		}
		if err := stream.Send(&sdkv1.RunStreamMessage{Envelope: &sdkv1.RunStreamMessage_SdkMessage{
			SdkMessage: &sdkv1.SdkMessage{Type: "status", Message: status},
		}}); err != nil {
			return err
		}
	}
	if agentID == "cursor-sse" {
		assistant, err := structpb.NewStruct(map[string]any{
			"agent_id": agentID, "run_id": "run-redaction",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "ordinary assistant output"},
			}},
		})
		if err != nil {
			return err
		}
		if err := stream.Send(&sdkv1.RunStreamMessage{Envelope: &sdkv1.RunStreamMessage_SdkMessage{
			SdkMessage: &sdkv1.SdkMessage{Type: "assistant", Message: assistant},
		}}); err != nil {
			return err
		}
	}
	const statusError = sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR
	result := &sdkv1.RunResult{
		RunId: "run-redaction", AgentId: agentID, Status: statusError,
		Usage: &sdkv1.TokenUsage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3},
	}
	switch agentID {
	case "cursor-quota":
		result.ErrorMessage = new("You've hit your usage limit for cursor-quota")
	case "cursor-quota-result":
		result.Result = "You've hit your usage limit for " + b.apiKey
		result.ErrorMessage = new("Run failed")
	default:
		result.Result = "ordinary assistant output with " + b.apiKey
		result.ErrorCode = new("RATE_LIMIT_EXCEEDED")
		result.ErrorMessage = new("result rejected " + b.apiKey + " for cursor-other-account")
	}
	if err := stream.Send(&sdkv1.RunStreamMessage{Envelope: &sdkv1.RunStreamMessage_Result{Result: &sdkv1.RunStreamResult{
		RunId: "run-redaction", AgentId: agentID, Status: statusError, Result: result,
	}}}); err != nil {
		return err
	}
	return stream.Send(&sdkv1.RunStreamMessage{Envelope: &sdkv1.RunStreamMessage_Done{
		Done: &sdkv1.RunStreamDone{RunId: "run-redaction", AgentId: agentID},
	}})
}

/**
 * @brief 允许 SDKRunner 通过正常 Bridge 删除流程清理测试 Agent。
 * @param ctx RPC 上下文。
 * @param request 当前 Agent 删除请求。
 * @return 成功删除响应。
 * @return 始终为 nil。
 */
func (b *cursorRedactionBridge) DeleteAgent(_ context.Context, _ *connect.Request[sdkv1.DeleteAgentRequest]) (*connect.Response[sdkv1.DeleteAgentResponse], error) {
	return connect.NewResponse(&sdkv1.DeleteAgentResponse{}), nil
}

/**
 * @brief 启动 SDKRunner，通过真实 Connect 边界调用模拟 Bridge。
 * @param t 测试上下文；注册 Companion 和 Connect 监听器清理。
 * @param apiKey 模拟账户的合成 User API Key。
 * @return 尚未连接的 SDKRunner，首次请求时完成 Bridge 探活。
 */
func newCursorRedactionTestRunner(t *testing.T, apiKey string) *cursorauth.SDKRunner {
	t.Helper()
	dir := t.TempDir()
	bridge := &cursorRedactionBridge{apiKey: apiKey, stopPath: filepath.Join(dir, "stop")}
	return newCursorProxyTestRunner(t, dir, bridge)
}

/**
 * @brief 启动测试用 Companion，保留 SDKRunner 的真实握手和 Connect RPC 边界。
 * @param t 当前测试上下文。
 * @param dir 测试目录；Bridge 的 Shutdown 应在此创建 stop 文件。
 * @param bridge 模拟 Agent 和控制 RPC 的同一本地服务。
 * @return 尚未连接的 SDKRunner。
 */
func newCursorProxyTestRunner(t *testing.T, dir string, bridge interface {
	sdkv1connect.SdkAgentServiceHandler
	sdkv1connect.SdkBridgeControlServiceHandler
}) *cursorauth.SDKRunner {
	t.Helper()
	bridgePath := filepath.Join(dir, "cursor-sdk-bridge")
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("test-bridge-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	agentPath, agentHandler := sdkv1connect.NewSdkAgentServiceHandler(bridge)
	controlPath, controlHandler := sdkv1connect.NewSdkBridgeControlServiceHandler(bridge)
	mux.Handle(agentPath, agentHandler)
	mux.Handle(controlPath, controlHandler)
	bridgeServer := httptest.NewServer(mux)
	t.Cleanup(bridgeServer.Close)
	ready, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "transport": "tcp", "protocol": "connect", "url": bridgeServer.URL, "authTokenFile": tokenPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCLOAD_TEST_BRIDGE_READY_PREFIX", string(ready[:len(ready)-1]))
	t.Setenv("CCLOAD_TEST_BRIDGE_STOP", filepath.Join(dir, "stop"))
	t.Setenv("SQLITE_PATH", filepath.Join(dir, "test.db"))
	const script = `#!/bin/sh
printf 'cursor-sdk-bridge ready %s,"pid":%s}\n' "$CCLOAD_TEST_BRIDGE_READY_PREFIX" "$$" >&2
while [ ! -f "$CCLOAD_TEST_BRIDGE_STOP" ]; do sleep 0.02; done
`
	if err := os.WriteFile(bridgePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return cursorauth.NewSDKRunner(bridgePath)
}

/**
 * @brief 模拟会挂起工具调用并能区分同一 Run 续接与另起 Run 的本地 Bridge。
 */
type cursorRoutingBridge struct {
	cursorRedactionBridge
	mu          sync.Mutex ///< 保护回调注册及 Agent 记录。
	callbackURL string     ///< 当前工具回调地址。
	callbackKey string     ///< 当前工具回调的 Bearer Token。
	created     []string   ///< 按创建顺序记录渠道名称。
}

/**
 * @brief 保存工具回调地址和鉴权令牌。
 * @param _ 本地 RPC 上下文。
 * @param req 工具回调注册请求。
 * @return 注册成功响应。
 * @return 始终为 nil。
 */
func (b *cursorRoutingBridge) SetToolCallback(_ context.Context, req *connect.Request[sdkv1.SetToolCallbackRequest]) (*connect.Response[sdkv1.SetToolCallbackResponse], error) {
	b.mu.Lock()
	b.callbackURL, b.callbackKey = req.Msg.GetUrl(), req.Msg.GetAuthToken()
	b.mu.Unlock()
	return connect.NewResponse(&sdkv1.SetToolCallbackResponse{}), nil
}

/**
 * @brief 记录创建操作并用渠道凭证区分 Agent。
 * @param _ 本地 RPC 上下文。
 * @param req 含测试凭证的 Agent 创建请求。
 * @return 包含 Agent ID 的成功响应。
 * @return 始终为 nil。
 */
func (b *cursorRoutingBridge) CreateAgent(_ context.Context, req *connect.Request[sdkv1.CreateAgentRequest]) (*connect.Response[sdkv1.CreateAgentResponse], error) {
	key := req.Msg.GetOptions().GetApiKey()
	b.mu.Lock()
	b.created = append(b.created, key)
	b.mu.Unlock()
	return connect.NewResponse(&sdkv1.CreateAgentResponse{AgentId: key}), nil
}

/**
 * @brief 初始轮发起工具回调并等待结果；历史重建时直接返回渠道标记。
 * @param ctx 本地 RPC 上下文。
 * @param req 新轮请求。
 * @param stream 模拟上游 SDK 流。
 * @return 流写入或工具回调错误；正常完成为 nil。
 */
func (b *cursorRoutingBridge) Send(ctx context.Context, req *connect.Request[sdkv1.SendRequest], stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
	agentID := req.Msg.GetAgentId()
	runID := "run-" + agentID
	var text string
	if strings.Contains(req.Msg.GetMessage().GetText(), "HOST_SESSION_RECOVERY") {
		text = "created fresh on " + agentID
	} else {
		status, _ := structpb.NewStruct(map[string]any{"status": "RUNNING", "run_id": runID, "agent_id": agentID})
		if err := stream.Send(&sdkv1.RunStreamMessage{Envelope: &sdkv1.RunStreamMessage_SdkMessage{SdkMessage: &sdkv1.SdkMessage{Type: "status", Message: status}}}); err != nil {
			return err
		}
		b.mu.Lock()
		callbackURL, callbackKey := b.callbackURL, b.callbackKey
		b.mu.Unlock()
		callback := sdkv1connect.NewSdkCustomToolCallbackServiceClient(http.DefaultClient, callbackURL)
		call := connect.NewRequest(&sdkv1.CallCustomToolRequest{AgentId: agentID, ToolName: "lookup", ToolCallId: new("upstream-call")})
		call.Header().Set("Authorization", "Bearer "+callbackKey)
		response, err := callback.CallCustomTool(ctx, call)
		if err != nil {
			return err
		}
		text = agentID + " received " + response.Msg.GetResult().GetFields()["value"].GetStringValue()
	}
	if err := stream.Send(&sdkv1.RunStreamMessage{Envelope: &sdkv1.RunStreamMessage_Result{Result: &sdkv1.RunStreamResult{
		RunId: runID, AgentId: agentID, Status: sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED,
		Result: &sdkv1.RunResult{RunId: runID, AgentId: agentID, Status: sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED,
			Result: text, Usage: &sdkv1.TokenUsage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}},
	}}}); err != nil {
		return err
	}
	return stream.Send(&sdkv1.RunStreamMessage{Envelope: &sdkv1.RunStreamMessage_Done{Done: &sdkv1.RunStreamDone{RunId: runID, AgentId: agentID}}})
}

/**
 * @brief 获取已创建 Agent 对应的渠道名称。
 * @return 按创建顺序排列的渠道名称独立副本。
 */
func (b *cursorRoutingBridge) createdAgents() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.created)
}

/**
 * @brief 验证原生 Cursor 工具结果通过代理回到原渠道与模型，且受普通候选约束。
 * @param t 当前测试上下文。
 */
func TestProxy_CursorToolContinuationRouting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Cursor Bridge discovery fixture uses a shell executable")
	}
	type scenario struct {
		name, path, first, continuation, callPath, resultPath string
		constraints                                           string
	}
	chat := scenario{
		name: "chat", path: "/v1/chat/completions",
		first:        `{"model":"custom-model","messages":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`,
		continuation: `{"model":"custom-model","messages":[{"role":"assistant","tool_calls":[{"id":%q,"type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":%q,"content":"tool-output"}]}`,
		callPath:     "choices.0.message.tool_calls.0.id", resultPath: "choices.0.message.content",
	}
	for _, scenario := range []scenario{
		chat,
		{
			name: "messages", path: "/v1/messages",
			first:        `{"model":"custom-model","max_tokens":128,"messages":[{"role":"user","content":"lookup"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`,
			continuation: `{"model":"custom-model","max_tokens":128,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"tool-output"}]}]}`,
			callPath:     `content.#(type=="tool_use").id`, resultPath: `content.#(type=="text").text`,
		},
		{
			name: "responses", path: "/v1/responses",
			first:        `{"model":"custom-model","input":"lookup","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`,
			continuation: `{"model":"custom-model","input":[{"type":"function_call","call_id":%q,"name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":%q,"output":"tool-output"}]}`,
			callPath:     `output.#(type=="function_call").call_id`, resultPath: `output.#(type=="message").content.0.text`,
		},
		{
			name: "gemini", path: "/v1beta/models/custom-model:generateContent",
			first:        `{"contents":[{"role":"user","parts":[{"text":"lookup"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"OBJECT"}}]}]}`,
			continuation: `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"lookup","id":%q,"args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","id":%q,"response":{"value":"tool-output"}}}]}]}`,
			callPath:     `candidates.0.content.parts.#(functionCall.name=="lookup").functionCall.id`,
			resultPath:   "candidates.0.content.parts.0.text",
		},
		{name: "independent_token", constraints: "token"},
		{name: "explicit_channel", constraints: "channel"},
		{name: "channel_cooldown", constraints: "cooldown"},
		{name: "model_cooldown", constraints: "model_cooldown"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if scenario.path == "" {
				scenario.path, scenario.first, scenario.continuation = chat.path, chat.first, chat.continuation
				scenario.callPath, scenario.resultPath = chat.callPath, chat.resultPath
			}
			modelEntries := []model.ModelEntry{
				{Model: "custom-model", RedirectModel: "upstream-a"},
				{Model: "custom-model", RedirectModel: "upstream-b"},
			}
			env, bridge := setupCursorRoutingTest(t, modelEntries)
			headers := map[string]string{}
			if scenario.constraints == "" {
				headers["Session-Id"], headers["Thread-Id"] = "tool-session", "tool-thread"
			}
			if scenario.name == "messages" {
				headers["X-Claude-Code-Agent-Id"] = "child"
				headers["X-Claude-Code-Session-Id"] = "f6aee470-c917-4936-919e-c7ff5b15ad8e"
			}
			first := doProxyRequest(t, env.engine, scenario.path, json.RawMessage(scenario.first), headers)
			id := gjson.GetBytes(first.Body.Bytes(), scenario.callPath).String()
			if first.Code != http.StatusOK || id == "" {
				t.Fatalf("first response HTTP %d, %s", first.Code, first.Body)
			}
			created := bridge.createdAgents()
			if len(created) != 1 {
				t.Fatalf("expected one Agent after the initial request, created = %v", created)
			}
			owner := created[0]
			alternate := "route-a"
			if owner == alternate {
				alternate = "route-b"
			}
			switch scenario.constraints {
			case "token":
				injectAPIToken(env.server.authService, "another-api-key", 0, 2)
				headers["Authorization"] = "Bearer another-api-key"
			case "channel":
				headers["x-ccload-channel"] = alternate
			case "cooldown", "model_cooldown":
				ctx := context.Background()
				configs, err := env.store.ListConfigs(ctx)
				if err != nil {
					t.Fatal(err)
				}
				index := slices.IndexFunc(configs, func(cfg *model.Config) bool { return cfg.Name == owner })
				if index < 0 {
					t.Fatalf("owner %q not found", owner)
				}
				if scenario.constraints == "cooldown" {
					err = env.store.SetChannelCooldown(ctx, configs[index].ID, time.Now().Add(time.Minute))
				} else {
					err = env.store.SetModelCooldown(ctx, configs[index].ID, "upstream-a", time.Now().Add(time.Minute))
				}
				if err != nil {
					t.Fatal(err)
				}
				env.server.invalidateCooldownCache()
			}
			payload := json.RawMessage(fmt.Sprintf(scenario.continuation, id, id))
			if scenario.name == "messages" {
				// Claude Code 也可在 metadata.user_id 中标识同一会话。
				payload = setJSONValue(payload, "metadata.user_id", "user_session_"+headers["X-Claude-Code-Session-Id"])
				delete(headers, "X-Claude-Code-Session-Id")
			}
			second := doProxyRequest(t, env.engine, scenario.path, payload, headers)
			created = bridge.createdAgents()
			wantText, wantCreated := owner+" received tool-output", []string{owner}
			if scenario.constraints != "" {
				wantText, wantCreated = "created fresh on "+alternate, []string{owner, alternate}
			}
			if text := gjson.GetBytes(second.Body.Bytes(), scenario.resultPath).String(); second.Code != http.StatusOK || !strings.Contains(text, wantText) || !slices.Equal(created, wantCreated) {
				t.Fatalf("continuation HTTP %d, created = %v, body = %s", second.Code, created, second.Body)
			}
			if scenario.constraints == "" {
				repeat := doProxyRequest(t, env.engine, scenario.path, payload, headers)
				created = bridge.createdAgents()
				if text := gjson.GetBytes(repeat.Body.Bytes(), scenario.resultPath).String(); repeat.Code != http.StatusOK || text != wantText || !slices.Equal(created, wantCreated) {
					t.Fatalf("replay HTTP %d, created = %v, body = %s", repeat.Code, created, repeat.Body)
				}
			}
		})
	}
}

/**
 * @brief 建立使用真实 Cursor SDK runner 和本地模拟 Bridge 的两个等优先级渠道。
 * @param t 当前测试上下文。
 * @param modelEntries 两个渠道使用的模型配置。
 * @return 代理测试环境。
 * @return 用于观察所创建 Agent 的 Bridge。
 */
func setupCursorRoutingTest(t *testing.T, modelEntries []model.ModelEntry) (*proxyTestEnv, *cursorRoutingBridge) {
	t.Helper()

	dir := t.TempDir()
	bridge := &cursorRoutingBridge{stopPath: filepath.Join(dir, "stop")}
	runner := newCursorProxyTestRunner(t, dir, bridge)
	env := setupProxyTestEnv(t, []testChannel{
		{name: "route-a", models: "custom-model", modelEntries: modelEntries, priority: 100, authType: model.AuthTypeCursorOAuth, oauthCredential: `{"api_key":"route-a","access_token":"test-session"}`},
		{name: "route-b", models: "custom-model", modelEntries: modelEntries, priority: 100, authType: model.AuthTypeCursorOAuth, oauthCredential: `{"api_key":"route-b","access_token":"test-session"}`},
	}, map[int]string{0: "https://unused.invalid", 1: "https://unused.invalid"})
	env.server.cursorRunner = runner
	return env, bridge
}

/**
 * @brief 验证代理脱敏 SDK 终态诊断，并在输出前遇到额度耗尽时冷却当前模型、切换渠道。
 * @param t 测试上下文。
 */
func TestProxy_CursorSDKRedactsEchoedUserAPIKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Cursor Bridge discovery fixture uses a shell executable")
	}
	const apiKey = `synthetic-cursor-"token\42`
	runner := newCursorRedactionTestRunner(t, apiKey)
	credential, err := (&cursorauth.Credential{APIKey: apiKey, AccessToken: "session-token"}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	backup := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode backup request: %v", err)
			http.Error(w, "invalid backup request", http.StatusBadRequest)
			return
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"id":"backup","object":"chat.completion.chunk","model":"cursor-quota-result","choices":[{"index":0,"delta":{"content":"backup answer"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"backup","object":"chat.completion","model":"cursor-quota","choices":[{"index":0,"message":{"role":"assistant","content":"backup answer"},"finish_reason":"stop"}]}`))
	}))
	env := setupProxyTestEnvWithSettings(t, []testChannel{
		{
			name: "cursor-redaction", upstreamProtocol: "openai", models: "cursor-json,cursor-sse,cursor-quota,cursor-quota-result",
			authType: model.AuthTypeCursorOAuth, oauthCredential: credential,
		},
		{name: "cursor-backup", upstreamProtocol: "openai", models: "cursor-quota,cursor-quota-result", apiKey: "backup-key"},
	}, map[int]string{0: "https://unused.invalid", 1: backup.URL}, map[string]string{"debug_log_enabled": "true"})
	env.server.cursorRunner = runner

	t.Run("terminal quota fails over", func(t *testing.T) {
		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model":    "cursor-quota",
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}, nil)
		if response.Code != http.StatusOK || gjson.GetBytes(response.Body.Bytes(), "choices.0.message.content").String() != "backup answer" {
			t.Fatalf("HTTP=%d body=%s", response.Code, response.Body)
		}
		modelCooldowns, err := env.store.GetAllModelCooldowns(t.Context())
		if err != nil || len(modelCooldowns) != 1 {
			t.Fatalf("model cooldowns=%v err=%v", modelCooldowns, err)
		}
		for _, models := range modelCooldowns {
			if len(models) != 1 || !models["cursor-quota"].After(time.Now()) {
				t.Fatalf("model cooldowns=%v, want only cursor-quota cooled", modelCooldowns)
			}
		}
		channelCooldowns, err := env.store.GetAllChannelCooldowns(t.Context())
		if err != nil || len(channelCooldowns) != 0 {
			t.Fatalf("channel cooldowns=%v err=%v", channelCooldowns, err)
		}
	})

	t.Run("terminal result quota fails over before SSE", func(t *testing.T) {
		const modelID = "cursor-quota-result"
		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": modelID, "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}, nil)
		if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatalf("HTTP=%d headers=%v body=%s", response.Code, response.Header(), response.Body)
		}
		var content strings.Builder
		var done bool
		for frame := range bytes.SplitSeq(response.Body.Bytes(), []byte("\n\n")) {
			_, data := parseSSEEventChunk(frame)
			switch string(data) {
			case "":
				continue
			case "[DONE]":
				done = true
			default:
				payload := gjson.ParseBytes(data)
				if payload.Get("id").String() != "backup" || payload.Get("error").Exists() {
					t.Fatalf("unexpected SSE frame: %s", data)
				}
				content.WriteString(payload.Get("choices.0.delta.content").String())
			}
		}
		if content.String() != "backup answer" || !done {
			t.Fatalf("want only a complete backup response, got %s", response.Body)
		}
		failure := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
			return entry.Model == modelID && entry.ChannelName == "cursor-redaction"
		})
		const wantReason = "You've hit your usage limit for [redacted]"
		const wantError = "cursor SDK_ERROR_CODE_USAGE_LIMIT_EXCEEDED: " + wantReason
		if failure == nil || failure.StatusCode != http.StatusTooManyRequests || !strings.Contains(failure.Message, wantError) {
			t.Fatalf("primary failure=%+v, want 429 with %q", failure, wantError)
		}
		modelCooldowns, err := env.store.GetAllModelCooldowns(t.Context())
		if err != nil || !modelCooldowns[failure.ChannelID][modelID].After(time.Now()) {
			t.Fatalf("primary model cooldowns=%v err=%v", modelCooldowns, err)
		}
		debug, err := env.store.GetDebugLogByLogID(t.Context(), failure.ID)
		if err != nil || debug == nil {
			t.Fatalf("primary debug log=%+v err=%v", debug, err)
		}
		var resultText string
		for frame := range bytes.SplitSeq(debug.RespBody, []byte("\n")) {
			if field := gjson.GetBytes(frame, "result.result.result"); field.Exists() {
				resultText = field.String()
			}
		}
		if resultText != wantReason || debug.UpstreamError != wantError {
			t.Fatalf("raw Bridge debug body=%s upstream error=%q", debug.RespBody, debug.UpstreamError)
		}
	})

	for _, streaming := range []bool{false, true} {
		modelID, wantHTTP, diagnostic := "cursor-json", http.StatusTooManyRequests, "RPC"
		if streaming {
			modelID, wantHTTP, diagnostic = "cursor-sse", http.StatusOK, "result"
		}
		t.Run(modelID, func(t *testing.T) {
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": modelID, "stream": streaming,
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			}, nil)
			if response.Code != wantHTTP {
				t.Fatalf("status=%d want=%d body=%s", response.Code, wantHTTP, response.Body.String())
			}
			var message string
			if streaming {
				var content string
				for block := range bytes.SplitSeq(response.Body.Bytes(), []byte("\n\n")) {
					_, data := parseSSEEventChunk(block)
					payload := gjson.ParseBytes(data)
					if field := payload.Get("error.message"); field.Exists() {
						message = field.String()
					}
					content += payload.Get("choices.0.delta.content").String()
				}
				if content != "ordinary assistant output" {
					t.Fatalf("assistant output=%q", content)
				}
			} else {
				message = gjson.GetBytes(response.Body.Bytes(), "error.message").String()
				hint := response.Header().Get("Retry-After")
				if seconds, err := strconv.Atoi(hint); err != nil || seconds < 17 || seconds > 20 {
					t.Fatalf("Retry-After=%q, want about 19 seconds", hint)
				}
			}
			wantMessage := "cursor SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED: " + diagnostic + " rejected [redacted] for cursor-other-account"
			if message != wantMessage {
				t.Fatalf("client error=%q want=%q", message, wantMessage)
			}
			entry := waitForProxyLog(t, env, modelID)
			debug, err := env.store.GetDebugLogByLogID(t.Context(), entry.ID)
			if err != nil || debug == nil {
				t.Fatalf("debug log=%+v err=%v", debug, err)
			}
			if entry.StatusCode != http.StatusTooManyRequests || !strings.Contains(entry.Message, wantMessage) || debug.UpstreamError != wantMessage {
				t.Fatalf("status=%d log=%q debug=%q want=%q", entry.StatusCode, entry.Message, debug.UpstreamError, wantMessage)
			}
			if streaming {
				var statusReason, finalReason, finalCode, finalSnapshot string
				for frame := range bytes.SplitSeq(debug.RespBody, []byte("\n")) {
					payload := gjson.ParseBytes(frame)
					if payload.Get("sdk_message.type").String() == "status" {
						statusReason = payload.Get("sdk_message.message.message").String()
					}
					if final := payload.Get("result.result"); final.Exists() {
						finalReason = final.Get("error_message").String()
						finalCode = final.Get("error_code").String()
						finalSnapshot = final.Get("result").String()
					}
				}
				if statusReason != "status rejected [redacted] for cursor-other-account" ||
					finalReason != "result rejected [redacted] for cursor-other-account" || finalCode != "RATE_LIMIT_EXCEEDED" ||
					finalSnapshot != "ordinary assistant output with [redacted]" {
					t.Fatalf("raw Bridge debug body=%s", debug.RespBody)
				}
			}
			for _, output := range []string{response.Body.String(), entry.Message, string(debug.RespBody), string(debug.TranslatedRespBody)} {
				if strings.Contains(output, "synthetic-cursor-") {
					t.Fatalf("credential leaked: %s", output)
				}
			}
		})
	}
}

/**
 * @brief 验证四种客户端协议的工具续接同时保留调用 ID、新指令与显式禁用策略。
 * @param t 测试上下文。
 */
func TestProxy_CursorToolResultWithUserInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, path, payload, choice string
		toolsDeclared               bool
	}{
		{"messages", "/v1/messages", `{"model":"composer-2.5","max_tokens":128,"tool_choice":{"type":"none"},"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_mix","name":"lookup","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_mix","content":"known_value"},{"type":"text","text":"请用中文概括"}]}]}`, "none", false},
		{"chat", "/v1/chat/completions", `{"model":"composer-2.5","messages":[
			{"role":"assistant","tool_calls":[{"id":"call_mix","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_mix","content":"known_value"},{"role":"user","content":"请用中文概括"}]}`, "auto", false},
		{"responses", "/v1/responses", `{"model":"composer-2.5","tool_choice":"none","input":[
			{"type":"function_call","call_id":"call_mix","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_mix","output":"known_value"},{"role":"user","content":"请用中文概括"}]}`, "none", false},
		{"gemini", "/v1beta/models/composer-2.5:generateContent", `{"toolConfig":{"functionCallingConfig":{"mode":"NONE"}},"contents":[
			{"role":"model","parts":[{"functionCall":{"name":"lookup","id":"call_mix","args":{}}}]},
			{"role":"user","parts":[{"functionResponse":{"id":"call_mix","name":"lookup","response":{"value":"known_value"}}},{"text":"请用中文概括"}]}]}`, "none", false},
		{"responses named inherited tool", "/v1/responses", `{"model":"composer-2.5","tool_choice":{"type":"function","name":"lookup"},"input":[
			{"type":"function_call","call_id":"call_mix","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_mix","output":"known_value"},{"role":"user","content":"请用中文概括"}]}`, "lookup", false},
		{"responses empty tools", "/v1/responses", `{"model":"composer-2.5","tools":[],"input":[
			{"type":"function_call","call_id":"call_mix","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_mix","output":"known_value"},{"role":"user","content":"请用中文概括"}]}`, "auto", true},
		{"gemini empty tools", "/v1beta/models/composer-2.5:generateContent", `{"tools":[],"contents":[
			{"role":"model","parts":[{"functionCall":{"name":"lookup","id":"call_mix","args":{}}}]},
			{"role":"user","parts":[{"functionResponse":{"id":"call_mix","name":"lookup","response":{"value":"known_value"}}},{"text":"请用中文概括"}]}]}`, "auto", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := setupProxyTestEnv(t, []testChannel{{
				name: "cursor", upstreamProtocol: "openai", models: "composer-2.5",
				authType: model.AuthTypeCursorOAuth, oauthCredential: `{"api_key":"test-key","access_token":"test-session"}`,
			}}, map[int]string{0: "https://unused.invalid"})
			var requests int
			env.server.cursorRunner = cursorRunnerFunc(func(ctx context.Context, credential *cursorauth.Credential, req cursorauth.Request) (<-chan cursorauth.Event, error) {
				requests++
				if (req.Tools != nil) != tc.toolsDeclared || req.ToolChoice != tc.choice || len(req.ToolResults) != 1 || req.ToolResults[0].CallID != "call_mix" ||
					!strings.Contains(req.ToolResults[0].Output, "known_value") || strings.Contains(req.ToolResults[0].Output, "请用中文概括") ||
					strings.Count(req.Prompt, "请用中文概括") != 1 {
					t.Errorf("tool_choice=%q results=%+v prompt=%q", req.ToolChoice, req.ToolResults, req.Prompt)
				}
				return cursorUsageEventsRunner{{Text: "完成", Done: true}}.Run(ctx, credential, req)
			})
			res := doProxyRequest(t, env.engine, tc.path, json.RawMessage(tc.payload), nil)
			if res.Code != http.StatusOK || requests != 1 {
				t.Fatalf("HTTP=%d SDK runs=%d body=%s", res.Code, requests, res.Body)
			}
		})
	}
}

/**
 * @brief 验证 Responses 的内联截图保留在对应的 Cursor 工具结果中。
 * @param t 测试生命周期。
 */
func TestProxy_CursorResponsesImageToolOutput(t *testing.T) {
	t.Parallel()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor", upstreamProtocol: "openai", models: "composer-2.5",
		authType: model.AuthTypeCursorOAuth, oauthCredential: `{"api_key":"fixture","access_token":"session"}`,
	}}, map[int]string{0: "https://unused.invalid"})
	called := false
	env.server.cursorRunner = cursorRunnerFunc(func(ctx context.Context, credential *cursorauth.Credential, req cursorauth.Request) (<-chan cursorauth.Event, error) {
		called = true
		if len(req.ToolResults) != 1 || req.ToolResults[0].CallID != "call_screen" || len(req.ToolResults[0].Content) != 2 {
			t.Errorf("tool results = %+v", req.ToolResults)
		}
		return cursorUsageEventsRunner{{Text: "done", Done: true}}.Run(ctx, credential, req)
	})
	res := doProxyRequest(t, env.engine, "/v1/responses", json.RawMessage(`{
		"model":"composer-2.5",
		"input":[
			{"type":"function_call","call_id":"call_screen","name":"screen","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_screen","output":[
				{"type":"input_text","text":"screenshot"},
				{"type":"input_image","image_url":"data:image/png;base64,aGk="}
			]}
		]
	}`), nil)
	if res.Code != http.StatusOK || !called {
		t.Fatalf("HTTP %d: %s", res.Code, res.Body)
	}
}

/**
 * @brief 验证多协议附件无法交给 Cursor 时保留用户输入的语义，返回明确错误。
 * @param t 测试上下文。
 */
func TestProxy_CursorUnsupportedAttachments(t *testing.T) {
	const modelID = "cursor-local-model"
	env := setupProxyTestEnv(t, []testChannel{{name: "cursor", upstreamProtocol: "openai", models: modelID, authType: model.AuthTypeCursorOAuth, oauthCredential: `{"access_token":"session","api_key":"key"}`}}, map[int]string{0: "https://unused.invalid"})
	env.server.cursorRunner = cursorRunnerFunc(func(context.Context, *cursorauth.Credential, cursorauth.Request) (<-chan cursorauth.Event, error) {
		t.Fatal("attachment was forwarded to Cursor")
		return nil, nil
	})
	for _, tc := range []struct{ name, path, raw string }{
		{"Chat audio", "/v1/chat/completions", `{"model":"cursor-local-model","messages":[{"role":"user","content":[{"type":"text","text":"listen"},{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}}]}]}`},
		{"Responses file", "/v1/responses", `{"model":"cursor-local-model","input":[{"role":"user","content":[{"type":"input_text","text":"read"},{"type":"input_file","file_id":"file-example"}]}]}`},
		{"Responses image ID", "/v1/responses", `{"model":"cursor-local-model","input":[{"role":"user","content":[{"type":"input_text","text":"inspect"},{"type":"input_image","file_id":"file-example"}]}]}`},
		{"Anthropic image ID", "/v1/messages", `{"model":"cursor-local-model","max_tokens":200,"messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image","source":{"type":"file","file_id":"file-example"}}]}]}`},
		{"Gemini audio", "/v1beta/models/cursor-local-model:generateContent", `{"contents":[{"role":"user","parts":[{"text":"listen"},{"inlineData":{"mimeType":"audio/wav","data":"UklGRg=="}}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := doProxyRequest(t, env.engine, tc.path, json.RawMessage(tc.raw), nil)
			if res.Code != http.StatusBadRequest || !strings.Contains(gjson.GetBytes(res.Body.Bytes(), "error.message").String(), "inline base64 images") {
				t.Fatalf("HTTP %d: %s", res.Code, res.Body)
			}
		})
	}
	t.Run("Responses file reaches a compatible native route", func(t *testing.T) {
		backup := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var raw json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
				t.Errorf("decode input: %v", err)
			}
			if got := gjson.GetBytes(raw, "input.0.content.1.file_id").Str; got != "file-example" {
				t.Errorf("backup file id = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_backup","object":"response","status":"completed","output":[]}`))
		}))
		defer backup.Close()
		fallback := setupProxyTestEnv(t, []testChannel{
			{name: "cursor", upstreamProtocol: "openai", priority: 100, models: modelID, authType: model.AuthTypeCursorOAuth, oauthCredential: `{"access_token":"session","api_key":"key"}`},
			{name: "native", upstreamProtocol: "codex", priority: 1, models: modelID},
		}, map[int]string{0: "https://unused.invalid", 1: backup.URL})
		fallback.server.cursorRunner = env.server.cursorRunner
		const raw = `{"model":"cursor-local-model","input":[{"role":"user","content":[{"type":"input_text","text":"read"},{"type":"input_file","file_id":"file-example"}]}]}`
		response := doProxyRequest(t, fallback.engine, "/v1/responses", json.RawMessage(raw), nil)
		if response.Code != http.StatusOK || gjson.GetBytes(response.Body.Bytes(), "id").Str != "resp_backup" {
			t.Fatalf("HTTP %d: %s", response.Code, response.Body)
		}
	})

}
