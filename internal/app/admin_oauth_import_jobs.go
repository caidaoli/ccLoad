package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	oauthCredentialImportJobRunning     = "running"
	oauthCredentialImportJobSucceeded   = "succeeded"
	oauthCredentialImportJobCancelled   = "cancelled"
	oauthCredentialImportJobRetention   = time.Hour
	oauthCredentialImportMaxRunningJobs = 1
)

var (
	errOAuthCredentialImportJobsBusy   = errors.New("too many OAuth credential import jobs are running")
	errOAuthCredentialImportJobsClosed = errors.New("OAuth credential import jobs are shutting down")
)

type oauthCredentialImportJobStart struct {
	JobID string `json:"job_id"`
	Total int    `json:"total"`
}

type oauthCredentialImportJobView struct {
	JobID       string                        `json:"job_id"`
	Status      string                        `json:"status"`
	Processed   int                           `json:"processed"`
	Total       int                           `json:"total"`
	Created     int                           `json:"created"`
	Skipped     int                           `json:"skipped"`
	Failed      int                           `json:"failed"`
	FileName    string                        `json:"file_name,omitempty"`
	Results     []oauthCredentialImportResult `json:"results"`
	Next        int                           `json:"next"`
	Error       string                        `json:"error,omitempty"`
	CreatedAt   time.Time                     `json:"created_at"`
	CompletedAt *time.Time                    `json:"completed_at,omitempty"`
}

type oauthCredentialImportJob struct {
	mu          sync.Mutex
	id          string
	status      string
	total       int
	created     int
	skipped     int
	failed      int
	fileName    string
	results     []oauthCredentialImportResult
	err         string
	createdAt   time.Time
	completedAt *time.Time
	cancel      context.CancelFunc
}

func (j *oauthCredentialImportJob) observe(event oauthCredentialImportEvent) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.fileName = event.FileName
	if event.Event == "progress" && event.Result != nil {
		j.results = append(j.results, *event.Result)
		j.created = event.Created
		j.skipped = event.Skipped
		j.failed = event.Failed
	}
	return true
}

func (j *oauthCredentialImportJob) finish(summary oauthCredentialImportSummary, completed bool, err string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.created = summary.Created
	j.skipped = summary.Skipped
	j.failed = summary.Failed
	j.fileName = ""
	j.err = err
	if completed {
		j.status = oauthCredentialImportJobSucceeded
	} else {
		j.status = oauthCredentialImportJobCancelled
	}
	now := time.Now()
	j.completedAt = &now
}

func (j *oauthCredentialImportJob) view(after int) (oauthCredentialImportJobView, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if after < 0 || after > len(j.results) {
		return oauthCredentialImportJobView{}, fmt.Errorf("after must be between 0 and %d", len(j.results))
	}
	results := append([]oauthCredentialImportResult(nil), j.results[after:]...)
	return oauthCredentialImportJobView{
		JobID:       j.id,
		Status:      j.status,
		Processed:   len(j.results),
		Total:       j.total,
		Created:     j.created,
		Skipped:     j.skipped,
		Failed:      j.failed,
		FileName:    j.fileName,
		Results:     results,
		Next:        len(j.results),
		Error:       j.err,
		CreatedAt:   j.createdAt,
		CompletedAt: j.completedAt,
	}, nil
}

type oauthCredentialImportJobManager struct {
	parentCtx  context.Context
	maxRunning int

	mu      sync.Mutex
	jobs    map[string]*oauthCredentialImportJob
	running int
	closing bool
	wg      sync.WaitGroup
}

type oauthCredentialImportJobRunner func(
	context.Context,
	oauthCredentialImportObserver,
) (oauthCredentialImportSummary, bool)

func newOAuthCredentialImportJobManager(parentCtx context.Context, maxRunning int) *oauthCredentialImportJobManager {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	if maxRunning <= 0 {
		maxRunning = oauthCredentialImportMaxRunningJobs
	}
	return &oauthCredentialImportJobManager{
		parentCtx:  parentCtx,
		maxRunning: maxRunning,
		jobs:       make(map[string]*oauthCredentialImportJob),
	}
}

