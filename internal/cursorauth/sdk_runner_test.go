package cursorauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
	createCalls   int
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
	getRunStopped chan struct{}
}

type testCursorHandler struct {
	sdkv1connect.UnimplementedSdkCursorServiceHandler

	request *sdkv1.ListModelsRequest
}

type shutdownErrorControlClient struct {
	sdkv1connect.SdkBridgeControlServiceClient
	err error
}

/**
 * @brief 通过客户端边界模拟 Send 失败或等待请求取消。
 */
type sendFailureAgentClient struct {
	sdkv1connect.SdkAgentServiceClient
	err           error
	sendFn        func(context.Context) error ///< 设置时替代立即返回错误的行为。
	deleteStarted chan struct{}
	deleteRelease chan struct{}
}

/**
 * @brief 在 Send 已发起后观察 DeleteAgent 清理边界。
 */
type deleteObservingAgentClient struct {
	sdkv1connect.SdkAgentServiceClient
	deleted chan struct{} ///< 删除 RPC 完成后关闭。
}

/**
 * @brief 执行真实删除 RPC 并通知测试。
 * @param ctx 清理上下文。
 * @param request 删除请求。
 * @return 上游删除响应；失败时为 nil。
 * @return 上游 RPC 错误；成功时为 nil。
 */
func (c *deleteObservingAgentClient) DeleteAgent(
	ctx context.Context,
	request *connect.Request[sdkv1.DeleteAgentRequest],
) (*connect.Response[sdkv1.DeleteAgentResponse], error) {
	defer close(c.deleted)
	return c.SdkAgentServiceClient.DeleteAgent(ctx, request)
}

/**
 * @brief 模拟 Send 的同步失败或取消等待。
 * @param ctx 当前 Agent 上下文。
 * @param request 未使用的发送请求。
 * @return 始终为 nil，不建立事件流。
 * @return 配置的失败或 sendFn 返回的取消错误。
 */
func (c *sendFailureAgentClient) Send(
	ctx context.Context,
	_ *connect.Request[sdkv1.SendRequest],
) (*connect.ServerStreamForClient[sdkv1.RunStreamMessage], error) {
	if c.sendFn != nil {
		return nil, c.sendFn(ctx)
	}
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
		{Id: "composer-2.5", Variants: []*sdkv1.ModelVariant{
			{Params: []*sdkv1.ModelParameterValue{{Id: "fast", Value: "true"}}, IsDefault: true},
			{Params: []*sdkv1.ModelParameterValue{{Id: "fast", Value: "false"}}},
		}},
		{Id: "composer-2.5-fast"},
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
	h.createCalls++
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
	stopped := h.getRunStopped
	h.mu.Unlock()
	if stopped != nil {
		defer close(stopped)
	}
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
	return &SDKRunner{bridge: bridge, idleTimeout: 3 * time.Second}
}

func TestSDKRunnerStartMakesBridgeReadyWithoutModelRequest(t *testing.T) {
	runner := newTestSDKRunner(t, &testAgentHandler{})
	if err := runner.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
}

func TestSDKRunnerCreateAgentDeadlineNamesOperationAndProxyDiagnostic(t *testing.T) {
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
	if err == nil ||
		(!strings.Contains(err.Error(), "CreateAgent exceeded its local deadline after") &&
			!strings.Contains(err.Error(), "CreateAgent returned deadline_exceeded after")) ||
		!strings.Contains(err.Error(), "inherited HTTP_PROXY/HTTPS_PROXY/ALL_PROXY") ||
		!strings.Contains(err.Error(), "returned no detail beyond deadline_exceeded") {
		t.Fatalf("Run() error = %v", err)
	}
	if strings.Contains(err.Error(), "CreateAgent exceeded its local deadline after") {
		if !strings.Contains(err.Error(), "operation limit=30s") {
			t.Fatalf("local deadline diagnostic omitted operation limit: %v", err)
		}
	} else if strings.Contains(err.Error(), "operation limit=") {
		t.Fatalf("remote deadline diagnostic claimed a local operation limit: %v", err)
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

func TestSDKRunnerListModelsAddsFastSuffix(t *testing.T) {
	handler := &testCursorHandler{}
	_, httpHandler := sdkv1connect.NewSdkCursorServiceHandler(handler)
	server := httptest.NewServer(httpHandler)
	t.Cleanup(server.Close)
	client := newBridgeClient(server.URL, "bridge-token")
	bridge := newBridge()
	bridge.state = bridgeRunning
	bridge.process = &bridgeProcess{client: client, exited: make(chan struct{})}
	runner := &SDKRunner{bridge: bridge, idleTimeout: 3 * time.Second}

	models, err := runner.ListModels(context.Background(), "user-api-key")
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	want := []string{"grok-4.6", "composer-2.5", "composer-2.5-fast"}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("ListModels() = %#v, want %#v", models, want)
	}
	if got := handler.request.GetOptions().GetApiKey(); got != "user-api-key" {
		t.Fatalf("ListModels API key = %q", got)
	}
}

/**
 * @brief 验证目录原生 ID 与 fast 别名在首次创建和续轮发送时一致。
 * @param t 测试上下文。
 */
func TestSDKRunnerRoutesCatalogFastModels(t *testing.T) {
	handler := &testAgentHandler{sendFn: finishedRunSend(t, "ok")}
	fast := &sdkv1.ModelParameterDefinition{Id: "fast", Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "true"}}}
	effort := &sdkv1.ModelParameterDefinition{Id: "reasoning_effort", Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "low"}}}
	runner, calls := newCatalogTestSDKRunner(t, handler, []*sdkv1.SdkModel{
		{Id: "native", Parameters: []*sdkv1.ModelParameterDefinition{fast}},
		{Id: "native-fast"},
		{Id: "native-high", Parameters: []*sdkv1.ModelParameterDefinition{fast, effort}},
		{Id: "variant", Variants: []*sdkv1.ModelVariant{{Params: []*sdkv1.ModelParameterValue{{Id: "fast", Value: "true"}}}}},
		{Id: "slow", Parameters: []*sdkv1.ModelParameterDefinition{{Id: "fast", Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "false"}}}}},
	})
	credential := &Credential{APIKey: "key"}
	start := ParseRequest([]byte(`{"model":"native-fast","messages":[{"role":"user","content":"hi"}]}`))
	start.SessionScope = testCursorSessionScope
	drainRun(t, mustRun(t, runner, credential, start))
	waitParkedAgents(t, runner, 1)
	handler.mu.Lock()
	created := handler.create.GetOptions().GetModel()
	handler.mu.Unlock()
	if created.GetId() != "native-fast" || len(created.GetParams()) != 0 || calls.Load() != 1 {
		t.Fatalf("cold CreateAgent model = %v, ListModels calls = %d", created, calls.Load())
	}

	next := ParseRequest([]byte(`{"model":"native-high-fast","reasoning_effort":"low","messages":[
		{"role":"user","content":"hi"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}
	]}`))
	next.SessionScope = testCursorSessionScope
	drainRun(t, mustRun(t, runner, credential, next))
	waitParkedAgents(t, runner, 1)
	handler.mu.Lock()
	sent, creates := handler.send.GetOptions().GetModel(), handler.createCalls
	handler.mu.Unlock()
	params := sent.GetParams()
	if creates != 1 || calls.Load() != 1 || sent.GetId() != "native-high" || len(params) != 2 ||
		params[0].GetId() != "fast" || params[0].GetValue() != "true" ||
		params[1].GetId() != "reasoning_effort" || params[1].GetValue() != "low" {
		t.Fatalf("reused Send model = %v, CreateAgent calls = %d, ListModels calls = %d", sent, creates, calls.Load())
	}
	models, err := runner.ListModels(t.Context(), credential.APIKey)
	want := []string{"native", "native-fast", "native-high", "native-high-fast", "variant", "variant-fast", "slow"}
	if err != nil || !reflect.DeepEqual(models, want) {
		t.Fatalf("ListModels() = %v, %v, want %v", models, err, want)
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
	runner := &SDKRunner{bridge: bridge, idleTimeout: 3 * time.Second}

	_, err := runner.ListModels(context.Background(), "user-api-key")
	if err == nil || !strings.Contains(err.Error(), "ListModels returned deadline_exceeded after") ||
		!strings.Contains(err.Error(), "inherited HTTP_PROXY/HTTPS_PROXY/ALL_PROXY") ||
		!strings.Contains(err.Error(), "returned no detail beyond deadline_exceeded") ||
		strings.Contains(err.Error(), "operation limit=") {
		t.Fatalf("ListModels() error = %v", err)
	}
}

/**
 * @brief 验证模型列表 RPC 保留远端超时原因，并遮蔽回显的当前账户凭证。
 * @param t 测试上下文。
 */
