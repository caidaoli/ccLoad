package cursorauth

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/cursorauth/sdkgen/sdk/v1/sdkv1connect"
)

/**
 * @brief SDK 会话测试中默认使用的可信网关身份。
 */
var testCursorSessionScope = SessionScope{ChannelID: 1, TokenID: 1}

type nativeToolControlHandler struct {
	sdkv1connect.UnimplementedSdkBridgeControlServiceHandler
	mu    sync.Mutex
	url   string
	token string
}

func (h *nativeToolControlHandler) SetToolCallback(
	_ context.Context,
	request *connect.Request[sdkv1.SetToolCallbackRequest],
) (*connect.Response[sdkv1.SetToolCallbackResponse], error) {
	h.mu.Lock()
	h.url = request.Msg.GetUrl()
	h.token = request.Msg.GetAuthToken()
	h.mu.Unlock()
	return connect.NewResponse(&sdkv1.SetToolCallbackResponse{}), nil
}

func (h *nativeToolControlHandler) callback() (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.url, h.token
}

/**
 * @brief 模拟带多轮自定义工具和逐轮用量的 SDK Agent。
 */
type nativeToolAgentHandler struct {
	sdkv1connect.UnimplementedSdkAgentServiceHandler
	control *nativeToolControlHandler
	nextID  atomic.Int32

	mu               sync.Mutex
	creates          map[string]*sdkv1.CreateAgentRequest
	createErr        error       ///< 单次模拟 CreateAgent 准备失败；受 mu 保护。
	deleted          chan string ///< 可选的删除通知。
	results          map[string]map[string]any
	getRuns          map[string]int
	usage            map[string]*sdkv1.TokenUsage
	turns            int
	turnDelay        time.Duration
	turnUsage        map[string]any
	toolBeforeStream bool                                                                                               ///< 首个工具回调在任何流式响应前到达。
	sent             []string                                                                                           ///< 按序记录的 Send 消息正文
	whileToolPending func(context.Context, *connect.ServerStream[sdkv1.RunStreamMessage], string, string) (bool, error) ///< true 表示已经发送终态
}

/**
 * @brief 登记测试 Agent 的原始选项。
 * @param _ 上游请求上下文。
 * @param request 包含模型和注册工具定义的新建请求。
 * @return 分配的测试 Agent ID；模拟失败时为 nil。
 * @return 已登记的一次性准备错误，成功时为 nil。
 */
func (h *nativeToolAgentHandler) CreateAgent(
	_ context.Context,
	request *connect.Request[sdkv1.CreateAgentRequest],
) (*connect.Response[sdkv1.CreateAgentResponse], error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.createErr != nil {
		err := h.createErr
		h.createErr = nil
		return nil, err
	}
	agentID := fmt.Sprintf("agent-%d", h.nextID.Add(1))
	h.creates[agentID] = request.Msg
	return connect.NewResponse(&sdkv1.CreateAgentResponse{AgentId: agentID}), nil
}

/**
 * @brief 每轮报告思考与用量并等待客户端工具结果，最后返回累计快照可补查的终态。
 * @param ctx 上游请求上下文。
 * @param request Send 请求。
 * @param stream SDK 响应流。
 * @return 发送、工具回调失败或请求取消时返回错误。
 */
