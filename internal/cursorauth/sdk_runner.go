package cursorauth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/cursorauth/sdkgen/sdk/v1/sdkv1connect"
)

// SDKRunner translates the existing inference Runner contract onto one shared
// managed Cursor SDK bridge.
type SDKRunner struct {
	bridge  *bridge
	timeout time.Duration

	resumeMu       sync.Mutex
	mu             sync.Mutex
	closed         bool
	active         sync.WaitGroup
	sessions       map[string]*sdkSession
	calls          map[string]*pendingToolCall
	completedCalls map[string]*sdkSession

	callbackMu       sync.Mutex
	callbackServer   *http.Server
	callbackListener net.Listener
	callbackURL      string
	callbackToken    string
	callbackClient   *bridgeClient
}

type sdkSession struct {
	agentID            string
	apiKey             string
	workdir            string
	client             sdkv1connect.SdkAgentServiceClient
	ctx                context.Context
	cancel             context.CancelFunc
	events             chan Event
	done               chan struct{}
	tools              map[string]struct{}
	captureRaw         bool
	eventMu            sync.Mutex
	inputTokenEstimate int
	terminated         bool
	terminal           *Event

	turnMu   sync.Mutex
	attached bool
}

// NewSDKRunner constructs the process-level Cursor inference runner. An
// optional explicit path binds the runner to the startup-validated bridge.
func NewSDKRunner(binaryPath ...string) *SDKRunner {
	return &SDKRunner{
		bridge: newBridge(binaryPath...), timeout: AgentTimeout,
		sessions: make(map[string]*sdkSession), calls: make(map[string]*pendingToolCall),
		completedCalls: make(map[string]*sdkSession),
	}
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

// ListModels returns Cursor's model IDs plus the -fast form accepted by the SDK.
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

	started := time.Now()
	requestCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	client, err := r.bridge.client(requestCtx)
	if err != nil {
		return nil, err
	}
	response, err := listCursorModels(requestCtx, client, apiKey)
	if err != nil && isBridgeTransportFailure(err) {
		replacement, replaced, replacementErr := r.bridge.replacementClient(requestCtx, client)
		if replacementErr != nil {
			return nil, replacementErr
		}
		if replaced {
			response, err = listCursorModels(requestCtx, replacement, apiKey)
		}
	}
	if err != nil {
		return nil, classifyBridgeOperationError("ListModels", requestCtx, RequestTimeout, started, err)
	}

	models := make([]string, 0, len(response.Msg.GetItems())*2)
	seen := make(map[string]struct{}, len(response.Msg.GetItems())*2)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		models = append(models, id)
	}
	for _, item := range response.Msg.GetItems() {
		id := strings.TrimSpace(item.GetId())
		if id == "" {
			continue
		}
		add(id)
		if supportsFastModelVariant(item) && !strings.HasSuffix(strings.ToLower(id), "-fast") {
			add(id + "-fast")
		}
	}
	if len(models) == 0 {
		return nil, errors.New("cursor SDK returned an empty model catalog")
	}
	return models, nil
}

func supportsFastModelVariant(model *sdkv1.SdkModel) bool {
	for _, variant := range model.GetVariants() {
		for _, parameter := range variant.GetParams() {
			if strings.EqualFold(strings.TrimSpace(parameter.GetId()), "fast") &&
				strings.EqualFold(strings.TrimSpace(parameter.GetValue()), "true") {
				return true
			}
		}
	}
	return false
}

func listCursorModels(
	ctx context.Context,
	client *bridgeClient,
	apiKey string,
) (*connect.Response[sdkv1.ListModelsResponse], error) {
	return client.cursor.ListModels(ctx, connect.NewRequest(&sdkv1.ListModelsRequest{
		Options: &sdkv1.CursorRequestOptions{ApiKey: apiKey},
	}))
}