func TestSDKRunnerListModelsPreservesRemoteDeadlineDiagnostic(t *testing.T) {
	const apiKey = "synthetic-cursor-list-models-token"
	handler := &testCursorHandlerFunc{listModels: func(request *sdkv1.ListModelsRequest) (*sdkv1.ListModelsResponse, error) {
		return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("proxy CONNECT failed for "+request.GetOptions().GetApiKey()))
	}}
	_, httpHandler := sdkv1connect.NewSdkCursorServiceHandler(handler)
	server := httptest.NewServer(httpHandler)
	t.Cleanup(server.Close)
	client := newBridgeClient(server.URL, "bridge-token")
	bridge := newBridge()
	bridge.state = bridgeRunning
	bridge.process = &bridgeProcess{client: client, exited: make(chan struct{})}
	runner := &SDKRunner{bridge: bridge, idleTimeout: 3 * time.Second}

	_, err := runner.ListModels(context.Background(), apiKey)
	bridgeErr, ok := errors.AsType[*BridgeError](err)
	if !ok || bridgeErr.ConnectCode != connect.CodeDeadlineExceeded || bridgeErr.Message != "proxy CONNECT failed for [redacted]" ||
		!strings.Contains(err.Error(), bridgeErr.Message) || strings.Contains(err.Error(), apiKey) ||
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
	runner := &SDKRunner{bridge: bridge, idleTimeout: 3 * time.Second}

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
	runner := &SDKRunner{bridge: bridge, idleTimeout: 3 * time.Second}

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
			TotalTokens: 26, ReasoningTokens: new(int64(2)),
		}
		if err := stream.Send(result); err != nil {
			return err
		}
		return stream.Send(runDone("agent-1", "run-1"))
	}
	runner, _ := newCatalogTestSDKRunner(t, handler, []*sdkv1.SdkModel{{Id: "model-1"}})
	events, err := runner.Run(context.Background(), &Credential{APIKey: "key-1"}, Request{Model: "model-1-fast", Prompt: "hello"})
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
	selection := handler.create.GetOptions().GetModel()
	if selection.GetId() != "model-1" || len(selection.GetParams()) != 1 ||
		selection.GetParams()[0].GetId() != "fast" || selection.GetParams()[0].GetValue() != "true" {
		t.Fatalf("CreateAgent model = %+v", selection)
	}
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

/**
 * @brief SDK thinking 消息独立于正文逐片发出，终态不重复已有增量。
 * @param t 测试上下文。
 */
func TestSDKRunnerSeparatesThinkingFromAssistantText(t *testing.T) {
	handler := &testAgentHandler{}
	handler.sendFn = func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
		result := runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, "Hello!")
		result.GetResult().GetResult().Usage = &sdkv1.TokenUsage{
			InputTokens: 11, OutputTokens: 7, TotalTokens: 18, ReasoningTokens: new(int64(2)),
		}
		for _, message := range []*sdkv1.RunStreamMessage{
			sdkMessage(t, "status", map[string]any{
				"agent_id": "agent-1", "run_id": "run-1", "status": "RUNNING", "type": "status",
			}),
			sdkMessage(t, "thinking", map[string]any{
				"agent_id": "agent-1", "run_id": "run-1", "text": "I should", "type": "thinking",
			}),
			sdkMessage(t, "thinking", map[string]any{
				"agent_id": "agent-1", "run_id": "run-1", "text": " check. ", "type": "thinking",
			}),
			sdkMessage(t, "assistant", map[string]any{
				"agent_id": "agent-1", "run_id": "run-1",
				"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Hello"}}},
			}),
			result, runDone("agent-1", "run-1"),
		} {
			if err := stream.Send(message); err != nil {
				return err
			}
		}
		return nil
	}
	runner := newTestSDKRunner(t, handler)
	events, err := runner.Run(context.Background(), &Credential{APIKey: "key"}, Request{Model: "model-1", Prompt: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var thinking, deltas, kinds []string
	var final Event
	for event := range events {
		if event.Ping {
			kinds = append(kinds, "ping")
		}
		if event.ThinkingDelta != "" {
			thinking = append(thinking, event.ThinkingDelta)
			kinds = append(kinds, "thinking")
			if event.Delta != "" || event.Text != "" || event.Done {
				t.Fatalf("thinking mixed with assistant text or terminal event: %+v", event)
			}
		}
		if event.Delta != "" {
			deltas = append(deltas, event.Delta)
			kinds = append(kinds, "assistant")
		}
		if event.Done {
			kinds = append(kinds, "done")
			final = event
		}
	}
	if !reflect.DeepEqual(thinking, []string{"I should", " check. "}) {
		t.Fatalf("thinking deltas = %#v", thinking)
	}
	if !reflect.DeepEqual(deltas, []string{"Hello", "!"}) {
		t.Fatalf("assistant deltas = %#v", deltas)
	}
	if !reflect.DeepEqual(kinds, []string{"ping", "thinking", "thinking", "assistant", "assistant", "done"}) {
		t.Fatalf("event order = %#v", kinds)
	}
	if final.Err != nil || !final.Done || final.Text != "Hello!" || final.ThinkingDelta != "" {
		t.Fatalf("terminal event = %+v", final)
	}
	if final.Usage == nil || *final.Usage != (Usage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18, ReasoningTokens: 2}) {
		t.Fatalf("terminal usage = %+v", final.Usage)
	}
}

/**
 * @brief 取消或上游失败之前收到的 thinking 仍可见，且不会成为正文。
 * @param t 测试上下文。
 */