func (h *nativeToolAgentHandler) Send(
	ctx context.Context,
	request *connect.Request[sdkv1.SendRequest],
	stream *connect.ServerStream[sdkv1.RunStreamMessage],
) error {
	agentID := request.Msg.GetAgentId()
	runID := "run-" + agentID
	h.mu.Lock()
	h.sent = append(h.sent, request.Msg.GetMessage().GetText())
	customTools := h.creates[agentID].GetOptions().GetLocal().GetCustomTools()
	h.mu.Unlock()
	if h.toolBeforeStream {
		// Flush Connect response headers without sending an SDK frame.
		if err := stream.Send(nil); err != nil {
			return err
		}
	} else {
		if err := stream.Send(nativeSDKMessage("status", map[string]any{
			"agent_id": agentID, "run_id": runID,
		})); err != nil {
			return err
		}
	}
	callbackURL, callbackToken := h.control.callback()
	callback := sdkv1connect.NewSdkCustomToolCallbackServiceClient(http.DefaultClient, callbackURL)
	var result map[string]any
	turns := 0
	if len(customTools) > 0 {
		turns = max(1, h.turns)
	}
	for turn := range turns {
		if h.turnDelay > 0 {
			select {
			case <-time.After(h.turnDelay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if !h.toolBeforeStream {
			if err := stream.Send(nativeSDKMessage("thinking", map[string]any{
				"agent_id": agentID, "run_id": runID, "text": fmt.Sprintf("Check %d.", turn+1), "type": "thinking",
			})); err != nil {
				return err
			}
		}
		if h.turnUsage != nil {
			if err := stream.Send(nativeSDKMessage("usage", map[string]any{
				"agent_id": agentID, "run_id": runID, "usage": h.turnUsage,
			})); err != nil {
				return err
			}
		}
		// The bridge-scoped ID may repeat across Agents and tool turns.
		callID := "bridge-call"
		toolRequest := connect.NewRequest(&sdkv1.CallCustomToolRequest{
			AgentId: agentID, ToolName: "lookup", ToolCallId: &callID,
			Args: mustStruct(map[string]any{"session": agentID}),
		})
		toolRequest.Header().Set("Authorization", "Bearer "+callbackToken)
		var response *connect.Response[sdkv1.CallCustomToolResponse]
		var err error
		if h.whileToolPending == nil {
			response, err = callback.CallCustomTool(ctx, toolRequest)
		} else {
			toolCtx, cancelTool := context.WithCancel(ctx)
			called := make(chan struct{})
			go func() {
				defer close(called)
				response, err = callback.CallCustomTool(toolCtx, toolRequest)
			}()
			finished, streamErr := h.whileToolPending(ctx, stream, agentID, runID)
			if finished || streamErr != nil {
				cancelTool()
				<-called
				return streamErr
			}
			<-called
			cancelTool()
		}
		if err != nil {
			return err
		}
		result = response.Msg.GetResult().AsMap()
		h.mu.Lock()
		h.results[agentID] = result
		h.mu.Unlock()
	}
	text := fmt.Sprintf("%s:%v", agentID, result["value"])
	if err := stream.Send(nativeSDKMessage("thinking", map[string]any{
		"agent_id": agentID, "run_id": runID, "text": "Finish.", "type": "thinking",
	})); err != nil {
		return err
	}
	if err := stream.Send(nativeSDKMessage("assistant", map[string]any{
		"agent_id": agentID, "run_id": runID,
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}},
	})); err != nil {
		return err
	}
	if err := stream.Send(runResult(agentID, runID, sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, text)); err != nil {
		return err
	}
	return stream.Send(runDone(agentID, runID))
}

func (h *nativeToolAgentHandler) GetRun(
	_ context.Context,
	request *connect.Request[sdkv1.GetRunRequest],
) (*connect.Response[sdkv1.GetRunResponse], error) {
	runID := request.Msg.GetRunId()
	agentID := request.Msg.GetOptions().GetAgentId()
	h.mu.Lock()
	h.getRuns[runID]++
	usage := h.usage[runID]
	if usage == nil {
		usage = &sdkv1.TokenUsage{
			InputTokens: 100, OutputTokens: 20, CacheReadTokens: 30, TotalTokens: 150,
		}
	}
	h.mu.Unlock()
	return connect.NewResponse(&sdkv1.GetRunResponse{Run: &sdkv1.RunSnapshot{
		RunId: runID, AgentId: agentID,
		Usage: usage,
	}}), nil
}

/**
 * @brief 记录对测试 Agent 的远端删除。
 * @param _ 上游请求上下文。
 * @param request 包含被删除的 Agent ID。
 * @return 空响应。
 * @return 始终为 nil。
 */
func (h *nativeToolAgentHandler) DeleteAgent(
	_ context.Context,
	request *connect.Request[sdkv1.DeleteAgentRequest],
) (*connect.Response[sdkv1.DeleteAgentResponse], error) {
	if h.deleted != nil {
		h.deleted <- request.Msg.GetAgentId()
	}
	return connect.NewResponse(&sdkv1.DeleteAgentResponse{}), nil
}

func nativeSDKMessage(kind string, payload map[string]any) *sdkv1.RunStreamMessage {
	value := mustStruct(payload)
	return &sdkv1.RunStreamMessage{Envelope: &sdkv1.RunStreamMessage_SdkMessage{
		SdkMessage: &sdkv1.SdkMessage{Type: kind, Message: value},
	}}
}

func mustStruct(values map[string]any) *structpb.Struct {
	value, err := structpb.NewStruct(values)
	if err != nil {
		panic(err)
	}
	return value
}

func newNativeToolTestRunner(t *testing.T) (*SDKRunner, *nativeToolAgentHandler) {
	t.Helper()
	control := &nativeToolControlHandler{}
	agent := &nativeToolAgentHandler{
		control: control, creates: make(map[string]*sdkv1.CreateAgentRequest),
		results: make(map[string]map[string]any), getRuns: make(map[string]int), usage: make(map[string]*sdkv1.TokenUsage),
	}
	agentPath, agentHTTP := sdkv1connect.NewSdkAgentServiceHandler(agent)
	controlPath, controlHTTP := sdkv1connect.NewSdkBridgeControlServiceHandler(control)
	mux := http.NewServeMux()
	mux.Handle(agentPath, agentHTTP)
	mux.Handle(controlPath, controlHTTP)
	mux.Handle(sdkv1connect.NewSdkCursorServiceHandler(&testCursorHandler{}))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newBridgeClient(server.URL, "bridge-token")
	client.workdir = t.TempDir()
	bridge := newBridge()
	bridge.state = bridgeRunning
	bridge.process = &bridgeProcess{client: client, exited: make(chan struct{})}
	runner := &SDKRunner{
		bridge: bridge, idleTimeout: 3 * time.Second,
		sessions: make(map[string]*sdkSession), calls: make(map[string]*pendingToolCall),
		completedCalls: make(map[string]*sdkSession),
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		runner.failAllSessions(ErrBridgeClosed)
		runner.closeToolCallbackServer(ctx)
	})
	return runner, agent
}

