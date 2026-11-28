package cursorauth

import (
	"context"
	"encoding/json"
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
	toolNames        []string ///< 可选的逐轮模拟工具名。
	turnDelay        time.Duration
	turnUsage        map[string]any
	toolTurnText     string                                                                                             ///< 每批工具回调之前的可选回复正文。
	toolBeforeStream bool                                                                                               ///< 首个工具回调在任何流式响应前到达。
	sent             []string                                                                                           ///< 按序记录的 Send 消息正文
	whileToolPending func(context.Context, *connect.ServerStream[sdkv1.RunStreamMessage], string, string) (bool, error) ///< true 表示已经发送终态
	finishTurn       func(*connect.ServerStream[sdkv1.RunStreamMessage], string, string) error                          ///< 可选的最后一轮 SDK 帧。
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
		if len(h.toolNames) > 0 {
			turns = len(h.toolNames)
		}
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
		if h.toolTurnText != "" {
			if err := stream.Send(nativeSDKMessage("assistant", map[string]any{
				"agent_id": agentID, "run_id": runID,
				"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": h.toolTurnText}}},
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
		toolName := "lookup"
		if len(h.toolNames) > 0 {
			toolName = h.toolNames[turn]
		}
		toolRequest := connect.NewRequest(&sdkv1.CallCustomToolRequest{
			AgentId: agentID, ToolName: toolName, ToolCallId: &callID,
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
	if h.finishTurn != nil {
		return h.finishTurn(stream, agentID, runID)
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

/**
 * @brief 同一客户端会话的并行线程各自收取工具调用和结果。
 * @param t 测试上下文。
 */
func TestSDKRunnerNativeToolsKeepConcurrentThreadsIsolated(t *testing.T) {
	runner, handler := newNativeToolTestRunner(t)
	credential := &Credential{APIKey: "shared-channel-key"}
	newRequest := func(prompt, threadID string) Request {
		scope := testCursorSessionScope
		scope.SessionID, scope.ThreadID = "session", threadID
		return Request{
			Model: "model-1", Prompt: prompt, ToolChoice: "auto", SessionScope: scope,
			Tools: []Tool{{
				Name: "lookup", Description: "lookup by session",
				Parameters: []byte(`{"type":"object","properties":{"session":{"type":"string"}},"required":["session"]}`),
			}},
		}
	}

	first, second := newRequest("first", "thread-a"), newRequest("second", "thread-b")
	firstEvents := mustRun(t, runner, credential, first)
	secondEvents := mustRun(t, runner, credential, second)
	firstCall := readNativeToolCall(t, firstEvents)
	secondCall := readNativeToolCall(t, secondEvents)
	if firstCall.ID == secondCall.ID || firstCall.Name != "lookup" || secondCall.Name != "lookup" {
		t.Fatalf("calls were not isolated: first=%+v second=%+v", firstCall, secondCall)
	}

	secondResult := Request{SessionScope: second.SessionScope, ToolResults: []ToolResult{{CallID: secondCall.ID, Output: "second-result"}}}
	firstResult := Request{SessionScope: first.SessionScope, ToolResults: []ToolResult{{CallID: firstCall.ID, Output: "first-result"}}}
	secondText := drainRun(t, mustRun(t, runner, credential, secondResult)).Text
	firstText := drainRun(t, mustRun(t, runner, credential, firstResult)).Text
	if firstText != "agent-1:first-result" || secondText != "agent-2:second-result" {
		t.Fatalf("thread results: first=%q second=%q", firstText, secondText)
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

/**
 * @brief 已认证的旧客户端省略会话和线程头时仍可提交工具结果并重放最终响应。
 * @param t 测试上下文。
 */
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
 * @brief 已完成的工具续轮只用本轮正文对齐终态，重试保持正文和思考的原始顺序。
 * @param t 测试上下文。
 */
func TestSDKRunnerReplaysCurrentToolTurnOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delta  string
		want   []string
		failed bool
	}{
		{"prefix from previous turn", "", []string{"thinking:Before.", "thinking:After.", "text:Disconnecting now.", "done"}, false},
		{"prefix from current turn", "Discon", []string{"thinking:Before.", "text:Discon", "thinking:After.", "text:necting now.", "done"}, false},
		{"partial text and failure", "Disconnect", []string{"thinking:Before.", "text:Disconnect", "thinking:After.", "error:cursor SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED: Upstream busy"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, handler := newNativeToolTestRunner(t)
			handler.toolTurnText = "Disconnect"
			handler.finishTurn = func(stream *connect.ServerStream[sdkv1.RunStreamMessage], agentID, runID string) error {
				thinking := func(value string) *sdkv1.RunStreamMessage {
					return nativeSDKMessage("thinking", map[string]any{"agent_id": agentID, "run_id": runID, "text": value})
				}
				frames := []*sdkv1.RunStreamMessage{thinking("Before.")}
				if tc.delta != "" {
					frames = append(frames, nativeSDKMessage("assistant", map[string]any{
						"agent_id": agentID, "run_id": runID,
						"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": tc.delta}}},
					}))
				}
				result := runResult(agentID, runID, sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, "Disconnecting now.")
				if tc.failed {
					const status = sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR
					result = runResult(agentID, runID, status, "Disconnect while leaking diagnostic text")
					result.GetResult().ErrorCode = new("RATE_LIMIT_EXCEEDED")
					result.GetResult().GetResult().ErrorMessage = new("Upstream busy")
				}
				for _, frame := range append(frames, thinking("After."), result, runDone(agentID, runID)) {
					if err := stream.Send(frame); err != nil {
						return err
					}
				}
				return nil
			}
			credential := &Credential{APIKey: "channel-key"}
			call := readNativeToolCall(t, mustRun(t, runner, credential, Request{
				Model: "model-1", Prompt: "lookup", SessionScope: testCursorSessionScope,
				Tools: []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
			}))
			request := Request{
				ToolResults: []ToolResult{{CallID: call.ID, Output: "value"}}, SessionScope: testCursorSessionScope,
			}
			for _, replay := range []bool{false, true} {
				var got []string
				var text strings.Builder
				for event := range mustRun(t, runner, credential, request) {
					if event.Replayed != replay || (replay && (event.Usage != nil || len(event.UsageIncrements) != 0 || event.SentEffort != nil)) {
						t.Fatalf("replay=%t, event=%+v", replay, event)
					}
					switch {
					case event.ThinkingDelta != "":
						got = append(got, "thinking:"+event.ThinkingDelta)
					case event.Delta != "":
						text.WriteString(event.Delta)
						got = append(got, "text:"+event.Delta)
					case event.Done:
						if event.Text != text.String() {
							t.Fatalf("terminal text=%q, emitted=%q", event.Text, text.String())
						}
						if event.Err != nil {
							got = append(got, "error:"+event.Err.Error())
						} else {
							got = append(got, "done")
						}
					}
				}
				if !slices.Equal(got, tc.want) {
					t.Fatalf("replay=%t, events=%q, want %q", replay, got, tc.want)
				}
			}
			if handler.nextID.Load() != 1 {
				t.Fatalf("created agents = %d, want 1", handler.nextID.Load())
			}
		})
	}
}

/**
 * @brief 工具结果重试按调用 ID 恢复尚未执行的下一批工具，不重复消费或发起 SDK Send。
 * @param t 测试上下文。
 */
func TestSDKRunnerReplaysNextPendingNativeToolBatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	runner, handler := newNativeToolTestRunner(t)
	handler.turns = 3
	handler.toolTurnText = "Checking lookup. "
	const toolTurnTokens = 12
	handler.turnUsage = map[string]any{"inputTokens": 10, "outputTokens": 2, "totalTokens": toolTurnTokens}
	credential := &Credential{APIKey: "channel-key"}
	scope := testCursorSessionScope
	scope.SessionID, scope.ThreadID = "session", "thread"
	request := func(body string) Request {
		req := ParseRequest([]byte(body))
		req.SessionScope = scope
		return req
	}
	result := func(callID, value string) Request {
		return request(fmt.Sprintf(`{"model":"model-1","messages":[{"role":"tool","tool_call_id":%q,"content":%q}]}`, callID, value))
	}
	run := func(req Request) <-chan Event {
		t.Helper()
		events, err := runner.Run(ctx, credential, req)
		if err != nil {
			t.Fatal(err)
		}
		return events
	}
	readPending := func(events <-chan Event, replayed bool, wantThinking string, wantTokens int) ToolCall {
		t.Helper()
		var calls []ToolCall
		var text, thinking strings.Builder
		var tokens int
		for event := range events {
			if event.Err != nil || event.Done || event.Replayed != replayed ||
				(replayed && (event.Usage != nil || len(event.UsageIncrements) != 0 || event.SentEffort != nil)) {
				t.Fatalf("unexpected pending tool event: %+v", event)
			}
			if event.ToolCall != nil {
				calls = append(calls, *event.ToolCall)
			}
			text.WriteString(event.Delta)
			thinking.WriteString(event.ThinkingDelta)
			if event.Usage != nil {
				tokens += event.Usage.TotalTokens
			}
		}
		if len(calls) != 1 || tokens != wantTokens || text.String() != handler.toolTurnText || thinking.String() != wantThinking {
			t.Fatalf("tool response = %+v, tokens = %d, text = %q, thinking = %q", calls, tokens, text.String(), thinking.String())
		}
		return calls[0]
	}
	start := request(`{"model":"model-1","messages":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	firstCall := readPending(run(start), false, "Check 1.", toolTurnTokens)
	firstResult := request(fmt.Sprintf(`{"model":"model-1","messages":[{"role":"user","content":"lookup"},{"role":"tool","tool_call_id":%q,"content":"first-result"}]}`, firstCall.ID))
	compacted := result(firstCall.ID, "first-result")
	secondTurn := run(firstResult)
	concurrentRetry := run(compacted)
	secondCall := readPending(secondTurn, false, "Check 2.", toolTurnTokens)
	for _, events := range []<-chan Event{concurrentRetry, run(compacted)} {
		if call := readPending(events, true, "Check 2.", 0); !reflect.DeepEqual(call, secondCall) {
			t.Fatalf("replay tool call = %+v, want %+v", call, secondCall)
		}
	}
	thirdCall := readPending(run(result(secondCall.ID, "second-result")), false, "Check 3.", toolTurnTokens)
	if firstCall.ID == secondCall.ID || secondCall.ID == thirdCall.ID || firstCall.ID == thirdCall.ID {
		t.Fatalf("expected distinct downstream call IDs, got %q, %q, and %q", firstCall.ID, secondCall.ID, thirdCall.ID)
	}
	advancedRetry := run(compacted)
	if event := <-advancedRetry; !event.Done || !event.Replayed || event.ToolCall != nil || event.Usage != nil || !errors.Is(event.Err, ErrToolTurnInProgress) {
		t.Fatalf("retry after next result was submitted = %+v", event)
	}
	if event, ok := <-advancedRetry; ok {
		t.Fatalf("unexpected event after replay conflict: %+v", event)
	}
	terminal := drainRun(t, run(result(thirdCall.ID, "last-result")))
	if terminal.Text != "agent-1:last-result" || terminal.Replayed || terminal.Usage == nil {
		t.Fatalf("original terminal = %+v", terminal)
	}
	stale := run(compacted)
	if retry := <-stale; !retry.Replayed || !retry.Done || !errors.Is(retry.Err, ErrToolTurnInProgress) || retry.Text != "" || retry.ToolCall != nil || retry.Usage != nil {
		t.Fatalf("stale replay after Run completion = %+v", retry)
	}
	if event, ok := <-stale; ok {
		t.Fatalf("unexpected event after stale replay conflict: %+v", event)
	}
	if retry := drainRun(t, run(result(thirdCall.ID, "last-result"))); !retry.Replayed || retry.Text != terminal.Text || retry.Usage != nil {
		t.Fatalf("replay of completed turn = %+v", retry)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.nextID.Load() != 1 || len(handler.sent) != 1 {
		t.Fatalf("created agents = %d, Send calls = %d, want 1 of each", handler.nextID.Load(), len(handler.sent))
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
		{"empty tools", `{"tools":[],"messages":[{"role":"tool","tool_call_id":%q,"content":"first-result"}]}`, true, false},
		{"empty functions", `{"functions":[],"messages":[{"role":"tool","tool_call_id":%q,"content":"first-result"}]}`, true, false},
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
			var thinking strings.Builder
			for event := range events {
				if event.ToolCall != nil {
					t.Fatalf("disabled tool call reached the client: %+v", event.ToolCall)
				}
				thinking.WriteString(event.ThinkingDelta)
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
			var replayThinking strings.Builder
			var replayTerminal Event
			for event := range replay {
				if !event.Replayed || event.ToolCall != nil {
					t.Fatalf("unexpected replay event: %+v", event)
				}
				replayThinking.WriteString(event.ThinkingDelta)
				if event.Done {
					replayTerminal = event
				}
			}
			if !replayTerminal.Done || replayTerminal.Err != terminal.Err || replayThinking.String() != thinking.String() {
				t.Fatalf("replay thinking=%q terminal=%+v, want thinking=%q and error %v", replayThinking.String(), replayTerminal, thinking.String(), terminal.Err)
			}
		})
	}
}

/**
 * @brief 验证具名选择与工具列表收紧原 Run 权限，并约束后续回调和重放。
 * @param t 测试上下文。
 */
func TestSDKRunnerNarrowsNativeToolPolicy(t *testing.T) {
	for _, restriction := range []struct {
		name string
		json string
	}{
		{"subset", `"tools":[{"type":"function","function":{"name":"none","parameters":{"type":"object"}}}]`},
		{"OpenAI named", `"tool_choice":{"type":"function","function":{"name":"none"}}`},
		{"Anthropic named", `"tool_choice":{"type":"tool","name":"none"}`},
		{"legacy named", `"function_call":{"name":"none"}`},
		{"Chat allowed_tools", `"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[{"type":"function","function":{"name":"none"}}]}}`},
		{"Responses allowed_tools", `"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"none"}]}`},
	} {
		t.Run(restriction.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			runner, handler := newNativeToolTestRunner(t)
			handler.toolNames = []string{"lookup", "none", "lookup"}
			credential := &Credential{APIKey: "channel-key"}
			run := func(body string) <-chan Event {
				t.Helper()
				req := ParseRequest([]byte(body))
				req.SessionScope = testCursorSessionScope
				events, err := runner.Run(ctx, credential, req)
				if err != nil {
					t.Fatal(err)
				}
				return events
			}
			const declarations = `"tools":[{"name":"lookup","input_schema":{"type":"object"}},{"name":"none","input_schema":{"type":"object"}}]`
			first := readNativeToolCall(t, run(`{`+declarations+`,"messages":[{"role":"user","content":"lookup"}]}`))
			if first.Name != "lookup" {
				t.Fatalf("initial callback = %q, want lookup", first.Name)
			}
			queuedError := make(chan error, 1)
			callbackURL, callbackToken := handler.control.callback()
			callback := sdkv1connect.NewSdkCustomToolCallbackServiceClient(http.DefaultClient, callbackURL)
			request := connect.NewRequest(&sdkv1.CallCustomToolRequest{AgentId: "agent-1", ToolName: "lookup"})
			request.Header().Set("Authorization", "Bearer "+callbackToken)
			go func() {
				_, err := callback.CallCustomTool(ctx, request)
				queuedError <- err
			}()
			session := runner.sessionByAgentID("agent-1")
			for len(session.events) == 0 {
				select {
				case <-ctx.Done():
					t.Fatal("the forbidden callback did not reach the paused run")
				case <-time.After(time.Millisecond):
				}
			}

			resultMessage := fmt.Sprintf(`"messages":[{"role":"tool","tool_call_id":%q,"content":"first-result"}]`, first.ID)
			resumeBody := "{" + restriction.json + "," + resultMessage + "}"
			events := run(resumeBody)
			conflict := ParseRequest([]byte("{" + declarations + "," + resultMessage + "}"))
			conflict.SessionScope = testCursorSessionScope
			if duplicate, err := runner.Run(ctx, credential, conflict); duplicate != nil || !errors.Is(err, ErrToolTurnInProgress) {
				t.Fatalf("retry with an expanded policy returned events=%v, error=%v, want conflict", duplicate, err)
			}
			forbidden := ParseRequest(fmt.Appendf(nil, `{"tool_choice":"none",%s}`, resultMessage))
			forbidden.SessionScope = testCursorSessionScope
			if duplicate, err := runner.Run(ctx, credential, forbidden); duplicate != nil || !errors.Is(err, ErrToolTurnInProgress) {
				t.Fatalf("retry disabling the next callback returned events=%v, error=%v, want conflict", duplicate, err)
			}
			second := readNativeToolCall(t, events)
			if second.Name != "none" {
				t.Fatalf("resume callback = %+v, want the permitted tool", second)
			}
			if err := <-queuedError; connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("queued callback error = %v, want permission denied", err)
			}
			for _, body := range []string{resumeBody, "{" + resultMessage + "}"} {
				if replay := readNativeToolCall(t, run(body)); !reflect.DeepEqual(replay, second) {
					t.Fatalf("replayed callback = %+v, want %+v", replay, second)
				}
			}

			var terminal Event
			for event := range run(fmt.Sprintf(`{"messages":[{"role":"tool","tool_call_id":%q,"content":"second-result"}]}`, second.ID)) {
				if event.ToolCall != nil {
					t.Fatalf("omitting tools reopened a forbidden callback: %+v", event)
				}
				if event.Done {
					terminal = event
				}
			}
			if !terminal.Done || connect.CodeOf(terminal.Err) != connect.CodePermissionDenied {
				t.Fatalf("last turn = %+v, want the forbidden callback denied", terminal)
			}
			handler.mu.Lock()
			defer handler.mu.Unlock()
			if got := handler.results["agent-1"]["value"]; got != "second-result" || handler.nextID.Load() != 1 {
				t.Fatalf("original run received %v across %d agents", got, handler.nextID.Load())
			}
		})
	}
}

