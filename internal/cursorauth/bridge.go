package cursorauth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/cursorauth/sdkgen/sdk/v1/sdkv1connect"
)

type bridgeState uint8

const (
	bridgeIdle bridgeState = iota
	bridgeStarting
	bridgeRunning
	bridgeClosing
	bridgeClosed
)

const (
	bridgeRestartBaseDelay    = 100 * time.Millisecond
	bridgeRestartMaxDelay     = 5 * time.Second
	bridgeRestartStableWindow = 30 * time.Second
	bridgeExitObservationTime = 250 * time.Millisecond
)

type bridgeClient struct {
	agent   sdkv1connect.SdkAgentServiceClient
	cursor  sdkv1connect.SdkCursorServiceClient
	control sdkv1connect.SdkBridgeControlServiceClient
	workdir string
}

type bridgeStart struct {
	done    chan struct{}
	process *bridgeProcess
	err     error
	restart bool
	delay   time.Duration
}

type bridgeProcess struct {
	cmd       *exec.Cmd
	client    *bridgeClient
	root      string
	exited    chan struct{}
	waitErr   error
	startedAt time.Time
	cleanOnce sync.Once
}

func (p *bridgeProcess) cleanup() {
	if p == nil {
		return
	}
	p.cleanOnce.Do(func() { _ = os.RemoveAll(p.root) })
}

// bridge owns the single cursor-sdk-bridge child process for this ccLoad
// process. Its zero value is not usable; construct it with newBridge.
type bridge struct {
	mu        sync.Mutex
	state     bridgeState
	lifeCtx   context.Context
	lifeStop  context.CancelFunc
	start     *bridgeStart
	process   *bridgeProcess
	closeDone chan struct{}
	closeErr  error
	binary    string
	restarts  int

	lookPath     func(string) (string, error)
	command      func(string, ...string) *exec.Cmd
	spawnProcess func() (*bridgeProcess, error)
}

func newBridge(binaryPath ...string) *bridge {
	lifeCtx, lifeStop := context.WithCancel(context.Background())
	binary := ""
	if len(binaryPath) > 0 {
		binary = strings.TrimSpace(binaryPath[0])
	}
	bridge := &bridge{
		state:     bridgeIdle,
		lifeCtx:   lifeCtx,
		lifeStop:  lifeStop,
		closeDone: make(chan struct{}),
		binary:    binary,
		lookPath:  exec.LookPath,
		command:   exec.Command,
	}
	bridge.spawnProcess = bridge.spawn
	return bridge
}

func (b *bridge) client(ctx context.Context) (*bridgeClient, error) {
	if b == nil {
		return nil, errors.New("cursor-sdk-bridge owner is unavailable")
	}
	for {
		b.mu.Lock()
		switch b.state {
		case bridgeIdle:
			attempt := b.beginStartLocked(false, 0)
			go b.startProcess(attempt)
			b.mu.Unlock()
			if err := waitBridgeStart(ctx, attempt); err != nil {
				return nil, err
			}
		case bridgeStarting:
			attempt := b.start
			b.mu.Unlock()
			if err := waitBridgeStart(ctx, attempt); err != nil {
				return nil, err
			}
		case bridgeRunning:
			process := b.process
			select {
			case <-process.exited:
				attempt := b.restartExitedProcessLocked(process)
				b.mu.Unlock()
				process.cleanup()
				if attempt != nil {
					go b.startProcess(attempt)
					if err := waitBridgeStart(ctx, attempt); err != nil {
						return nil, err
					}
				}
				continue
			default:
				client := process.client
				b.mu.Unlock()
				return client, nil
			}
		case bridgeClosing, bridgeClosed:
			b.mu.Unlock()
			return nil, ErrBridgeClosed
		default:
			b.mu.Unlock()
			return nil, errors.New("invalid cursor-sdk-bridge state")
		}
	}
}

func (b *bridge) beginStartLocked(restart bool, delay time.Duration) *bridgeStart {
	attempt := &bridgeStart{
		done:    make(chan struct{}),
		restart: restart,
		delay:   delay,
	}
	b.start = attempt
	b.state = bridgeStarting
	return attempt
}

