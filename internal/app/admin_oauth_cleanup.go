package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/testutil"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	oauthCredentialCleanupJobRunning   = "running"
	oauthCredentialCleanupJobSucceeded = "succeeded"
	oauthCredentialCleanupJobFailed    = "failed"
	oauthCredentialCleanupJobCancelled = "cancelled"

	oauthCredentialCleanupJobRetention   = time.Hour
	oauthCredentialCleanupMaxRunningJobs = 1
	oauthCredentialCleanupWorkers        = 8

	oauthCredentialCleanupActionDisable = "disable"
	oauthCredentialCleanupActionDelete  = "delete"
)

var (
	errOAuthCredentialCleanupJobsBusy        = errors.New("an OAuth credential cleanup job is already running")
	errOAuthCredentialCleanupJobsClosed      = errors.New("OAuth credential cleanup jobs are shutting down")
	errOAuthCredentialCleanupRequestConflict = errors.New("Idempotency-Key is already bound to a different cleanup selection")
)

type oauthCredentialCleanupJobStart struct {
	JobID    string `json:"job_id"`
	Total    int    `json:"total"`
	AuthType string `json:"auth_type"`
	Model    string `json:"model"`
	Action   string `json:"action"`
}

type oauthCredentialCleanupJobRequest struct {
	AuthType string `json:"auth_type"`
	Model    string `json:"model"`
	Action   string `json:"action"`
	// Reject stale clients explicitly: silently ignoring channel_id would
	// unexpectedly broaden a single-channel request to every provider channel.
	ChannelID *int64 `json:"channel_id"`
}

type oauthCredentialCleanupOptions struct {
	AuthType     string   `json:"auth_type"`
	ChannelCount int      `json:"channel_count"`
	Models       []string `json:"models"`
}