// Run starts a native Cursor SDK turn, or resumes a suspended custom-tool
// callback when the client returns tool results.
func (r *SDKRunner) Run(
	ctx context.Context,
	credential *Credential,
	request Request,
) (<-chan Event, error) {
	if r == nil || r.bridge == nil {
		return nil, errors.New("cursor SDK runner is unavailable")
	}
	if credential == nil || strings.TrimSpace(credential.APIKey) == "" {
		return nil, ErrMissingAPIKey
	}
	if len(request.ToolResults) > 0 {
		return r.resumeToolRun(ctx, credential, request.ToolResults, request.InputTokenEstimate)
	}
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" {
		return nil, errors.New("cursor prompt is required")
	}
	model := strings.TrimSpace(request.Model)
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
	customTools, err := customToolDefinitions(request)
	if err != nil {
		return nil, err
	}
	if len(customTools) > 0 {
		if err := r.ensureToolCallback(ctx, client); err != nil {
			return nil, err
		}
	}
	allowedTools := []string{}
	if len(customTools) > 0 {
		// Cursor exposes SDK custom tools through its synthetic MCP server.
		// An empty allowlist disables that MCP family along with every built-in.
		allowedTools = []string{"mcp"}
	}
	apiKey := strings.TrimSpace(credential.APIKey)
	createStarted := time.Now()
	createCtx, createCancel := context.WithTimeout(ctx, RequestTimeout)
	created, err := client.agent.CreateAgent(createCtx, connect.NewRequest(&sdkv1.CreateAgentRequest{
		Options: &sdkv1.AgentOptions{
			Model:  cursorModelSelection(model),
			ApiKey: apiKey,
			Local: &sdkv1.LocalAgentOptions{
				Cwd: []string{client.workdir}, CustomTools: customTools,
			},
			Tools: &sdkv1.ToolList{Names: allowedTools},
		},
	}))
	if err != nil {
		classifiedErr := classifyBridgeOperationError("CreateAgent", createCtx, RequestTimeout, createStarted, err)
		createCancel()
		return nil, classifiedErr
	}
	createCancel()
	agentID := strings.TrimSpace(created.Msg.GetAgentId())
	if agentID == "" {
		return nil, errors.New("cursor-sdk-bridge CreateAgent returned an empty agent_id")
	}

	runTimeout := r.timeout
	if runTimeout <= 0 {
		runTimeout = AgentTimeout
	}
	// Native custom tools may suspend this run across multiple downstream HTTP
	// requests, so its lifetime cannot be derived from any one request context.
	runStarted := time.Now()
	runCtx, stopRun := context.WithTimeout(context.Background(), runTimeout)
	session := &sdkSession{
		agentID: agentID, apiKey: apiKey, workdir: client.workdir,
		client: client.agent, ctx: runCtx, cancel: stopRun,
		events: make(chan Event, 64), done: make(chan struct{}),
		tools:              make(map[string]struct{}, len(customTools)),
		inputTokenEstimate: request.InputTokenEstimate,
		captureRaw:         rawResponseCaptureEnabled(ctx),
	}
	for name := range customTools {
		session.tools[name] = struct{}{}
	}
	if err := r.registerSession(session); err != nil {
		stopRun()
		go r.deleteAgent(client.agent, agentID, client.workdir, apiKey)
		return nil, err
	}
	stream, err := client.agent.Send(runCtx, connect.NewRequest(&sdkv1.SendRequest{
		AgentId: agentID,
		Message: &sdkv1.UserMessage{Text: prompt},
		Options: &sdkv1.SendOptions{
			EnableDeltas: false,
			EnableSteps:  false,
		},
	}))
	if err != nil {
		classifiedErr := classifyBridgeOperationError("Send", runCtx, runTimeout, runStarted, err)
		session.abort(classifiedErr)
		r.unregisterSession(session)
		// Transfer the active-run reference to cleanup. A failed Send must return
		// immediately; DeleteAgent/CloseAgent have their own independent deadlines.
		finish = false
		go func() {
			defer r.active.Done()
			r.deleteAgent(client.agent, agentID, client.workdir, apiKey)
		}()
		return nil, classifiedErr
	}

	finish = false
	go r.consumeRun(session, stream, runTimeout, runStarted)
	return session.nextTurn(ctx, nil)
}

func cursorModelSelection(model string) *sdkv1.ModelSelection {
	selection := &sdkv1.ModelSelection{Id: model}
	const fastSuffix = "-fast"
	if !strings.HasSuffix(strings.ToLower(model), fastSuffix) {
		return selection
	}
	base := strings.TrimSpace(model[:len(model)-len(fastSuffix)])
	if base == "" {
		return selection
	}
	selection.Id = base
	selection.Params = []*sdkv1.ModelParameterValue{{Id: "fast", Value: "true"}}
	return selection
}