func TestSDKRunnerPreservesThinkingOnTermination(t *testing.T) {
	for _, callerCancels := range []bool{false, true} {
		name := "upstream error"
		if callerCancels {
			name = "caller cancellation"
		}
		t.Run(name, func(t *testing.T) {
			handler := &testAgentHandler{}
			handler.sendFn = func(ctx context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
				if err := stream.Send(sdkMessage(t, "thinking", map[string]any{
					"agent_id": "agent-1", "run_id": "run-1", "text": "Still checking.", "type": "thinking",
				})); err != nil {
					return err
				}
				if callerCancels {
					<-ctx.Done()
					return ctx.Err()
				}
				result := runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR, "")
				code := "upstream failed"
				result.GetResult().ErrorCode = &code
				if err := stream.Send(result); err != nil {
					return err
				}
				return stream.Send(runDone("agent-1", "run-1"))
			}
			runner := newTestSDKRunner(t, handler)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			events, err := runner.Run(ctx, &Credential{APIKey: "key"}, Request{Model: "model-1", Prompt: "hello"})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			var thinking []string
			var final Event
			for event := range events {
				if event.Delta != "" || event.Text != "" {
					t.Fatalf("thinking became assistant text: %+v", event)
				}
				if event.ThinkingDelta != "" {
					thinking = append(thinking, event.ThinkingDelta)
					if callerCancels {
						cancel()
					}
				}
				if event.Done {
					final = event
				}
			}
			if !reflect.DeepEqual(thinking, []string{"Still checking."}) || !final.Done || final.Err == nil {
				t.Fatalf("thinking=%#v terminal=%+v", thinking, final)
			}
			if callerCancels {
				if !errors.Is(final.Err, context.Canceled) {
					t.Fatalf("cancellation error = %v", final.Err)
				}
			} else if !strings.Contains(final.Err.Error(), "upstream failed") {
				t.Fatalf("upstream error = %v", final.Err)
			}
		})
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
			TotalTokens: 29, ReasoningTokens: new(int64(2)),
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

/**
 * @brief 活动请求持续时间超过工具会话空闲预算时仍可正常完成。
 * @param t 测试上下文。
 */
func TestSDKRunnerActiveRequestOutlivesSessionIdleTimeout(t *testing.T) {
	const idleTimeout = 20 * time.Millisecond
	handler := &testAgentHandler{}
	handler.sendFn = func(ctx context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
		if err := stream.Send(sdkMessage(t, "status", map[string]any{
			"agent_id": "agent-1", "run_id": "run-1",
		})); err != nil {
			return err
		}
		timer := time.NewTimer(5 * idleTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := stream.Send(runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, "done")); err != nil {
			return err
		}
		return stream.Send(runDone("agent-1", "run-1"))
	}
	runner := newTestSDKRunner(t, handler)
	runner.idleTimeout = idleTimeout
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	events, err := runner.Run(ctx, &Credential{APIKey: "test-key"}, Request{Prompt: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var final Event
	for event := range events {
		if event.Done {
			final = event
		}
	}
	if !final.Done || final.Err != nil || final.Text != "done" {
		t.Fatalf("active request final = %+v", final)
	}
}

/**
 * @brief Send 尚未返回事件流时，请求取消仍中止调用并删除 Agent。
 * @param t 测试上下文。
 */
func TestSDKRunnerCallerCancellationStopsSynchronousSend(t *testing.T) {
	for _, tc := range []struct {
		name             string
		timeout          time.Duration
		cancelAfterStart bool
		wantErr          error
	}{
		{name: "cancelled", timeout: time.Second, cancelAfterStart: true, wantErr: context.Canceled},
		{name: "deadline", timeout: 100 * time.Millisecond, wantErr: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sendStarted := make(chan struct{})
			deleteStarted := make(chan struct{})
			deleteRelease := make(chan struct{})
			t.Cleanup(func() { close(deleteRelease) })
			runner := newTestSDKRunner(t, &testAgentHandler{})
			client := runner.bridge.process.client
			client.agent = &sendFailureAgentClient{
				SdkAgentServiceClient: client.agent,
				sendFn: func(ctx context.Context) error {
					close(sendStarted)
					<-ctx.Done()
					return ctx.Err()
				},
				deleteStarted: deleteStarted, deleteRelease: deleteRelease,
			}
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			returned := make(chan error, 1)
			go func() {
				_, err := runner.Run(ctx, &Credential{APIKey: "test-key"}, Request{Prompt: "hello"})
				returned <- err
			}()
			select {
			case <-sendStarted:
			case <-time.After(time.Second):
				t.Fatal("Send did not start")
			}
			if tc.cancelAfterStart {
				cancel()
			}
			select {
			case err := <-returned:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Run() error = %v, want %v", err, tc.wantErr)
				}
			case <-time.After(time.Second):
				t.Fatal("synchronous Send ignored caller cancellation")
			}
			select {
			case <-deleteStarted:
			case <-time.After(time.Second):
				t.Fatal("cancelled Send did not delete its Agent")
			}
		})
	}
}

/**
 * @brief 无后续请求的工具会话在空闲预算耗尽后删除，旧结果按历史开启新 Agent。
 * @param t 测试上下文。
 */
func TestSDKRunnerReclaimsIdleToolSession(t *testing.T) {
	runner, _ := newNativeToolTestRunner(t)
	runner.idleTimeout = 20 * time.Millisecond
	client := runner.bridge.process.client
	agentClient := client.agent
	deleted := make(chan struct{})
	client.agent = &deleteObservingAgentClient{SdkAgentServiceClient: agentClient, deleted: deleted}
	credential := &Credential{APIKey: "test-key"}
	request := Request{
		Prompt: "lookup", ToolChoice: "auto", SessionScope: testCursorSessionScope,
		Tools: []Tool{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	events, err := runner.Run(ctx, credential, request)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	call := readNativeToolCall(t, events)
	select {
	case <-deleted:
	case <-ctx.Done():
		t.Fatal("idle tool session was not deleted")
	}
	client.agent = agentClient
	runner.idleTimeout = time.Second
	request.ToolResults = []ToolResult{{CallID: call.ID, Output: "late-result"}}
	events, err = runner.Run(ctx, credential, request)
	if err != nil {
		t.Fatalf("Run() after idle expiry error = %v", err)
	}
	nextCall := readNativeToolCall(t, events)
	var arguments struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(nextCall.Arguments, &arguments); err != nil {
		t.Fatalf("decode new tool arguments: %v", err)
	}
	if arguments.Session != "agent-2" {
		t.Fatalf("tool call after idle expiry used %q, want new agent-2", arguments.Session)
	}
	if got := resumeNativeToolCall(t, runner, credential, nextCall.ID, "new-result"); got != "agent-2:new-result" {
		t.Fatalf("new tool session result = %q", got)
	}
}

func TestSDKRunnerCallerCancellationInterruptsUsageFallback(t *testing.T) {
	getRunStarted := make(chan struct{})
	getRunRelease := make(chan struct{})
	getRunStopped := make(chan struct{})
	handler := &testAgentHandler{getRunStarted: getRunStarted, getRunRelease: getRunRelease, getRunStopped: getRunStopped}
	handler.sendFn = func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
		if err := stream.Send(runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, "done")); err != nil {
			return err
		}
		return stream.Send(runDone("agent-1", "run-1"))
	}
	runner := newTestSDKRunner(t, handler)
	t.Cleanup(func() { close(getRunRelease) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	select {
	case <-getRunStopped:
	case <-time.After(time.Second):
		t.Fatal("GetRun RPC kept running after caller cancellation")
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

/**
 * @brief 真实 usage 流事件与终态只记一次，异常结束仍补查并保留已报告用量。
 * @param t 测试上下文。
 */
func TestSDKRunnerPreservesIncrementalUsageOnTermination(t *testing.T) {
	for _, status := range []sdkv1.RunLifecycleStatus{
		sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED,
		sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR,
		sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_EXPIRED,
	} {
		t.Run(status.String(), func(t *testing.T) {
			handler := &testAgentHandler{runSnapshot: &sdkv1.RunSnapshot{
				AgentId: "agent-1", RunId: "run-1", Status: status,
				Usage: &sdkv1.TokenUsage{InputTokens: 15, OutputTokens: 7, CacheReadTokens: 5, TotalTokens: 27},
			}}
			handler.sendFn = func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
				for _, event := range []*sdkv1.RunStreamMessage{
					sdkMessage(t, "usage", map[string]any{"agent_id": "agent-1", "run_id": "run-1", "usage": map[string]any{
						"inputTokens": 10, "outputTokens": 2, "cacheReadTokens": 3, "totalTokens": 15,
					}}),
					runResult("agent-1", "run-1", status, ""), runDone("agent-1", "run-1"),
				} {
					if err := stream.Send(event); err != nil {
						return err
					}
				}
				return nil
			}
			runner := newTestSDKRunner(t, handler)
			events, err := runner.Run(context.Background(), &Credential{APIKey: "key"}, Request{Prompt: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			var total Usage
			var final Event
			for event := range events {
				if event.Usage != nil {
					total.Add(event.Usage)
				}
				if event.Done {
					final = event
				}
			}
			if total != (Usage{InputTokens: 15, OutputTokens: 7, CacheReadTokens: 5, TotalTokens: 27}) {
				t.Fatalf("usage was lost or double counted: %+v", total)
			}
			if final.Usage == nil || *final.Usage != (Usage{InputTokens: 5, OutputTokens: 5, CacheReadTokens: 2, TotalTokens: 12}) {
				t.Fatalf("terminal remainder = %+v", final.Usage)
			}
			if (final.Err == nil) != (status == sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED) {
				t.Fatalf("terminal error=%v status=%v", final.Err, status)
			}
		})
	}
}

/**
 * @brief 使用 v1.2.0 实际报告口径验证输入包含缓存时不会重复计费。
 * @param t 测试上下文。
 */
func TestSDKRunnerNormalizesLocalUsageWithoutDoubleCountingCache(t *testing.T) {
	handler := &testAgentHandler{}
	handler.sendFn = func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
		if err := stream.Send(sdkMessage(t, "usage", map[string]any{"agent_id": "agent-1", "run_id": "run-1", "usage": map[string]any{
			"inputTokens": 22285, "outputTokens": 403, "cacheReadTokens": 16447, "cacheWriteTokens": 5828, "totalTokens": 22688,
		}})); err != nil {
			return err
		}
		result := runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, "")
		result.GetResult().GetResult().Usage = &sdkv1.TokenUsage{InputTokens: 22285, OutputTokens: 403, CacheReadTokens: 16447, CacheWriteTokens: 5828, TotalTokens: 22688}
		if err := stream.Send(result); err != nil {
			return err
		}
		return stream.Send(runDone("agent-1", "run-1"))
	}
	runner := newTestSDKRunner(t, handler)
	events, err := runner.Run(context.Background(), &Credential{APIKey: "key"}, Request{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var total Usage
	for event := range events {
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		if event.Usage != nil {
			total.Add(event.Usage)
		}
	}
	if total != (Usage{InputTokens: 10, OutputTokens: 403, CacheReadTokens: 16447, CacheWriteTokens: 5828, TotalTokens: 22688}) {
		t.Fatalf("normalized local usage = %+v", total)
	}
}

/**
 * @brief 缓存分项矛盾的 SDK 用量不结算，合法报告仍能补齐实际消费。
 * @param t 测试上下文。
 */
func TestSDKRunnerSkipsContradictoryUsage(t *testing.T) {
	invalidDelta := map[string]any{"inputTokens": 10, "outputTokens": 2, "cacheReadTokens": 7, "cacheWriteTokens": 4, "totalTokens": 12}
	validDelta := map[string]any{"inputTokens": 10, "outputTokens": 2, "cacheReadTokens": 6, "cacheWriteTokens": 4, "totalTokens": 12}
	invalidTotal := &sdkv1.TokenUsage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 7, CacheWriteTokens: 4, TotalTokens: 12}
	validTotal := &sdkv1.TokenUsage{InputTokens: 20, OutputTokens: 6, CacheReadTokens: 10, CacheWriteTokens: 6, TotalTokens: 26}
	wantDelta := Usage{OutputTokens: 2, CacheReadTokens: 6, CacheWriteTokens: 4, TotalTokens: 12}
	wantRemainder := Usage{InputTokens: 4, OutputTokens: 4, CacheReadTokens: 4, CacheWriteTokens: 2, TotalTokens: 14}
	for _, tt := range []struct {
		name      string
		deltas    []map[string]any
		terminal  *sdkv1.TokenUsage
		snapshot  *sdkv1.TokenUsage
		wantDelta []Usage
		wantFinal *Usage
	}{
		{
			name: "none usable", deltas: []map[string]any{invalidDelta},
			terminal: invalidTotal, snapshot: invalidTotal,
		},
		{
			name: "valid reports after contradiction", deltas: []map[string]any{invalidDelta, validDelta},
			terminal: validTotal, wantDelta: []Usage{wantDelta}, wantFinal: &wantRemainder,
		},
		{
			name: "valid snapshot replaces contradictory terminal", deltas: []map[string]any{validDelta},
			terminal: invalidTotal, snapshot: validTotal, wantDelta: []Usage{wantDelta}, wantFinal: &wantRemainder,
		},
		{
			name: "other conventions", deltas: []map[string]any{
				{"inputTokens": 3, "outputTokens": 2, "cacheReadTokens": 7, "cacheWriteTokens": 4, "totalTokens": 16},
				{"inputTokens": 3, "outputTokens": 2, "cacheReadTokens": 7, "cacheWriteTokens": 4},
			},
			wantDelta: []Usage{
				{InputTokens: 3, OutputTokens: 2, CacheReadTokens: 7, CacheWriteTokens: 4, TotalTokens: 16},
				{InputTokens: 3, OutputTokens: 2, CacheReadTokens: 7, CacheWriteTokens: 4},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handler := &testAgentHandler{runSnapshot: &sdkv1.RunSnapshot{AgentId: "agent-1", RunId: "run-1", Usage: tt.snapshot}}
			handler.sendFn = func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
				for _, delta := range tt.deltas {
					message := sdkMessage(t, "usage", map[string]any{"agent_id": "agent-1", "run_id": "run-1", "usage": delta})
					if err := stream.Send(message); err != nil {
						return err
					}
				}
				result := runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, "answer")
				result.GetResult().GetResult().Usage = tt.terminal
				if err := stream.Send(result); err != nil {
					return err
				}
				return stream.Send(runDone("agent-1", "run-1"))
			}
			runner := newTestSDKRunner(t, handler)
			events, err := runner.Run(t.Context(), &Credential{APIKey: "key"}, Request{Prompt: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			var deltas []Usage
			var final Event
			for event := range events {
				if event.Done {
					final = event
				} else if event.Usage != nil {
					deltas = append(deltas, *event.Usage)
				}
			}
			if !reflect.DeepEqual(deltas, tt.wantDelta) {
				t.Fatalf("incremental usage = %+v, want %+v", deltas, tt.wantDelta)
			}
			if !final.Done || final.Err != nil || final.Text != "answer" {
				t.Fatalf("unexpected final event: %+v", final)
			}
			if !reflect.DeepEqual(final.Usage, tt.wantFinal) {
				t.Fatalf("terminal usage = %+v, want %+v", final.Usage, tt.wantFinal)
			}
		})
	}
}

/**
 * @brief 慢消费者取消后仍收到已缓冲的真实用量，且不重复结算。
 * @param t 测试上下文。
 */
func TestSDKRunnerCancellationPreservesBufferedUsage(t *testing.T) {
	handler := &testAgentHandler{getRunStarted: make(chan struct{}), getRunRelease: make(chan struct{})}
	handler.sendFn = func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
		usage := sdkMessage(t, "usage", map[string]any{"agent_id": "agent-1", "run_id": "run-1", "usage": map[string]any{
			"inputTokens": 10, "outputTokens": 2, "totalTokens": 12,
		}})
		if err := stream.Send(usage); err != nil {
			return err
		}
		for range 40 {
			if err := stream.Send(sdkMessage(t, "assistant", map[string]any{"message": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "x"}},
			}})); err != nil {
				return err
			}
		}
		return stream.Send(usage)
	}
	runner := newTestSDKRunner(t, handler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := runner.Run(ctx, &Credential{APIKey: "key"}, Request{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-handler.getRunStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream messages were not consumed")
	}
	cancel()
	var total Usage
	var final Event
	for event := range events {
		if event.Usage != nil {
			total.Add(event.Usage)
		}
		if event.Done {
			final = event
		}
	}
	if total != (Usage{InputTokens: 20, OutputTokens: 4, TotalTokens: 24}) {
		t.Fatalf("cancellation lost or duplicated usage: %+v", total)
	}
	if !final.Done || !errors.Is(final.Err, context.Canceled) {
		t.Fatalf("terminal event = %+v", final)
	}
}

/**
 * @brief 终态只有通用失败文案时，状态诊断保留旧版凭证恢复行为。
 * @param t 测试上下文。
 */
func TestSDKRunnerClassifiesTerminalAuthenticationFailure(t *testing.T) {
	const diagnosis = "Authentication error If you are logged in, try logging out and back in."
	for _, message := range []string{"", "run failed", "Unknown error", "Cursor SDK run failed."} {
		t.Run(message, func(t *testing.T) {
			handler := &testAgentHandler{sendFn: func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
				if err := stream.Send(sdkMessage(t, "status", map[string]any{
					"run_id": "run-1", "message": diagnosis,
				})); err != nil {
					return err
				}
				if err := stream.Send(sdkMessage(t, "status", map[string]any{
					"run_id": "run-1", "status": "RUNNING", "message": "Waiting for response",
				})); err != nil {
					return err
				}
				result := runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR, "")
				if message != "" {
					result.GetResult().GetResult().ErrorMessage = &message
				}
				if err := stream.Send(result); err != nil {
					return err
				}
				return stream.Send(runDone("agent-1", "run-1"))
			}}
			runner := newTestSDKRunner(t, handler)
			events, err := runner.Run(t.Context(), &Credential{APIKey: "key-1"}, Request{Model: "model-1", Prompt: "hello"})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			var runErr error
			for event := range events {
				if event.Done {
					runErr = event.Err
				}
			}
			if !IsCredentialRejected(runErr) || !strings.Contains(runErr.Error(), diagnosis) {
				t.Fatalf("terminal error = %v, want rejected credential with %q", runErr, diagnosis)
			}
		})
	}
}

/**
 * @brief 验证 SDK RPC 与 Run 终态使用同样的权限、凭证和上下文超限分类。
 * @param t 测试上下文。
 */
func TestSDKRunnerClassifiesAccountAndContextFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		sdkCode  sdkv1.SdkErrorCode
		runCode  string
		message  string
		classify func(error) bool
		want     bool
	}{
		{"missing key", sdkv1.SdkErrorCode_SDK_ERROR_CODE_API_KEY_NOT_FOUND, "api-key-not-found", "key does not exist", IsCredentialRejected, true},
		{"plan", sdkv1.SdkErrorCode_SDK_ERROR_CODE_PLAN_REQUIRED, "SDK_ERROR_CODE_PLAN_REQUIRED", "plan required", IsAccountCapabilityUnavailable, true},
		{"role", sdkv1.SdkErrorCode_SDK_ERROR_CODE_ROLE_FORBIDDEN, "role_forbidden", "role forbidden", IsAccountCapabilityUnavailable, true},
		{"feature", sdkv1.SdkErrorCode_SDK_ERROR_CODE_FEATURE_UNAVAILABLE, "feature_unavailable", "feature unavailable", IsAccountCapabilityUnavailable, true},
		{"context message", sdkv1.SdkErrorCode_SDK_ERROR_CODE_VALIDATION_ERROR, "validation_error", "Maximum context length exceeded", IsContextLengthExceeded, true},
		{"context code in message", sdkv1.SdkErrorCode_SDK_ERROR_CODE_VALIDATION_ERROR, "validation_error", "context_length_exceeded: requested inputs too long", IsContextLengthExceeded, true},
		{"too many tokens", sdkv1.SdkErrorCode_SDK_ERROR_CODE_VALIDATION_ERROR, "validation_error", "Too many tokens", IsContextLengthExceeded, false},
		{"throttle message", sdkv1.SdkErrorCode_SDK_ERROR_CODE_UPSTREAM_ERROR, "upstream_error", "Token rate limit exceeded for this context window", IsContextLengthExceeded, false},
		{"structured throttle", sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED, "rate_limit_exceeded", "Maximum context length exceeded", IsContextLengthExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stage := range []string{"create", "terminal"} {
				t.Run(stage, func(t *testing.T) {
					handler := &testAgentHandler{}
					if stage == "create" {
						detail, err := connect.NewErrorDetail(&sdkv1.SdkErrorDetails{SdkErrorCode: tc.sdkCode, Message: tc.message})
						if err != nil {
							t.Fatal(err)
						}
						createErr := connect.NewError(connect.CodeFailedPrecondition, errors.New(tc.message))
						createErr.AddDetail(detail)
						handler.createErr = createErr
					} else {
						handler.sendFn = func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
							result := runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR, "")
							result.GetResult().GetResult().ErrorCode = &tc.runCode
							result.GetResult().GetResult().ErrorMessage = &tc.message
							if err := stream.Send(result); err != nil {
								return err
							}
							return stream.Send(runDone("agent-1", "run-1"))
						}
					}
					runner := newTestSDKRunner(t, handler)
					events, err := runner.Run(t.Context(), &Credential{APIKey: "fake-user-credential"}, Request{Model: "m", Prompt: "hello"})
					if stage == "terminal" {
						if err != nil {
							t.Fatalf("Run() failed before terminal event: %v", err)
						}
						for event := range events {
							if event.Done {
								err = event.Err
							}
						}
					}
					if err == nil || tc.classify(err) != tc.want || !strings.Contains(err.Error(), tc.message) {
						t.Fatalf("Run failure=%v want classification=%t and message=%q", err, tc.want, tc.message)
					}
				})
			}
		})
	}
}

