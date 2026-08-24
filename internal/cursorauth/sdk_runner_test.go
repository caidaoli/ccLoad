package cursorauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/cursorauth/sdkgen/sdk/v1/sdkv1connect"
)

type testAgentHandler struct {
	sdkv1connect.UnimplementedSdkAgentServiceHandler

	mu            sync.Mutex
	create        *sdkv1.CreateAgentRequest
	createErr     error
	createFn      func(context.Context) error
	send          *sdkv1.SendRequest
	getRun        *sdkv1.GetRunRequest
	runSnapshot   *sdkv1.RunSnapshot
	deleted       *sdkv1.DeleteAgentRequest
	cancelled     *sdkv1.CancelRunRequest
	sendFn        func(context.Context, *connect.ServerStream[sdkv1.RunStreamMessage]) error
	sendCalls     int
	cancelCalled  chan struct{}
	deleteStarted chan struct{}
	deleteRelease chan struct{}
	getRunStarted chan struct{}
	getRunRelease chan struct{}
}

type testCursorHandler struct {
	sdkv1connect.UnimplementedSdkCursorServiceHandler

	request *sdkv1.ListModelsRequest
}

type shutdownErrorControlClient struct {
	sdkv1connect.SdkBridgeControlServiceClient
	err error
}

type sendFailureAgentClient struct {
	sdkv1connect.SdkAgentServiceClient
	err           error
	deleteStarted chan struct{}
	deleteRelease chan struct{}
}

func (c *sendFailureAgentClient) Send(
	context.Context,
	*connect.Request[sdkv1.SendRequest],
) (*connect.ServerStreamForClient[sdkv1.RunStreamMessage], error) {
	return nil, c.err
}