func (b *bridge) restartExitedProcessLocked(process *bridgeProcess) *bridgeStart {
	if b.state != bridgeRunning || b.process != process {
		return nil
	}
	b.process = nil
	if b.lifeCtx.Err() != nil {
		b.state = bridgeIdle
		return nil
	}
	if !process.startedAt.IsZero() && time.Since(process.startedAt) >= bridgeRestartStableWindow {
		b.restarts = 0
	}
	b.restarts++
	return b.beginStartLocked(true, bridgeRestartDelay(b.restarts))
}

func bridgeRestartDelay(restarts int) time.Duration {
	if restarts <= 1 {
		return 0
	}
	delay := bridgeRestartBaseDelay
	for attempt := 2; attempt < restarts && delay < bridgeRestartMaxDelay; attempt++ {
		delay *= 2
	}
	if delay > bridgeRestartMaxDelay {
		return bridgeRestartMaxDelay
	}
	return delay
}

func waitBridgeStart(ctx context.Context, attempt *bridgeStart) error {
	if attempt == nil {
		return errors.New("cursor-sdk-bridge start state is missing")
	}
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-attempt.done:
		return attempt.err
	}
}

func (b *bridge) startProcess(attempt *bridgeStart) {
	if attempt.delay > 0 {
		timer := time.NewTimer(attempt.delay)
		select {
		case <-timer.C:
		case <-b.lifeCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			attempt.err = ErrBridgeClosed
			close(attempt.done)
			return
		}
	}

	// A zero-delay restart can be queued immediately before Close cancels the
	// bridge lifecycle. Gate every start, not only delayed ones, so that queued
	// work cannot spawn a child after shutdown has already taken ownership.
	b.mu.Lock()
	canStart := b.state == bridgeStarting && b.start == attempt && b.lifeCtx.Err() == nil
	b.mu.Unlock()
	if !canStart {
		attempt.err = ErrBridgeClosed
		close(attempt.done)
		return
	}

	process, err := b.spawnProcess()
	if err != nil {
		attempt.err = err
		var retry *bridgeStart
		b.mu.Lock()
		if b.state == bridgeStarting && b.start == attempt {
			if attempt.restart && b.lifeCtx.Err() == nil {
				b.restarts++
				retry = b.beginStartLocked(true, bridgeRestartDelay(b.restarts))
			} else {
				b.state = bridgeIdle
			}
		}
		b.mu.Unlock()
		close(attempt.done)
		if attempt.restart {
			if retry != nil {
				log.Printf(
					"[WARN] cursor-sdk-bridge automatic restart failed: %v; retrying in %s",
					err,
					retry.delay,
				)
				go b.startProcess(retry)
			} else if b.lifeCtx.Err() == nil {
				log.Printf("[WARN] cursor-sdk-bridge automatic restart failed: %v", err)
			}
		}
		return
	}
	if process.startedAt.IsZero() {
		process.startedAt = time.Now()
	}

	attempt.process = process
	published := false
	b.mu.Lock()
	if b.state == bridgeStarting && b.start == attempt {
		b.process = process
		b.state = bridgeRunning
		published = true
	} else {
		attempt.err = ErrBridgeClosed
	}
	b.mu.Unlock()
	close(attempt.done)

	if !published {
		_ = process.cmd.Process.Kill()
		<-process.exited
		process.cleanup()
		return
	}
	go b.watchProcess(process)
}

func (b *bridge) watchProcess(process *bridgeProcess) {
	<-process.exited
	b.mu.Lock()
	attempt := b.restartExitedProcessLocked(process)
	b.mu.Unlock()
	process.cleanup()
	if attempt == nil {
		return
	}
	if process.waitErr != nil {
		log.Printf(
			"[WARN] cursor-sdk-bridge exited unexpectedly: %v; restarting in %s",
			process.waitErr,
			attempt.delay,
		)
	} else {
		log.Printf("[WARN] cursor-sdk-bridge exited unexpectedly; restarting in %s", attempt.delay)
	}
	go b.startProcess(attempt)
}