/**
 * @brief 测试错误终态补发的正文及诊断均遮蔽 SDK 回显的凭证。
 * @param t 测试上下文。
 */
func TestSDKRunnerRedactsFailedResultText(t *testing.T) {
	const key = "fixture-credential-for-redaction"
	const diagnosis = "Model rejected credential " + key
	handler := &testAgentHandler{sendFn: func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
		result := runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR, diagnosis)
		result.GetResult().GetResult().ErrorCode = new("RATE_LIMIT_EXCEEDED")
		result.GetResult().GetResult().ErrorMessage = new(diagnosis)
		if err := stream.Send(result); err != nil {
			return err
		}
		return stream.Send(runDone("agent-1", "run-1"))
	}}
	runner := newTestSDKRunner(t, handler)
	ctx := WithRawResponseCapture(t.Context())
	events, err := runner.Run(ctx, &Credential{APIKey: key}, Request{Model: "m", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	var final Event
	for event := range events {
		if event.SentEffort != nil {
			t.Fatalf("immediately rejected run reported effort: %+v", event)
		}
		if strings.Contains(string(event.RawResponse), key) {
			t.Fatal("debug frame retained the credential")
		}
		streamed.WriteString(event.Delta)
		if event.Done {
			final = event
		}
	}
	const want = "Model rejected credential [redacted]"
	if streamed.String() != want || final.Text != want {
		t.Fatalf("streamed=%q final=%q, want %q", streamed.String(), final.Text, want)
	}
	if _, limited := RateLimitRetryTime(final.Err, time.Now()); !limited || !strings.Contains(final.Err.Error(), want) {
		t.Fatalf("terminal error=%v, want a redacted rate limit diagnosis", final.Err)
	}
}

/**
 * @brief 验证 Run 信封的通用错误码可与内层的明确上下文超限诊断并存。
 * @param t 测试上下文。
 */
func TestSDKRunnerClassifiesTerminalContextCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, outerCode, innerCode, message string }{
		{"inner context code", "validation_error", "CONTEXT_LENGTH_EXCEEDED", "upstream rejected request"},
		{"context message", "upstream_error", "", "Maximum context length exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := &testAgentHandler{sendFn: func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
				result := runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR, "")
				result.GetResult().ErrorCode = &tc.outerCode
				result.GetResult().GetResult().ErrorCode = &tc.innerCode
				result.GetResult().GetResult().ErrorMessage = &tc.message
				if err := stream.Send(result); err != nil {
					return err
				}
				return stream.Send(runDone("agent-1", "run-1"))
			}}
			runner := newTestSDKRunner(t, handler)
			events, err := runner.Run(t.Context(), &Credential{APIKey: "key"}, Request{Model: "m", Prompt: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			for event := range events {
				if event.Done {
					if !IsContextLengthExceeded(event.Err) || !strings.Contains(event.Err.Error(), tc.message) {
						t.Fatalf("terminal failure=%v", event.Err)
					}
					return
				}
			}
			t.Fatal("missing terminal error")
		})
	}
}