func (r *SDKRunner) resumeToolRun(
	ctx context.Context,
	credential *Credential,
	results []ToolResult,
	inputTokenEstimate int,
) (<-chan Event, error) {
	// Serialize this short state transition so concurrent retries cannot race
	// between the pending and completed-call indexes.
	r.resumeMu.Lock()
	defer r.resumeMu.Unlock()

	var session *sdkSession
	pendingResults := make([]ToolResult, 0, len(results))
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrBridgeClosed
	}
	for _, result := range results {
		callID := strings.TrimSpace(result.CallID)
		pending := r.calls[callID]
		pendingResolved := false
		if pending != nil {
			pending.mu.Lock()
			pendingResolved = pending.resolved
			pending.mu.Unlock()
		}
		if pending == nil || pending.session == nil || pendingResolved {
			completedSession := r.completedCalls[callID]
			if completedSession == nil {
				r.mu.Unlock()
				return nil, fmt.Errorf("%w: call_id %q", ErrToolSessionNotFound, result.CallID)
			}
			if session == nil {
				session = completedSession
			} else if session != completedSession {
				r.mu.Unlock()
				return nil, errors.New("tool results from different Cursor sessions cannot share one request")
			}
			continue
		}
		pendingResults = append(pendingResults, result)
		if session == nil {
			session = pending.session
		} else if session != pending.session {
			r.mu.Unlock()
			return nil, errors.New("tool results from different Cursor sessions cannot share one request")
		}
	}
	r.mu.Unlock()
	if session == nil {
		return nil, ErrToolSessionNotFound
	}
	if subtleAPIKeyMismatch(session.apiKey, credential.APIKey) {
		return nil, ErrToolSessionNotFound
	}
	if len(pendingResults) == 0 {
		session.setInputTokenEstimate(inputTokenEstimate)
		return replayCompletedToolTurn(ctx, session), nil
	}
	return session.nextTurn(ctx, func() error {
		session.setInputTokenEstimate(inputTokenEstimate)
		return r.resolveToolResults(session, pendingResults)
	})
}

func replayCompletedToolTurn(ctx context.Context, session *sdkSession) <-chan Event {
	output := make(chan Event, 1)
	go func() {
		defer close(output)
		select {
		case <-session.done:
		case <-ctx.Done():
			output <- Event{Done: true, Err: context.Cause(ctx), Replayed: true}
			return
		}
		terminal, ok := session.terminalEvent()
		if !ok {
			terminal = Event{Done: true, Err: ErrToolSessionNotFound}
		}
		terminal.Replayed = true
		terminal.Usage = nil
		output <- terminal
	}()
	return output
}

func (r *SDKRunner) resolveToolResults(session *sdkSession, results []ToolResult) error {
	type preparedResult struct {
		pending *pendingToolCall
		value   *structpb.Struct
	}
	prepared := make([]preparedResult, 0, len(results))
	values := make([]*structpb.Struct, len(results))
	for index, result := range results {
		value, err := callbackResult(result)
		if err != nil {
			return fmt.Errorf("encode result for Cursor custom tool %q: %w", result.CallID, err)
		}
		values[index] = value
	}

	r.mu.Lock()
	seen := make(map[*pendingToolCall]struct{}, len(results))
	for index, result := range results {
		pending := r.calls[strings.TrimSpace(result.CallID)]
		if pending == nil || pending.session != session {
			for _, item := range prepared {
				item.pending.mu.Unlock()
			}
			r.mu.Unlock()
			return fmt.Errorf("%w: call_id %q", ErrToolSessionNotFound, result.CallID)
		}
		if _, duplicate := seen[pending]; duplicate {
			for _, item := range prepared {
				item.pending.mu.Unlock()
			}
			r.mu.Unlock()
			return fmt.Errorf("tool result %q appears more than once", result.CallID)
		}
		seen[pending] = struct{}{}
		pending.mu.Lock()
		if pending.resolved {
			pending.mu.Unlock()
			for _, item := range prepared {
				item.pending.mu.Unlock()
			}
			r.mu.Unlock()
			return fmt.Errorf("tool result %q was already submitted", result.CallID)
		}
		prepared = append(prepared, preparedResult{pending: pending, value: values[index]})
	}
	for _, item := range prepared {
		item.pending.resolved = true
	}
	if r.completedCalls == nil {
		r.completedCalls = make(map[string]*sdkSession)
	}
	for index, item := range prepared {
		result := results[index]
		result.CallID = strings.TrimSpace(result.CallID)
		r.completedCalls[result.CallID] = item.pending.session
	}
	for _, item := range prepared {
		item.pending.mu.Unlock()
	}
	r.mu.Unlock()
	for _, item := range prepared {
		item.pending.result <- toolCallbackResult{value: item.value}
	}
	return nil
}