/**
 * @brief 校验新名称或不同定义不能替换原 Run 的工具，且原结果仍可正常提交。
 * @param t 测试上下文。
 */
func TestSDKRunnerRejectsNativeToolPolicyChangesBeforeResolving(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	runner, handler := newNativeToolTestRunner(t)
	credential := &Credential{APIKey: "channel-key"}
	start, err := runner.Run(ctx, credential, Request{
		Prompt: "lookup", SessionScope: testCursorSessionScope,
		Tools: []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := readNativeToolCall(t, start)
	for _, policy := range []string{
		`"tools":[{"name":"newTool","input_schema":{"type":"object"}}]`,
		`"tools":[{"name":"lookup","input_schema":{"type":"object","required":["changed"]}}]`,
		`"tool_choice":{"type":"function","function":{"name":"newTool"}}`,
		`"tool_choice":{"type":"function","function":{}}`,
		`"tool_choice":{"type":"web_search"}`,
		`"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"web_search"}]}`,
		`"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"lookup","namespace":"remote"}]}`,
	} {
		changed := ParseRequest(fmt.Appendf(nil, `{%s,"messages":[{"role":"tool","tool_call_id":%q,"content":"rejected"}]}`, policy, call.ID))
		changed.SessionScope = testCursorSessionScope
		if events, err := runner.Run(ctx, credential, changed); events != nil || !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("changed native policy %s returned events=%v, error=%v, want invalid request", policy, events, err)
		}
	}
	if got := resumeNativeToolCall(t, runner, credential, call.ID, "accepted"); got != "agent-1:accepted" || handler.nextID.Load() != 1 {
		t.Fatalf("retry after policy errors returned %q across %d agents", got, handler.nextID.Load())
	}
}

/**
 * @brief 混合续轮继承原 Run 收紧后的权限，启动重试仍沿用同一权限。
 * @param t 测试上下文。
 */
func TestSDKRunnerMixedTurnInheritsNarrowedToolPolicy(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	runner, handler := newNativeToolTestRunner(t)
	handler.toolNames = []string{"lookup", "keep"}
	credential := &Credential{APIKey: "channel-key"}
	run := func(body []byte) (<-chan Event, error) {
		request := ParseRequest(body)
		request.SessionScope = testCursorSessionScope
		return runner.Run(ctx, credential, request)
	}
	initial, err := run([]byte(`{"messages":[{"role":"user","content":"lookup"}],
		"tools":[{"name":"lookup","input_schema":{"type":"object"}},{"name":"keep","input_schema":{"type":"object"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	first := readNativeToolCall(t, initial)
	narrowed, err := run(fmt.Appendf(nil, `{"tool_choice":{"type":"tool","name":"keep"},
		"messages":[{"role":"tool","tool_call_id":%q,"content":"first"}]}`, first.ID))
	if err != nil {
		t.Fatal(err)
	}
	second := readNativeToolCall(t, narrowed)
	original := runner.sessionByAgentID("agent-1")
	const mixed = `"messages":[{"role":"tool","tool_call_id":%q,"content":"second"},{"role":"user","content":"follow-up"}]`
	malformed := fmt.Appendf(nil, `{"tool_choice":{"type":"tool","name":"lookup"},`+mixed+`}`, second.ID)
	if events, err := run(malformed); events != nil || !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("mixed request selecting a withdrawn tool returned events=%v, error=%v", events, err)
	}

	handler.mu.Lock()
	handler.createErr = connect.NewError(connect.CodeUnavailable, errors.New("temporary create failure"))
	handler.mu.Unlock()
	valid := fmt.Appendf(nil, `{`+mixed+`}`, second.ID)
	if events, err := run(valid); events != nil || err == nil {
		t.Fatalf("failed replacement returned events=%v, error=%v", events, err)
	}

	events, err := run(valid)
	if err != nil {
		t.Fatal(err)
	}
	var terminal Event
	for event := range events {
		if event.ToolCall != nil {
			t.Fatalf("mixed replacement exposed a withdrawn tool: %+v", event)
		}
		if event.Done {
			terminal = event
		}
	}
	if !terminal.Done || connect.CodeOf(terminal.Err) != connect.CodePermissionDenied {
		t.Fatalf("mixed replacement = %+v, want lookup denied", terminal)
	}
	select {
	case <-original.done:
	case <-ctx.Done():
		t.Fatal("original run did not finish")
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	registered := handler.creates["agent-2"].GetOptions().GetLocal().GetCustomTools()
	if len(registered) != 1 || registered["keep"] == nil || handler.results["agent-1"]["value"] != "second" {
		t.Fatalf("mixed replacement registered %v, original results = %v", slices.Sorted(maps.Keys(registered)), handler.results)
	}
}

/**
 * @brief 混合轮只交付一次原生结果，保留新的用户内容并重放待处理调用和最终结果。
 * @param t 测试上下文。
 */
func TestSDKRunnerPreservesUserInputWithToolResults(t *testing.T) {
	scope := testCursorSessionScope
	scope.SessionID, scope.ThreadID = "session", "thread"
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
			const toolTurnTokens = 12
			handler.turnUsage = map[string]any{"inputTokens": 10, "outputTokens": 2, "totalTokens": toolTurnTokens}
			handler.toolTurnText = "Checking lookup. "
			finishReplacement := make(chan struct{})
			handler.whileToolPending = func(ctx context.Context, _ *connect.ServerStream[sdkv1.RunStreamMessage], agentID, _ string) (bool, error) {
				if agentID != "agent-2" {
					return false, nil
				}
				select {
				case <-finishReplacement:
					return false, nil
				case <-ctx.Done():
					return false, ctx.Err()
				}
			}
			key := &Credential{APIKey: "channel-key"}
			first, err := runner.Run(ctx, key, Request{
				Model: "model-1", Prompt: "lookup", SessionScope: scope,
				Tools: []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
			})
			if err != nil {
				t.Fatal(err)
			}
			call := readNativeToolCall(t, first)
			request := ParseRequest(fmt.Appendf(nil, tc.body, call.ID))
			request.SessionScope = scope
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
			wantTerminalTokens := 150
			var terminal Event
			if tc.wantTools {
				readPending := func(output <-chan Event, replayed bool, wantTokens int) ToolCall {
					t.Helper()
					var toolCall ToolCall
					var text, thinking string
					var tokens int
					for event := range output {
						if event.Err != nil || event.Done || event.Replayed != replayed ||
							(replayed && (event.Usage != nil || event.SentEffort != nil)) {
							t.Fatalf("unexpected pending tool event: %+v", event)
						}
						if event.ToolCall != nil {
							if toolCall.ID != "" {
								t.Fatalf("duplicate tool call: %+v", event.ToolCall)
							}
							toolCall = *event.ToolCall
						}
						text += event.Delta
						thinking += event.ThinkingDelta
						if event.Usage != nil {
							tokens += event.Usage.TotalTokens
						}
					}
					if toolCall.ID == "" || tokens != wantTokens || text != handler.toolTurnText || thinking != "Check 1." {
						t.Fatalf("pending tool call = %+v, tokens = %d, text = %q, thinking = %q", toolCall, tokens, text, thinking)
					}
					return toolCall
				}
				followUp := readPending(events, false, toolTurnTokens)
				if followUp.ID == call.ID {
					t.Fatal("replacement Run reused a previous tool call")
				}
				duplicate, err := runner.Run(ctx, key, request)
				if err != nil {
					t.Fatalf("retry while waiting for tool result: %v", err)
				}
				if got := readPending(duplicate, true, 0); !reflect.DeepEqual(got, followUp) {
					t.Fatalf("pending tool replay = %+v, want %+v", got, followUp)
				}
				changed := request
				changed.ToolChoice = "none"
				if duplicate, err := runner.Run(ctx, key, changed); duplicate != nil || !errors.Is(err, ErrToolTurnInProgress) {
					t.Fatalf("changed continuation = %v, %v", duplicate, err)
				}
				continued, err := runner.Run(ctx, key, Request{
					ToolResults:  []ToolResult{{CallID: followUp.ID, Output: "replacement result"}},
					SessionScope: scope,
				})
				if err != nil {
					t.Fatal(err)
				}
				if duplicate, err := runner.Run(ctx, key, request); duplicate != nil || !errors.Is(err, ErrToolTurnInProgress) {
					t.Fatalf("retry after result delivery = %v, %v", duplicate, err)
				}
				close(finishReplacement)
				terminal = drainRun(t, continued)
				want = "agent-2:replacement result"
				wantTerminalTokens -= toolTurnTokens
			} else {
				terminal = drainRun(t, events)
			}
			if terminal.Text != want || terminal.Replayed || terminal.Usage == nil || terminal.Usage.TotalTokens != wantTerminalTokens {
				t.Fatalf("replacement terminal = %+v, want %q", terminal, want)
			}
			replay, err := runner.Run(ctx, key, request)
			if tc.wantTools {
				if replay != nil || !errors.Is(err, ErrToolTurnInProgress) {
					t.Fatalf("stale mixed request replay = %v, %v", replay, err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if terminal := drainRun(t, replay); !terminal.Replayed || terminal.Text != want || terminal.Usage != nil {
				t.Fatalf("mixed request replay = %+v", terminal)
			}
			for deleted := ""; deleted != "agent-1"; {
				select {
				case deleted = <-handler.deleted:
				case <-ctx.Done():
					t.Fatal("original Agent was not deleted")
				}
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
 * @brief 原生工具续轮的模型查询要求客户端会话、线程及 Claude Code 代理身份完全匹配。
 * @param t 测试上下文。
 */
func TestSDKRunnerToolContinuationModelMatchesClientIdentity(t *testing.T) {
	const model = "model-1"
	credential := &Credential{APIKey: "channel-key"}
	scope := testCursorSessionScope
	scope.SessionID, scope.ThreadID = "session-a", "thread-a"
	scope.ClaudeSessionID, scope.ClaudeAgentID = "claude-session-a", "claude-agent-a"
	session := &sdkSession{scope: scope, apiKey: credential.APIKey, model: model}
	runner := &SDKRunner{
		calls:          map[string]*pendingToolCall{"pending": {session: session}},
		completedCalls: map[string]*sdkSession{"completed": session},
	}
	for _, tc := range []struct {
		name, sessionID, threadID, claudeSessionID, claudeAgentID, wantModel string
	}{
		{"matching identity", "session-a", "thread-a", "claude-session-a", "claude-agent-a", model},
		{"different session", "session-b", "thread-a", "claude-session-a", "claude-agent-a", ""},
		{"different thread", "session-a", "thread-b", "claude-session-a", "claude-agent-a", ""},
		{"different Claude session", "session-a", "thread-a", "claude-session-b", "claude-agent-a", ""},
		{"different Claude agent", "session-a", "thread-a", "claude-session-a", "claude-agent-b", ""},
		{"Claude main agent", "session-a", "thread-a", "claude-session-a", "", ""},
		{"missing session", "", "thread-a", "claude-session-a", "claude-agent-a", ""},
		{"missing thread", "session-a", "", "claude-session-a", "claude-agent-a", ""},
		{"missing client identity", "", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := scope
			candidate.SessionID, candidate.ThreadID = tc.sessionID, tc.threadID
			candidate.ClaudeSessionID, candidate.ClaudeAgentID = tc.claudeSessionID, tc.claudeAgentID
			for _, callID := range []string{"pending", "completed"} {
				if got := runner.ToolContinuationModel(candidate, credential, []ToolResult{{CallID: callID}}); got != tc.wantModel {
					t.Fatalf("%s call model = %q, want %q", callID, got, tc.wantModel)
				}
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
 * @brief 相同历史的普通续轮按客户端会话、线程或 Claude Code 主/子代理分别复用 Agent。
 * @param t 测试上下文。
 */
func TestSDKRunnerSeparatesParkedAgentSessionsAndThreads(t *testing.T) {
	runner, handler := newNativeToolTestRunner(t)
	var sentAgentIDs []string
	handler.finishTurn = func(stream *connect.ServerStream[sdkv1.RunStreamMessage], agentID, runID string) error {
		handler.mu.Lock()
		sentAgentIDs = append(sentAgentIDs, agentID)
		handler.mu.Unlock()
		if err := stream.Send(runResult(agentID, runID, sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, "same answer")); err != nil {
			return err
		}
		return stream.Send(runDone(agentID, runID))
	}
	credential := &Credential{APIKey: "key"}
	start := ParseRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"same question"}]}`))
	scopes := []SessionScope{
		{SessionID: "session-a", ThreadID: "thread-a"},
		{SessionID: "session-a", ThreadID: "thread-b"},
		{SessionID: "session-b", ThreadID: "thread-a"},
		{ClaudeSessionID: "claude-session-a"},
		{ClaudeSessionID: "claude-session-a", ClaudeAgentID: "claude-agent-a"},
		{ClaudeSessionID: "claude-session-a", ClaudeAgentID: "claude-agent-b"},
		{ClaudeSessionID: "claude-session-b"},
	}
	for index := range scopes {
		scopes[index].ChannelID, scopes[index].TokenID = testCursorSessionScope.ChannelID, testCursorSessionScope.TokenID
		start.SessionScope = scopes[index]
		drainRun(t, mustRun(t, runner, credential, start))
		waitParkedAgents(t, runner, index+1)
	}
	followUp := ParseRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"same question"},
		{"role":"assistant","content":"same answer"},{"role":"user","content":"follow-up"}]}`))
	for _, scope := range scopes {
		followUp.SessionScope = scope
		drainRun(t, mustRun(t, runner, credential, followUp))
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	count := len(scopes)
	if got := handler.nextID.Load(); int(got) != count || len(sentAgentIDs) != 2*count || !slices.Equal(sentAgentIDs[:count], sentAgentIDs[count:]) {
		t.Fatalf("created %d agents, sends targeted %q, want %d distinct agents followed by the same agents", got, sentAgentIDs, count)
	}
	for _, message := range handler.sent[count:] {
		if message != "user: follow-up" {
			t.Fatalf("follow-up Send = %q, want only the new message", message)
		}
	}
}

/**
 * @brief 校验重复指令继续原 Run，变更指令先完成回调再发送到新 Run。
 * @param t 测试上下文。
 */
func TestSDKRunnerAppliesChangedToolTurnInstructions(t *testing.T) {
	for _, tt := range []struct {
		name          string
		resume        string
		newDirective  string
		wantSameAgent bool
	}{
		{"repeated system", `{"role":"system","content":"original"},%s`, "", true},
		{"omitted system", `%s`, "", true},
		{"changed system", `{"role":"system","content":"updated system"},%s`, "system: updated system", false},
		{"developer after result", `{"role":"system","content":"original"},%s,{"role":"developer","content":"updated developer"}`, "developer: updated developer", false},
		{"cleared system", `{"role":"system","content":""},%s`, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner, handler := newNativeToolTestRunner(t)
			handler.deleted = make(chan string, 2)
			credential := &Credential{APIKey: "key"}
			start := ParseRequest([]byte(`{"model":"model-1","messages":[{"role":"system","content":"original"},{"role":"user","content":"lookup"}],
				"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`))
			start.SessionScope = testCursorSessionScope
			call := readNativeToolCall(t, mustRun(t, runner, credential, start))
			output := fmt.Sprintf(`{"role":"tool","tool_call_id":%q,"content":"result"}`, call.ID)
			resume := ParseRequest([]byte(`{"tool_choice":"none","messages":[` + fmt.Sprintf(tt.resume, output) + `]}`))
			resume.SessionScope = start.SessionScope
			terminal := drainRun(t, mustRun(t, runner, credential, resume))
			want := "agent-1:result"
			if !tt.wantSameAgent {
				want = "agent-2:<nil>"
				timeout := time.After(5 * time.Second)
				for deleted := ""; deleted != "agent-1"; {
					select {
					case deleted = <-handler.deleted:
					case <-timeout:
						t.Fatal("original Agent did not finish after callback delivery")
					}
				}
			}
			if terminal.Text != want {
				t.Fatalf("got %q, want %q", terminal.Text, want)
			}
			handler.mu.Lock()
			defer handler.mu.Unlock()
			if handler.results["agent-1"]["value"] != "result" {
				t.Fatalf("original callbacks = %v", handler.results)
			}
			if !tt.wantSameAgent && (len(handler.sent) != 2 || !strings.Contains(handler.sent[1], tt.newDirective) ||
				(tt.name == "cleared system" && strings.Contains(handler.sent[1], "system: original"))) {
				t.Fatalf("new Run Send = %q", handler.sent)
			}
		})
	}
}

