package cursorauth

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/cursorauth/sdkgen/sdk/v1/sdkv1connect"
)

// SDKRunner translates the existing inference Runner contract onto one shared
// managed Cursor SDK bridge.
type SDKRunner struct {
	bridge  *bridge
	timeout time.Duration

	mu     sync.Mutex
	closed bool
	active sync.WaitGroup
}

// NewSDKRunner constructs the process-level Cursor inference runner. An
// optional explicit path binds the runner to the startup-validated bridge.
func NewSDKRunner(binaryPath ...string) *SDKRunner {
	return &SDKRunner{bridge: newBridge(binaryPath...), timeout: AgentTimeout}
}

// Start eagerly launches and validates the managed bridge without requiring a
// model request. It is safe to call concurrently with Run or ListModels.
func (r *SDKRunner) Start(ctx context.Context) error {
	if r == nil || r.bridge == nil {
		return errors.New("cursor SDK runner is unavailable")
	}
	if err := r.begin(); err != nil {
		return err
	}
	defer r.active.Done()
	_, err := r.bridge.client(ctx)
	return err
}

// ListModels returns Cursor's model IDs verbatim. SDK variants are parameter
// choices, not synthetic model names, so they are deliberately not expanded.
func (r *SDKRunner) ListModels(ctx context.Context, apiKey string) ([]string, error) {
	if r == nil || r.bridge == nil {
		return nil, errors.New("cursor SDK runner is unavailable")
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}
	if err := r.begin(); err != nil {
		return nil, err
	}
	defer r.active.Done()

	client, err := r.bridge.client(ctx)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	response, err := client.cursor.ListModels(requestCtx, connect.NewRequest(&sdkv1.ListModelsRequest{
		Options: &sdkv1.CursorRequestOptions{ApiKey: apiKey},
	}))
	cancel()
	if err != nil {
		return nil, classifyBridgeError(err)
	}

	models := make([]string, 0, len(response.Msg.GetItems()))
	seen := make(map[string]struct{}, len(response.Msg.GetItems()))
	for _, item := range response.Msg.GetItems() {
		id := strings.TrimSpace(item.GetId())
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, id)
	}
	if len(models) == 0 {
		return nil, errors.New("cursor SDK returned an empty model catalog")
	}
	return models, nil
}

// Run creates one isolated SDK Agent and streams its assistant output.
func (r *SDKRunner) Run(
	ctx context.Context,
	credential *Credential,
	model, prompt string,
) (<-chan Event, error) {
	if r == nil || r.bridge == nil {
		return nil, errors.New("cursor SDK runner is unavailable")
	}
	if credential == nil || strings.TrimSpace(credential.APIKey) == "" {
		return nil, ErrMissingAPIKey
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, errors.New("cursor prompt is required")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = "default"
	}
	if err := r.begin(); err != nil {
		return nil, err
	}
	finish := true
	defer func() {
		if finish {
			r.active.Done()
		}
	}()

	client, err := r.bridge.client(ctx)
	if err != nil {
		return nil, err
	}
	apiKey := strings.TrimSpace(credential.APIKey)
	createCtx, createCancel := context.WithTimeout(ctx, RequestTimeout)
	created, err := client.agent.CreateAgent(createCtx, connect.NewRequest(&sdkv1.CreateAgentRequest{
		Options: &sdkv1.AgentOptions{
			Model:  &sdkv1.ModelSelection{Id: model},
			ApiKey: apiKey,
			Local:  &sdkv1.LocalAgentOptions{Cwd: []string{client.workdir}},
			Tools:  &sdkv1.ToolList{Names: []string{}},
		},
	}))
	createCancel()
	if err != nil {
		return nil, classifyBridgeError(err)
	}
	agentID := strings.TrimSpace(created.Msg.GetAgentId())
	if agentID == "" {
		return nil, errors.New("cursor-sdk-bridge CreateAgent returned an empty agent_id")
	}

	runTimeout := r.timeout
	if runTimeout <= 0 {
		runTimeout = AgentTimeout
	}
	runCtx, stopRun := context.WithTimeout(context.WithoutCancel(ctx), runTimeout)
	stream, err := client.agent.Send(runCtx, connect.NewRequest(&sdkv1.SendRequest{
		AgentId: agentID,
		Message: &sdkv1.UserMessage{Text: prompt},
		Options: &sdkv1.SendOptions{
			EnableDeltas: false,
			EnableSteps:  false,
		},
	}))
	if err != nil {
		stopRun()
		r.deleteAgent(client.agent, agentID, client.workdir, apiKey)
		return nil, classifyBridgeError(err)
	}

	events := make(chan Event, 16)
	finish = false
	go r.consumeRun(ctx, runCtx, stopRun, client.agent, stream, agentID, client.workdir, apiKey, events)
	return events, nil
}