func (m *oauthCredentialImportJobManager) Start(
	server *Server,
	batch *oauthCredentialImportBatch,
) (oauthCredentialImportJobStart, error) {
	if server == nil || batch == nil {
		return oauthCredentialImportJobStart{}, errors.New("OAuth credential import job is unavailable")
	}
	return m.start(len(batch.Files), func(
		ctx context.Context,
		observer oauthCredentialImportObserver,
	) (oauthCredentialImportSummary, bool) {
		defer wipeOAuthCredentialImportBatch(batch)
		return server.runOAuthCredentialImport(ctx, batch, observer)
	})
}

func (m *oauthCredentialImportJobManager) start(
	total int,
	runner oauthCredentialImportJobRunner,
) (oauthCredentialImportJobStart, error) {
	if total <= 0 || runner == nil {
		return oauthCredentialImportJobStart{}, errors.New("OAuth credential import job is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing || m.parentCtx.Err() != nil {
		return oauthCredentialImportJobStart{}, errOAuthCredentialImportJobsClosed
	}
	if m.running >= m.maxRunning {
		return oauthCredentialImportJobStart{}, fmt.Errorf("%w (%d/%d)", errOAuthCredentialImportJobsBusy, m.running, m.maxRunning)
	}

	ctx, cancel := context.WithCancel(m.parentCtx)
	now := time.Now()
	job := &oauthCredentialImportJob{
		id:        "ocij_" + uuid.New().String(),
		status:    oauthCredentialImportJobRunning,
		total:     total,
		results:   make([]oauthCredentialImportResult, 0, total),
		createdAt: now,
		cancel:    cancel,
	}
	m.evictExpiredLocked(now)
	m.jobs[job.id] = job
	m.running++
	m.wg.Add(1)

	go func() {
		defer m.finishRunningJob()
		defer cancel()
		summary, completed := runner(ctx, job.observe)
		errMessage := ""
		if !completed && ctx.Err() != nil {
			errMessage = ctx.Err().Error()
		}
		job.finish(summary, completed, errMessage)
	}()

	return oauthCredentialImportJobStart{JobID: job.id, Total: job.total}, nil
}

func (m *oauthCredentialImportJobManager) Get(id string, after int) (oauthCredentialImportJobView, bool, error) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return oauthCredentialImportJobView{}, false, nil
	}
	view, err := job.view(after)
	return view, true, err
}

func (m *oauthCredentialImportJobManager) finishRunningJob() {
	m.mu.Lock()
	m.running--
	m.mu.Unlock()
	m.wg.Done()
}