type oauthCredentialCleanupCounts struct {
	Healthy   int `json:"healthy"`
	Refreshed int `json:"refreshed"`
	Disabled  int `json:"disabled"`
	Deleted   int `json:"deleted"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

type oauthCredentialCleanupEvent struct {
	Event     string `json:"event"`
	Sequence  int    `json:"sequence"`
	JobID     string `json:"job_id"`
	Processed int    `json:"processed"`
	Total     int    `json:"total"`
	oauthCredentialCleanupCounts

	ChannelID   int64    `json:"channel_id,omitempty"`
	ChannelName string   `json:"channel_name,omitempty"`
	AuthType    string   `json:"auth_type,omitempty"`
	Models      []string `json:"models,omitempty"`
	Model       string   `json:"model,omitempty"`
	Status      string   `json:"status,omitempty"`
	StatusCode  int      `json:"status_code,omitempty"`
	Error       string   `json:"error,omitempty"`
}

type oauthCredentialCleanupJobView struct {
	Status string                        `json:"status"`
	Events []oauthCredentialCleanupEvent `json:"events"`
	Next   int                           `json:"next"`
	Error  string                        `json:"error,omitempty"`
}

type oauthCredentialCleanupJob struct {
	mu              sync.Mutex
	destructiveMu   sync.Mutex
	id              string
	requestID       string
	authType        string
	model           string
	action          string
	status          string
	total           int
	processed       int
	counts          oauthCredentialCleanupCounts
	events          []oauthCredentialCleanupEvent
	err             string
	completedAt     *time.Time
	cancel          context.CancelFunc
	cancelRequested bool
}

func (j *oauthCredentialCleanupJob) appendEvent(event oauthCredentialCleanupEvent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.appendEventLocked(event)
}

func (j *oauthCredentialCleanupJob) appendEventLocked(event oauthCredentialCleanupEvent) {
	event.Sequence = len(j.events) + 1
	event.JobID = j.id
	event.Processed = j.processed
	event.Total = j.total
	event.oauthCredentialCleanupCounts = j.counts
	event.Models = append([]string(nil), event.Models...)
	j.events = append(j.events, event)
}

func (j *oauthCredentialCleanupJob) finishChannel(event oauthCredentialCleanupEvent) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cancelRequested && event.Status != "deleted" && event.Status != "disabled" {
		return false
	}

	j.processed++
	switch event.Status {
	case "healthy":
		j.counts.Healthy++
	case "refreshed":
		j.counts.Refreshed++
	case "disabled":
		j.counts.Disabled++
	case "deleted":
		j.counts.Deleted++
	case "skipped":
		j.counts.Skipped++
	default:
		j.counts.Failed++
	}
	event.Event = "progress"
	j.appendEventLocked(event)
	return true
}

func (j *oauthCredentialCleanupJob) finishRun(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	switch {
	case j.cancelRequested || errors.Is(err, context.Canceled):
		j.status = oauthCredentialCleanupJobCancelled
		j.err = ""
	case err != nil:
		j.status = oauthCredentialCleanupJobFailed
		j.err = err.Error()
	default:
		j.status = oauthCredentialCleanupJobSucceeded
	}
	now := time.Now()
	j.completedAt = &now
	j.appendEventLocked(oauthCredentialCleanupEvent{
		Event:  "complete",
		Status: j.status,
		Error:  j.err,
	})
}

func (j *oauthCredentialCleanupJob) mutateIfNotCancelled(
	action func() (bool, error),
) (bool, error) {
	j.destructiveMu.Lock()
	defer j.destructiveMu.Unlock()
	j.mu.Lock()
	cancelled := j.cancelRequested
	j.mu.Unlock()
	if cancelled {
		return false, context.Canceled
	}
	return action()
}

func (j *oauthCredentialCleanupJob) view(after int) (oauthCredentialCleanupJobView, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if after < 0 || after > len(j.events) {
		return oauthCredentialCleanupJobView{}, fmt.Errorf("after must be between 0 and %d", len(j.events))
	}
	return oauthCredentialCleanupJobView{
		Status: j.status,
		Events: append([]oauthCredentialCleanupEvent(nil), j.events[after:]...),
		Next:   len(j.events),
		Error:  j.err,
	}, nil
}

type oauthCredentialCleanupJobManager struct {
	parentCtx context.Context

	mu       sync.Mutex
	jobs     map[string]*oauthCredentialCleanupJob
	requests map[string]string
	running  int
	closing  bool
	wg       sync.WaitGroup
}

func newOAuthCredentialCleanupJobManager(parentCtx context.Context) *oauthCredentialCleanupJobManager {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	return &oauthCredentialCleanupJobManager{
		parentCtx: parentCtx,
		jobs:      make(map[string]*oauthCredentialCleanupJob),
		requests:  make(map[string]string),
	}
}

func (m *oauthCredentialCleanupJobManager) Start(
	server *Server,
	configs []*model.Config,
	requestID string,
	authType string,
	modelName string,
	action string,
) (oauthCredentialCleanupJobStart, error) {
	if server == nil {
		return oauthCredentialCleanupJobStart{}, errors.New("OAuth credential cleanup is unavailable")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing || m.parentCtx.Err() != nil {
		return oauthCredentialCleanupJobStart{}, errOAuthCredentialCleanupJobsClosed
	}
	now := time.Now()
	m.evictExpiredLocked(now)
	if started, ok, err := m.startForRequestLocked(requestID, authType, modelName, action); ok || err != nil {
		return started, err
	}
	if m.running >= oauthCredentialCleanupMaxRunningJobs {
		return oauthCredentialCleanupJobStart{}, errOAuthCredentialCleanupJobsBusy
	}

	ctx, cancel := context.WithCancel(m.parentCtx)
	job := &oauthCredentialCleanupJob{
		id:        "occj_" + uuid.New().String(),
		requestID: requestID,
		authType:  authType,
		model:     modelName,
		action:    action,
		status:    oauthCredentialCleanupJobRunning,
		total:     len(configs),
		events:    make([]oauthCredentialCleanupEvent, 0, len(configs)*2+2),
		cancel:    cancel,
	}
	job.appendEvent(oauthCredentialCleanupEvent{Event: "start"})
	m.jobs[job.id] = job
	if requestID != "" {
		m.requests[requestID] = job.id
	}
	m.running++
	m.wg.Add(1)

	go func() {
		defer cancel()
		err := server.runOAuthCredentialCleanup(ctx, job, configs, modelName)
		m.finishRunningJob(job, err)
	}()

	return job.start(), nil
}

func (j *oauthCredentialCleanupJob) start() oauthCredentialCleanupJobStart {
	return oauthCredentialCleanupJobStart{
		JobID: j.id, Total: j.total, AuthType: j.authType, Model: j.model, Action: j.action,
	}
}

func (m *oauthCredentialCleanupJobManager) startForRequest(
	requestID, authType, modelName, action string,
) (oauthCredentialCleanupJobStart, bool, error) {
	if requestID == "" {
		return oauthCredentialCleanupJobStart{}, false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evictExpiredLocked(time.Now())
	return m.startForRequestLocked(requestID, authType, modelName, action)
}

func (m *oauthCredentialCleanupJobManager) startForRequestLocked(
	requestID, authType, modelName, action string,
) (oauthCredentialCleanupJobStart, bool, error) {
	id, ok := m.requests[requestID]
	if !ok {
		return oauthCredentialCleanupJobStart{}, false, nil
	}
	job, ok := m.jobs[id]
	if !ok {
		delete(m.requests, requestID)
		return oauthCredentialCleanupJobStart{}, false, nil
	}
	if job.authType != authType || job.model != modelName || job.action != action {
		return oauthCredentialCleanupJobStart{}, false, errOAuthCredentialCleanupRequestConflict
	}
	return job.start(), true, nil
}

func (m *oauthCredentialCleanupJobManager) Cancel(id string) (string, bool) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return "", false
	}
	job.mu.Lock()
	if job.status != oauthCredentialCleanupJobRunning {
		status := job.status
		job.mu.Unlock()
		return status, true
	}
	job.cancelRequested = true
	job.cancel()
	job.mu.Unlock()

	// Wait for any mutation that crossed the gate before cancellation. Once this
	// returns, no later channel mutation can begin for this job.
	job.destructiveMu.Lock()
	job.mu.Lock()
	status := job.status
	job.mu.Unlock()
	job.destructiveMu.Unlock()
	if status == oauthCredentialCleanupJobRunning {
		return "cancelling", true
	}
	return status, true
}

func (m *oauthCredentialCleanupJobManager) Get(
	id string,
	after int,
) (oauthCredentialCleanupJobView, bool, error) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return oauthCredentialCleanupJobView{}, false, nil
	}
	view, err := job.view(after)
	return view, true, err
}

func (m *oauthCredentialCleanupJobManager) finishRunningJob(
	job *oauthCredentialCleanupJob,
	err error,
) {
	m.mu.Lock()
	job.finishRun(err)
	m.running--
	m.mu.Unlock()
	m.wg.Done()
}

func (m *oauthCredentialCleanupJobManager) evictExpiredLocked(now time.Time) {
	for id, job := range m.jobs {
		job.mu.Lock()
		completedAt := job.completedAt
		job.mu.Unlock()
		if completedAt != nil && now.Sub(*completedAt) >= oauthCredentialCleanupJobRetention {
			delete(m.jobs, id)
			if job.requestID != "" {
				delete(m.requests, job.requestID)
			}
		}
	}
}

func (m *oauthCredentialCleanupJobManager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closing = true
	cancels := make([]context.CancelFunc, 0, m.running)
	for _, job := range m.jobs {
		job.mu.Lock()
		if job.status == oauthCredentialCleanupJobRunning {
			cancels = append(cancels, job.cancel)
		}
		job.mu.Unlock()
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) ensureOAuthCredentialCleanupJobs() *oauthCredentialCleanupJobManager {
	s.oauthCredentialCleanupJobsMu.Lock()
	defer s.oauthCredentialCleanupJobsMu.Unlock()
	if s.oauthCredentialCleanupJobs == nil {
		s.oauthCredentialCleanupJobs = newOAuthCredentialCleanupJobManager(s.baseCtx)
	}
	return s.oauthCredentialCleanupJobs
}

func (s *Server) currentOAuthCredentialCleanupJobs() *oauthCredentialCleanupJobManager {
	s.oauthCredentialCleanupJobsMu.Lock()
	defer s.oauthCredentialCleanupJobsMu.Unlock()
	return s.oauthCredentialCleanupJobs
}

func normalizeOAuthCredentialCleanupAuthType(value string) string {
	authType := model.NormalizeAuthType(value)
	switch authType {
	case model.AuthTypeCodexOAuth, model.AuthTypeAntigravityOAuth,
		model.AuthTypeXAIOAuth, model.AuthTypeAnthropicOAuth,
		model.AuthTypeZAIOAuth, model.AuthTypeCursorOAuth, model.AuthTypeZedOAuth:
		return authType
	default:
		return ""
	}
}

func normalizeOAuthCredentialCleanupModel(value string) string {
	modelName := strings.TrimSpace(value)
	if modelName == "" || modelName == "*" || len(modelName) > 255 {
		return ""
	}
	return modelName
}

func normalizeOAuthCredentialCleanupAction(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "", oauthCredentialCleanupActionDisable:
		return oauthCredentialCleanupActionDisable
	case oauthCredentialCleanupActionDelete:
		return oauthCredentialCleanupActionDelete
	default:
		return ""
	}
}

func oauthCredentialCleanupScope(
	configs []*model.Config,
	authType string,
) ([]*model.Config, []string) {
	oauthConfigs := make([]*model.Config, 0, len(configs))
	models := make(map[string]struct{})
	for _, cfg := range configs {
		if cfg == nil || cfg.GetAuthType() != authType || strings.TrimSpace(cfg.OAuthCredential) == "" {
			continue
		}
		oauthConfigs = append(oauthConfigs, cfg)
		for _, modelName := range cfg.GetModels() {
			if modelName = normalizeOAuthCredentialCleanupModel(modelName); modelName != "" {
				models[modelName] = struct{}{}
			}
		}
	}
	sort.Slice(oauthConfigs, func(i, j int) bool {
		left, right := oauthConfigs[i], oauthConfigs[j]
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		leftName := strings.ToLower(strings.TrimSpace(left.Name))
		rightName := strings.ToLower(strings.TrimSpace(right.Name))
		if leftName != rightName {
			return leftName < rightName
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return left.ID < right.ID
	})
	modelNames := make([]string, 0, len(models))
	for modelName := range models {
		modelNames = append(modelNames, modelName)
	}
	sort.Strings(modelNames)
	return oauthConfigs, modelNames
}

// HandleOAuthCredentialCleanupOptions returns the de-duplicated concrete model
// union for every channel using one OAuth provider.
func (s *Server) HandleOAuthCredentialCleanupOptions(c *gin.Context) {
	authType := normalizeOAuthCredentialCleanupAuthType(c.Query("auth_type"))
	if authType == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "a supported OAuth auth_type is required")
		return
	}
	configs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	oauthConfigs, modelNames := oauthCredentialCleanupScope(configs, authType)
	RespondJSON(c, http.StatusOK, oauthCredentialCleanupOptions{
		AuthType: authType, ChannelCount: len(oauthConfigs), Models: modelNames,
	})
}

// HandleStartOAuthCredentialCleanupJob starts one server-owned cleanup. The
// destructive work is deliberately detached from the upload connection so an
// SSE disconnect cannot leave a half-cleaned credential set.
func (s *Server) HandleStartOAuthCredentialCleanupJob(c *gin.Context) {
	requestID := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if requestID == "" {
		requestID = uuid.NewString()
	} else if !validOAuthCredentialCleanupRequestID(requestID) {
		RespondErrorMsg(c, http.StatusBadRequest, "Idempotency-Key must contain 1-128 letters, digits, dots, underscores, colons, or hyphens")
		return
	}
	manager := s.ensureOAuthCredentialCleanupJobs()
	var input oauthCredentialCleanupJobRequest
	if err := c.ShouldBindJSON(&input); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "auth_type and model are required")
		return
	}
	authType := normalizeOAuthCredentialCleanupAuthType(input.AuthType)
	modelName := normalizeOAuthCredentialCleanupModel(input.Model)
	action := normalizeOAuthCredentialCleanupAction(input.Action)
	if authType == "" || modelName == "" || action == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "auth_type must be a supported OAuth type, model must be concrete, and action must be disable or delete when provided")
		return
	}
	if input.ChannelID != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "channel_id is not supported; cleanup always tests every channel for the selected OAuth auth_type")
		return
	}
	if started, ok, err := manager.startForRequest(requestID, authType, modelName, action); err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	} else if ok {
		RespondJSON(c, http.StatusAccepted, started)
		return
	}
	configs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	oauthConfigs, modelNames := oauthCredentialCleanupScope(configs, authType)
	modelIndex := sort.SearchStrings(modelNames, modelName)
	if modelIndex >= len(modelNames) || modelNames[modelIndex] != modelName {
		RespondErrorMsg(c, http.StatusBadRequest, "the selected model is not configured on any channel for this OAuth auth_type")
		return
	}
	started, err := manager.Start(s, oauthConfigs, requestID, authType, modelName, action)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, errOAuthCredentialCleanupRequestConflict) {
			status = http.StatusConflict
		} else if errors.Is(err, errOAuthCredentialCleanupJobsBusy) {
			status = http.StatusTooManyRequests
		}
		RespondError(c, status, err)
		return
	}
	RespondJSON(c, http.StatusAccepted, started)
}

// HandleCancelOAuthCredentialCleanupJob cancels queued tests and prevents an
// in-flight refresh from mutating a channel once it returns.
func (s *Server) HandleCancelOAuthCredentialCleanupJob(c *gin.Context) {
	manager := s.currentOAuthCredentialCleanupJobs()
	if manager == nil {
		RespondErrorMsg(c, http.StatusNotFound, "OAuth credential cleanup job not found")
		return
	}
	status, ok := manager.Cancel(c.Param("id"))
	if !ok {
		RespondErrorMsg(c, http.StatusNotFound, "OAuth credential cleanup job not found")
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"job_id": c.Param("id"), "status": status})
}

func validOAuthCredentialCleanupRequestID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("._:-", char) {
			continue
		}
		return false
	}
	return true
}

// HandleOAuthCredentialCleanupStream streams retained events after the given
// sequence. Reconnecting with after=N resumes the same background job.
func (s *Server) HandleOAuthCredentialCleanupStream(c *gin.Context) {
	after := 0
	if rawAfter := c.Query("after"); rawAfter != "" {
		parsed, err := strconv.Atoi(rawAfter)
		if err != nil || parsed < 0 {
			RespondErrorMsg(c, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = parsed
	}
	manager := s.currentOAuthCredentialCleanupJobs()
	if manager == nil {
		RespondErrorMsg(c, http.StatusNotFound, "OAuth credential cleanup job not found")
		return
	}
	if _, ok, err := manager.Get(c.Param("id"), after); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	} else if !ok {
		RespondErrorMsg(c, http.StatusNotFound, "OAuth credential cleanup job not found")
		return
	}

	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Header("X-Content-Type-Options", "nosniff")
	disableResponseWriteTimeout(c.Writer, "OAuth credential cleanup stream")
	c.Status(http.StatusOK)

	cursor := after
	for {
		view, ok, err := manager.Get(c.Param("id"), cursor)
		if err != nil {
			_ = writeSSEEvent(c, "error", oauthCredentialCleanupEvent{Event: "error", Error: err.Error()})
			return
		}
		if !ok {
			_ = writeSSEEvent(c, "error", oauthCredentialCleanupEvent{Event: "error", Error: "OAuth credential cleanup job not found"})
			return
		}
		for _, event := range view.Events {
			if err := writeSSEEvent(c, event.Event, event); err != nil {
				return
			}
			cursor = event.Sequence
		}
		if view.Status != oauthCredentialCleanupJobRunning {
			return
		}
		select {
		case <-c.Request.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *Server) runOAuthCredentialCleanup(
	ctx context.Context,
	job *oauthCredentialCleanupJob,
	configs []*model.Config,
	modelName string,
) error {
	if len(configs) == 0 {
		return nil
	}

	workerCount := min(oauthCredentialCleanupWorkers, len(configs))
	tasks := make(chan *model.Config)
	var workers sync.WaitGroup
	var finalizeMu sync.Mutex
	var finalizeErr error
	finalizeDeletion := func() error {
		finalizeMu.Lock()
		defer finalizeMu.Unlock()
		err := s.finalizeDeletedChannels(1)
		if err != nil {
			finalizeErr = errors.Join(finalizeErr, err)
		}
		return err
	}
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for cfg := range tasks {
				if ctx.Err() != nil {
					return
				}
				baseEvent := oauthCredentialCleanupEvent{
					ChannelID: cfg.ID, ChannelName: cfg.Name, AuthType: cfg.GetAuthType(),
					Models: cfg.GetModels(), Model: modelName,
				}
				if job.action == oauthCredentialCleanupActionDisable && !cfg.Enabled {
					baseEvent.Status = "skipped"
					baseEvent.Error = "channel is already disabled"
					job.finishChannel(baseEvent)
					continue
				}
				job.appendEvent(withOAuthCredentialCleanupStage(baseEvent, "testing"))
				outcome := s.cleanupOAuthCredentialChannel(ctx, job, cfg, baseEvent, finalizeDeletion)
				job.finishChannel(outcome)
			}
		}()
	}

dispatch:
	for _, cfg := range configs {
		select {
		case <-ctx.Done():
			break dispatch
		case tasks <- cfg:
		}
	}
	close(tasks)
	workers.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return finalizeErr
}

func withOAuthCredentialCleanupStage(event oauthCredentialCleanupEvent, stage string) oauthCredentialCleanupEvent {
	event.Event = stage
	event.Status = stage
	event.Error = ""
	event.StatusCode = 0
	return event
}

func (s *Server) cleanupOAuthCredentialChannel(
	ctx context.Context,
	job *oauthCredentialCleanupJob,
	cfg *model.Config,
	event oauthCredentialCleanupEvent,
	finalizeDeletion func() error,
) oauthCredentialCleanupEvent {
	result, testedCfg, rejectedAccessToken, err := s.testOAuthCredentialChannel(ctx, cfg, event.Model)
	if err != nil {
		event.Status = "failed"
		event.Error = err.Error()
		return event
	}
	if ctx.Err() != nil {
		event.Status = "failed"
		event.Error = ctx.Err().Error()
		return event
	}
	event.StatusCode = getResultIntOrDefault(result, "status_code", 0)
	if getResultBoolOrDefault(result, "success", false) {
		event.Status = "healthy"
		return event
	}
	if event.StatusCode != http.StatusUnauthorized {
		event.Status = "failed"
		event.Error = oauthCredentialCleanupTestError(result)
		return event
	}
	if testedCfg != nil {
		cfg = testedCfg
	}

	job.appendEvent(withOAuthCredentialCleanupStage(event, "refreshing"))
	if _, _, handled, refreshErr := s.prepareRejectedOAuthChannelTestAuth(ctx, cfg, rejectedAccessToken); !handled || refreshErr != nil {
		if refreshErr == nil {
			refreshErr = errors.New("OAuth credential refresh is unavailable")
		}
		if !oauthRefreshTokenRejected(refreshErr) {
			event.Status = "failed"
			event.Error = "refresh credential: " + refreshErr.Error()
			return event
		}
		if ctx.Err() != nil {
			event.Status = "failed"
			event.Error = ctx.Err().Error()
			return event
		}
		stage := "disabling"
		if job.action == oauthCredentialCleanupActionDelete {
			stage = "deleting"
		}
		mutationEvent := withOAuthCredentialCleanupStage(event, stage)
		mutationEvent.Error = refreshErr.Error()
		mutated, mutationErr := job.mutateIfNotCancelled(func() (bool, error) {
			job.appendEvent(mutationEvent)
			if job.action == oauthCredentialCleanupActionDelete {
				return s.deleteChannelIfOAuthCredentialMatches(ctx, cfg)
			}
			return s.disableChannelIfOAuthSnapshotMatches(ctx, cfg)
		})
		if mutationErr != nil {
			if errors.Is(mutationErr, context.Canceled) {
				event.Status = "failed"
				event.Error = mutationErr.Error()
				return event
			}
			event.Status = "failed"
			event.Error = fmt.Sprintf("refresh failed: %v; %s channel: %v", refreshErr, job.action, mutationErr)
			return event
		}
		if !mutated {
			event.Status = "skipped"
			event.Error = "channel configuration changed while cleanup was running; kept current channel"
			return event
		}
		event.Status = "disabled"
		if job.action == oauthCredentialCleanupActionDelete {
			event.Status = "deleted"
		}
		event.Error = refreshErr.Error()
		if job.action == oauthCredentialCleanupActionDelete && finalizeDeletion != nil {
			if finalizeErr := finalizeDeletion(); finalizeErr != nil {
				event.Error += "; synchronize deleted channel state: " + finalizeErr.Error()
			}
		}
		return event
	}
	if ctx.Err() != nil {
		event.Status = "failed"
		event.Error = ctx.Err().Error()
		return event
	}

	freshCfg, err := s.store.GetConfig(ctx, cfg.ID)
	if err != nil {
		event.Status = "failed"
		event.Error = "reload refreshed channel: " + err.Error()
		return event
	}
	event.Models = freshCfg.GetModels()
	job.appendEvent(withOAuthCredentialCleanupStage(event, "retesting"))
	result, _, _, err = s.testOAuthCredentialChannel(ctx, freshCfg, event.Model)
	if err != nil {
		event.Status = "failed"
		event.Error = err.Error()
		return event
	}
	if ctx.Err() != nil {
		event.Status = "failed"
		event.Error = ctx.Err().Error()
		return event
	}
	event.StatusCode = getResultIntOrDefault(result, "status_code", 0)
	if !getResultBoolOrDefault(result, "success", false) {
		event.Status = "failed"
		event.Error = oauthCredentialCleanupTestError(result)
		return event
	}
	event.Status = "refreshed"
	return event
}

type oauthTokenEndpointFailure interface {
	error
	StatusCode() int
	UpstreamResponseBody() string
}

func oauthRefreshTokenRejected(err error) bool {
	if errors.Is(err, codexauth.ErrPersonalAccessTokenCannotRefresh) {
		return true
	}
	var endpointFailure oauthTokenEndpointFailure
	if !errors.As(err, &endpointFailure) {
		return false
	}
	statusCode := endpointFailure.StatusCode()
	// The upstream already rejected the access token before this refresh attempt.
	// A token endpoint 401 is definitive even when the provider omits a JSON body.
	if statusCode == http.StatusUnauthorized {
		return true
	}
	if statusCode != http.StatusBadRequest && statusCode != http.StatusForbidden {
		return false
	}
	var payload any
	if sonic.Unmarshal([]byte(endpointFailure.UpstreamResponseBody()), &payload) != nil {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(findOAuthErrorCode(payload)))
	switch code {
	case "invalid_grant", "invalid_token", "invalid_refresh_token", "refresh_token_expired", "refresh_token_revoked", "expired_token":
		return true
	default:
		return false
	}
}

func findOAuthErrorCode(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		for _, key := range []string{"error", "code", "type"} {
			if code := findOAuthErrorCode(typed[key]); strings.TrimSpace(code) != "" {
				return code
			}
		}
	}
	return ""
}

func (s *Server) testOAuthCredentialChannel(
	ctx context.Context,
	cfg *model.Config,
	modelName string,
) (map[string]any, *model.Config, string, error) {
	runtimeCfg, keySelection, handled, err := s.prepareOAuthChannelTestAuth(ctx, cfg, oauthCredentialRefreshIfNeeded)
	if !handled {
		return nil, cfg, "", errors.New("channel does not use OAuth credentials")
	}
	if err != nil && runtimeCfg == nil {
		return nil, cfg, "", err
	}
	testedAccessToken := oauthCredentialTestAccessToken(cfg, runtimeCfg, keySelection)
	testedCfg := cfg.Clone()
	if currentCfg, loadErr := s.store.GetConfig(ctx, cfg.ID); loadErr == nil &&
		oauthCredentialMatchesTestRuntime(currentCfg, runtimeCfg, keySelection) {
		// Keep the persisted routing/model snapshot that actually produced this
		// request. Only adopt the credential and revision written by an automatic
		// refresh. A concurrent admin edit must make the later conditional mutation
		// fail even when it deliberately preserves the OAuth credential.
		testedCfg.OAuthCredential = currentCfg.OAuthCredential
		testedCfg.UpdatedAt = currentCfg.UpdatedAt
	}
	req := &testutil.TestChannelRequest{
		Model:           modelName,
		ClientProtocol:  oauthCredentialCleanupProtocol(cfg),
		Content:         configuredChannelTestContent(s.configService),
		Stream:          false,
		WaitForCapacity: true,
	}
	return s.testChannelAPI(ctx, runtimeCfg, keySelection.requestCredential, req), testedCfg, testedAccessToken, nil
}

func oauthCredentialMatchesTestRuntime(
	persisted, runtimeCfg *model.Config,
	selection channelTestKeySelection,
) bool {
	if persisted == nil || runtimeCfg == nil || persisted.GetAuthType() != runtimeCfg.GetAuthType() {
		return false
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if sonic.Unmarshal([]byte(persisted.OAuthCredential), &payload) != nil || payload.AccessToken == "" {
		return false
	}
	runtimeAccessToken := oauthCredentialTestAccessToken(persisted, runtimeCfg, selection)
	return payload.AccessToken == runtimeAccessToken
}

func oauthCredentialTestAccessToken(
	persisted, runtimeCfg *model.Config,
	selection channelTestKeySelection,
) string {
	if persisted == nil || runtimeCfg == nil {
		return ""
	}
	switch {
	case persisted.UsesCodexOAuth():
		return runtimeCfg.CodexAccessToken
	case persisted.UsesAntigravityOAuth():
		return runtimeCfg.AntigravityAccessToken
	case persisted.UsesXAIOAuth(), persisted.UsesAnthropicOAuth(), persisted.UsesZAIOAuth(), persisted.UsesCursorOAuth(), persisted.UsesZedOAuth():
		return selection.requestCredential
	}
	return ""
}

func oauthCredentialCleanupProtocol(cfg *model.Config) string {
	if cfg == nil {
		return string(protocol.Anthropic)
	}
	switch {
	case cfg.UsesCodexOAuth(), cfg.UsesXAIOAuth(), cfg.UsesZedOAuth():
		return string(protocol.Codex)
	case cfg.UsesAntigravityOAuth():
		return string(protocol.Gemini)
	default:
		return string(protocol.Anthropic)
	}
}

func oauthCredentialCleanupTestError(result map[string]any) string {
	if message := strings.TrimSpace(getResultString(result, "error")); message != "" {
		return message
	}
	if statusCode := getResultIntOrDefault(result, "status_code", 0); statusCode > 0 {
		return fmt.Sprintf("channel conversation test returned HTTP %d", statusCode)
	}
	return "channel conversation test failed"
}