func (r *SDKRunner) begin() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrBridgeClosed
	}
	r.active.Add(1)
	return nil
}

func (r *SDKRunner) consumeRun(
	callerCtx context.Context,
	runCtx context.Context,
	stopRun context.CancelFunc,
	client sdkv1connect.SdkAgentServiceClient,
	stream *connect.ServerStreamForClient[sdkv1.RunStreamMessage],
	agentID, workdir, apiKey string,
	events chan<- Event,
) {
	defer r.active.Done()
	defer close(events)
	canceller := newRunCanceller(client, agentID)
	state := &sdkRunState{
		agentID:    agentID,
		onRunID:    canceller.SetRunID,
		onTerminal: canceller.Terminal,
	}
	callerCallbackDone := make(chan struct{})
	stopCallerCallback := context.AfterFunc(callerCtx, func() {
		canceller.Request()
		close(callerCallbackDone)
	})
	runCallbackDone := make(chan struct{})
	stopRunCallback := context.AfterFunc(runCtx, func() {
		canceller.Request()
		close(runCallbackDone)
	})

	deliver := true
	deliverEvent := func(event Event) {
		if !deliver {
			return
		}
		select {
		case events <- event:
		case <-callerCtx.Done():
			deliver = false
		}
	}
	captureRawResponse := rawResponseCaptureEnabled(callerCtx)
	var consumeErr error
	for stream.Receive() {
		message := stream.Msg()
		if captureRawResponse {
			raw, marshalErr := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(message)
			if marshalErr == nil {
				deliverEvent(Event{RawResponse: raw})
			}
		}
		batch, err := state.consume(message)
		if err != nil {
			consumeErr = err
			break
		}
		for i := range batch {
			deliverEvent(batch[i])
		}
	}
	if consumeErr == nil {
		consumeErr = state.finalError(stream.Err())
	}
	if closeErr := stream.Close(); closeErr != nil && consumeErr == nil {
		consumeErr = classifyBridgeError(closeErr)
	}
	if state.status == sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_CANCELLED && callerCtx.Err() != nil {
		consumeErr = context.Cause(callerCtx)
	}
	if consumeErr != nil {
		canceller.Request()
	}

	if stopCallerCallback() {
		close(callerCallbackDone)
	} else {
		<-callerCallbackDone
	}
	stopRun()
	if stopRunCallback() {
		close(runCallbackDone)
	} else {
		<-runCallbackDone
	}
	canceller.Wait()
	if state.status == sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED && !hasTokenUsage(state.usage) {
		if usage := loadRunUsage(client, state.runID, agentID, workdir, apiKey); hasTokenUsage(usage) {
			state.usage = usage
		}
	}
	r.deleteAgent(client, agentID, workdir, apiKey)
	finalEvent := Event{Text: state.text, Done: true, Err: consumeErr, Usage: state.usage}
	if callerCtx.Err() == nil {
		events <- finalEvent
	} else {
		select {
		case events <- finalEvent:
		default:
		}
	}
}