/**
 * @brief 验证无效客户端工具 schema 在创建 Agent 前得到确定的请求错误。
 * @param t 测试上下文。
 */
func TestSDKRunnerRejectsInvalidClientToolSchema(t *testing.T) {
	t.Parallel()
	handler := &testAgentHandler{}
	runner := newTestSDKRunner(t, handler)
	request := ParseRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":[]}}]}`))
	events, err := runner.Run(t.Context(), &Credential{APIKey: "key"}, request)
	if events != nil || !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Run() events=%v err=%v", events, err)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.createCalls != 0 {
		t.Fatalf("unexpected Agent creation attempts=%d", handler.createCalls)
	}
}

/**
 * @brief 成功快照省略正文或末段空白不同时保留流式正文，缺少 done 或内容冲突时报告失败。
 * @param t 测试上下文。
 */
func TestSDKRunnerPreservesStreamedTextAtCompletion(t *testing.T) {
	const answer = "Checking.\nFinal answer.\n"
	for _, tc := range []struct {
		name, snapshot, wantError string
		done                      bool
	}{
		{name: "empty snapshot", done: true},
		{name: "last message without newline", snapshot: "Final answer.", done: true},
		{name: "extra surrounding whitespace", snapshot: " Final answer.\t", done: true},
		{name: "additional newline", snapshot: answer + "\n", done: true},
		{name: "partial word", snapshot: "swer.", done: true, wantError: "diverged"},
		{name: "missing done", wantError: "before done"},
		{name: "conflicting snapshot", snapshot: "different answer", done: true, wantError: "diverged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := &testAgentHandler{}
			handler.sendFn = func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
				message := sdkMessage(t, "assistant", map[string]any{
					"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": answer}}},
				})
				if err := stream.Send(message); err != nil {
					return err
				}
				result := runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, tc.snapshot)
				result.GetResult().GetResult().Usage = &sdkv1.TokenUsage{OutputTokens: 2}
				if err := stream.Send(result); err != nil {
					return err
				}
				if tc.done {
					return stream.Send(runDone("agent-1", "run-1"))
				}
				return nil
			}
			runner := newTestSDKRunner(t, handler)
			events := mustRun(t, runner, &Credential{APIKey: "test-account"}, Request{Prompt: "hello"})
			var streamed strings.Builder
			var terminal Event
			for event := range events {
				streamed.WriteString(event.Delta)
				if event.Done {
					terminal = event
				}
			}
			if !terminal.Done || terminal.Text != answer || streamed.String() != answer || terminal.Usage == nil || terminal.Usage.OutputTokens != 2 {
				t.Fatalf("streamed=%q terminal=%+v", streamed.String(), terminal)
			}
			if tc.wantError == "" {
				if terminal.Err != nil {
					t.Fatal(terminal.Err)
				}
			} else if terminal.Err == nil || !strings.Contains(terminal.Err.Error(), tc.wantError) {
				t.Fatalf("error=%v, want %q", terminal.Err, tc.wantError)
			}
		})
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