func (c *sendFailureAgentClient) DeleteAgent(
	ctx context.Context,
	_ *connect.Request[sdkv1.DeleteAgentRequest],
) (*connect.Response[sdkv1.DeleteAgentResponse], error) {
	close(c.deleteStarted)
	select {
	case <-c.deleteRelease:
		return connect.NewResponse(&sdkv1.DeleteAgentResponse{}), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *shutdownErrorControlClient) Shutdown(
	_ context.Context,
	_ *connect.Request[sdkv1.ShutdownRequest],
) (*connect.Response[sdkv1.ShutdownResponse], error) {
	return nil, c.err
}

func (h *testCursorHandler) ListModels(
	_ context.Context,
	request *connect.Request[sdkv1.ListModelsRequest],
) (*connect.Response[sdkv1.ListModelsResponse], error) {
	h.request = request.Msg
	return connect.NewResponse(&sdkv1.ListModelsResponse{Items: []*sdkv1.SdkModel{
		{Id: "grok-4.6", Variants: []*sdkv1.ModelVariant{{DisplayName: "High"}}},
		{Id: "composer-2.5"},
		{Id: "grok-4.6"},
		{Id: "  "},
	}}), nil
}

func (h *testAgentHandler) CreateAgent(
	ctx context.Context,
	request *connect.Request[sdkv1.CreateAgentRequest],
) (*connect.Response[sdkv1.CreateAgentResponse], error) {
	h.mu.Lock()
	h.create = request.Msg
	createErr := h.createErr
	createFn := h.createFn
	h.mu.Unlock()
	if createFn != nil {
		if err := createFn(ctx); err != nil {
			return nil, err
		}
	}
	if createErr != nil {
		return nil, createErr
	}
	return connect.NewResponse(&sdkv1.CreateAgentResponse{AgentId: "agent-1"}), nil
}

func (h *testAgentHandler) Send(
	ctx context.Context,
	request *connect.Request[sdkv1.SendRequest],
	stream *connect.ServerStream[sdkv1.RunStreamMessage],
) error {
	h.mu.Lock()
	h.send = request.Msg
	h.sendCalls++
	h.mu.Unlock()
	return h.sendFn(ctx, stream)
}

type testCursorHandlerFunc struct {
	sdkv1connect.UnimplementedSdkCursorServiceHandler
	listModels func(*sdkv1.ListModelsRequest) (*sdkv1.ListModelsResponse, error)
}

func (h *testCursorHandlerFunc) ListModels(
	_ context.Context,
	request *connect.Request[sdkv1.ListModelsRequest],
) (*connect.Response[sdkv1.ListModelsResponse], error) {
	response, err := h.listModels(request.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}

func (h *testAgentHandler) CancelRun(
	_ context.Context,
	request *connect.Request[sdkv1.CancelRunRequest],
) (*connect.Response[sdkv1.CancelRunResponse], error) {
	h.mu.Lock()
	h.cancelled = request.Msg
	called := h.cancelCalled
	h.mu.Unlock()
	if called != nil {
		close(called)
	}
	return connect.NewResponse(&sdkv1.CancelRunResponse{}), nil
}

func (h *testAgentHandler) GetRun(
	ctx context.Context,
	request *connect.Request[sdkv1.GetRunRequest],
) (*connect.Response[sdkv1.GetRunResponse], error) {
	h.mu.Lock()
	h.getRun = request.Msg
	deleted := h.deleted != nil
	snapshot := h.runSnapshot
	started := h.getRunStarted
	release := h.getRunRelease
	h.mu.Unlock()
	if started != nil {
		close(started)
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if deleted {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("agent was deleted"))
	}
	return connect.NewResponse(&sdkv1.GetRunResponse{Run: snapshot}), nil
}

func (h *testAgentHandler) DeleteAgent(
	_ context.Context,
	request *connect.Request[sdkv1.DeleteAgentRequest],
) (*connect.Response[sdkv1.DeleteAgentResponse], error) {
	h.mu.Lock()
	h.deleted = request.Msg
	started := h.deleteStarted
	release := h.deleteRelease
	h.mu.Unlock()
	if started != nil {
		close(started)
	}
	if release != nil {
		<-release
	}
	return connect.NewResponse(&sdkv1.DeleteAgentResponse{}), nil
}

func newTestSDKRunner(t *testing.T, handler *testAgentHandler) *SDKRunner {
	t.Helper()
	_, httpHandler := sdkv1connect.NewSdkAgentServiceHandler(handler)
	server := httptest.NewServer(httpHandler)
	t.Cleanup(server.Close)
	client := newBridgeClient(server.URL, "bridge-token")
	client.workdir = t.TempDir()
	bridge := newBridge()
	bridge.state = bridgeRunning
	bridge.process = &bridgeProcess{client: client, exited: make(chan struct{})}
	return &SDKRunner{bridge: bridge, timeout: 3 * time.Second}
}

func TestSDKRunnerStartMakesBridgeReadyWithoutModelRequest(t *testing.T) {
	runner := newTestSDKRunner(t, &testAgentHandler{})
	if err := runner.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
}

func TestSDKRunnerCreateAgentLocalDeadlineNamesOperationAndProxyDiagnostic(t *testing.T) {
	for _, key := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
		"http_proxy", "https_proxy", "all_proxy",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("HTTP_PROXY", "http://user:secret@proxy.example:7890")
	handler := &testAgentHandler{
		createFn: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	runner := newTestSDKRunner(t, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := runner.Run(ctx, &Credential{APIKey: "key-1"}, Request{Model: "model-1", Prompt: "hello"})
	if err == nil || !strings.Contains(err.Error(), "CreateAgent exceeded its local deadline after") ||
		!strings.Contains(err.Error(), "operation limit=30s") ||
		!strings.Contains(err.Error(), "inherited HTTP_PROXY/HTTPS_PROXY/ALL_PROXY") ||
		!strings.Contains(err.Error(), "returned no detail beyond deadline_exceeded") {
		t.Fatalf("Run() error = %v", err)
	}
	if strings.Contains(err.Error(), "user:secret") || strings.Contains(err.Error(), "proxy.example") {
		t.Fatalf("Run() leaked proxy value: %v", err)
	}
}

func TestBridgeCrashRestartsOnceForConcurrentCallers(t *testing.T) {
	firstClient := &bridgeClient{}
	secondClient := &bridgeClient{}
	firstExited := make(chan struct{})
	secondExited := make(chan struct{})
	var spawnCount atomic.Int32

	bridge := newBridge()
	bridge.spawnProcess = func() (*bridgeProcess, error) {
		switch spawnCount.Add(1) {
		case 1:
			return &bridgeProcess{client: firstClient, exited: firstExited}, nil
		case 2:
			return &bridgeProcess{client: secondClient, exited: secondExited}, nil
		default:
			return nil, errors.New("unexpected extra bridge restart")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := bridge.client(ctx)
	if err != nil || client != firstClient {
		t.Fatalf("first client = (%p, %v), want (%p, nil)", client, err, firstClient)
	}
	close(firstExited)

	const callers = 16
	clients := make(chan *bridgeClient, callers)
	errorsCh := make(chan error, callers)
	var callersDone sync.WaitGroup
	callersDone.Add(callers)
	for range callers {
		go func() {
			defer callersDone.Done()
			got, clientErr := bridge.client(ctx)
			clients <- got
			errorsCh <- clientErr
		}()
	}
	callersDone.Wait()
	close(clients)
	close(errorsCh)
	for clientErr := range errorsCh {
		if clientErr != nil {
			t.Fatalf("replacement client error = %v", clientErr)
		}
	}
	for got := range clients {
		if got != secondClient {
			t.Fatalf("replacement client = %p, want %p", got, secondClient)
		}
	}
	if got := spawnCount.Load(); got != 2 {
		t.Fatalf("bridge starts = %d, want 2", got)
	}

	bridge.mu.Lock()
	bridge.state = bridgeClosing
	bridge.lifeStop()
	bridge.mu.Unlock()
	close(secondExited)
}

func TestBridgeAutomaticRestartRetriesStartFailuresUntilRecovery(t *testing.T) {
	firstClient := &bridgeClient{}
	recoveredClient := &bridgeClient{}
	firstExited := make(chan struct{})
	recoveredExited := make(chan struct{})
	var spawnCount atomic.Int32

	bridge := newBridge()
	bridge.spawnProcess = func() (*bridgeProcess, error) {
		switch spawnCount.Add(1) {
		case 1:
			return &bridgeProcess{client: firstClient, exited: firstExited}, nil
		case 2, 3:
			return nil, errors.New("restart probe failed")
		case 4:
			return &bridgeProcess{client: recoveredClient, exited: recoveredExited}, nil
		default:
			return nil, errors.New("unexpected extra bridge restart")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := bridge.client(ctx)
	if err != nil || client != firstClient {
		t.Fatalf("first client = (%p, %v), want (%p, nil)", client, err, firstClient)
	}
	close(firstExited)

	for spawnCount.Load() < 4 {
		select {
		case <-ctx.Done():
			t.Fatalf("automatic restart did not recover: starts=%d", spawnCount.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	client, err = bridge.client(ctx)
	if err != nil || client != recoveredClient {
		t.Fatalf("recovered client = (%p, %v), want (%p, nil)", client, err, recoveredClient)
	}
	if got := spawnCount.Load(); got != 4 {
		t.Fatalf("bridge starts = %d, want 4", got)
	}

	bridge.mu.Lock()
	bridge.state = bridgeClosing
	bridge.lifeStop()
	bridge.mu.Unlock()
	close(recoveredExited)
}

func TestBridgeCloseStopsPendingAutomaticRestarts(t *testing.T) {
	firstClient := &bridgeClient{}
	firstExited := make(chan struct{})
	var spawnCount atomic.Int32

	bridge := newBridge()
	bridge.spawnProcess = func() (*bridgeProcess, error) {
		if spawnCount.Add(1) == 1 {
			return &bridgeProcess{client: firstClient, exited: firstExited}, nil
		}
		return nil, errors.New("restart probe failed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := bridge.client(ctx)
	if err != nil || client != firstClient {
		t.Fatalf("first client = (%p, %v), want (%p, nil)", client, err, firstClient)
	}
	close(firstExited)
	for spawnCount.Load() < 2 {
		select {
		case <-ctx.Done():
			t.Fatal("automatic restart did not start")
		case <-time.After(time.Millisecond):
		}
	}
	if err := bridge.close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	startsAfterClose := spawnCount.Load()
	time.Sleep(2 * bridgeRestartBaseDelay)
	if got := spawnCount.Load(); got != startsAfterClose {
		t.Fatalf("bridge restarted after Close(): starts=%d, want %d", got, startsAfterClose)
	}
}

func TestBridgeCloseCancelsQueuedImmediateRestartBeforeSpawn(t *testing.T) {
	bridge := newBridge()
	var spawnCount atomic.Int32
	bridge.spawnProcess = func() (*bridgeProcess, error) {
		spawnCount.Add(1)
		return nil, errors.New("spawn must not run after Close")
	}

	bridge.mu.Lock()
	attempt := bridge.beginStartLocked(true, 0)
	bridge.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	closeErr := make(chan error, 1)
	go func() { closeErr <- bridge.close(ctx) }()
	<-bridge.lifeCtx.Done()
	bridge.startProcess(attempt)
	if err := <-closeErr; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := spawnCount.Load(); got != 0 {
		t.Fatalf("bridge starts after Close = %d, want 0", got)
	}
	if !errors.Is(attempt.err, ErrBridgeClosed) {
		t.Fatalf("start error = %v, want ErrBridgeClosed", attempt.err)
	}
}

func TestSDKRunnerCloseUsesProcessExitAsShutdownResult(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		waitErr error
		wantErr bool
	}{
		{name: "clean exit suppresses reset"},
		{name: "failed exit keeps reset", waitErr: errors.New("exit status 1"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			exited := make(chan struct{})
			close(exited)
			bridge := newBridge()
			bridge.state = bridgeRunning
			bridge.process = &bridgeProcess{
				client: &bridgeClient{control: &shutdownErrorControlClient{
					err: errors.New("unavailable: read tcp: connection reset by peer"),
				}},
				exited:  exited,
				waitErr: test.waitErr,
			}
			runner := &SDKRunner{bridge: bridge}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := runner.Close(ctx)
			if (err != nil) != test.wantErr {
				t.Fatalf("Close() error = %v, wantErr = %t", err, test.wantErr)
			}
		})
	}
}

func TestSDKRunnerListModelsUsesExactSDKIDsWithoutExpandingVariants(t *testing.T) {
	handler := &testCursorHandler{}
	_, httpHandler := sdkv1connect.NewSdkCursorServiceHandler(handler)
	server := httptest.NewServer(httpHandler)
	t.Cleanup(server.Close)
	client := newBridgeClient(server.URL, "bridge-token")
	bridge := newBridge()
	bridge.state = bridgeRunning
	bridge.process = &bridgeProcess{client: client, exited: make(chan struct{})}
	runner := &SDKRunner{bridge: bridge, timeout: 3 * time.Second}

	models, err := runner.ListModels(context.Background(), "user-api-key")
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	want := []string{"grok-4.6", "composer-2.5"}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("ListModels() = %#v, want %#v", models, want)
	}
	if got := handler.request.GetOptions().GetApiKey(); got != "user-api-key" {
		t.Fatalf("ListModels API key = %q", got)
	}
}

func TestSDKRunnerListModelsRemoteDeadlineDoesNotClaimLocalLimit(t *testing.T) {
	for _, key := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
		"http_proxy", "https_proxy", "all_proxy",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("HTTPS_PROXY", "http://proxy.example:7890")
	handler := &testCursorHandlerFunc{listModels: func(*sdkv1.ListModelsRequest) (*sdkv1.ListModelsResponse, error) {
		return nil, connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded)
	}}
	_, httpHandler := sdkv1connect.NewSdkCursorServiceHandler(handler)
	server := httptest.NewServer(httpHandler)
	t.Cleanup(server.Close)
	client := newBridgeClient(server.URL, "bridge-token")
	bridge := newBridge()
	bridge.state = bridgeRunning
	bridge.process = &bridgeProcess{client: client, exited: make(chan struct{})}
	runner := &SDKRunner{bridge: bridge, timeout: 3 * time.Second}

	_, err := runner.ListModels(context.Background(), "user-api-key")
	if err == nil || !strings.Contains(err.Error(), "ListModels returned deadline_exceeded after") ||
		!strings.Contains(err.Error(), "inherited HTTP_PROXY/HTTPS_PROXY/ALL_PROXY") ||
		!strings.Contains(err.Error(), "returned no detail beyond deadline_exceeded") ||
		strings.Contains(err.Error(), "operation limit=") {
		t.Fatalf("ListModels() error = %v", err)
	}
}

func TestSDKRunnerListModelsPreservesRemoteDeadlineDiagnostic(t *testing.T) {
	handler := &testCursorHandlerFunc{listModels: func(*sdkv1.ListModelsRequest) (*sdkv1.ListModelsResponse, error) {
		return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("proxy CONNECT failed"))
	}}
	_, httpHandler := sdkv1connect.NewSdkCursorServiceHandler(handler)
	server := httptest.NewServer(httpHandler)
	t.Cleanup(server.Close)
	client := newBridgeClient(server.URL, "bridge-token")
	bridge := newBridge()
	bridge.state = bridgeRunning
	bridge.process = &bridgeProcess{client: client, exited: make(chan struct{})}
	runner := &SDKRunner{bridge: bridge, timeout: 3 * time.Second}

	_, err := runner.ListModels(context.Background(), "user-api-key")
	if err == nil || !strings.Contains(err.Error(), "proxy CONNECT failed") ||
		strings.Contains(err.Error(), "returned no detail beyond deadline_exceeded") {
		t.Fatalf("ListModels() error = %v", err)
	}
}

func TestSDKRunnerListModelsRetriesOnceAfterBridgeCrash(t *testing.T) {
	firstExited := make(chan struct{})
	secondExited := make(chan struct{})
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32

	firstHandler := &testCursorHandlerFunc{listModels: func(request *sdkv1.ListModelsRequest) (*sdkv1.ListModelsResponse, error) {
		firstCalls.Add(1)
		if got := request.GetOptions().GetApiKey(); got != "account-a" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("wrong API key"))
		}
		go func() {
			time.Sleep(25 * time.Millisecond)
			close(firstExited)
		}()
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("bridge crashed"))
	}}
	_, firstHTTPHandler := sdkv1connect.NewSdkCursorServiceHandler(firstHandler)
	firstServer := httptest.NewServer(firstHTTPHandler)
	defer firstServer.Close()

	secondHandler := &testCursorHandlerFunc{listModels: func(request *sdkv1.ListModelsRequest) (*sdkv1.ListModelsResponse, error) {
		secondCalls.Add(1)
		if got := request.GetOptions().GetApiKey(); got != "account-a" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("wrong API key"))
		}
		return &sdkv1.ListModelsResponse{Items: []*sdkv1.SdkModel{{Id: "model-a"}}}, nil
	}}
	_, secondHTTPHandler := sdkv1connect.NewSdkCursorServiceHandler(secondHandler)
	secondServer := httptest.NewServer(secondHTTPHandler)
	defer secondServer.Close()

	firstClient := newBridgeClient(firstServer.URL, "bridge-token-1")
	secondClient := newBridgeClient(secondServer.URL, "bridge-token-2")
	var spawnCount atomic.Int32
	bridge := newBridge()
	bridge.spawnProcess = func() (*bridgeProcess, error) {
		switch spawnCount.Add(1) {
		case 1:
			return &bridgeProcess{client: firstClient, exited: firstExited}, nil
		case 2:
			return &bridgeProcess{client: secondClient, exited: secondExited}, nil
		default:
			return nil, errors.New("unexpected extra bridge restart")
		}
	}
	runner := &SDKRunner{bridge: bridge, timeout: 3 * time.Second}

	models, err := runner.ListModels(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if !reflect.DeepEqual(models, []string{"model-a"}) {
		t.Fatalf("ListModels() = %#v, want [model-a]", models)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 || spawnCount.Load() != 2 {
		t.Fatalf(
			"calls = (first:%d second:%d starts:%d), want (1, 1, 2)",
			firstCalls.Load(), secondCalls.Load(), spawnCount.Load(),
		)
	}

	bridge.mu.Lock()
	bridge.state = bridgeClosing
	bridge.lifeStop()
	bridge.mu.Unlock()
	close(secondExited)
}

func TestSDKRunnerListModelsKeepsConcurrentChannelAPIKeysIsolated(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]int)
	handler := &testCursorHandlerFunc{listModels: func(request *sdkv1.ListModelsRequest) (*sdkv1.ListModelsResponse, error) {
		apiKey := request.GetOptions().GetApiKey()
		mu.Lock()
		seen[apiKey]++
		mu.Unlock()
		return &sdkv1.ListModelsResponse{Items: []*sdkv1.SdkModel{{Id: "model-" + apiKey}}}, nil
	}}
	_, httpHandler := sdkv1connect.NewSdkCursorServiceHandler(handler)
	server := httptest.NewServer(httpHandler)
	defer server.Close()

	client := newBridgeClient(server.URL, "bridge-token")
	bridge := newBridge()
	bridge.state = bridgeRunning
	bridge.process = &bridgeProcess{client: client, exited: make(chan struct{})}
	runner := &SDKRunner{bridge: bridge, timeout: 3 * time.Second}

	type result struct {
		models []string
		err    error
	}
	results := make(chan result, 2)
	for _, apiKey := range []string{"account-a", "account-b"} {
		go func() {
			models, err := runner.ListModels(context.Background(), apiKey)
			results <- result{models: models, err: err}
		}()
	}
	gotModels := make(map[string]bool)
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatalf("ListModels() error = %v", got.err)
		}
		if len(got.models) != 1 {
			t.Fatalf("ListModels() = %#v", got.models)
		}
		gotModels[got.models[0]] = true
	}
	if !gotModels["model-account-a"] || !gotModels["model-account-b"] {
		t.Fatalf("models = %#v", gotModels)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["account-a"] != 1 || seen["account-b"] != 1 {
		t.Fatalf("API keys observed = %#v", seen)
	}
}

func TestSDKRunnerAssistantBlocksAreChunksAndDeletesAgent(t *testing.T) {
	handler := &testAgentHandler{deleteStarted: make(chan struct{})}
	handler.sendFn = func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
		assistant := sdkMessage(t, "assistant", map[string]any{
			"agent_id": "agent-1",
			"run_id":   "run-1",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "He"},
				map[string]any{"type": "tool_use", "name": "ignored"},
				map[string]any{"type": "text", "text": "llo"},
			}},
		})
		if err := stream.Send(assistant); err != nil {
			return err
		}
		result := runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, "Hello!")
		result.GetResult().GetResult().Usage = &sdkv1.TokenUsage{
			InputTokens: 11, OutputTokens: 7, CacheReadTokens: 5, CacheWriteTokens: 3,
			TotalTokens: 26, ReasoningTokens: int64Pointer(2),
		}
		if err := stream.Send(result); err != nil {
			return err
		}
		return stream.Send(runDone("agent-1", "run-1"))
	}
	runner := newTestSDKRunner(t, handler)
	events, err := runner.Run(context.Background(), &Credential{APIKey: "key-1"}, Request{Model: "model-1", Prompt: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var deltas []string
	var final Event
	for event := range events {
		if event.Delta != "" {
			deltas = append(deltas, event.Delta)
		}
		if event.Done {
			final = event
		}
	}
	if !reflect.DeepEqual(deltas, []string{"He", "llo", "!"}) {
		t.Fatalf("deltas = %#v", deltas)
	}
	if final.Err != nil || final.Text != "Hello!" || !final.Done {
		t.Fatalf("final = %+v", final)
	}
	if final.Usage == nil || *final.Usage != (Usage{
		InputTokens: 11, OutputTokens: 7, CacheReadTokens: 5, CacheWriteTokens: 3,
		TotalTokens: 26, ReasoningTokens: 2,
	}) {
		t.Fatalf("final usage = %+v", final.Usage)
	}
	select {
	case <-handler.deleteStarted:
	case <-time.After(time.Second):
		t.Fatal("DeleteAgent cleanup did not start")
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.create.GetOptions().GetApiKey() != "key-1" || handler.create.GetOptions().GetTools() == nil ||
		len(handler.create.GetOptions().GetTools().GetNames()) != 0 {
		t.Fatalf("CreateAgent request = %+v", handler.create)
	}
	if handler.send.GetOptions().GetEnableDeltas() || handler.send.GetOptions().GetEnableSteps() {
		t.Fatalf("Send options = %+v", handler.send.GetOptions())
	}
	if handler.deleted.GetAgentId() != "agent-1" || handler.deleted.GetOptions().GetApiKey() != "key-1" ||
		handler.deleted.GetOptions().GetCwd() == "" {
		t.Fatalf("DeleteAgent request = %+v", handler.deleted)
	}
}

func TestSDKRunnerDoesNotReplaySendAfterStreamTransportFailure(t *testing.T) {
	handler := &testAgentHandler{}
	handler.sendFn = func(_ context.Context, _ *connect.ServerStream[sdkv1.RunStreamMessage]) error {
		return connect.NewError(connect.CodeUnavailable, errors.New("bridge stream lost"))
	}
	runner := newTestSDKRunner(t, handler)

	events, runErr := runner.Run(
		context.Background(),
		&Credential{APIKey: "key-1"},
		Request{Model: "model-1", Prompt: "hello"},
	)
	if runErr == nil {
		var final Event
		for event := range events {
			if event.Done {
				final = event
			}
		}
		runErr = final.Err
	}
	if runErr == nil {
		t.Fatal("Run() succeeded after stream transport failure")
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.sendCalls != 1 {
		t.Fatalf("Send calls = %d, want 1", handler.sendCalls)
	}
}

func TestSDKRunnerKeepsInterimAssistantAndLoadsMissingUsageFromSnapshot(t *testing.T) {
	handler := &testAgentHandler{runSnapshot: &sdkv1.RunSnapshot{
		AgentId: "agent-1",
		RunId:   "run-1",
		Status:  sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED,
		Usage: &sdkv1.TokenUsage{
			InputTokens: 13, OutputTokens: 8, CacheReadTokens: 5, CacheWriteTokens: 3,
			TotalTokens: 29, ReasoningTokens: int64Pointer(2),
		},
	}}
	handler.sendFn = func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
		for _, message := range []*sdkv1.RunStreamMessage{
			sdkMessage(t, "assistant", map[string]any{
				"agent_id": "agent-1", "run_id": "run-1",
				"message": map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "Checking.\n"},
				}},
			}),
			sdkMessage(t, "assistant", map[string]any{
				"agent_id": "agent-1", "run_id": "run-1",
				"message": map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "Final answer."},
				}},
			}),
			runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, "Final answer."),
			runDone("agent-1", "run-1"),
		} {
			if err := stream.Send(message); err != nil {
				return err
			}
		}
		return nil
	}

	runner := newTestSDKRunner(t, handler)
	ctx := WithRawResponseCapture(context.Background())
	events, err := runner.Run(ctx, &Credential{APIKey: "key-1"}, Request{Model: "model-1", Prompt: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var deltas []string
	var rawEnvelopes []map[string]any
	var final Event
	for event := range events {
		if len(event.RawResponse) > 0 {
			var envelope map[string]any
			if err := json.Unmarshal(event.RawResponse, &envelope); err != nil {
				t.Fatalf("decode raw response: %v body=%s", err, event.RawResponse)
			}
			rawEnvelopes = append(rawEnvelopes, envelope)
		}
		if event.Delta != "" {
			deltas = append(deltas, event.Delta)
		}
		if event.Done {
			final = event
		}
	}
	if !reflect.DeepEqual(deltas, []string{"Checking.\n", "Final answer."}) {
		t.Fatalf("deltas = %#v", deltas)
	}
	if len(rawEnvelopes) != 4 || rawEnvelopes[0]["sdk_message"] == nil ||
		rawEnvelopes[1]["sdk_message"] == nil || rawEnvelopes[2]["result"] == nil ||
		rawEnvelopes[3]["done"] == nil {
		t.Fatalf("raw envelopes = %#v", rawEnvelopes)
	}
	if final.Err != nil || final.Text != "Checking.\nFinal answer." || !final.Done {
		t.Fatalf("final = %+v", final)
	}
	if final.Usage == nil || *final.Usage != (Usage{
		InputTokens: 13, OutputTokens: 8, CacheReadTokens: 5, CacheWriteTokens: 3,
		TotalTokens: 29, ReasoningTokens: 2,
	}) {
		t.Fatalf("final usage = %+v", final.Usage)
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.getRun.GetRunId() != "run-1" ||
		handler.getRun.GetOptions().GetRuntime() != sdkv1.Runtime_RUNTIME_LOCAL ||
		handler.getRun.GetOptions().GetAgentId() != "agent-1" ||
		handler.getRun.GetOptions().GetApiKey() != "key-1" ||
		handler.getRun.GetOptions().GetCwd() == "" {
		t.Fatalf("GetRun request = %+v", handler.getRun)
	}
}

func TestSDKRunnerSendFailureReturnsBeforeAgentCleanup(t *testing.T) {
	deleteStarted := make(chan struct{})
	deleteRelease := make(chan struct{})
	runner := newTestSDKRunner(t, &testAgentHandler{})
	client := runner.bridge.process.client
	client.agent = &sendFailureAgentClient{
		SdkAgentServiceClient: client.agent,
		err:                   connect.NewError(connect.CodeUnavailable, errors.New("send handshake failed")),
		deleteStarted:         deleteStarted,
		deleteRelease:         deleteRelease,
	}
	t.Cleanup(func() { close(deleteRelease) })

	returned := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background(), &Credential{APIKey: "key-1"}, Request{Model: "model-1", Prompt: "hello"})
		returned <- err
	}()
	select {
	case <-deleteStarted:
	case <-time.After(time.Second):
		t.Fatal("DeleteAgent cleanup did not start")
	}
	select {
	case err := <-returned:
		if err == nil || !strings.Contains(err.Error(), "send handshake failed") {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DeleteAgent cleanup delayed the synchronous Send failure")
	}
}

func TestSDKRunnerCallerCancellationInterruptsUsageFallback(t *testing.T) {
	getRunStarted := make(chan struct{})
	getRunRelease := make(chan struct{})
	handler := &testAgentHandler{getRunStarted: getRunStarted, getRunRelease: getRunRelease}
	handler.sendFn = func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
		if err := stream.Send(runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, "done")); err != nil {
			return err
		}
		return stream.Send(runDone("agent-1", "run-1"))
	}
	runner := newTestSDKRunner(t, handler)
	ctx, cancel := context.WithCancel(context.Background())
	events, err := runner.Run(ctx, &Credential{APIKey: "key-1"}, Request{Model: "model-1", Prompt: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	select {
	case <-getRunStarted:
	case <-time.After(time.Second):
		t.Fatal("missing-usage GetRun fallback did not start")
	}
	cancel()
	done := make(chan struct{})
	go func() {
		for range events {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("GetRun fallback ignored caller cancellation")
	}
}

func TestSDKRunnerCallerCancellationStopsSendWithoutRunID(t *testing.T) {
	sendStopped := make(chan struct{})
	deleteStarted := make(chan struct{})
	deleteRelease := make(chan struct{})
	defer close(deleteRelease)
	handler := &testAgentHandler{deleteStarted: deleteStarted, deleteRelease: deleteRelease}
	handler.sendFn = func(ctx context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
		if err := stream.Send(&sdkv1.RunStreamMessage{}); err != nil {
			return err
		}
		<-ctx.Done()
		close(sendStopped)
		return ctx.Err()
	}
	runner := newTestSDKRunner(t, handler)
	ctx, cancel := context.WithCancel(context.Background())
	events, err := runner.Run(ctx, &Credential{APIKey: "key-1"}, Request{Model: "model-1", Prompt: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	cancel()
	select {
	case <-sendStopped:
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not stop the Send stream")
	}
	finalDone := make(chan Event, 1)
	go func() {
		var final Event
		for event := range events {
			if event.Done {
				final = event
			}
		}
		finalDone <- final
	}()
	var final Event
	select {
	case final = <-finalDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup delayed the final event")
	}
	if !errors.Is(final.Err, context.Canceled) {
		t.Fatalf("final error = %v, want context.Canceled", final.Err)
	}
	select {
	case <-deleteStarted:
	case <-time.After(time.Second):
		t.Fatal("DeleteAgent cleanup did not start")
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.cancelled != nil {
		t.Fatalf("CancelRun request without a run ID = %+v", handler.cancelled)
	}
	if handler.deleted == nil || handler.deleted.GetAgentId() != "agent-1" {
		t.Fatalf("DeleteAgent request = %+v", handler.deleted)
	}
}

func TestSDKRunStateRequiresTerminalSequenceAndRejectsDivergence(t *testing.T) {
	state := &sdkRunState{agentID: "agent-1"}
	if _, err := state.consume(sdkMessage(t, "assistant", map[string]any{
		"agent_id": "agent-1", "run_id": "run-1",
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Hi"}}},
	})); err != nil {
		t.Fatal(err)
	}
	if err := state.finalError(nil); err == nil || !strings.Contains(err.Error(), "before result") {
		t.Fatalf("missing result error = %v", err)
	}
	if _, err := state.consume(runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, "Hello")); err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("divergent result error = %v", err)
	}

	state = &sdkRunState{agentID: "agent-1"}
	if _, err := state.consume(runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, "")); err != nil {
		t.Fatal(err)
	}
	if err := state.finalError(nil); err == nil || !strings.Contains(err.Error(), "before done") {
		t.Fatalf("missing done error = %v", err)
	}
}

func TestSDKRunStateUsesStatusFailureMessage(t *testing.T) {
	state := &sdkRunState{agentID: "agent-1"}
	if _, err := state.consume(sdkMessage(t, "status", map[string]any{
		"agent_id": "agent-1", "run_id": "run-1", "message": "human readable failure",
	})); err != nil {
		t.Fatal(err)
	}
	result := runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR, "")
	code := "MACHINE_CODE"
	result.GetResult().ErrorCode = &code
	if _, err := state.consume(result); err != nil {
		t.Fatal(err)
	}
	if _, err := state.consume(runDone("agent-1", "run-1")); err != nil {
		t.Fatal(err)
	}
	if err := state.finalError(nil); err == nil || !strings.Contains(err.Error(), "human readable failure") ||
		strings.Contains(err.Error(), code) {
		t.Fatalf("final error = %v", err)
	}
}

func TestClassifyBridgeErrorPreservesDetailsAndRejectsOnlyUnauthorized(t *testing.T) {
	requestID, helpURL, provider := "req-full", "https://help.example", "provider-a"
	limit, remaining, reset := uint64(10), uint64(0), uint64(123)
	detail, err := connect.NewErrorDetail(&sdkv1.SdkErrorDetails{
		RequestId:    &requestID,
		SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED,
		Message:      "bad key",
		HelpUrl:      &helpURL,
		Provider:     &provider,
		RetryAfter:   durationpb.New(1500 * time.Millisecond),
		RateLimit: &sdkv1.RateLimitInfo{
			Limit: &limit, Remaining: &remaining, ResetEpochSeconds: &reset,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rpcErr := connect.NewError(connect.CodeUnauthenticated, errors.New("ignored"))
	rpcErr.AddDetail(detail)
	classified := classifyBridgeError(rpcErr)
	var bridgeErr *BridgeError
	if !errors.As(classified, &bridgeErr) {
		t.Fatalf("classified error = %T", classified)
	}
	if bridgeErr.RequestID != requestID || bridgeErr.Provider != provider || bridgeErr.HelpURL != helpURL ||
		bridgeErr.RetryAfter != 1500*time.Millisecond || bridgeErr.RateLimit.Remaining == nil ||
		*bridgeErr.RateLimit.Remaining != 0 || !IsCredentialRejected(classified) {
		t.Fatalf("BridgeError = %+v", bridgeErr)
	}
	bare := classifyBridgeError(connect.NewError(connect.CodeUnauthenticated, errors.New("Unauthorized")))
	if IsCredentialRejected(bare) {
		t.Fatal("bare bridge bearer error must not reject the channel credential")
	}
}

func TestBridgeTransportFailureExcludesStructuredSDKErrors(t *testing.T) {
	transportErr := connect.NewError(connect.CodeUnavailable, errors.New("connection reset"))
	if !isBridgeTransportFailure(transportErr) {
		t.Fatal("plain unavailable transport error was not classified as a bridge failure")
	}

	detail, err := connect.NewErrorDetail(&sdkv1.SdkErrorDetails{
		SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_UPSTREAM_ERROR,
		Message:      "provider unavailable",
	})
	if err != nil {
		t.Fatal(err)
	}
	providerErr := connect.NewError(connect.CodeUnavailable, errors.New("provider unavailable"))
	providerErr.AddDetail(detail)
	if isBridgeTransportFailure(providerErr) {
		t.Fatal("structured provider error was treated as a bridge transport failure")
	}
}

func sdkMessage(t *testing.T, kind string, payload map[string]any) *sdkv1.RunStreamMessage {
	t.Helper()
	value, err := structpb.NewStruct(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &sdkv1.RunStreamMessage{Envelope: &sdkv1.RunStreamMessage_SdkMessage{
		SdkMessage: &sdkv1.SdkMessage{Type: kind, Message: value},
	}}
}

func runResult(agentID, runID string, status sdkv1.RunLifecycleStatus, text string) *sdkv1.RunStreamMessage {
	return &sdkv1.RunStreamMessage{Envelope: &sdkv1.RunStreamMessage_Result{
		Result: &sdkv1.RunStreamResult{
			AgentId: agentID,
			RunId:   runID,
			Status:  status,
			Result: &sdkv1.RunResult{
				AgentId: agentID, RunId: runID, Status: status, Result: text,
			},
		},
	}}
}

func runDone(agentID, runID string) *sdkv1.RunStreamMessage {
	return &sdkv1.RunStreamMessage{Envelope: &sdkv1.RunStreamMessage_Done{
		Done: &sdkv1.RunStreamDone{AgentId: agentID, RunId: runID},
	}}
}

func int64Pointer(value int64) *int64 {
	return &value
}