func TestSDKRunnerNativeToolsKeepConcurrentSessionsIsolated(t *testing.T) {
	runner, handler := newNativeToolTestRunner(t)
	credential := &Credential{APIKey: "shared-channel-key"}
	newRequest := func(prompt string) Request {
		return Request{
			Model: "model-1", Prompt: prompt, ToolChoice: "auto", SessionScope: testCursorSessionScope,
			Tools: []Tool{{
				Name: "lookup", Description: "lookup by session",
				Parameters: []byte(`{"type":"object","properties":{"session":{"type":"string"}},"required":["session"]}`),
			}},
		}
	}

	first, err := runner.Run(context.Background(), credential, newRequest("first"))
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	second, err := runner.Run(context.Background(), credential, newRequest("second"))
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	firstCall := readNativeToolCall(t, first)
	secondCall := readNativeToolCall(t, second)
	if firstCall.ID == secondCall.ID || firstCall.Name != "lookup" || secondCall.Name != "lookup" {
		t.Fatalf("calls were not isolated: first=%+v second=%+v", firstCall, secondCall)
	}

	secondText := resumeNativeToolCall(t, runner, credential, secondCall.ID, "second-result")
	firstText := resumeNativeToolCall(t, runner, credential, firstCall.ID, "first-result")
	if firstText != "agent-1:first-result" || secondText != "agent-2:second-result" {
		t.Fatalf("cross-session result routing: first=%q second=%q", firstText, secondText)
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()
	for _, agentID := range []string{"agent-1", "agent-2"} {
		created := handler.creates[agentID]
		if created == nil || len(created.GetOptions().GetLocal().GetCustomTools()) != 1 ||
			created.GetOptions().GetTools() == nil ||
			!reflect.DeepEqual(created.GetOptions().GetTools().GetNames(), []string{"mcp"}) {
			t.Fatalf("%s CreateAgent options = %+v", agentID, created)
		}
	}
	if handler.results["agent-1"]["value"] != "first-result" ||
		handler.results["agent-2"]["value"] != "second-result" {
		t.Fatalf("callback results = %+v", handler.results)
	}
}

func TestSDKRunnerReplaysCompletedNativeToolTurn(t *testing.T) {
	runner, _ := newNativeToolTestRunner(t)
	credential := &Credential{APIKey: "channel-key"}
	events, err := runner.Run(context.Background(), credential, Request{
		Model: "model-1", Prompt: "lookup", ToolChoice: "auto", SessionScope: testCursorSessionScope,
		Tools: []Tool{{
			Name: "lookup", Parameters: []byte(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	call := readNativeToolCall(t, events)
	result := ToolResult{CallID: call.ID, Output: "value"}

	first := resumeNativeToolCall(t, runner, credential, call.ID, result.Output)
	replayed, err := runner.Run(context.Background(), credential, Request{ToolResults: []ToolResult{result}, SessionScope: testCursorSessionScope})
	if err != nil {
		t.Fatalf("duplicate Run() error = %v", err)
	}
	var replayedText string
	var replayedEvent bool
	var replayedUsage *Usage
	for event := range replayed {
		if event.Err != nil {
			t.Fatalf("duplicate turn event error = %v", event.Err)
		}
		if event.Text != "" {
			replayedText = event.Text
		}
		replayedEvent = replayedEvent || event.Replayed
		replayedUsage = event.Usage
	}
	if replayedText != first || !replayedEvent || replayedUsage != nil {
		t.Fatalf("duplicate turn = text %q, replayed %v, usage %+v; want %q, true, nil", replayedText, replayedEvent, replayedUsage, first)
	}
}

/**
 * @brief 原生恢复交付已有工具结果，显式禁用随后的回调与待发调用；省略配置则继续透传。
 * @param t 测试上下文。
 */
func TestSDKRunnerHonorsToolChoiceOnNativeResume(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		disabled   bool
		queuedCall bool
	}{
		{"OpenAI", `{"tool_choice":"none","messages":[{"role":"tool","tool_call_id":%q,"content":"first-result"}]}`, true, false},
		{"Anthropic", `{"tool_choice":{"type":"none"},"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"first-result"}]}]}`, true, true},
		{"legacy", `{"function_call":"none","messages":[{"role":"function","id":%q,"content":"first-result"}]}`, true, false},
		{"omitted", `{"messages":[{"role":"tool","tool_call_id":%q,"content":"first-result"}]}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			runner, handler := newNativeToolTestRunner(t)
			handler.turns = 2
			credential := &Credential{APIKey: "channel-key"}
			first, err := runner.Run(ctx, credential, Request{
				Model: "model-1", Prompt: "lookup", SessionScope: testCursorSessionScope,
				Tools: []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
			})
			if err != nil {
				t.Fatal(err)
			}
			call := readNativeToolCall(t, first)
			var queuedError chan error
			if tc.queuedCall {
				callbackURL, callbackToken := handler.control.callback()
				callback := sdkv1connect.NewSdkCustomToolCallbackServiceClient(http.DefaultClient, callbackURL)
				request := connect.NewRequest(&sdkv1.CallCustomToolRequest{AgentId: "agent-1", ToolName: "lookup"})
				request.Header().Set("Authorization", "Bearer "+callbackToken)
				queuedError = make(chan error, 1)
				go func() {
					_, err := callback.CallCustomTool(ctx, request)
					queuedError <- err
				}()
				// 等待第二个 RPC 的事件入队，再附着禁用工具的请求。
				session := runner.sessionByAgentID("agent-1")
				for len(session.events) == 0 {
					select {
					case <-ctx.Done():
						t.Fatal("the callback did not reach the paused run")
					case <-time.After(time.Millisecond):
					}
				}
			}

			resume := ParseRequest(fmt.Appendf(nil, tc.body, call.ID))
			resume.SessionScope = testCursorSessionScope
			events, err := runner.Run(ctx, credential, resume)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.disabled {
				second := readNativeToolCall(t, events)
				if got := resumeNativeToolCall(t, runner, credential, second.ID, "second-result"); got != "agent-1:second-result" {
					t.Fatalf("omitted choice did not preserve the original run: %q", got)
				}
				return
			}
			var terminal Event
			for event := range events {
				if event.ToolCall != nil {
					t.Fatalf("disabled tool call reached the client: %+v", event.ToolCall)
				}
				if event.Done {
					terminal = event
				}
			}
			if !terminal.Done || connect.CodeOf(terminal.Err) != connect.CodePermissionDenied {
				t.Fatalf("resume terminal = %+v, want callback permission denied", terminal)
			}
			if queuedError != nil {
				if err := <-queuedError; connect.CodeOf(err) != connect.CodePermissionDenied {
					t.Fatalf("queued callback error = %v, want permission denied", err)
				}
			}
			handler.mu.Lock()
			gotResult := handler.results["agent-1"]["value"]
			handler.mu.Unlock()
			if gotResult != "first-result" || handler.nextID.Load() != 1 {
				t.Fatalf("original run did not receive the accepted result: %v", gotResult)
			}
			replay, err := runner.Run(ctx, credential, resume)
			if err != nil {
				t.Fatal(err)
			}
			if event := <-replay; !event.Replayed || event.ToolCall != nil || event.Err != terminal.Err {
				t.Fatalf("replay = %+v, want saved terminal error without a tool call", event)
			}
		})
	}
}

/**
 * @brief 混合轮只交付一次原生结果，保留新的用户内容并重放替换轮的结果。
 * @param t 测试上下文。
 */
func TestSDKRunnerPreservesUserInputWithToolResults(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		failsOnce bool
		wantTools bool
	}{
		{
			name: "Anthropic same message",
			body: `{"messages":[{"role":"user","content":[
				{"type":"tool_result","tool_use_id":%q,"content":"first-result"},
				{"type":"text","text":"additional instruction"}]}]}`,
			wantTools: true,
		},
		{
			name: "Anthropic separate message with tools disabled",
			body: `{"tool_choice":{"type":"none"},"messages":[
				{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"first-result"}]},
				{"role":"user","content":"additional instruction"}]}`,
		},
		{
			name: "OpenAI separate message and failed setup",
			body: `{"messages":[{"role":"tool","tool_call_id":%q,"content":"first-result"},
				{"role":"user","content":"additional instruction"}]}`,
			failsOnce: true,
			wantTools: true,
		},
		{
			name: "OpenAI explicit empty tools",
			body: `{"tools":[],"messages":[{"role":"tool","tool_call_id":%q,"content":"first-result"},
				{"role":"user","content":"additional instruction"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			runner, handler := newNativeToolTestRunner(t)
			handler.deleted = make(chan string, 4)
			key := &Credential{APIKey: "channel-key"}
			settled := make(chan Usage, 1)
			first, err := runner.Run(ctx, key, Request{
				Model: "model-1", Prompt: "lookup", SessionScope: testCursorSessionScope,
				Tools:           []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
				OnDetachedUsage: func(usage Usage) { settled <- usage },
			})
			if err != nil {
				t.Fatal(err)
			}
			call := readNativeToolCall(t, first)
			request := ParseRequest(fmt.Appendf(nil, tc.body, call.ID))
			request.SessionScope = testCursorSessionScope
			if tc.failsOnce {
				handler.mu.Lock()
				handler.createErr = connect.NewError(connect.CodeUnavailable, errors.New("temporary create failure"))
				handler.mu.Unlock()
				if events, err := runner.Run(ctx, key, request); events != nil || connect.CodeOf(err) != connect.CodeUnavailable {
					t.Fatalf("failed setup = %v, %v", events, err)
				}
			}
			events, err := runner.Run(ctx, key, request)
			if err != nil {
				t.Fatal(err)
			}
			want := "agent-2:<nil>"
			var terminal Event
			if tc.wantTools {
				followUp := readNativeToolCall(t, events)
				if followUp.ID == call.ID {
					t.Fatal("replacement Run reused a previous tool call")
				}
				changed := request
				changed.ToolChoice = "none"
				for _, repeat := range []Request{request, changed} {
					if duplicate, err := runner.Run(ctx, key, repeat); duplicate != nil || !errors.Is(err, ErrToolTurnInProgress) {
						t.Fatalf("overlapping continuation = %v, %v", duplicate, err)
					}
				}
				continued, err := runner.Run(ctx, key, Request{
					ToolResults:  []ToolResult{{CallID: followUp.ID, Output: "replacement result"}},
					SessionScope: testCursorSessionScope,
				})
				if err != nil {
					t.Fatal(err)
				}
				terminal = drainRun(t, continued)
				want = "agent-2:replacement result"
			} else {
				terminal = drainRun(t, events)
			}
			if terminal.Text != want || terminal.Replayed || terminal.Usage == nil || terminal.Usage.TotalTokens != 150 {
				t.Fatalf("replacement terminal = %+v, want %q", terminal, want)
			}
			replay, err := runner.Run(ctx, key, request)
			if err != nil {
				t.Fatal(err)
			}
			if terminal := drainRun(t, replay); !terminal.Replayed || terminal.Text != want || terminal.Usage != nil {
				t.Fatalf("mixed request replay = %+v", terminal)
			}
			for deleted := ""; deleted != "agent-1"; {
				select {
				case deleted = <-handler.deleted:
				case <-ctx.Done():
					t.Fatal("original Agent was not deleted")
				}
			}
			select {
			case usage := <-settled:
				if usage.TotalTokens != 150 {
					t.Fatalf("original detached usage = %+v", usage)
				}
			default:
				t.Fatal("original Agent deletion preceded usage settlement")
			}
			handler.mu.Lock()
			defer handler.mu.Unlock()
			if got := handler.results["agent-1"]["value"]; got != "first-result" {
				t.Fatalf("original callback result = %v, want first-result", got)
			}
			if len(handler.sent) != 2 || strings.Count(handler.sent[1], "additional instruction") != 1 ||
				strings.Count(handler.sent[1], "first-result") != 1 ||
				!strings.HasSuffix(handler.sent[1], cursorSessionRecoveryNote) {
				t.Fatalf("Send messages = %q", handler.sent)
			}
			gotTools := len(handler.creates["agent-2"].GetOptions().GetLocal().GetCustomTools()) != 0
			if gotTools != tc.wantTools {
				t.Fatalf("replacement registered tools = %v, want %v", gotTools, tc.wantTools)
			}
		})
	}
}

/**
 * @brief 同一批已完成结果可以携带不同的后续输入，重试只重放最新匹配的后续轮。
 * @param t 测试上下文。
 */
func TestSDKRunnerReplaysLatestDistinctMixedToolTurn(t *testing.T) {
	runner, handler := newNativeToolTestRunner(t)
	credential := &Credential{APIKey: "channel-key"}
	call := readNativeToolCall(t, mustRun(t, runner, credential, Request{
		Model: "model-1", Prompt: "lookup", SessionScope: testCursorSessionScope,
		Tools: []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
	}))
	inputs := []struct{ history, result, instruction string }{
		{"first history", "value", "same instruction"},
		{"updated history", "value", "same instruction"},
		{"updated history", "updated result", "same instruction"},
		{"updated history", "updated result", "new instruction"},
	}
	for index, input := range inputs {
		request := ParseRequest(fmt.Appendf(nil, `{"tool_choice":"none","messages":[
			{"role":"system","content":%q},{"role":"tool","tool_call_id":%q,"content":%q},{"role":"user","content":%q}
		]}`, input.history, call.ID, input.result, input.instruction))
		request.SessionScope = testCursorSessionScope
		want := fmt.Sprintf("agent-%d:<nil>", index+2)
		if terminal := drainRun(t, mustRun(t, runner, credential, request)); terminal.Text != want || terminal.Replayed {
			t.Fatalf("fresh turn = %+v, want %s", terminal, want)
		}
		if terminal := drainRun(t, mustRun(t, runner, credential, request)); terminal.Text != want || !terminal.Replayed || terminal.SentEffort != nil {
			t.Fatalf("replayed turn = %+v, want %s", terminal, want)
		}
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.sent) != 1+len(inputs) || handler.results["agent-1"]["value"] != "value" {
		t.Fatalf("Send messages = %q", handler.sent)
	}
	for index, input := range inputs {
		prompt := handler.sent[index+1]
		if !strings.Contains(prompt, input.history) || !strings.Contains(prompt, input.result) || !strings.Contains(prompt, input.instruction) {
			t.Fatalf("Send %d omitted current input: %q", index+1, prompt)
		}
	}
}

/**
 * @brief 确认工具结果只在原渠道、调用方、上游密钥与路由模型均匹配时恢复或重放。
 * @param t 测试上下文。
 */
func TestSDKRunnerScopesNativeToolResumeAndReplay(t *testing.T) {
	const originalModel = "model-1"
	credential := &Credential{APIKey: "channel-key"}
	for _, tt := range []struct {
		name       string
		scope      SessionScope
		credential *Credential
		model      string
	}{
		{"other token", SessionScope{ChannelID: 1, TokenID: 2}, credential, originalModel},
		{"other channel", SessionScope{ChannelID: 2, TokenID: 1}, credential, originalModel},
		{"other upstream key", testCursorSessionScope, &Credential{APIKey: "different-key"}, originalModel},
		{"no token", SessionScope{ChannelID: 1}, credential, originalModel},
		{"other model", testCursorSessionScope, credential, "model-2"},
		{"fast variant", testCursorSessionScope, credential, originalModel + "-fast"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner, handler := newNativeToolTestRunner(t)
			start := Request{
				Model: originalModel, Prompt: "lookup", SessionScope: testCursorSessionScope,
				Tools: []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
			}
			original := readNativeToolCall(t, mustRun(t, runner, credential, start))
			foreign := start
			foreign.SessionScope = tt.scope
			foreign.Model = tt.model
			foreign.ToolResults = []ToolResult{{CallID: original.ID, Output: "foreign"}}
			pendingFallback := readNativeToolCall(t, mustRun(t, runner, tt.credential, foreign))
			owned := Request{Model: originalModel, SessionScope: testCursorSessionScope, ToolResults: []ToolResult{{CallID: original.ID, Output: "owned"}}}
			if final := drainRun(t, mustRun(t, runner, credential, owned)); final.Replayed || final.Text != "agent-1:owned" {
				t.Fatalf("original result = %+v; foreign request changed the original session", final)
			}
			completedFallback := readNativeToolCall(t, mustRun(t, runner, tt.credential, foreign))
			if pendingFallback.ID == original.ID || completedFallback.ID == original.ID || pendingFallback.ID == completedFallback.ID {
				t.Fatalf("expected independent fallback calls, got %q, %q and %q", original.ID, pendingFallback.ID, completedFallback.ID)
			}
			if replay := drainRun(t, mustRun(t, runner, credential, owned)); !replay.Replayed || replay.Text != "agent-1:owned" {
				t.Fatalf("original replay = %+v; foreign request changed the completed session", replay)
			}
			handler.mu.Lock()
			defer handler.mu.Unlock()
			if len(handler.sent) != 3 || !strings.HasSuffix(handler.sent[1], cursorSessionRecoveryNote) ||
				!strings.HasSuffix(handler.sent[2], cursorSessionRecoveryNote) {
				t.Fatalf("expected fresh runs with recovery history, got Send messages %q", handler.sent)
			}
		})
	}
}

/**
 * @brief 未认证的调用正常收到工具事件，无法恢复的 Run 随后立即清理。
 * @param t 测试上下文。
 */
func TestSDKRunnerDeletesUnscopedToolSession(t *testing.T) {
	runner, _ := newNativeToolTestRunner(t)
	runner.idleTimeout = time.Minute
	client := runner.bridge.process.client
	deleted := make(chan struct{})
	client.agent = &deleteObservingAgentClient{SdkAgentServiceClient: client.agent, deleted: deleted}
	request := Request{
		Prompt: "lookup", Tools: []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	events, err := runner.Run(ctx, &Credential{APIKey: "channel-key"}, request)
	if err != nil {
		t.Fatal(err)
	}
	readNativeToolCall(t, events)
	select {
	case <-deleted:
	case <-ctx.Done():
		t.Fatal("unscoped Agent was not deleted after delivering its tool call")
	}
}

/**
 * @brief 验证既有模型后缀在 CreateAgent 上传为基础 ID 与声明的参数。
 * @param t 测试上下文。
 */
func TestSDKRunnerResolvesModelSuffixes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, id string
		params map[string]string
	}{
		{"claude-opus-5", "claude-opus-5", nil},
		{"claude-opus-5-fast", "claude-opus-5", map[string]string{"fast": "true"}},
		{"claude-opus-5-thinking-xhigh", "claude-opus-5", map[string]string{"effort": "extra-high"}},
		{"claude-opus-5-thinking-xhigh-fast", "claude-opus-5", map[string]string{"fast": "true", "effort": "extra-high"}},
		{"gpt-5.6-sol-high", "gpt-5.6-sol", map[string]string{"reasoning": "high"}},
		{"gpt-5.4-mini", "gpt-5.4-mini", nil},
		{"composer-2.5", "composer-2.5", nil},
		{"gpt-5.6-sol-xhigh-fast", "gpt-5.6-sol", map[string]string{"fast": "true", "reasoning": "xhigh"}},
		{"claude-opus-5-extra-high", "claude-opus-5", map[string]string{"effort": "extra-high"}},
	}
	effortParam := []*sdkv1.ModelParameterDefinition{{Id: "effort", Values: []*sdkv1.ModelParameterDefinitionValue{
		{Value: "extra-high"}, {Value: "high"},
	}}}
	handler := &testAgentHandler{sendFn: finishedRunSend(t, "ok")}
	runner, _ := newCatalogTestSDKRunner(t, handler, []*sdkv1.SdkModel{
		{Id: "claude-opus-5", Parameters: effortParam}, {Id: "gpt-5.6-sol", Parameters: []*sdkv1.ModelParameterDefinition{
			{Id: "reasoning", Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "high"}, {Value: "xhigh"}}},
		}},
	})
	for _, tc := range cases {
		drainRun(t, mustRun(t, runner, &Credential{APIKey: "key"}, Request{Model: tc.in, Prompt: "hi"}))
		handler.mu.Lock()
		model := handler.create.GetOptions().GetModel()
		handler.mu.Unlock()
		got := map[string]string{}
		for _, param := range model.GetParams() {
			got[param.GetId()] = param.GetValue()
		}
		if model.GetId() != tc.id || len(model.GetParams()) != len(tc.params) || !maps.Equal(got, tc.params) {
			t.Errorf("%q sent model %v, want %q %v", tc.in, model, tc.id, tc.params)
		}
	}
}

func TestSDKRunnerFallsBackToNewTurnWhenToolSessionIsMissing(t *testing.T) {
	runner, handler := newNativeToolTestRunner(t)
	credential := &Credential{APIKey: "channel-key"}
	request := ParseRequest([]byte(`{
		"model":"claude-opus-5","tools":[{"name":"lookup","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"continue"},{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"call_stale_missing","content":"lookup failed","is_error":true}
		]}]
	}`))
	request.SessionScope = testCursorSessionScope
	events, err := runner.Run(t.Context(), credential, request)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if call := readNativeToolCall(t, events); call.ID == "" {
		t.Fatal("expected a new native tool call after stale tool_result")
	}
	handler.mu.Lock()
	created := len(handler.creates)
	handler.mu.Unlock()
	if created != 1 {
		t.Fatalf("CreateAgent count = %d, want 1 (stale tool_result must start a new turn)", created)
	}
	handler.mu.Lock()
	sent := handler.sent[0]
	handler.mu.Unlock()
	if !strings.HasPrefix(sent, "user: continue") || !strings.Contains(sent, "[tool_result tool_use_id=call_stale_missing is_error=true] lookup failed") ||
		!strings.HasSuffix(sent, cursorSessionRecoveryNote) {
		t.Fatalf("recovery prompt = %q", sent)
	}
}

/**
 * @brief 工具回调与普通续轮只检查实际重发的图片，丢失会话后按完整历史检查。
 * @param t 测试上下文。
 */
func TestSDKRunnerIgnoresURLImagesAlreadyInActiveHistory(t *testing.T) {
	runner, handler := newNativeToolTestRunner(t)
	handler.toolBeforeStream = true
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	credential := &Credential{APIKey: "channel-key"}
	tools := []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}}
	confirmedTool := func(request Request) ToolCall {
		t.Helper()
		events, err := runner.Run(ctx, credential, request)
		if err != nil {
			t.Fatal(err)
		}
		var call ToolCall
		for event := range events {
			if event.Err != nil {
				t.Fatal(event.Err)
			}
			if event.ToolCall != nil {
				if event.SentEffort == nil || *event.SentEffort != "" {
					t.Fatalf("tool-first send did not confirm the default effort: %+v", event)
				}
				call = *event.ToolCall
			}
		}
		if call.ID == "" {
			t.Fatal("missing tool callback")
		}
		return call
	}
	start := Request{Model: "model-1", Prompt: "lookup", Tools: tools, SessionScope: testCursorSessionScope}
	call := confirmedTool(start)
	const user = `{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/history.png"}}]}`
	result := fmt.Sprintf(`{"role":"tool","tool_call_id":%q,"content":"value"}`, call.ID)
	resume := ParseRequest(fmt.Appendf(nil, `{"model":"model-1","messages":[%s,%s]}`, user, result))
	resume.Tools = tools
	resume.SessionScope = testCursorSessionScope
	var final Event
	for event := range mustRun(t, runner, credential, resume) {
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		if event.SentEffort != nil {
			t.Fatalf("pure tool resume started a new model turn: %+v", event)
		}
		if event.Done {
			final = event
		}
	}
	if !final.Done || final.Text != "agent-1:value" {
		t.Fatalf("tool resume terminal = %+v", final)
	}
	waitParkedAgents(t, runner, 1)

	followUp := ParseRequest(fmt.Appendf(nil, `{"model":"model-1","messages":[%s,%s,
		{"role":"assistant","content":%q}, {"role":"user","content":"follow up"}
	]}`, user, result, final.Text))
	followUp.Tools = tools
	followUp.SessionScope = testCursorSessionScope
	confirmedTool(followUp)
	handler.mu.Lock()
	created, sent := len(handler.creates), slices.Clone(handler.sent)
	handler.mu.Unlock()
	if created != 1 || !slices.Equal(sent, []string{"lookup", "user: follow up"}) {
		t.Fatalf("CreateAgent=%d Send messages=%q, want one agent and only the new input on the second Send", created, sent)
	}

	resume.ToolResults[0].CallID = "stale-tool-call"
	events, err := runner.Run(t.Context(), credential, resume)
	if !errors.Is(err, ErrURLImageUnsupported) || events != nil {
		t.Fatalf("stale tool result Run() events=%v err=%v, want URL image capability error for the replayed history", events, err)
	}
}