func TestSDKRunStateEmitsPingForRunningStatus(t *testing.T) {
	state := &sdkRunState{agentID: "agent-1"}
	events, err := state.consume(sdkMessage(t, "status", map[string]any{
		"agent_id": "agent-1", "run_id": "run-1", "status": "RUNNING", "offset": float64(1),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !events[0].Ping || events[0].Delta != "" || events[0].Done || events[0].Err != nil {
		t.Fatalf("running status events = %#v", events)
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

func TestSDKRunStateClassifiesUsageLimitFailure(t *testing.T) {
	const message = "You've hit your usage limit for Opus Switch to a different model or set a Spend Limit to continue with Opus."
	state := &sdkRunState{agentID: "agent-1"}
	if _, err := state.consume(sdkMessage(t, "status", map[string]any{
		"agent_id": "agent-1", "run_id": "run-1", "message": message,
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := state.consume(runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := state.consume(runDone("agent-1", "run-1")); err != nil {
		t.Fatal(err)
	}
	err := state.finalError(nil)
	if !IsUsageLimitExceeded(err) || IsCredentialRejected(err) || !strings.Contains(err.Error(), message) {
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

/**
 * @brief 验证只有结构化 SDK 限流可提供恢复提示，拒绝无效时间。
 * @param t 测试上下文。
 */
func TestSDKRateLimitRetryTime(t *testing.T) {
	t.Parallel()
	now := time.Unix(2_000_000_000, 0)
	future, stale, overflow := uint64(now.Unix()+15), uint64(now.Unix()-1), ^uint64(0)
	for _, tt := range []struct {
		name    string
		code    sdkv1.SdkErrorCode
		retry   *durationpb.Duration
		reset   *uint64
		want    time.Time
		limited bool
	}{
		{"relative first", sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED, durationpb.New(1500 * time.Millisecond), &future, now.Add(1500 * time.Millisecond), true},
		{"negative falls back to reset", sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED, durationpb.New(-time.Second), &future, time.Unix(int64(future), 0), true},
		{"unrepresentable falls back to reset", sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED, &durationpb.Duration{Seconds: 31_536_000_000}, &future, time.Unix(int64(future), 0), true},
		{"stale reset", sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED, nil, &stale, time.Time{}, true},
		{"overflow reset", sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED, nil, &overflow, time.Time{}, true},
		{"bare resource exhausted", sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNSPECIFIED, nil, nil, time.Time{}, false},
		{"usage cap is separate", sdkv1.SdkErrorCode_SDK_ERROR_CODE_USAGE_LIMIT_EXCEEDED, durationpb.New(time.Minute), &future, time.Time{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rpcErr := connect.NewError(connect.CodeResourceExhausted, errors.New("rate limit exceeded"))
			if tt.code != sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNSPECIFIED {
				detail, err := connect.NewErrorDetail(&sdkv1.SdkErrorDetails{
					SdkErrorCode: tt.code, RetryAfter: tt.retry, RateLimit: &sdkv1.RateLimitInfo{ResetEpochSeconds: tt.reset},
				})
				if err != nil {
					t.Fatal(err)
				}
				rpcErr.AddDetail(detail)
			}
			got, limited := RateLimitRetryTime(classifyBridgeError(rpcErr), now)
			if !got.Equal(tt.want) || limited != tt.limited {
				t.Fatalf("RateLimitRetryTime() = %v, %t; want %v, %t", got, limited, tt.want, tt.limited)
			}
		})
	}
}

/**
 * @brief 只有 result 提供具体诊断时，已知或未知错误码均不能把诊断发成正文。
 * @param t 测试上下文。
 */
func TestSDKRunnerReportsResultOnlyDiagnostic(t *testing.T) {
	for _, tc := range []struct{ name, code, message, detail string }{
		{"missing error fields", "", "", "MCP tool call timed out after 60s"},
		{"generic error message", "RATE_LIMIT_EXCEEDED", "Unknown error.", "try again later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := &testAgentHandler{sendFn: func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
				result := runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR, tc.detail)
				inner := result.GetResult().GetResult()
				inner.ErrorCode, inner.ErrorMessage = &tc.code, &tc.message
				if err := stream.Send(result); err != nil {
					return err
				}
				return stream.Send(runDone("agent-1", "run-1"))
			}}
			runner := newTestSDKRunner(t, handler)
			var terminal Event
			for event := range mustRun(t, runner, &Credential{APIKey: "test-account"}, Request{Prompt: "hello"}) {
				if event.Text != "" || event.Delta != "" {
					t.Fatalf("diagnostic delivered as text: %+v", event)
				}
				if event.Done {
					terminal = event
				}
			}
			if terminal.Err == nil || !strings.Contains(terminal.Err.Error(), tc.detail) {
				t.Fatalf("terminal=%+v, want diagnostic %q", terminal, tc.detail)
			}
		})
	}
}

/**
 * @brief 保留已经流出的正文，并从 Run 终态内外层提取明确拒绝。
 * @param t 测试上下文。
 */
func TestSDKRunnerClassifiesTerminalModelFailures(t *testing.T) {
	for _, tt := range []struct {
		name      string
		code      string
		finalText string
		wantText  string
		want      sdkv1.SdkErrorCode
	}{
		{"rate limit without final text", "RATE_LIMIT_EXCEEDED", "", "partial", sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED},
		{"rate limit with final suffix", "RATE_LIMIT_EXCEEDED", "partial answer", "partial answer", sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED},
		{"invalid model", "SDK_ERROR_CODE_INVALID_MODEL", "not the partial text", "partial", sdkv1.SdkErrorCode_SDK_ERROR_CODE_INVALID_MODEL},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handler := &testAgentHandler{}
			handler.sendFn = func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
				if err := stream.Send(sdkMessage(t, "assistant", map[string]any{
					"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "partial"}}},
				})); err != nil {
					return err
				}
				result := runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR, tt.finalText)
				inner := result.GetResult().GetResult()
				message := "model rejected the request"
				inner.ErrorMessage = &message
				if tt.want == sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED {
					inner.ErrorCode = &tt.code
				} else {
					result.GetResult().ErrorCode = &tt.code
				}
				if err := stream.Send(result); err != nil {
					return err
				}
				return stream.Send(runDone("agent-1", "run-1"))
			}
			runner := newTestSDKRunner(t, handler)
			events := mustRun(t, runner, &Credential{APIKey: "key"}, Request{Prompt: "hello"})
			var final Event
			var text strings.Builder
			for event := range events {
				text.WriteString(event.Delta)
				if event.Done {
					final = event
				}
			}
			bridgeErr, ok := errors.AsType[*BridgeError](final.Err)
			if !ok || bridgeErr.SDKCode != tt.want || bridgeErr.Message != "model rejected the request" || final.Text != tt.wantText || text.String() != tt.wantText {
				t.Fatalf("body=%q terminal=%+v error=%v", text.String(), final, final.Err)
			}
		})
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

/**
 * @brief 创建同时提供 Agent 与模型目录服务的测试执行器。
 * @param t 测试上下文。
 * @param handler Agent 服务。
 * @param models ListModels 返回的模型目录。
 * @return 测试执行器与 ListModels 调用计数。
 */
func newCatalogTestSDKRunner(t *testing.T, handler *testAgentHandler, models []*sdkv1.SdkModel) (*SDKRunner, *atomic.Int32) {
	t.Helper()
	var listCalls atomic.Int32
	agentPath, agentHTTP := sdkv1connect.NewSdkAgentServiceHandler(handler)
	cursorPath, cursorHTTP := sdkv1connect.NewSdkCursorServiceHandler(&testCursorHandlerFunc{
		listModels: func(*sdkv1.ListModelsRequest) (*sdkv1.ListModelsResponse, error) {
			listCalls.Add(1)
			return &sdkv1.ListModelsResponse{Items: models}, nil
		},
	})
	mux := http.NewServeMux()
	mux.Handle(agentPath, agentHTTP)
	mux.Handle(cursorPath, cursorHTTP)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newBridgeClient(server.URL, "bridge-token")
	client.workdir = t.TempDir()
	bridge := newBridge()
	bridge.state = bridgeRunning
	bridge.process = &bridgeProcess{client: client, exited: make(chan struct{})}
	return &SDKRunner{bridge: bridge, idleTimeout: 3 * time.Second}, &listCalls
}

/**
 * @brief 每次 Send 都以固定正文正常完成 Run。
 * @param t 测试上下文。
 * @param text 回复正文。
 * @return 可赋给 testAgentHandler.sendFn 的处理函数。
 */
func finishedRunSend(t *testing.T, text string) func(context.Context, *connect.ServerStream[sdkv1.RunStreamMessage]) error {
	t.Helper()
	return func(_ context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
		if err := stream.Send(runResult("agent-1", "run-1", sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, text)); err != nil {
			return err
		}
		return stream.Send(runDone("agent-1", "run-1"))
	}
}

/**
 * @brief 读完事件流并返回终态。
 * @param t 测试上下文。
 * @param events Run 返回的事件流。
 * @return 终态事件。
 */
func drainRun(t *testing.T, events <-chan Event) Event {
	t.Helper()
	var final Event
	for event := range events {
		if event.Done {
			final = event
		}
	}
	if final.Err != nil {
		t.Fatalf("run error = %v", final.Err)
	}
	return final
}

/**
 * @brief 等待 Run 结束后停放的 Agent 数量达到预期。
 * @param t 测试上下文。
 * @param runner 测试执行器。
 * @param want 预期数量。
 */
func waitParkedAgents(t *testing.T, runner *SDKRunner, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		runner.mu.Lock()
		got := len(runner.parked)
		runner.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("parked agents = %d, want %d", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSDKRunnerSendsCatalogEffortAndImages(t *testing.T) {
	handler := &testAgentHandler{}
	handler.sendFn = finishedRunSend(t, "ok")
	runner, listCalls := newCatalogTestSDKRunner(t, handler, []*sdkv1.SdkModel{{
		Id: "claude-opus-5",
		Parameters: []*sdkv1.ModelParameterDefinition{{Id: "effort", Values: []*sdkv1.ModelParameterDefinitionValue{
			{Value: "low"}, {Value: "extra-high"},
		}}},
	}})
	credential := &Credential{APIKey: "key"}
	events, err := runner.Run(t.Context(), credential, Request{
		Model: "claude-opus-5-thinking-xhigh-fast", Prompt: "user: compare\n[image]", Effort: "low",
		Images: []Image{{URL: "https://example.com/a.png"}},
		Tools:  []Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if !errors.Is(err, ErrURLImageUnsupported) || events != nil {
		t.Fatalf("Run() events=%v err=%v, want URL image capability error", events, err)
	}
	handler.mu.Lock()
	createCalls, sendCalls := handler.createCalls, handler.sendCalls
	handler.mu.Unlock()
	if createCalls != 0 || sendCalls != 0 || listCalls.Load() != 0 {
		t.Fatalf("unsupported image called CreateAgent=%d Send=%d ListModels=%d", createCalls, sendCalls, listCalls.Load())
	}

	finished := drainRun(t, mustRun(t, runner, credential, Request{
		Model: "claude-opus-5-thinking-xhigh-fast", Prompt: "user: compare\n[image]\n[image]", Effort: "low",
		Images: []Image{{Data: "aGk=", MimeType: "image/png"}, {Data: "b2s=", MimeType: "image/jpeg"}},
	}))
	if finished.SentEffort == nil || *finished.SentEffort != "xhigh" {
		t.Fatalf("terminal=%+v, want confirmation of xhigh", finished)
	}
	handler.mu.Lock()
	params := handler.create.GetOptions().GetModel().GetParams()
	images := handler.send.GetMessage().GetImages()
	handler.mu.Unlock()
	wantParams := []string{"fast=true", "effort=extra-high"}
	var gotParams []string
	for _, param := range params {
		gotParams = append(gotParams, param.GetId()+"="+param.GetValue())
	}
	if !reflect.DeepEqual(gotParams, wantParams) {
		t.Fatalf("model params = %v, want %v", gotParams, wantParams)
	}
	if len(images) != 2 || images[0].GetData().GetData() != "aGk=" || images[0].GetData().GetMimeType() != "image/png" ||
		images[1].GetData().GetData() != "b2s=" || images[1].GetData().GetMimeType() != "image/jpeg" {
		t.Fatalf("send images = %+v", images)
	}

	finished = drainRun(t, mustRun(t, runner, credential, Request{Model: "claude-opus-5", Prompt: "hi", Effort: "medium"}))
	if finished.SentEffort == nil || *finished.SentEffort != "" {
		t.Fatalf("terminal=%+v, want confirmation without an explicit effort", finished)
	}
	handler.mu.Lock()
	params = handler.create.GetOptions().GetModel().GetParams()
	handler.mu.Unlock()
	if len(params) != 0 {
		t.Fatalf("undeclared effort was sent: %+v", params)
	}
	if listCalls.Load() != 1 {
		t.Fatalf("ListModels calls = %d, want cached single call", listCalls.Load())
	}
}

/**
 * @brief 验证 CreateAgent 使用各模型声明的思考参数与开关取值。
 * @param t 测试上下文。
 */
func TestSDKRunnerMapsCatalogReasoningParameters(t *testing.T) {
	catalog := []*sdkv1.SdkModel{
		{
			Id: "gemini-4-argon",
			Parameters: []*sdkv1.ModelParameterDefinition{
				{Id: "reasoning_effort", Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "high"}}},
			},
		},
		{
			Id: "claude-opus-5",
			Parameters: []*sdkv1.ModelParameterDefinition{
				{Id: "effort", Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "medium"}, {Value: "high"}}},
				{Id: "thinking", Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "false"}, {Value: "true"}}},
			},
		},
		{
			Id: "claude-variant",
			Variants: []*sdkv1.ModelVariant{
				{Params: []*sdkv1.ModelParameterValue{{Id: "reasoning_effort", Value: "extra-high"}, {Id: "thinking", Value: "true"}}},
				{Params: []*sdkv1.ModelParameterValue{{Id: "reasoning_effort", Value: "off"}}},
			},
		},
		{
			Id: "claude-fixed",
			Parameters: []*sdkv1.ModelParameterDefinition{
				{Id: "effort", Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "high"}}},
				{Id: "thinking", Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "false"}}},
			},
		},
		{Id: "gpt-5.4-mini", Parameters: []*sdkv1.ModelParameterDefinition{
			{Id: "reasoning", Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "high"}, {Value: "none"}}},
		}},
		{Id: "claude-haiku-4-5", Parameters: []*sdkv1.ModelParameterDefinition{
			{Id: "thinking", Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "false"}, {Value: "true"}}},
		}},
		{Id: "limited-variant", Variants: []*sdkv1.ModelVariant{
			{Params: []*sdkv1.ModelParameterValue{{Id: "reasoning", Value: "low"}, {Id: "thinking", Value: "true"}}},
			{Params: []*sdkv1.ModelParameterValue{{Id: "thinking", Value: "false"}}},
		}},
	}
	credential := &Credential{APIKey: "key"}
	for _, tc := range []struct {
		name, model, effort string
		want                map[string]string
	}{
		{"reasoning high", "gpt-5.4-mini", "high", map[string]string{"reasoning": "high"}},
		{"reasoning off", "gpt-5.4-mini", "none", map[string]string{"reasoning": "none"}},
		{"boolean on", "claude-haiku-4-5", "high", map[string]string{"thinking": "true"}},
		{"boolean off", "claude-haiku-4-5", "none", map[string]string{"thinking": "false"}},
		{"boolean intermediate", "claude-haiku-4-5", "medium", map[string]string{}},
		{"variant reasoning", "limited-variant", "low", map[string]string{"reasoning": "low"}},
		{"variant omits high", "limited-variant", "high", map[string]string{}},
		{"reasoning effort", "gemini-4-argon", "high", map[string]string{"reasoning_effort": "high"}},
		{"coupled effort", "claude-opus-5", "medium", map[string]string{"effort": "medium", "thinking": "true"}},
		{"thinking off", "claude-opus-5", "none", map[string]string{"thinking": "false"}},
		{"unsupported level", "claude-opus-5", "low", map[string]string{}},
		{"variant effort", "claude-variant", "xhigh", map[string]string{"reasoning_effort": "extra-high", "thinking": "true"}},
		{"off as effort", "claude-variant", "none", map[string]string{"reasoning_effort": "off"}},
		{"no on toggle", "claude-fixed", "high", map[string]string{"effort": "high"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := &testAgentHandler{sendFn: finishedRunSend(t, "ok")}
			runner, _ := newCatalogTestSDKRunner(t, handler, catalog)
			for _, body := range []string{
				`{"messages":[{"role":"user","content":"hi"}]}`,
				`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}]}`,
			} {
				request := ParseRequest([]byte(body))
				request.Model, request.Effort, request.SessionScope = tc.model, tc.effort, testCursorSessionScope
				drainRun(t, mustRun(t, runner, credential, request))
				waitParkedAgents(t, runner, 1)
			}
			handler.mu.Lock()
			defer handler.mu.Unlock()
			if handler.createCalls != 1 || handler.sendCalls != 2 {
				t.Fatalf("CreateAgent calls = %d, Send calls = %d", handler.createCalls, handler.sendCalls)
			}
			for op, model := range map[string]*sdkv1.ModelSelection{
				"CreateAgent": handler.create.GetOptions().GetModel(), "Send": handler.send.GetOptions().GetModel(),
			} {
				got := make(map[string]string, len(model.GetParams()))
				for _, param := range model.GetParams() {
					got[param.GetId()] = param.GetValue()
				}
				if model.GetId() != tc.model || len(model.GetParams()) != len(tc.want) || !reflect.DeepEqual(got, tc.want) {
					t.Errorf("%s model = %v, want %s %v", op, model, tc.model, tc.want)
				}
			}
		})
	}
}