// replacementClient returns a new client only after the failed client has
// been detached from the managed process. It never kills a live bridge merely
// because one RPC failed.
func (b *bridge) replacementClient(
	ctx context.Context,
	failed *bridgeClient,
) (*bridgeClient, bool, error) {
	if b == nil || failed == nil {
		return nil, false, nil
	}
	b.mu.Lock()
	if b.state == bridgeClosing || b.state == bridgeClosed {
		b.mu.Unlock()
		return nil, false, ErrBridgeClosed
	}
	process := b.process
	waitForExit := b.state == bridgeRunning && process != nil && process.client == failed
	b.mu.Unlock()
	if waitForExit {
		timer := time.NewTimer(bridgeExitObservationTime)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, false, context.Cause(ctx)
		case <-process.exited:
		case <-timer.C:
			b.mu.Lock()
			stillCurrent := b.state == bridgeRunning && b.process == process
			b.mu.Unlock()
			if stillCurrent {
				return nil, false, nil
			}
		}
	}

	client, err := b.client(ctx)
	return client, true, err
}

func (b *bridge) spawn() (*bridgeProcess, error) {
	binary, err := b.bridgePath()
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp("", "ccload-cursor-sdk-")
	if err != nil {
		return nil, fmt.Errorf("create cursor-sdk-bridge workspace: %w", err)
	}
	fail := func(err error) (*bridgeProcess, error) {
		_ = os.RemoveAll(root)
		return nil, err
	}
	workspace := filepath.Join(root, "workspace")
	stateRoot := bridgeStateRoot()
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return fail(fmt.Errorf("create cursor-sdk-bridge workspace: %w", err))
	}
	if err := prepareBridgeStateRoot(stateRoot); err != nil {
		return fail(err)
	}

	cmd := b.command(binary,
		"--host", "127.0.0.1",
		"--port", "0",
		"--workspace", workspace,
		"--state-root", stateRoot,
		"--local-store", `{"type":"sqlite"}`,
	)
	cmd.Dir = workspace
	cmd.Env = bridgeEnvironment()
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fail(fmt.Errorf("open cursor-sdk-bridge stderr: %w", err))
	}
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("%w: start %s: %w", ErrAgentMissing, binary, err))
	}

	process := &bridgeProcess{cmd: cmd, root: root, exited: make(chan struct{})}
	go func() {
		process.waitErr = cmd.Wait()
		close(process.exited)
	}()
	readyCh := scanBridgeReady(stderr)
	startCtx, cancel := context.WithTimeout(b.lifeCtx, BridgeStartupTimeout)
	defer cancel()

	var ready *bridgeReady
	select {
	case <-startCtx.Done():
		_ = cmd.Process.Kill()
		<-process.exited
		process.cleanup()
		return nil, fmt.Errorf("cursor-sdk-bridge startup: %w", context.Cause(startCtx))
	case <-process.exited:
		process.cleanup()
		if process.waitErr != nil {
			return nil, fmt.Errorf("cursor-sdk-bridge exited during startup: %w", process.waitErr)
		}
		return nil, errors.New("cursor-sdk-bridge exited during startup")
	case result := <-readyCh:
		if result.err != nil {
			_ = cmd.Process.Kill()
			<-process.exited
			process.cleanup()
			return nil, result.err
		}
		ready = result.ready
	}
	if ready.PID != cmd.Process.Pid {
		_ = cmd.Process.Kill()
		<-process.exited
		process.cleanup()
		return nil, fmt.Errorf("cursor-sdk-bridge ready pid %d does not match child pid %d", ready.PID, cmd.Process.Pid)
	}
	tokenBytes, err := os.ReadFile(ready.AuthTokenFile)
	if err != nil {
		_ = cmd.Process.Kill()
		<-process.exited
		process.cleanup()
		return nil, fmt.Errorf("read cursor-sdk-bridge auth token: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || len(token) > 64<<10 {
		_ = cmd.Process.Kill()
		<-process.exited
		process.cleanup()
		return nil, errors.New("cursor-sdk-bridge auth token is invalid")
	}
	client := newBridgeClient(ready.URL, token)
	client.workdir = workspace
	if err := validateBridgeClient(startCtx, client); err != nil {
		_ = cmd.Process.Kill()
		<-process.exited
		process.cleanup()
		return nil, err
	}
	process.client = client
	return process, nil
}

type bridgeReadyResult struct {
	ready *bridgeReady
	err   error
}