/**
 * @brief 检查并行工具结果须整批提交，显式串行时后续调用逐轮交付。
 * @param t 测试上下文。
 */
func TestSDKRunnerHandlesConcurrentToolCallbacks(t *testing.T) {
	for _, tt := range []struct {
		name   string
		option string
		serial bool
	}{
		{"parallel batch", "", false},
		{"serial OpenAI", `,"parallel_tool_calls":false`, true},
		{"serial Anthropic", `,"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner, handler := newNativeToolTestRunner(t)
			credential := &Credential{APIKey: "key"}
			secondResult := make(chan any, 1)
			handler.whileToolPending = func(ctx context.Context, _ *connect.ServerStream[sdkv1.RunStreamMessage], agentID, _ string) (bool, error) {
				url, token := handler.control.callback()
				callback := sdkv1connect.NewSdkCustomToolCallbackServiceClient(http.DefaultClient, url)
				request := connect.NewRequest(&sdkv1.CallCustomToolRequest{AgentId: agentID, ToolName: "other"})
				request.Header().Set("Authorization", "Bearer "+token)
				response, err := callback.CallCustomTool(ctx, request)
				if err == nil {
					secondResult <- response.Msg.GetResult().AsMap()["value"]
				}
				return false, err
			}
			start := ParseRequest([]byte(`{"model":"model-1","messages":[{"role":"user","content":"lookup"}],
				"tools":[{"name":"lookup","input_schema":{"type":"object"}},{"name":"other","input_schema":{"type":"object"}}]` + tt.option + `}`))
			start.SessionScope = testCursorSessionScope
			toolResults := func(events <-chan Event) []ToolResult {
				t.Helper()
				var results []ToolResult
				for event := range events {
					if event.Done {
						t.Fatalf("wanted tool calls, got terminal %+v", event)
					}
					if event.ToolCall != nil {
						results = append(results, ToolResult{CallID: event.ToolCall.ID, Output: event.ToolCall.Name + " result"})
					}
				}
				return results
			}
			first := toolResults(mustRun(t, runner, credential, start))
			resume := Request{SessionScope: start.SessionScope}
			if tt.serial {
				if len(first) != 1 {
					t.Fatalf("first serial turn calls = %+v", first)
				}
				resume.ToolResults = first
				second := toolResults(mustRun(t, runner, credential, resume))
				if len(second) != 1 || second[0].CallID == first[0].CallID {
					t.Fatalf("second serial turn calls = %+v, first = %+v", second, first)
				}
				resume.ToolResults = second
			} else {
				if len(first) != 2 {
					t.Fatalf("parallel turn calls = %+v", first)
				}
				resume.ToolResults = []ToolResult{{CallID: first[0].CallID, Output: "partial result must not be committed"}}
				if events, err := runner.Run(t.Context(), credential, resume); events != nil || !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("partial tool submission = %v, %v", events, err)
				}
				resume.ToolResults = first
			}
			if terminal := drainRun(t, mustRun(t, runner, credential, resume)); terminal.Text != "agent-1:lookup result" {
				t.Fatalf("terminal = %+v", terminal)
			}
			if got := <-secondResult; got != "other result" {
				t.Fatalf("other callback result = %v", got)
			}
		})
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
 * @brief 验证客户端内联工具图片按顺序送达原 Run，远程图片失败后原调用仍可续接。
 * @param t 测试上下文。
 */
func TestSDKRunnerNativeToolResultImages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, result string
		isError      bool
		image        map[string]any
	}{
		{"messages", `{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"is_error":true,"content":[
			{"type":"text","text":"before"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}},
			{"type":"text","text":"after"}]}]}`, true,
			map[string]any{"type": "image", "data": "aGk=", "mimeType": "image/png"}},
		{"chat", `{"role":"tool","tool_call_id":%q,"content":[
			{"type":"text","text":"before"},
			{"type":"image_url","image_url":{"url":"data:;base64,aGk"}},
			{"type":"text","text":"after"}]}`, false,
			map[string]any{"type": "image", "data": "aGk"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner, handler := newNativeToolTestRunner(t)
			credential := &Credential{APIKey: "channel-key"}
			call := readNativeToolCall(t, mustRun(t, runner, credential, Request{
				Model: "model-1", Prompt: "lookup", SessionScope: testCursorSessionScope,
				Tools: []Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}},
			}))
			if tc.name == "chat" {
				for _, invalid := range []struct {
					url       string
					suffix    string
					wantError error
					message   string
				}{
					{"https://example.com/result.png", "", ErrInvalidRequest, ErrURLImageUnsupported.Error()},
					{"https://example.com/result.png", `,{"role":"user","content":"new turn"}`, ErrURLImageUnsupported, ErrURLImageUnsupported.Error()},
					{"data:image/png;base64,aG?=", "", ErrInvalidRequest, "tool result image must contain valid base64 data"},
					{"data:image/png;base64,", "", ErrInvalidRequest, "tool result image must contain valid base64 data"},
				} {
					request := ParseRequest(fmt.Appendf(nil, `{"messages":[{"role":"tool","tool_call_id":%q,"content":[
						{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}},
						{"type":"image_url","image_url":{"url":%q}}]}%s]}`, call.ID, invalid.url, invalid.suffix))
					request.SessionScope = testCursorSessionScope
					events, err := runner.Run(t.Context(), credential, request)
					if events != nil || !errors.Is(err, invalid.wantError) || !strings.Contains(err.Error(), invalid.message) {
						t.Fatalf("Run() events=%v err=%v, want %v containing %q", events, err, invalid.wantError, invalid.message)
					}
				}
			}
			request := ParseRequest(fmt.Appendf(nil, `{"messages":[%s]}`, fmt.Sprintf(tc.result, call.ID)))
			request.SessionScope = testCursorSessionScope
			var done bool
			for event := range mustRun(t, runner, credential, request) {
				if event.Err != nil || event.Replayed {
					t.Fatalf("native tool result event=%+v", event)
				}
				done = done || event.Done
			}
			if !done {
				t.Fatal("native tool turn did not finish")
			}
			want := map[string]any{"isError": tc.isError, "content": []any{
				map[string]any{"type": "text", "text": "before"},
				tc.image,
				map[string]any{"type": "text", "text": "after"},
			}}
			handler.mu.Lock()
			defer handler.mu.Unlock()
			if got := handler.results["agent-1"]; !reflect.DeepEqual(got, want) || len(handler.creates) != 1 {
				t.Fatalf("callback result=%v, want %v, agents=%d", got, want, len(handler.creates))
			}
		})
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
	events, err := session.nextTurn(context.Background(), nil, false, nil)
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

/**
 * @brief 晚于工具回调到达的本步用量随发出工具调用的请求交付，而不是排给恢复请求。
 * @param t 测试上下文。
 */
func TestSDKSessionToolBatchCarriesLateStepUsage(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	session := &sdkSession{ctx: ctx, cancel: cancel, events: make(chan Event, 4), done: make(chan struct{})}
	events, err := session.nextTurn(t.Context(), nil, false, nil)
	if err != nil {
		t.Fatalf("nextTurn() error = %v", err)
	}
	if !session.emit(Event{ToolCall: &ToolCall{ID: "call-a", Name: "lookup", Arguments: []byte(`{}`)}}) {
		t.Fatal("emit tool call failed")
	}
	time.Sleep(5 * toolBatchWindow)
	want := Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}
	if !session.emit(Event{Usage: &want}) {
		t.Fatal("emit usage failed")
	}
	var got Usage
	calls := 0
	for event := range events {
		if event.ToolCall != nil {
			calls++
		}
		if event.Usage != nil {
			got.Add(event.Usage)
		}
	}
	if calls != 1 || got != want {
		t.Fatalf("tool turn calls = %d, usage = %+v, want %+v", calls, got, want)
	}
	session.abort(context.Canceled)
}

/**
 * @brief 同批结果有一项无法提交时，其余回调不被交付。
 * @param t 测试上下文。
 */
func TestResolveToolResultsIsAtomic(t *testing.T) {
	session := &sdkSession{}
	first := &pendingToolCall{session: session, result: make(chan toolCallbackResult, 1)}
	second := &pendingToolCall{session: &sdkSession{}, result: make(chan toolCallbackResult, 1)}
	runner := &SDKRunner{calls: map[string]*pendingToolCall{"call-a": first, "call-b": second}}
	err := runner.resolveToolResults(session, []ToolResult{
		{CallID: "call-a", Output: "a"}, {CallID: "call-b", Output: "b"},
	}, nil, "")
	if err == nil {
		t.Fatal("resolveToolResults() unexpectedly succeeded")
	}
	if runner.calls["call-a"] != first || len(first.result) != 0 {
		t.Fatalf("first result was partially committed: queued=%d", len(first.result))
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
	events, err := session.nextTurn(context.Background(), nil, false, nil)
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
			events, err := session.nextTurn(t.Context(), nil, false, nil)
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
 * @brief 工具暂停后到达的用量与终态补差由恢复请求领用一次，重放不重复计费。
 * @param t 测试上下文。
 */
func TestSDKRunnerResumeCarriesQueuedToolUsage(t *testing.T) {
	runner, handler := newNativeToolTestRunner(t)
	handler.turnUsage = map[string]any{"inputTokens": 10, "outputTokens": 2, "totalTokens": 12}
	handler.usage["run-agent-1"] = &sdkv1.TokenUsage{InputTokens: 17, OutputTokens: 7, CacheReadTokens: 2, TotalTokens: 26}
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
		for i := 0; err == nil && i < 80; i++ {
			err = stream.Send(nativeSDKMessage("status", map[string]any{"status": "RUNNING"}))
		}
		return false, err
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credential := &Credential{APIKey: "channel-key"}
	firstCtx, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	events, err := runner.Run(firstCtx, credential, Request{
		Prompt: "lookup", Tools: []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
		SessionScope: testCursorSessionScope,
	})
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
	// Fill the queue so the resume must drain queued usage before the terminal.
	for len(session.events) < cap(session.events) {
		select {
		case <-ctx.Done():
			t.Fatal("the late usage did not reach the SDK")
		case <-time.After(time.Millisecond):
		}
	}

	reply := Request{ToolResults: []ToolResult{{CallID: call.ID, Output: "ok"}}, SessionScope: testCursorSessionScope}
	resumed, err := runner.Run(ctx, credential, reply)
	if err != nil {
		t.Fatal(err)
	}
	var got Usage
	for event := range resumed {
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		if event.Usage != nil {
			got.Add(event.Usage)
		}
	}
	if want := (Usage{InputTokens: 7, OutputTokens: 5, CacheReadTokens: 2, TotalTokens: 14}); got != want {
		t.Fatalf("resumed usage = %+v, want %+v", got, want)
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
}