func loadRunUsage(
	client sdkv1connect.SdkAgentServiceClient,
	runID, agentID, workdir, apiKey string,
) *Usage {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), BridgeCleanupTimeout)
	defer cancel()
	response, err := client.GetRun(ctx, connect.NewRequest(&sdkv1.GetRunRequest{
		RunId: runID,
		Options: &sdkv1.GetRunOptions{
			Runtime: sdkv1.Runtime_RUNTIME_LOCAL,
			Cwd:     workdir,
			AgentId: agentID,
			ApiKey:  apiKey,
		},
	}))
	if err != nil || response == nil || response.Msg.GetRun() == nil {
		return nil
	}
	run := response.Msg.GetRun()
	if strings.TrimSpace(run.GetRunId()) != runID {
		return nil
	}
	if snapshotAgentID := strings.TrimSpace(run.GetAgentId()); snapshotAgentID != "" && snapshotAgentID != agentID {
		return nil
	}
	return usageFromSDK(run.GetUsage())
}

func hasTokenUsage(usage *Usage) bool {
	return usage != nil && (usage.InputTokens > 0 || usage.OutputTokens > 0 ||
		usage.CacheReadTokens > 0 || usage.CacheWriteTokens > 0 ||
		usage.TotalTokens > 0 || usage.ReasoningTokens > 0)
}

func (r *SDKRunner) deleteAgent(
	client sdkv1connect.SdkAgentServiceClient,
	agentID, workdir, apiKey string,
) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), BridgeCleanupTimeout)
	defer cancel()
	_, err := client.DeleteAgent(cleanupCtx, connect.NewRequest(&sdkv1.DeleteAgentRequest{
		AgentId: agentID,
		Options: &sdkv1.AgentOperationOptions{Cwd: workdir, ApiKey: apiKey},
	}))
	if err != nil {
		log.Printf("[WARN] 删除 Cursor SDK Agent %s 失败: %v", agentID, classifyBridgeError(err))
		closeCtx, closeCancel := context.WithTimeout(context.Background(), BridgeCleanupTimeout)
		_, closeErr := client.CloseAgent(closeCtx, connect.NewRequest(&sdkv1.CloseAgentRequest{
			AgentId: agentID,
		}))
		closeCancel()
		if closeErr != nil {
			log.Printf("[WARN] 释放 Cursor SDK Agent %s 失败: %v", agentID, classifyBridgeError(closeErr))
		}
	}
}

// Close waits for active runs and terminates the managed bridge process.
func (r *SDKRunner) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if !r.closed {
		r.closed = true
	}
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.active.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		_ = r.bridge.close(ctx)
		return context.Cause(ctx)
	case <-done:
	}
	return r.bridge.close(ctx)
}

type runCanceller struct {
	client  sdkv1connect.SdkAgentServiceClient
	agentID string

	mu        sync.Mutex
	runID     string
	requested bool
	issued    bool
	terminal  bool
	active    sync.WaitGroup
}

func newRunCanceller(client sdkv1connect.SdkAgentServiceClient, agentID string) *runCanceller {
	return &runCanceller{client: client, agentID: agentID}
}

func (c *runCanceller) Request() {
	c.mu.Lock()
	c.requested = true
	c.issueLocked()
	c.mu.Unlock()
}

func (c *runCanceller) SetRunID(runID string) {
	c.mu.Lock()
	if c.runID == "" {
		c.runID = runID
	}
	c.issueLocked()
	c.mu.Unlock()
}

func (c *runCanceller) Terminal(runID string) {
	c.mu.Lock()
	if c.runID == "" {
		c.runID = runID
	}
	c.terminal = true
	c.mu.Unlock()
}

func (c *runCanceller) issueLocked() {
	if !c.requested || c.issued || c.terminal || c.runID == "" {
		return
	}
	c.issued = true
	runID := c.runID
	c.active.Add(1)
	go func() {
		defer c.active.Done()
		cancelCtx, cancel := context.WithTimeout(context.Background(), BridgeCleanupTimeout)
		defer cancel()
		_, err := c.client.CancelRun(cancelCtx, connect.NewRequest(&sdkv1.CancelRunRequest{
			RunId: runID, AgentId: &c.agentID,
		}))
		if err != nil {
			log.Printf("[WARN] 取消 Cursor SDK Run %s 失败: %v", runID, classifyBridgeError(err))
		}
	}()
}

func (c *runCanceller) Wait() {
	c.active.Wait()
}