func scanBridgeReady(stderr io.Reader) <-chan bridgeReadyResult {
	result := make(chan bridgeReadyResult, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		delivered := false
		for scanner.Scan() {
			ready, matched, err := parseBridgeReadyLine(scanner.Text())
			if !matched || delivered {
				continue
			}
			delivered = true
			result <- bridgeReadyResult{ready: ready, err: err}
		}
		if !delivered {
			err := scanner.Err()
			if err == nil {
				err = errors.New("cursor-sdk-bridge exited without a ready line")
			}
			result <- bridgeReadyResult{err: err}
		}
	}()
	return result
}

func newBridgeClient(baseURL, token string) *bridgeClient {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2: false,
	}
	httpClient := &http.Client{Transport: &bridgeAuthTransport{token: token, next: transport}}
	return &bridgeClient{
		agent:   sdkv1connect.NewSdkAgentServiceClient(httpClient, baseURL),
		cursor:  sdkv1connect.NewSdkCursorServiceClient(httpClient, baseURL),
		control: sdkv1connect.NewSdkBridgeControlServiceClient(httpClient, baseURL),
	}
}

type bridgeAuthTransport struct {
	token string
	next  http.RoundTripper
}

func (t *bridgeAuthTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.next.RoundTrip(clone)
}

func validateBridgeClient(ctx context.Context, client *bridgeClient) error {
	if _, err := client.control.Ping(ctx, connect.NewRequest(&sdkv1.PingRequest{})); err != nil {
		return fmt.Errorf("cursor-sdk-bridge Ping: %w", err)
	}
	response, err := client.control.GetVersion(ctx, connect.NewRequest(&sdkv1.GetVersionRequest{}))
	if err != nil {
		return fmt.Errorf("cursor-sdk-bridge GetVersion: %w", err)
	}
	if response.Msg.GetProtocolVersion() != BridgeProtocol {
		return fmt.Errorf("cursor-sdk-bridge protocol %q, need %q", response.Msg.GetProtocolVersion(), BridgeProtocol)
	}
	for _, capability := range requiredBridgeCapabilities {
		if !slices.Contains(response.Msg.GetCapabilities(), capability) {
			return fmt.Errorf("cursor-sdk-bridge lacks required capability %q", capability)
		}
	}
	return nil
}

func (b *bridge) bridgePath() (string, error) {
	name := "cursor-sdk-bridge"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if b != nil && b.binary != "" {
		return usableAbsoluteBridgePath(b.binary, runtime.GOOS, "configured bridge path")
	}
	if override := strings.TrimSpace(os.Getenv("CURSOR_SDK_BRIDGE_BIN")); override != "" {
		return usableAbsoluteBridgePath(override, runtime.GOOS, "CURSOR_SDK_BRIDGE_BIN")
	}
	if executable, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(executable), name)
		if isUsableBridgeFile(sibling, runtime.GOOS) {
			return absoluteBridgePath(sibling)
		}
	}
	managed := managedBridgeBinaryPath(runtime.GOOS)
	if isUsableBridgeFile(managed, runtime.GOOS) {
		return absoluteBridgePath(managed)
	}
	path, err := b.lookPath(name)
	if err != nil || strings.TrimSpace(path) == "" {
		return "", fmt.Errorf(
			"%w: restart ccload to install %s automatically, or place it beside ccload/on PATH",
			ErrAgentMissing,
			name,
		)
	}
	return absoluteBridgePath(path)
}

func usableAbsoluteBridgePath(path, goos, source string) (string, error) {
	if !isUsableBridgeFile(path, goos) {
		return "", fmt.Errorf("%w: %s %q is invalid", ErrAgentMissing, source, path)
	}
	return absoluteBridgePath(path)
}

func absoluteBridgePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve cursor-sdk-bridge path %q: %w", path, err)
	}
	return filepath.Clean(absolute), nil
}

func bridgeEnvironment() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(strings.ToUpper(key), "CURSOR_") {
			continue
		}
		env = append(env, value)
	}
	return append(env, "CURSOR_SDK_CLIENT_LANGUAGE=go")
}