/**
 * @brief 验证模型目录按凭证隔离，并在写入时清理过期目录。
 * @param t 测试上下文。
 */
func TestSDKRunnerModelCatalogPrunesExpiredEntriesOnWrite(t *testing.T) {
	handler := &testAgentHandler{sendFn: finishedRunSend(t, "ok")}
	runner, listCalls := newCatalogTestSDKRunner(t, handler, []*sdkv1.SdkModel{{Id: "model-a"}})
	if _, err := runner.ListModels(t.Context(), " old-key "); err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	run := func(key string) {
		t.Helper()
		drainRun(t, mustRun(t, runner, &Credential{APIKey: key}, Request{Model: "model-a", Prompt: "hi", Effort: "high"}))
	}
	run("old-key")
	if got := listCalls.Load(); got != 1 {
		t.Fatalf("ListModels calls after using the fresh catalog = %d, want 1", got)
	}

	runner.mu.Lock()
	for key, catalog := range runner.catalogs {
		catalog.expiresAt = time.Time{}
		runner.catalogs[key] = catalog
	}
	runner.mu.Unlock()

	run("new-key")
	runner.mu.Lock()
	retained := len(runner.catalogs)
	runner.mu.Unlock()
	if retained != 1 {
		t.Fatalf("catalogs retained after refresh = %d, want 1", retained)
	}
	run("old-key")
	run("new-key")
	if got := listCalls.Load(); got != 3 {
		t.Fatalf("ListModels calls after refreshing both keys = %d, want 3", got)
	}
}

/**
 * @brief 普通续轮只发送新增消息，准备续用失败时保留停放 Agent。
 * @param t 测试上下文。
 */
func TestSDKRunnerReusesParkedAgentForOrdinaryTurn(t *testing.T) {
	handler := &testAgentHandler{}
	handler.sendFn = finishedRunSend(t, "Hello there")
	runner := newTestSDKRunner(t, handler)
	credential := &Credential{APIKey: "key"}
	start := ParseRequest([]byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
	))
	start.SessionScope = testCursorSessionScope
	drainRun(t, mustRun(t, runner, credential, start))
	waitParkedAgents(t, runner, 1)

	const followUp = `{"model":"m2","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"Hello there"},
		{"role":"user","content":[{"type":"text","text":"next"},{"type":"image_url","image_url":{"url":"https://example.com/c.png"}}]}
	]}`
	remoteFollowUp := ParseRequest([]byte(followUp))
	remoteFollowUp.SessionScope = testCursorSessionScope
	events, err := runner.Run(t.Context(), credential, remoteFollowUp)
	if !errors.Is(err, ErrURLImageUnsupported) || events != nil {
		t.Fatalf("Run() events=%v err=%v, want URL image capability error for the new message", events, err)
	}
	inlineFollowUp := ParseRequest([]byte(strings.Replace(followUp, "https://example.com/c.png", "data:image/png;base64,aGk=", 1)))
	inlineFollowUp.SessionScope = testCursorSessionScope
	needsCatalog := inlineFollowUp
	needsCatalog.Effort = "high"
	events, err = runner.Run(t.Context(), credential, needsCatalog)
	if bridgeErr, ok := errors.AsType[*BridgeError](err); !ok || bridgeErr.ConnectCode != connect.CodeUnimplemented || events != nil {
		t.Fatalf("Run() events=%v err=%v, want ListModels failure from the agent-only bridge", events, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	events, err = runner.Run(canceled, credential, inlineFollowUp)
	if !errors.Is(err, context.Canceled) || events != nil {
		t.Fatalf("Run() events=%v err=%v, want canceled before taking the parked Agent", events, err)
	}
	drainRun(t, mustRun(t, runner, credential, inlineFollowUp))
	handler.mu.Lock()
	createCalls, sendCalls, send := handler.createCalls, handler.sendCalls, handler.send
	deleted := handler.deleted
	handler.mu.Unlock()
	if createCalls != 1 || sendCalls != 2 || deleted != nil {
		t.Fatalf("create=%d send=%d deleted=%+v, want reuse without delete", createCalls, sendCalls, deleted)
	}
	if send.GetAgentId() != "agent-1" || send.GetMessage().GetText() != "user: next\n[image]" ||
		len(send.GetMessage().GetImages()) != 1 || send.GetMessage().GetImages()[0].GetData().GetData() != "aGk=" || send.GetOptions().GetModel().GetId() != "m2" {
		t.Fatalf("reused send = %+v", send)
	}
	waitParkedAgents(t, runner, 1)

	diverged := ParseRequest([]byte(`{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"edited reply"},
		{"role":"user","content":"next"}
	]}`))
	diverged.SessionScope = testCursorSessionScope
	drainRun(t, mustRun(t, runner, credential, diverged))
	handler.mu.Lock()
	createCalls, send = handler.createCalls, handler.send
	handler.mu.Unlock()
	if createCalls != 2 || !strings.HasPrefix(send.GetMessage().GetText(), "user: hi") {
		t.Fatalf("diverged history create=%d send=%q, want new agent with full prompt", createCalls, send.GetMessage().GetText())
	}
}

/**
 * @brief 回传原文时续用停放 Agent，编辑历史时向新 Agent 发送修改后的完整内容。
 * @param t 测试上下文。
 */
func TestSDKRunnerMatchesExactParkedReply(t *testing.T) {
	const reply = "  SELECT 'a b'\n\tFROM kv  \n"
	const toolLine = "[tool_use id=toolu_1 name=lookup] {}"
	for _, tt := range []struct {
		name     string
		reply    string
		content  string
		wantBody string
		reuse    bool
	}{
		{
			name: "same string", reply: reply, reuse: true,
			content: `"  SELECT 'a b'\n\tFROM kv  \n"`,
		},
		{
			name: "same text blocks around thinking", reply: reply, reuse: true,
			content: `[{"type":"text","text":"  SELECT 'a b'"},{"type":"thinking","thinking":"reasoning"},{"type":"text","text":"\n\tFROM kv  \n"}]`,
		},
		{
			name: "edited SQL literal", reply: reply,
			content:  `"  SELECT 'ab'\n\tFROM kv  \n"`,
			wantBody: "  SELECT 'ab'\n\tFROM kv  \n",
		},
		{
			name: "edited initial indent", reply: reply,
			content:  `"SELECT 'a b'\n\tFROM kv  \n"`,
			wantBody: "SELECT 'a b'\n\tFROM kv  \n",
		},
		{
			name: "edited trailing whitespace in text block", reply: reply,
			content:  `[{"type":"text","text":"  SELECT 'a b'\n"},{"type":"text","text":"\tFROM kv"}]`,
			wantBody: "  SELECT 'a b'\n\tFROM kv",
		},
		{
			name: "literal tool marker replaced with tool call", reply: "note\n" + toolLine,
			content:  `[{"type":"text","text":"note"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]`,
			wantBody: "note\n" + toolLine,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handler := &testAgentHandler{sendFn: finishedRunSend(t, tt.reply)}
			runner := newTestSDKRunner(t, handler)
			credential := &Credential{APIKey: "key"}
			start := ParseRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			start.SessionScope = testCursorSessionScope
			drainRun(t, mustRun(t, runner, credential, start))
			waitParkedAgents(t, runner, 1)

			followUp := ParseRequest([]byte(`{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":` + tt.content + `},
				{"role":"user","content":"next"}
			]}`))
			followUp.SessionScope = testCursorSessionScope
			drainRun(t, mustRun(t, runner, credential, followUp))
			wantCreates, wantPrompt := 1, "user: next"
			if !tt.reuse {
				wantCreates, wantPrompt = 2, "user: hi\n\nassistant: "+tt.wantBody+"\n\nuser: next"
			}
			handler.mu.Lock()
			defer handler.mu.Unlock()
			if got := handler.send.GetMessage().GetText(); handler.createCalls != wantCreates || handler.sendCalls != 2 || got != wantPrompt {
				t.Fatalf("CreateAgent calls=%d, Send calls=%d, prompt=%q; want %d, 2, %q", handler.createCalls, handler.sendCalls, got, wantCreates, wantPrompt)
			}
		})
	}
}