func subtleAPIKeyMismatch(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	return left == "" || right == "" || left != right
}

func (s *sdkSession) nextTurn(ctx context.Context, before func() error) (<-chan Event, error) {
	s.turnMu.Lock()
	if s.attached {
		s.turnMu.Unlock()
		return nil, errors.New("cursor tool session already has an attached request")
	}
	s.attached = true
	s.turnMu.Unlock()
	release := func() {
		s.turnMu.Lock()
		s.attached = false
		s.turnMu.Unlock()
	}
	if before != nil {
		if err := before(); err != nil {
			release()
			return nil, err
		}
	}

	output := make(chan Event, 16)
	go func() {
		defer close(output)
		defer release()
		var text strings.Builder
		var batchTimer *time.Timer
		var batch <-chan time.Time
		stopBatchTimer := func() {
			if batchTimer != nil && !batchTimer.Stop() {
				select {
				case <-batchTimer.C:
				default:
				}
			}
		}
		defer stopBatchTimer()
		deliver := func(event Event) bool {
			if event.Delta != "" {
				text.WriteString(event.Delta)
			}
			if event.Delta != "" || event.Text != "" || event.Done {
				event.Text = text.String()
			}
			select {
			case output <- event:
				return true
			case <-ctx.Done():
				s.cancel()
				return false
			}
		}
		for {
			select {
			case event := <-s.events:
				if !deliver(event) {
					return
				}
				if event.Done {
					return
				}
				if event.ToolCall != nil {
					stopBatchTimer()
					batchTimer = time.NewTimer(10 * time.Millisecond)
					batch = batchTimer.C
				}
			case <-batch:
				return
			case <-s.done:
				for {
					select {
					case event := <-s.events:
						if !deliver(event) {
							return
						}
					default:
						if terminal, ok := s.terminalEvent(); ok {
							deliver(terminal)
						}
						return
					}
				}
			case <-ctx.Done():
				s.cancel()
				select {
				case output <- Event{Text: text.String(), Done: true, Err: context.Cause(ctx)}:
				default:
				}
				return
			}
		}
	}()
	return output, nil
}

func (s *sdkSession) emit(event Event) bool {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.terminated {
		return false
	}
	select {
	case s.events <- event:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *sdkSession) finish(event Event) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.terminated {
		return
	}
	s.terminal = &event
	s.terminated = true
	close(s.done)
}

func (s *sdkSession) setInputTokenEstimate(value int) {
	if value <= 0 {
		return
	}
	s.eventMu.Lock()
	s.inputTokenEstimate = value
	s.eventMu.Unlock()
}

func (s *sdkSession) estimatedUsage() *Usage {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.inputTokenEstimate <= 0 {
		return nil
	}
	return &Usage{InputTokens: s.inputTokenEstimate, TotalTokens: s.inputTokenEstimate}
}

func (s *sdkSession) terminalEvent() (Event, bool) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.terminal == nil {
		return Event{}, false
	}
	return *s.terminal, true
}

func (s *sdkSession) abort(err error) {
	s.cancel()
	s.finish(Event{Done: true, Err: err})
}

func (s *sdkSession) allowsTool(name string) bool {
	_, ok := s.tools[name]
	return ok
}

func (r *SDKRunner) registerSession(session *sdkSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrBridgeClosed
	}
	if r.sessions == nil {
		r.sessions = make(map[string]*sdkSession)
	}
	if _, exists := r.sessions[session.agentID]; exists {
		return fmt.Errorf("cursor SDK returned duplicate agent_id %q", session.agentID)
	}
	r.sessions[session.agentID] = session
	return nil
}

func (r *SDKRunner) unregisterSession(session *sdkSession) {
	r.mu.Lock()
	if r.sessions[session.agentID] == session {
		delete(r.sessions, session.agentID)
	}
	r.mu.Unlock()
}

func (r *SDKRunner) sessionByAgentID(agentID string) *sdkSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[agentID]
}

func (r *SDKRunner) registerToolCall(callID string, pending *pendingToolCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrBridgeClosed
	}
	if r.calls == nil {
		r.calls = make(map[string]*pendingToolCall)
	}
	if _, exists := r.calls[callID]; exists {
		return fmt.Errorf("duplicate Cursor custom-tool call_id %q", callID)
	}
	r.calls[callID] = pending
	return nil
}