func bridgeStateRoot() string {
	candidates := make([]string, 0, 3)
	if database := strings.TrimSpace(os.Getenv("SQLITE_PATH")); database != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(database), "cursor-sdk"))
	}

	if cache, err := os.UserCacheDir(); err == nil && strings.TrimSpace(cache) != "" {
		candidates = append(candidates, filepath.Join(cache, "ccload", "cursor-sdk"))
	}
	candidates = append(candidates, temporaryBridgeStateRoot())
	return firstWritableBridgeStateRoot(candidates)
}

func temporaryBridgeStateRoot() string {
	return absoluteBridgeStateRoot(filepath.Join(os.TempDir(), "ccload", "cursor-sdk"))
}

func firstWritableBridgeStateRoot(candidates []string) string {
	for _, candidate := range candidates {
		root := absoluteBridgeStateRoot(candidate)
		if err := prepareBridgeStateRoot(root); err == nil {
			return root
		}
	}
	return absoluteBridgeStateRoot(candidates[len(candidates)-1])
}

func absoluteBridgeStateRoot(root string) string {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return filepath.Clean(root)
	}
	return filepath.Clean(absolute)
}

func prepareBridgeStateRoot(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create cursor-sdk-bridge state root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("secure cursor-sdk-bridge state root: %w", err)
	}
	probe, err := os.CreateTemp(root, ".write-probe-*")
	if err != nil {
		return fmt.Errorf("verify cursor-sdk-bridge state root: %w", err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probePath)
		return fmt.Errorf("verify cursor-sdk-bridge state root: %w", err)
	}
	if err := os.Remove(probePath); err != nil {
		return fmt.Errorf("clean cursor-sdk-bridge state root probe: %w", err)
	}
	return nil
}

func (b *bridge) close(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	switch b.state {
	case bridgeClosed:
		err := b.closeErr
		b.mu.Unlock()
		return err
	case bridgeClosing:
		done := b.closeDone
		b.mu.Unlock()
		return waitBridgeClose(ctx, done, b)
	default:
		b.state = bridgeClosing
		b.lifeStop()
		done := b.closeDone
		go b.closeProcess()
		b.mu.Unlock()
		return waitBridgeClose(ctx, done, b)
	}
}

func waitBridgeClose(ctx context.Context, done <-chan struct{}, bridge *bridge) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-done:
		bridge.mu.Lock()
		err := bridge.closeErr
		bridge.mu.Unlock()
		return err
	}
}

func (b *bridge) closeProcess() {
	deadline := time.Now().Add(BridgeShutdownGrace)
	b.mu.Lock()
	attempt := b.start
	process := b.process
	b.mu.Unlock()

	if process == nil && attempt != nil {
		// Cancellation makes production starts bounded, but the owner must still
		// wait for the in-flight attempt. Declaring the bridge closed while that
		// goroutine can later publish or kill a child is a false shutdown result.
		<-attempt.done
		process = attempt.process
	}

	var closeErr error
	var shutdownErr error
	if process != nil {
		if process.client != nil && time.Now().Before(deadline) {
			shutdownCtx, cancel := context.WithDeadline(context.Background(), deadline)
			_, err := process.client.control.Shutdown(shutdownCtx, connect.NewRequest(&sdkv1.ShutdownRequest{
				GraceSeconds: uint32(BridgeShutdownGrace / time.Second),
			}))
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				shutdownErr = err
			}
		}
		exitedNaturally := false
		remaining := time.Until(deadline)
		if remaining > 0 {
			timer := time.NewTimer(remaining)
			select {
			case <-process.exited:
				exitedNaturally = true
			case <-timer.C:
				_ = process.cmd.Process.Kill()
				<-process.exited
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		} else {
			_ = process.cmd.Process.Kill()
			<-process.exited
		}
		// Shutdown deliberately terminates the bridge process. Some bridge
		// versions close their listener before Connect can deliver the response,
		// which surfaces as EOF/connection reset even though the child exits 0.
		// When the Shutdown transport fails, the process exit status is the
		// authoritative result.
		if shutdownErr != nil && (!exitedNaturally || process.waitErr != nil) {
			closeErr = fmt.Errorf("shutdown cursor-sdk-bridge: %w", shutdownErr)
		}
		process.cleanup()
	}

	b.mu.Lock()
	b.process = nil
	b.state = bridgeClosed
	b.closeErr = closeErr
	close(b.closeDone)
	b.mu.Unlock()
}