/**
 * @brief 从完整历史新建 Agent 时保留末尾 assistant 预填文本的空白。
 * @param t 测试上下文。
 */
func TestSDKRunnerPreservesAssistantPrefill(t *testing.T) {
	handler := &testAgentHandler{sendFn: finishedRunSend(t, "ok")}
	runner := newTestSDKRunner(t, handler)
	request := ParseRequest([]byte(`{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"\t  partial  \n"}
	]}`))
	drainRun(t, mustRun(t, runner, &Credential{APIKey: "key"}, request))
	handler.mu.Lock()
	defer handler.mu.Unlock()
	const want = "user: hi\n\nassistant: \t  partial  \n"
	if got := handler.send.GetMessage().GetText(); got != want {
		t.Fatalf("Send prompt=%q, want %q", got, want)
	}
}

/**
 * @brief 验证真实 Connect 流只在首次输出前明确报告 Agent 失效时允许新建。
 * @param t 测试上下文。
 */
func TestSDKRunnerParkedSendRejection(t *testing.T) {
	for _, tt := range []struct {
		name       string
		code       sdkv1.SdkErrorCode
		canReplace bool
		started    bool
	}{
		{name: "credential", code: sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED},
		{name: "invalid model", code: sdkv1.SdkErrorCode_SDK_ERROR_CODE_INVALID_MODEL},
		{name: "usage limit", code: sdkv1.SdkErrorCode_SDK_ERROR_CODE_USAGE_LIMIT_EXCEEDED},
		{name: "rate limit", code: sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED},
		{name: "transport"},
		{name: "missing agent", code: sdkv1.SdkErrorCode_SDK_ERROR_CODE_AGENT_NOT_FOUND, canReplace: true},
		{name: "already started", code: sdkv1.SdkErrorCode_SDK_ERROR_CODE_AGENT_NOT_FOUND, started: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sendErr := connect.NewError(connect.CodeUnavailable, errors.New("parked send rejected"))
			if tt.code != sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNSPECIFIED {
				detail, err := connect.NewErrorDetail(&sdkv1.SdkErrorDetails{SdkErrorCode: tt.code, Message: "parked send rejected"})
				if err != nil {
					t.Fatal(err)
				}
				sendErr = connect.NewError(connect.CodeFailedPrecondition, errors.New("parked send rejected"))
				sendErr.AddDetail(detail)
			}
			handler := &testAgentHandler{}
			finish := finishedRunSend(t, "hello")
			handler.sendFn = func(ctx context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
				handler.mu.Lock()
				resumed := handler.send.GetOptions().GetModel() != nil
				handler.mu.Unlock()
				if !resumed {
					return finish(ctx, stream)
				}
				if tt.started {
					if err := stream.Send(sdkMessage(t, "status", map[string]any{"status": "RUNNING"})); err != nil {
						return err
					}
				}
				return sendErr
			}
			runner := newTestSDKRunner(t, handler)
			credential := &Credential{APIKey: "key"}
			initial := ParseRequest([]byte(
				`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			))
			initial.SessionScope = testCursorSessionScope
			drainRun(t, mustRun(t, runner, credential, initial))
			waitParkedAgents(t, runner, 1)
			followUp := ParseRequest([]byte(`{"model":"m","messages":[
				{"role":"user","content":"hi"}, {"role":"assistant","content":"hello"}, {"role":"user","content":"next"}
			]}`))
			followUp.SessionScope = testCursorSessionScope
			events, err := runner.Run(t.Context(), credential, followUp)
			wantCreates, wantSends := 1, 2
			if tt.canReplace {
				if err != nil || events == nil {
					t.Fatalf("replacement Run() events=%v err=%v", events, err)
				}
				drainRun(t, events)
				wantCreates, wantSends = 2, 3
			} else {
				if tt.started {
					if err != nil || events == nil {
						t.Fatalf("started Run() events=%v err=%v", events, err)
					}
					for event := range events {
						if event.SentEffort != nil {
							t.Fatalf("started but rejected run reported an effort: %+v", event)
						}
						if event.Done {
							err = event.Err
						}
					}
				} else if events != nil {
					t.Fatalf("rejected Run() returned events=%v", events)
				}
				if bridgeErr, ok := errors.AsType[*BridgeError](err); !ok || bridgeErr.SDKCode != tt.code {
					t.Fatalf("rejected Run() err=%v, want %s", err, tt.code)
				}
			}
			handler.mu.Lock()
			createCalls, sendCalls := handler.createCalls, handler.sendCalls
			handler.mu.Unlock()
			if createCalls != wantCreates || sendCalls != wantSends {
				t.Fatalf("created agents=%d sends=%d; want %d, %d", createCalls, sendCalls, wantCreates, wantSends)
			}
		})
	}
}

/**
 * @brief 相同历史与上游凭证不能跨渠道或调用方续用普通 Agent。
 * @param t 测试上下文。
 */
func TestSDKRunnerScopesParkedAgents(t *testing.T) {
	for _, tt := range []struct {
		name  string
		scope SessionScope
	}{
		{"other token", SessionScope{ChannelID: 1, TokenID: 2}},
		{"other channel", SessionScope{ChannelID: 2, TokenID: 1}},
		{"no token", SessionScope{ChannelID: 1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handler := &testAgentHandler{sendFn: finishedRunSend(t, "Hello")}
			runner := newTestSDKRunner(t, handler)
			credential := &Credential{APIKey: "key"}
			start := ParseRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			start.SessionScope = testCursorSessionScope
			drainRun(t, mustRun(t, runner, credential, start))
			waitParkedAgents(t, runner, 1)

			followUp := ParseRequest([]byte(`{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"Hello"},
				{"role":"user","content":"next"}
			]}`))
			followUp.SessionScope = tt.scope
			drainRun(t, mustRun(t, runner, credential, followUp))
			handler.mu.Lock()
			defer handler.mu.Unlock()
			if got := handler.send.GetMessage().GetText(); handler.createCalls != 2 || got != followUp.Prompt {
				t.Fatalf("CreateAgent calls=%d, Send=%q; want 2 calls and full history %q", handler.createCalls, got, followUp.Prompt)
			}
		})
	}
}

/**
 * @brief 启动 Run 并在失败时终止测试。
 * @param t 测试上下文。
 * @param runner 测试执行器。
 * @param credential Cursor 凭证。
 * @param request 当前请求。
 * @return 事件流。
 */
func mustRun(t *testing.T, runner *SDKRunner, credential *Credential, request Request) <-chan Event {
	t.Helper()
	events, err := runner.Run(context.Background(), credential, request)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return events
}