func (m *oauthCredentialImportJobManager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closing = true
	cancels := make([]context.CancelFunc, 0, m.running)
	for _, job := range m.jobs {
		job.mu.Lock()
		if job.status == oauthCredentialImportJobRunning {
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

func (m *oauthCredentialImportJobManager) evictExpiredLocked(now time.Time) {
	for id, job := range m.jobs {
		job.mu.Lock()
		completedAt := job.completedAt
		job.mu.Unlock()
		if completedAt != nil && now.Sub(*completedAt) >= oauthCredentialImportJobRetention {
			delete(m.jobs, id)
		}
	}
}

func wipeOAuthCredentialImportBatch(batch *oauthCredentialImportBatch) {
	if batch == nil {
		return
	}
	for i := range batch.Files {
		clear(batch.Files[i].Raw)
		batch.Files[i].Raw = nil
	}
}

func (s *Server) ensureOAuthCredentialImportJobs() *oauthCredentialImportJobManager {
	s.oauthCredentialImportJobsMu.Lock()
	defer s.oauthCredentialImportJobsMu.Unlock()
	if s.oauthCredentialImportJobs == nil {
		s.oauthCredentialImportJobs = newOAuthCredentialImportJobManager(s.baseCtx, oauthCredentialImportMaxRunningJobs)
	}
	return s.oauthCredentialImportJobs
}

func (s *Server) currentOAuthCredentialImportJobs() *oauthCredentialImportJobManager {
	s.oauthCredentialImportJobsMu.Lock()
	defer s.oauthCredentialImportJobsMu.Unlock()
	return s.oauthCredentialImportJobs
}

func (s *Server) startOAuthCredentialImportJob(c *gin.Context) (oauthCredentialImportJobStart, bool) {
	batch, status, err := s.prepareOAuthCredentialImport(c, "")
	if err != nil {
		RespondError(c, status, err)
		return oauthCredentialImportJobStart{}, false
	}
	started, err := s.ensureOAuthCredentialImportJobs().Start(s, batch)
	if err != nil {
		wipeOAuthCredentialImportBatch(batch)
		status = http.StatusServiceUnavailable
		if errors.Is(err, errOAuthCredentialImportJobsBusy) {
			status = http.StatusTooManyRequests
		}
		RespondError(c, status, err)
		return oauthCredentialImportJobStart{}, false
	}
	return started, true
}

// HandleStartOAuthCredentialImportJob uploads credentials and starts an import
// owned by the server lifecycle instead of the upload request.
func (s *Server) HandleStartOAuthCredentialImportJob(c *gin.Context) {
	started, ok := s.startOAuthCredentialImportJob(c)
	if !ok {
		return
	}
	RespondJSON(c, http.StatusAccepted, started)
}

// HandleOAuthCredentialImportJob returns an incremental job snapshot. The
// after cursor is the number of results already consumed by the caller.
func (s *Server) HandleOAuthCredentialImportJob(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	after := 0
	if rawAfter := c.Query("after"); rawAfter != "" {
		parsed, err := strconv.Atoi(rawAfter)
		if err != nil || parsed < 0 {
			RespondErrorMsg(c, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = parsed
	}
	manager := s.currentOAuthCredentialImportJobs()
	if manager == nil {
		RespondErrorMsg(c, http.StatusNotFound, fmt.Sprintf("OAuth credential import job %s not found", c.Param("id")))
		return
	}
	view, ok, err := manager.Get(c.Param("id"), after)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if !ok {
		RespondErrorMsg(c, http.StatusNotFound, fmt.Sprintf("OAuth credential import job %s not found", c.Param("id")))
		return
	}
	RespondJSON(c, http.StatusOK, view)
}

// HandleImportOAuthCredentialsStream keeps the legacy event contract while
// running the import independently. A broken SSE connection no longer cancels
// the job.
func (s *Server) HandleImportOAuthCredentialsStream(c *gin.Context) {
	started, ok := s.startOAuthCredentialImportJob(c)
	if !ok {
		return
	}
	s.streamOAuthCredentialImportJob(c, started)
}

func (s *Server) streamOAuthCredentialImportJob(c *gin.Context, started oauthCredentialImportJobStart) {
	manager := s.currentOAuthCredentialImportJobs()
	if manager == nil {
		return
	}

	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Header("X-Content-Type-Options", "nosniff")
	disableResponseWriteTimeout(c.Writer, "OAuth credential import stream")
	c.Status(http.StatusOK)
	if writeOAuthCredentialImportEvent(c, oauthCredentialImportEvent{Event: "start", JobID: started.JobID, Total: started.Total}) != nil {
		return
	}

	cursor := 0
	counts := oauthCredentialImportSummary{}
	for {
		view, exists, err := manager.Get(started.JobID, cursor)
		if err != nil || !exists {
			return
		}
		for _, result := range view.Results {
			if writeOAuthCredentialImportEvent(c, oauthCredentialImportEvent{
				Event:     "processing",
				Processed: cursor,
				Total:     view.Total,
				Created:   counts.Created,
				Skipped:   counts.Skipped,
				Failed:    counts.Failed,
				FileName:  result.FileName,
			}) != nil {
				return
			}
			appendOAuthCredentialImportResult(&counts, result)
			cursor++
			resultCopy := result
			if writeOAuthCredentialImportEvent(c, oauthCredentialImportEvent{
				Event:     "progress",
				Processed: cursor,
				Total:     view.Total,
				Created:   counts.Created,
				Skipped:   counts.Skipped,
				Failed:    counts.Failed,
				FileName:  result.FileName,
				Result:    &resultCopy,
			}) != nil {
				return
			}
		}
		if view.Status != oauthCredentialImportJobRunning {
			_ = writeOAuthCredentialImportEvent(c, oauthCredentialImportEvent{
				Event:     "complete",
				Processed: view.Processed,
				Total:     view.Total,
				Created:   view.Created,
				Skipped:   view.Skipped,
				Failed:    view.Failed,
			})
			return
		}
		select {
		case <-c.Request.Context().Done():
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}
