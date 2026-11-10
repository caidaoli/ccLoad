package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/cursorauth"
	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
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
 * @brief Cursor 思考增量经四种 HTTP 协议返回时保留空白与 Unicode，并与正文分离。
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
					{Delta: "答", Text: "答"}, {Delta: "案", Text: "答案"}, {Done: true, Text: "答案"},
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
				{Done: true, Text: "答1答2"},
			}
			response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
				"model": "composer-2.5", "stream": streaming, "max_tokens": 256,
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
				"tools":    []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}},
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
			}
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
	}, map[string]string{"anthropic-version": "2023-06-01"})
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
	for _, tt := range []struct {
		name         string
		events       cursorUsageEventsRunner
		resumed      bool
		failed       bool
		uncommitted  bool
		wantEstimate bool
		wantBillable cursorauth.Usage
		wantClient   cursorauth.Usage
	}{
		{
			name: "tool turn preserves real usage over callback estimate",
			events: cursorUsageEventsRunner{
				{Usage: first},
				{ToolCall: &cursorauth.ToolCall{ID: "call_lookup", Name: "lookup", Arguments: json.RawMessage(`{}`)},
					Usage: &cursorauth.Usage{InputTokens: 900_000}, UsageEstimated: true},
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
			env := setupProxyTestEnv(t, []testChannel{{
				name: "cursor-incremental-usage", upstreamProtocol: "openai",
				modelEntries: []model.ModelEntry{{Model: "grok-4.6", Pricing: &util.CustomModelPrice{
					InputPrice: &inputPrice, OutputPrice: &outputPrice, CacheReadPrice: &cacheReadPrice, CacheWritePrice: &cacheWritePrice,
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
			wantCost := (float64(want.InputTokens)*inputPrice + float64(want.OutputTokens)*outputPrice +
				float64(want.CacheReadTokens)*cacheReadPrice + float64(want.CacheWriteTokens)*cacheWritePrice) / 1_000_000
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
		toolCalls:          []cursorauth.ToolCall{{ID: "call_lookup", Name: "lookup", Arguments: json.RawMessage(`{}`)}},
		toolUsage:          &cursorauth.Usage{InputTokens: 100, TotalTokens: 100},
		toolUsageEstimated: true,
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
	if payload.Usage.PromptTokens <= 0 || payload.Usage.CompletionTokens != 0 || payload.Usage.TotalTokens != payload.Usage.PromptTokens {
		t.Fatalf("tool response usage = %+v", payload.Usage)
	}
	if runner.request.InputTokenEstimate <= 0 {
		t.Fatalf("runner request is missing the input-token estimate: %+v", runner.request)
	}

	entry := waitForProxyLog(t, env, "grok-4.6")
	if entry.InputTokens != 0 || entry.OutputTokens != 0 || entry.CacheReadInputTokens != 0 || entry.Cost != 0 {
		t.Fatalf("estimated intermediate usage must not be billed: %+v", entry)
	}
	if entry.UsageEstimate == nil || entry.UsageEstimate.InputTokens <= 0 || entry.UsageEstimate.OutputTokens <= 0 {
		t.Fatalf("tool turn is missing its separate usage estimate: %+v", entry.UsageEstimate)
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
			for _, usageEvent := range []bool{false, true} {
				name := fmt.Sprintf("%s/stream=%t/usage_event=%t", clientProtocol, streaming, usageEvent)
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
					events := cursorUsageEventsRunner{{Text: "final answer", Done: true, Usage: cumulative}}
					if usageEvent {
						events = cursorUsageEventsRunner{
							{Usage: cumulative}, {Text: "final answer", Done: true, Usage: &cursorauth.Usage{}},
						}
					}
					env.server.cursorRunner = events
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
					if entry.InputTokens != cumulative.InputTokens || entry.OutputTokens != cumulative.OutputTokens ||
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
			if streaming {
				entry := waitForProxyLog(t, env, "composer-2.5")
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
