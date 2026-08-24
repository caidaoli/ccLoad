package cursorauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/cursorauth/sdkgen/sdk/v1/sdkv1connect"
)

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

type nativeToolAgentHandler struct {
	sdkv1connect.UnimplementedSdkAgentServiceHandler
	control *nativeToolControlHandler
	nextID  atomic.Int32

	mu      sync.Mutex
	creates map[string]*sdkv1.CreateAgentRequest
	results map[string]map[string]any
}

func (h *nativeToolAgentHandler) CreateAgent(
	_ context.Context,
	request *connect.Request[sdkv1.CreateAgentRequest],
) (*connect.Response[sdkv1.CreateAgentResponse], error) {
	agentID := fmt.Sprintf("agent-%d", h.nextID.Add(1))
	h.mu.Lock()
	h.creates[agentID] = request.Msg
	h.mu.Unlock()
	return connect.NewResponse(&sdkv1.CreateAgentResponse{AgentId: agentID}), nil
}

func (h *nativeToolAgentHandler) Send(
	ctx context.Context,
	request *connect.Request[sdkv1.SendRequest],
	stream *connect.ServerStream[sdkv1.RunStreamMessage],
) error {
	agentID := request.Msg.GetAgentId()
	runID := "run-" + agentID
	if err := stream.Send(nativeSDKMessage("status", map[string]any{
		"agent_id": agentID, "run_id": runID,
	})); err != nil {
		return err
	}
	callbackURL, callbackToken := h.control.callback()
	callback := sdkv1connect.NewSdkCustomToolCallbackServiceClient(http.DefaultClient, callbackURL)
	// Deliberately reuse the bridge-scoped ID across Agents. ccLoad must expose
	// distinct downstream call IDs so concurrent sessions cannot collide.
	callID := "bridge-call"
	toolRequest := connect.NewRequest(&sdkv1.CallCustomToolRequest{
		AgentId: agentID, ToolName: "lookup", ToolCallId: &callID,
		Args: mustStruct(map[string]any{"session": agentID}),
	})
	toolRequest.Header().Set("Authorization", "Bearer "+callbackToken)
	response, err := callback.CallCustomTool(ctx, toolRequest)
	if err != nil {
		return err
	}
	result := response.Msg.GetResult().AsMap()
	h.mu.Lock()
	h.results[agentID] = result
	h.mu.Unlock()
	text := fmt.Sprintf("%s:%v", agentID, result["value"])
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

func (h *nativeToolAgentHandler) DeleteAgent(
	_ context.Context,
	_ *connect.Request[sdkv1.DeleteAgentRequest],
) (*connect.Response[sdkv1.DeleteAgentResponse], error) {
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
		results: make(map[string]map[string]any),
	}
	agentPath, agentHTTP := sdkv1connect.NewSdkAgentServiceHandler(agent)
	controlPath, controlHTTP := sdkv1connect.NewSdkBridgeControlServiceHandler(control)
	mux := http.NewServeMux()
	mux.Handle(agentPath, agentHTTP)
	mux.Handle(controlPath, controlHTTP)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newBridgeClient(server.URL, "bridge-token")
	client.workdir = t.TempDir()
	bridge := newBridge()
	bridge.state = bridgeRunning
	bridge.process = &bridgeProcess{client: client, exited: make(chan struct{})}
	runner := &SDKRunner{
		bridge: bridge, timeout: 3 * time.Second,
		sessions: make(map[string]*sdkSession), calls: make(map[string]*pendingToolCall),
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
			Model: "model-1", Prompt: prompt, ToolChoice: "auto",
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

func TestSDKSessionBatchesConcurrentNativeToolCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &sdkSession{
		ctx: ctx, cancel: cancel, events: make(chan Event, 4), done: make(chan struct{}),
	}
	events, err := session.nextTurn(context.Background(), nil)
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
	})
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &sdkSession{
		ctx: ctx, cancel: cancel, events: make(chan Event, 2), done: make(chan struct{}),
	}
	if !session.emit(Event{RawResponse: []byte(`{"offset":"1"}`)}) ||
		!session.emit(Event{Delta: "answer", Text: "answer"}) {
		t.Fatal("failed to fill session event buffer")
	}
	wantErr := context.DeadlineExceeded
	session.finish(Event{Text: "answer", Done: true, Err: wantErr, Usage: &Usage{TotalTokens: 7}})
	events, err := session.nextTurn(context.Background(), nil)
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
		ToolResults: []ToolResult{{CallID: callID, Output: output}},
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