/**
 * @brief 工具暂停时只转交真实 SDK 用量，无报告时不补估算或查询累计快照。
 * @param t 测试上下文。
 */
func TestSDKRunnerDoesNotEstimateUsageAtNativeToolBoundary(t *testing.T) {
	runner, handler := newNativeToolTestRunner(t)
	events, err := runner.Run(context.Background(), &Credential{APIKey: "channel-key"}, Request{
		Model: "model-1", Prompt: "lookup", ToolChoice: "auto", SessionScope: testCursorSessionScope,
		Tools: []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var call ToolCall
	for event := range events {
		if event.Err != nil {
			t.Fatalf("tool turn event error = %v", event.Err)
		}
		if event.ToolCall != nil {
			call = *event.ToolCall
		}
		if event.Usage != nil {
			t.Fatalf("unexpected tool turn usage: %+v", event.Usage)
		}
	}
	if call.ID == "" {
		t.Fatalf("missing tool call: %+v", call)
	}
	handler.mu.Lock()
	getRunCalls := handler.getRuns["run-agent-1"]
	handler.mu.Unlock()
	if getRunCalls != 0 {
		t.Fatalf("tool callback unexpectedly queried GetRun %d times", getRunCalls)
	}
	_ = resumeNativeToolCall(t, runner, &Credential{APIKey: "channel-key"}, call.ID, "value")
}

func TestSDKRunnerReplaysCompletedToolResultAfterClientCompaction(t *testing.T) {
	runner, _ := newNativeToolTestRunner(t)
	credential := &Credential{APIKey: "channel-key"}
	events, err := runner.Run(context.Background(), credential, Request{
		Model: "model-1", Prompt: "lookup", ToolChoice: "auto", SessionScope: testCursorSessionScope,
		Tools: []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	call := readNativeToolCall(t, events)
	_ = resumeNativeToolCall(t, runner, credential, call.ID, "original")

	replayed, err := runner.Run(context.Background(), credential, Request{
		ToolResults: []ToolResult{{CallID: call.ID, Output: "[compacted artifact reference]"}}, SessionScope: testCursorSessionScope,
	})
	if err != nil {
		t.Fatalf("compacted duplicate Run() error = %v", err)
	}
	var final Event
	for event := range replayed {
		final = event
	}
	if final.Err != nil || !final.Replayed || final.Text != "agent-1:original" {
		t.Fatalf("compacted duplicate event = %+v", final)
	}
}

func TestSDKRunnerConcurrentlyReplaysDuplicateToolResult(t *testing.T) {
	runner, _ := newNativeToolTestRunner(t)
	credential := &Credential{APIKey: "channel-key"}
	events, err := runner.Run(context.Background(), credential, Request{
		Model: "model-1", Prompt: "lookup", ToolChoice: "auto", SessionScope: testCursorSessionScope,
		Tools: []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	call := readNativeToolCall(t, events)

	type outcome struct {
		text     string
		replayed bool
		err      error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			turn, runErr := runner.Run(context.Background(), credential, Request{
				ToolResults: []ToolResult{{CallID: call.ID, Output: "value"}}, SessionScope: testCursorSessionScope,
			})
			result := outcome{err: runErr}
			if runErr == nil {
				for event := range turn {
					if event.Err != nil {
						result.err = event.Err
					}
					if event.Text != "" {
						result.text = event.Text
					}
					result.replayed = result.replayed || event.Replayed
				}
			}
			outcomes <- result
		}()
	}
	close(start)

	replays := 0
	for range 2 {
		result := <-outcomes
		if result.err != nil || result.text != "agent-1:value" {
			t.Fatalf("concurrent duplicate outcome = %+v", result)
		}
		if result.replayed {
			replays++
		}
	}
	if replays != 1 {
		t.Fatalf("replayed responses = %d, want 1", replays)
	}
}

func TestSDKSessionBatchesConcurrentNativeToolCalls(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	session := &sdkSession{
		ctx: ctx, cancel: cancel, events: make(chan Event, 4), done: make(chan struct{}),
	}
	events, err := session.nextTurn(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("nextTurn() error = %v", err)
	}
	for _, id := range []string{"call-a", "call-b"} {
		call := ToolCall{ID: id, Name: "lookup", Arguments: []byte(`{}`)}
		if !session.emit(Event{ToolCall: &call}) {
			t.Fatalf("emit %s failed", id)
		}
	}
	var ids []string
	for event := range events {
		if event.ToolCall != nil {
			ids = append(ids, event.ToolCall.ID)
		}
	}
	if len(ids) != 2 || ids[0] != "call-a" || ids[1] != "call-b" {
		t.Fatalf("batched calls = %v", ids)
	}
	session.abort(context.Canceled)
}

func TestResolveToolResultsIsAtomic(t *testing.T) {
	session := &sdkSession{}
	first := &pendingToolCall{session: session, result: make(chan toolCallbackResult, 1)}
	second := &pendingToolCall{session: session, result: make(chan toolCallbackResult, 1), resolved: true}
	runner := &SDKRunner{calls: map[string]*pendingToolCall{"call-a": first, "call-b": second}}
	err := runner.resolveToolResults(session, []ToolResult{
		{CallID: "call-a", Output: "a"}, {CallID: "call-b", Output: "b"},
	}, false)
	if err == nil {
		t.Fatal("resolveToolResults() unexpectedly succeeded")
	}
	first.mu.Lock()
	firstResolved := first.resolved
	first.mu.Unlock()
	if firstResolved || len(first.result) != 0 {
		t.Fatalf("first result was partially committed: resolved=%v queued=%d", firstResolved, len(first.result))
	}
}

func TestSDKSessionDeliversTerminalAfterFullEventBuffer(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	session := &sdkSession{
		ctx: ctx, cancel: cancel, events: make(chan Event, 2), done: make(chan struct{}),
	}
	if !session.emit(Event{RawResponse: []byte(`{"offset":"1"}`)}) ||
		!session.emit(Event{Delta: "answer", Text: "answer"}) {
		t.Fatal("failed to fill session event buffer")
	}
	wantErr := context.DeadlineExceeded
	session.finish(Event{Text: "answer", Done: true, Err: wantErr, Usage: &Usage{TotalTokens: 7}})
	events, err := session.nextTurn(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("nextTurn() error = %v", err)
	}
	var final Event
	for event := range events {
		if event.Done {
			final = event
		}
	}
	if !final.Done || final.Err != wantErr || final.Usage == nil || final.Usage.TotalTokens != 7 {
		t.Fatalf("terminal event = %+v", final)
	}
}

/**
 * @brief SDK 已停止而终态尚未发布时，已附着请求等待终态并领用真实用量。
 * @param t 测试上下文。
 */
func TestSDKSessionToolBatchWaitsForStoppingRun(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
	}{
		{name: "bridge closing", cause: ErrBridgeClosed},
		{name: "normal terminal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			session := &sdkSession{ctx: ctx, cancel: cancel, events: make(chan Event, 2), done: make(chan struct{})}
			settlements := make(chan Usage, 1)
			events, err := session.nextTurn(t.Context(), func(usage Usage) { settlements <- usage }, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			session.turnMu.Lock()
			emitted := session.emit(Event{ToolCall: &ToolCall{ID: "call-1", Name: "lookup"}})
			cancel(test.cause)
			session.turnMu.Unlock()
			if !emitted {
				t.Fatal("failed to send tool event before stopping the run")
			}
			if event := <-events; event.ToolCall == nil {
				t.Fatalf("first event = %+v, want tool call", event)
			}
			select {
			case event, open := <-events:
				t.Fatalf("tool batch ended before terminal publication: event=%+v open=%v", event, open)
			case <-time.After(50 * time.Millisecond):
			}
			want := Usage{TotalTokens: 7}
			session.finish(Event{Done: true, Err: test.cause, Usage: &want})
			if final := <-events; !final.Done || final.Err != test.cause || final.Usage == nil || *final.Usage != want {
				t.Fatalf("terminal event = %+v, want usage %+v and error %v", final, want, test.cause)
			}
			if _, open := <-events; open {
				t.Fatal("events stayed open after terminal delivery")
			}
			select {
			case usage := <-settlements:
				t.Fatalf("attached run produced a detached settlement: %+v", usage)
			default:
			}
		})
	}
}

func readNativeToolCall(t *testing.T, events <-chan Event) ToolCall {
	t.Helper()
	var call ToolCall
	for event := range events {
		if event.Err != nil {
			t.Fatalf("tool turn error = %v", event.Err)
		}
		if event.ToolCall != nil {
			call = *event.ToolCall
		}
	}
	if call.ID == "" {
		t.Fatal("tool turn ended without a native tool call")
	}
	return call
}

func resumeNativeToolCall(t *testing.T, runner *SDKRunner, credential *Credential, callID, output string) string {
	t.Helper()
	events, err := runner.Run(context.Background(), credential, Request{
		ToolResults: []ToolResult{{CallID: callID, Output: output}}, SessionScope: testCursorSessionScope,
	})
	if err != nil {
		t.Fatalf("resume %s error = %v", callID, err)
	}
	var text string
	for event := range events {
		if event.Err != nil {
			t.Fatalf("resume %s event error = %v", callID, event.Err)
		}
		if event.Text != "" {
			text = event.Text
		}
	}
	return text
}

/**
 * @brief 多轮活动会话超过空闲预算仍可恢复，各轮思考、用量及终态差额只发出一次。
 * @param t 测试上下文。
 */
func TestSDKRunnerToolTurnsPreserveLifetimeAndIncrementalUsage(t *testing.T) {
	runner, handler := newNativeToolTestRunner(t)
	runner.idleTimeout = 80 * time.Millisecond
	handler.turns = 4
	handler.turnDelay = 35 * time.Millisecond
	handler.turnUsage = map[string]any{
		"inputTokens": 10, "outputTokens": 2, "cacheReadTokens": 3,
		"cacheWriteTokens": 1, "totalTokens": 16, "reasoningTokens": 1,
	}
	handler.usage["run-agent-1"] = &sdkv1.TokenUsage{
		InputTokens: 45, OutputTokens: 10, CacheReadTokens: 12,
		CacheWriteTokens: 4, TotalTokens: 71, ReasoningTokens: new(int64(4)),
	}
	credential := &Credential{APIKey: "channel-key"}
	req := Request{Model: "model-1", Prompt: "lookup", Tools: []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}}, SessionScope: testCursorSessionScope}
	started := time.Now()
	var sum Usage
	for turn := range 5 {
		ctx, cancel := context.WithCancel(context.Background())
		events, err := runner.Run(ctx, credential, req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		var call ToolCall
		var actual Usage
		var final Event
		var thinking []string
		for event := range events {
			if event.Err != nil {
				cancel()
				t.Fatal(event.Err)
			}
			if event.ToolCall != nil {
				call = *event.ToolCall
			}
			if event.ThinkingDelta != "" {
				thinking = append(thinking, event.ThinkingDelta)
				if event.Delta != "" || event.Text != "" {
					cancel()
					t.Fatalf("turn %d thinking mixed with assistant text: %+v", turn, event)
				}
			}
			if event.Usage != nil {
				actual.Add(event.Usage)
			}
			if event.Done {
				final = event
			}
		}
		cancel()
		sum.Add(&actual)
		wantThinking := "Finish."
		if turn < 4 {
			wantThinking = fmt.Sprintf("Check %d.", turn+1)
		}
		if !reflect.DeepEqual(thinking, []string{wantThinking}) {
			t.Fatalf("turn %d thinking=%#v, want %q", turn, thinking, wantThinking)
		}
		if turn < 4 {
			if call.ID == "" || actual != (Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 1, TotalTokens: 16, ReasoningTokens: 1}) {
				t.Fatalf("turn %d call=%+v usage=%+v", turn, call, actual)
			}
			req = Request{ToolResults: []ToolResult{{CallID: call.ID, Output: "ok"}}, SessionScope: testCursorSessionScope}
		} else if !final.Done || actual != (Usage{InputTokens: 5, OutputTokens: 2, TotalTokens: 7}) {
			t.Fatalf("terminal=%+v residual=%+v", final, actual)
		}
	}
	if time.Since(started) <= runner.idleTimeout {
		t.Fatal("test did not span the idle timeout budget")
	}
	if sum != (Usage{InputTokens: 45, OutputTokens: 10, CacheReadTokens: 12, CacheWriteTokens: 4, TotalTokens: 71, ReasoningTokens: 4}) {
		t.Fatalf("usage counted twice: %+v", sum)
	}
	if handler.nextID.Load() != 1 {
		t.Fatalf("active session was replaced: creates=%d", handler.nextID.Load())
	}
}

/**
 * @brief 暂停后只结算已收到的增量及终态补差；恢复与后台结算各自只能领用一次。
 * @param t 测试上下文。
 */
func TestSDKRunnerSettlesDetachedToolUsage(t *testing.T) {
	for _, mode := range []string{"terminal", "idle", "close", "resume"} {
		t.Run(mode, func(t *testing.T) {
			runner, handler := newNativeToolTestRunner(t)
			if mode == "idle" {
				runner.idleTimeout = 500 * time.Millisecond
			}
			handler.turnUsage = map[string]any{"inputTokens": 10, "outputTokens": 2, "totalTokens": 12}
			cumulative := &sdkv1.TokenUsage{InputTokens: 17, OutputTokens: 7, CacheReadTokens: 2, TotalTokens: 26}
			handler.usage["run-agent-1"] = cumulative
			report := make(chan struct{})
			handler.whileToolPending = func(ctx context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage], agentID, runID string) (bool, error) {
				select {
				case <-report:
				case <-ctx.Done():
					return false, ctx.Err()
				}
				err := stream.Send(nativeSDKMessage("usage", map[string]any{
					"agent_id": agentID, "run_id": runID, "usage": map[string]any{
						"inputTokens": 4, "outputTokens": 3, "cacheReadTokens": 2, "totalTokens": 9,
					},
				}))
				if err == nil && mode == "resume" {
					for range 80 {
						if err := stream.Send(nativeSDKMessage("status", map[string]any{"status": "RUNNING"})); err != nil {
							return false, err
						}
					}
				}
				if err != nil || mode != "terminal" {
					return false, err
				}
				result := runResult(agentID, runID, sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, "")
				result.GetResult().GetResult().Usage = cumulative
				if err := stream.Send(result); err != nil {
					return true, err
				}
				return true, stream.Send(runDone(agentID, runID))
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			detached := make(chan Usage, 2)
			credential := &Credential{APIKey: "channel-key"}
			request := Request{
				Prompt: "lookup", Tools: []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
				SessionScope: testCursorSessionScope, OnDetachedUsage: func(usage Usage) { detached <- usage },
			}
			firstCtx, cancelFirst := context.WithCancel(ctx)
			defer cancelFirst()
			events, err := runner.Run(firstCtx, credential, request)
			if err != nil {
				t.Fatal(err)
			}
			var initial Usage
			var call ToolCall
			for event := range events {
				if event.Err != nil || event.Done {
					t.Fatalf("tool turn ended early: %+v", event)
				}
				if event.ToolCall != nil {
					call = *event.ToolCall
				}
				if event.Usage != nil {
					initial.Add(event.Usage)
				}
			}
			cancelFirst()
			if call.ID == "" || initial != (Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}) {
				t.Fatalf("initial tool call = %+v, usage = %+v", call, initial)
			}
			session := runner.sessionByAgentID("agent-1")
			close(report)

			wantLate := Usage{InputTokens: 4, OutputTokens: 3, CacheReadTokens: 2, TotalTokens: 9}
			if mode == "resume" || mode == "terminal" {
				wantLate = Usage{InputTokens: 7, OutputTokens: 5, CacheReadTokens: 2, TotalTokens: 14}
			}
			if mode == "close" || mode == "resume" {
				// Synchronize with late usage reaching the SDK, or the queue filling before a resume.
				pending := 1
				if mode == "resume" {
					pending = cap(session.events)
				}
				for len(session.events) < pending {
					select {
					case <-ctx.Done():
						t.Fatal("the late usage did not reach the SDK")
					case <-time.After(time.Millisecond):
					}
				}
			}
			if mode == "close" {
				close(runner.bridge.process.exited)
				if err := runner.Close(ctx); err != nil {
					t.Fatalf("Close() error = %v", err)
				}
			}
			if mode == "resume" {
				reply := Request{ToolResults: []ToolResult{{CallID: call.ID, Output: "ok"}}, SessionScope: testCursorSessionScope, OnDetachedUsage: request.OnDetachedUsage}
				events, err := runner.Run(ctx, credential, reply)
				if err != nil {
					t.Fatal(err)
				}
				var resumed Usage
				for event := range events {
					if event.Err != nil {
						t.Fatal(event.Err)
					}
					if event.Usage != nil {
						resumed.Add(event.Usage)
					}
				}
				if resumed != wantLate {
					t.Fatalf("resumed usage = %+v, want %+v", resumed, wantLate)
				}
				replay, err := runner.Run(ctx, credential, reply)
				if err != nil {
					t.Fatal(err)
				}
				for event := range replay {
					if !event.Replayed || event.Usage != nil {
						t.Fatalf("replayed terminal = %+v", event)
					}
				}
			} else {
				select {
				case usage := <-detached:
					if usage != wantLate {
						t.Fatalf("detached usage = %+v, want %+v", usage, wantLate)
					}
				case <-ctx.Done():
					t.Fatal("missing detached usage")
				}
			}
			if mode == "terminal" {
				// A resume that selected this session before callback cleanup can still read its terminal.
				lateEvents, err := session.nextTurn(ctx, request.OnDetachedUsage, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				var terminal Event
				for event := range lateEvents {
					if event.Usage != nil {
						t.Fatalf("late resume repeated settled usage: %+v", event.Usage)
					}
					if event.Done {
						terminal = event
					}
				}
				if !terminal.Done || terminal.Err != nil {
					t.Fatalf("late resume terminal = %+v", terminal)
				}
			}
			runner.active.Wait()
			select {
			case usage := <-detached:
				t.Fatalf("extra detached usage: %+v", usage)
			default:
			}
		})
	}
}