func (r *SDKRunner) unregisterToolCall(callID string, pending *pendingToolCall) {
	r.mu.Lock()
	if r.calls[callID] == pending {
		delete(r.calls, callID)
	}
	r.mu.Unlock()
}

func (r *SDKRunner) failAllSessions(err error) {
	r.mu.Lock()
	sessions := make([]*sdkSession, 0, len(r.sessions))
	for _, session := range r.sessions {
		sessions = append(sessions, session)
	}
	r.mu.Unlock()
	for _, session := range sessions {
		session.abort(err)
	}
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
	session *sdkSession,
	stream *connect.ServerStreamForClient[sdkv1.RunStreamMessage],
	runTimeout time.Duration,
	runStarted time.Time,
) {
	defer r.active.Done()
	runCtx := session.ctx
	client := session.client
	agentID := session.agentID
	canceller := newRunCanceller(client, agentID)
	state := &sdkRunState{
		agentID: agentID,
		onRunID: func(runID string) {
			canceller.SetRunID(runID)
		},
		onTerminal: canceller.Terminal,
	}
	runCallbackDone := make(chan struct{})
	stopRunCallback := context.AfterFunc(runCtx, func() {
		canceller.Request()
		close(runCallbackDone)
	})

	deliverEvent := func(event Event) {
		session.emit(event)
	}
	var consumeErr error
	for stream.Receive() {
		message := stream.Msg()
		if session.captureRaw {
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
	if state.status == sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_CANCELLED && runCtx.Err() != nil {
		consumeErr = context.Cause(runCtx)
	}
	if consumeErr != nil && errors.Is(context.Cause(runCtx), context.DeadlineExceeded) {
		consumeErr = classifyBridgeOperationError("Agent run", runCtx, runTimeout, runStarted, consumeErr)
	}
	if consumeErr != nil {
		canceller.Request()
	}

	session.cancel()
	if stopRunCallback() {
		close(runCallbackDone)
	} else {
		<-runCallbackDone
	}
	if state.status == sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED && !hasTokenUsage(state.usage) {
		if usage := loadRunUsage(context.Background(), client, state.runID, agentID, session.workdir, session.apiKey); hasTokenUsage(usage) {
			state.usage = usage
		}
	}
	finalEvent := Event{Text: state.text, Done: true, Err: consumeErr, Usage: state.usage}
	if !hasTokenUsage(finalEvent.Usage) {
		finalEvent.Usage = session.estimatedUsage()
		finalEvent.UsageEstimated = finalEvent.Usage != nil
	}
	session.finish(finalEvent)
	r.unregisterSession(session)
	r.expireCompletedToolTurns(session)

	// Cancellation and durable Agent deletion are cleanup, not response work.
	// Keep them tracked by r.active for orderly shutdown, but never make the
	// client or request-duration metric wait behind their independent deadlines.
	canceller.Wait()
	r.deleteAgent(client, agentID, session.workdir, session.apiKey)
}

func (r *SDKRunner) expireCompletedToolTurns(session *sdkSession) {
	retention := r.timeout
	if retention <= 0 {
		retention = AgentTimeout
	}
	time.AfterFunc(retention, func() {
		r.mu.Lock()
		for callID, completedSession := range r.completedCalls {
			if completedSession == session {
				delete(r.completedCalls, callID)
			}
		}
		r.mu.Unlock()
	})
}

func loadRunUsage(
	parentCtx context.Context,
	client sdkv1connect.SdkAgentServiceClient,
	runID, agentID, workdir, apiKey string,
) *Usage {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(parentCtx, BridgeCleanupTimeout)
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
	r.failAllSessions(ErrBridgeClosed)

	done := make(chan struct{})
	go func() {
		r.active.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		r.closeToolCallbackServer(ctx)
		_ = r.bridge.close(ctx)
		return context.Cause(ctx)
	case <-done:
	}
	r.closeToolCallbackServer(ctx)
	return r.bridge.close(ctx)
}

func (r *SDKRunner) closeToolCallbackServer(ctx context.Context) {
	r.callbackMu.Lock()
	server := r.callbackServer
	r.callbackServer = nil
	r.callbackListener = nil
	r.callbackClient = nil
	r.callbackMu.Unlock()
	if server != nil {
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
		}
	}
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
