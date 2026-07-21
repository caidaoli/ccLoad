package app

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"

	"github.com/google/uuid"
)

// FingerprintJobType 区分标定 vs 测试任务。
type FingerprintJobType string

// FingerprintJobCalibrate and FingerprintJobTest are the two job kinds.
const (
	FingerprintJobCalibrate FingerprintJobType = "calibrate"
	FingerprintJobTest      FingerprintJobType = "test"
)

// FingerprintProgress 实时采样进度。
type FingerprintProgress struct {
	Done    int `json:"done"`
	Total   int `json:"total"`
	Success int `json:"success"`
	Failed  int `json:"failed"`
}

// FingerprintMatch 单条基线比对结果。
type FingerprintMatch struct {
	Score            float64                `json:"score"`
	CosineSimilarity float64                `json:"cosine_similarity"`
	JSDivergence     float64                `json:"js_divergence"`
	ModeScore        float64                `json:"mode_score"`
	ModeMatch        bool                   `json:"mode_match"`
	Baseline         model.ModelFingerprint `json:"baseline"`
	TestStats        model.FingerprintStats `json:"test_stats"`
}

// FingerprintTestResult 测试任务的最终结果。
type FingerprintTestResult struct {
	Matches      []FingerprintMatch     `json:"matches"`
	Distribution []float64              `json:"distribution"`
	Stats        model.FingerprintStats `json:"stats"`
	SampleCount  int                    `json:"sample_count"`
	RawData      []int                  `json:"raw_data,omitempty"`
}

// FingerprintJobView 是 GET /fingerprints/jobs/:id 的 JSON 响应。
type FingerprintJobView struct {
	ID        string              `json:"id"`
	Type      FingerprintJobType  `json:"type"`
	Status    string              `json:"status"`
	Progress  FingerprintProgress `json:"progress"`
	Result    any                 `json:"result"`
	Error     string              `json:"error"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
}

// fpJob 内部 job 状态，含 cancel 句柄。
type fpJob struct {
	mu        sync.Mutex
	id        string
	jobType   FingerprintJobType
	status    string
	progress  FingerprintProgress
	result    any
	errStr    string
	createdAt time.Time
	updatedAt time.Time
	cancel    context.CancelFunc
}

func (j *fpJob) view() FingerprintJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	return FingerprintJobView{
		ID:        j.id,
		Type:      j.jobType,
		Status:    j.status,
		Progress:  j.progress,
		Result:    j.result,
		Error:     j.errStr,
		CreatedAt: j.createdAt,
		UpdatedAt: j.updatedAt,
	}
}

func (j *fpJob) setProgress(p FingerprintProgress, now time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.progress = p
	j.updatedAt = now
}

func (j *fpJob) finish(status string, result any, errStr string, now time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = status
	j.result = result
	j.errStr = errStr
	j.updatedAt = now
}

// calibrateReq POST /fingerprints/calibrate body。
type calibrateReq struct {
	Name        string `json:"name" binding:"required"`
	ChannelID   int64  `json:"channel_id" binding:"required"`
	Model       string `json:"model" binding:"required"`
	Iterations  int    `json:"iterations"`
	Concurrency int    `json:"concurrency"`
	KeyIndex    int    `json:"key_index"`
}

// testFingerprintReq POST /fingerprints/test body。
type testFingerprintReq struct {
	ChannelID     int64  `json:"channel_id" binding:"required"`
	Model         string `json:"model" binding:"required"`
	FingerprintID *int64 `json:"fingerprint_id"`
	Iterations    int    `json:"iterations"`
	Concurrency   int    `json:"concurrency"`
	KeyIndex      int    `json:"key_index"`
}

// FingerprintJobManager 管理内存 job（最多 2 个同时运行，完成后保留 1h）。
type FingerprintJobManager struct {
	maxRunning int

	mu         sync.Mutex
	jobs       map[string]*fpJob
	runningCnt atomic.Int32
}

// NewFingerprintJobManager 构造，maxRunning ≤ 0 归 2。
func NewFingerprintJobManager(maxRunning int) *FingerprintJobManager {
	if maxRunning <= 0 {
		maxRunning = 2
	}
	return &FingerprintJobManager{
		maxRunning: maxRunning,
		jobs:       make(map[string]*fpJob),
	}
}

// Get 返回 job view（含已过期 job，惰性清理在 StartXxx 时触发）。
func (m *FingerprintJobManager) Get(id string) (*FingerprintJobView, bool) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return nil, false
	}
	v := j.view()
	return &v, true
}

// Cancel 取消一个 running job；已结束的 job 也返回 nil（幂等）。
func (m *FingerprintJobManager) Cancel(id string) error {
	m.mu.Lock()
	j, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("job %s not found", id)
	}
	j.mu.Lock()
	status := j.status
	cancel := j.cancel
	j.mu.Unlock()
	if status == "running" {
		cancel()
	}
	return nil
}

// StartCalibrate 启动标定 job；超出 maxRunning 返回 error。
func (m *FingerprintJobManager) StartCalibrate(s *Server, req calibrateReq) (string, error) {
	iters, conc, errMsg := util.ClampFingerprintParams(req.Iterations, req.Concurrency)
	if errMsg != "" {
		return "", fmt.Errorf("invalid params: %s", errMsg)
	}

	if err := m.reserveSlot(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithCancel(context.Background())
	j := m.addJob(FingerprintJobCalibrate, cancel, iters)

	go func() {
		defer m.runningCnt.Add(-1)
		defer cancel()

		samples, cancelled := m.runSampling(ctx, j, s, req.ChannelID, req.Model, req.KeyIndex, iters, conc)

		if len(samples) < util.FingerprintMinValidSamples {
			if cancelled {
				j.finish("cancelled", nil, "cancelled before enough valid samples", time.Now())
			} else {
				j.finish("failed", nil, fmt.Sprintf("insufficient valid samples: %d/%d required", len(samples), util.FingerprintMinValidSamples), time.Now())
			}
			return
		}

		// 有效样本 ≥ 40，构建 fingerprint
		dist := util.FingerprintDistribution(samples)
		utilStats := util.CalculateFingerprintStats(samples)
		fp := &model.ModelFingerprint{
			Name:          req.Name,
			ChannelID:     &req.ChannelID,
			Model:         req.Model,
			SampleCount:   len(samples),
			Distribution:  dist,
			Stats:         statsToModel(utilStats),
			RawData:       samples,
			PromptVersion: util.FingerprintPromptVersion,
		}
		// 尝试获取渠道元信息快照
		if cfg, err := s.store.GetConfig(ctx, req.ChannelID); err == nil && cfg != nil {
			fp.ChannelName = cfg.Name
			fp.ChannelType = cfg.ChannelType
		}

		created, err := s.store.CreateModelFingerprint(context.Background(), fp)
		if err != nil {
			j.finish("failed", nil, fmt.Sprintf("store fingerprint: %v", err), time.Now())
			return
		}

		status := "succeeded"
		if cancelled {
			status = "succeeded" // 有效样本足够，取消不阻止成功
		}
		j.finish(status, created, "", time.Now())
	}()

	return j.id, nil
}

// StartTest 启动测试 job；超出 maxRunning 返回 error。
func (m *FingerprintJobManager) StartTest(s *Server, req testFingerprintReq) (string, error) {
	iters, conc, errMsg := util.ClampFingerprintParams(req.Iterations, req.Concurrency)
	if errMsg != "" {
		return "", fmt.Errorf("invalid params: %s", errMsg)
	}

	if err := m.reserveSlot(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithCancel(context.Background())
	j := m.addJob(FingerprintJobTest, cancel, iters)

	go func() {
		defer m.runningCnt.Add(-1)
		defer cancel()

		samples, cancelled := m.runSampling(ctx, j, s, req.ChannelID, req.Model, req.KeyIndex, iters, conc)

		if len(samples) < util.FingerprintMinValidSamples {
			if cancelled {
				j.finish("cancelled", nil, "cancelled before enough valid samples", time.Now())
			} else {
				j.finish("failed", nil, fmt.Sprintf("insufficient valid samples: %d/%d required", len(samples), util.FingerprintMinValidSamples), time.Now())
			}
			return
		}

		dist := util.FingerprintDistribution(samples)
		utilStats := util.CalculateFingerprintStats(samples)
		modelStats := statsToModel(utilStats)

		// 加载 baseline(s)
		var baselines []*model.ModelFingerprint
		if req.FingerprintID != nil {
			fp, err := s.store.GetModelFingerprint(context.Background(), *req.FingerprintID)
			if err != nil || fp == nil {
				j.finish("failed", nil, fmt.Sprintf("baseline not found: id=%d", *req.FingerprintID), time.Now())
				return
			}
			baselines = []*model.ModelFingerprint{fp}
		} else {
			all, err := s.store.ListModelFingerprints(context.Background())
			if err != nil {
				j.finish("failed", nil, fmt.Sprintf("list fingerprints: %v", err), time.Now())
				return
			}
			for _, fp := range all {
				if fp.PromptVersion == util.FingerprintPromptVersion {
					baselines = append(baselines, fp)
				}
			}
		}

		if len(baselines) == 0 {
			j.finish("failed", nil, "no compatible baselines found (prompt_version=v1)", time.Now())
			return
		}

		matches := make([]FingerprintMatch, 0, len(baselines))
		for _, baseline := range baselines {
			baseStats := util.FingerprintSampleStats{
				Mean:      baseline.Stats.Mean,
				Median:    baseline.Stats.Median,
				StdDev:    baseline.Stats.StdDev,
				Min:       baseline.Stats.Min,
				Max:       baseline.Stats.Max,
				Unique:    baseline.Stats.Unique,
				Mode:      baseline.Stats.Mode,
				ModeCount: baseline.Stats.ModeCount,
			}
			sim := util.CalculateFingerprintSimilarity(dist, baseline.Distribution, utilStats, baseStats)
			matches = append(matches, FingerprintMatch{
				Score:            sim.OverallScore,
				CosineSimilarity: sim.CosineSimilarity,
				JSDivergence:     sim.JSDivergence,
				ModeScore:        sim.ModeScore,
				ModeMatch:        utilStats.Mode == baseline.Stats.Mode,
				Baseline:         *baseline,
				TestStats:        modelStats,
			})
		}
		sort.Slice(matches, func(i, k int) bool {
			return matches[i].Score > matches[k].Score
		})

		result := &FingerprintTestResult{
			Matches:      matches,
			Distribution: dist,
			Stats:        modelStats,
			SampleCount:  len(samples),
			RawData:      samples,
		}

		status := "succeeded"
		if cancelled {
			status = "succeeded"
		}
		j.finish(status, result, "", time.Now())
	}()

	return j.id, nil
}

// reserveSlot 检查并原子增加 running count；超出返回 error。
func (m *FingerprintJobManager) reserveSlot() error {
	for {
		cur := m.runningCnt.Load()
		if cur >= int32(m.maxRunning) {
			return fmt.Errorf("too many running fingerprint jobs (%d/%d)", cur, m.maxRunning)
		}
		if m.runningCnt.CompareAndSwap(cur, cur+1) {
			return nil
		}
	}
}

// addJob 生成 job id、注册到 map，顺便惰性清理过期 job。
func (m *FingerprintJobManager) addJob(jobType FingerprintJobType, cancel context.CancelFunc, iters int) *fpJob {
	id := "fpj_" + uuid.New().String()
	now := time.Now()
	j := &fpJob{
		id:        id,
		jobType:   jobType,
		status:    "running",
		cancel:    cancel,
		createdAt: now,
		updatedAt: now,
		progress:  FingerprintProgress{Total: iters},
	}
	m.mu.Lock()
	m.evictExpired()
	m.jobs[id] = j
	m.mu.Unlock()
	return j
}

// evictExpired 删除完成超 1h 的 job（mu 已持有）。
func (m *FingerprintJobManager) evictExpired() {
	cutoff := time.Now().Add(-1 * time.Hour)
	for id, j := range m.jobs {
		j.mu.Lock()
		done := j.status != "running"
		expired := j.updatedAt.Before(cutoff)
		j.mu.Unlock()
		if done && expired {
			delete(m.jobs, id)
		}
	}
}

// runSampling 用 worker pool 并发采样，返回有效数字列表和 cancelled 标志。
func (m *FingerprintJobManager) runSampling(
	ctx context.Context,
	j *fpJob,
	s *Server,
	channelID int64,
	modelName string,
	keyIndex int,
	iterations, concurrency int,
) (samples []int, cancelled bool) {
	cfg, err := s.store.GetConfig(ctx, channelID)
	if err != nil || cfg == nil {
		j.finish("failed", nil, fmt.Sprintf("channel %d not found: %v", channelID, err), time.Now())
		return nil, false
	}

	keys, err := s.store.GetAPIKeys(ctx, channelID)
	if err != nil || len(keys) == 0 {
		j.finish("failed", nil, fmt.Sprintf("channel %d has no API keys", channelID), time.Now())
		return nil, false
	}
	// 选取 keyIndex 的 key，越界则用第一个
	if keyIndex < 0 || keyIndex >= len(keys) {
		keyIndex = 0
	}
	apiKey := keys[keyIndex].APIKey

	temp := 1.0
	workCh := make(chan struct{}, iterations)
	for i := 0; i < iterations; i++ {
		workCh <- struct{}{}
	}
	close(workCh)

	var (
		mu        sync.Mutex
		nums      []int
		done      atomic.Int32
		succeeded atomic.Int32
		failed    atomic.Int32
	)

	wg := &sync.WaitGroup{}
	workers := concurrency
	if workers > iterations {
		workers = iterations
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range workCh {
				if ctx.Err() != nil {
					return
				}
				testReq := &testutil.TestChannelRequest{
					Model:       modelName,
					Content:     util.FingerprintPrompt,
					MaxTokens:   10,
					Temperature: &temp,
					Stream:      false,
					KeyIndex:    keyIndex,
				}
				result := s.executeChannelTestWithCooldown(ctx, cfg, keyIndex, apiKey, testReq, false)
				done.Add(1)
				text, _ := result["response_text"].(string)
				if success, _ := result["success"].(bool); success {
					if n, ok := util.ParseFingerprintNumber(text); ok {
						succeeded.Add(1)
						mu.Lock()
						nums = append(nums, n)
						mu.Unlock()
					} else {
						failed.Add(1)
					}
				} else {
					failed.Add(1)
				}
				j.setProgress(FingerprintProgress{
					Done:    int(done.Load()),
					Total:   iterations,
					Success: int(succeeded.Load()),
					Failed:  int(failed.Load()),
				}, time.Now())
			}
		}()
	}

	wg.Wait()
	cancelled = ctx.Err() != nil
	return nums, cancelled
}

// statsToModel 将 util.FingerprintSampleStats 映射到 model.FingerprintStats。
func statsToModel(s util.FingerprintSampleStats) model.FingerprintStats {
	return model.FingerprintStats{
		Mean:      s.Mean,
		Median:    s.Median,
		StdDev:    s.StdDev,
		Min:       s.Min,
		Max:       s.Max,
		Unique:    s.Unique,
		Mode:      s.Mode,
		ModeCount: s.ModeCount,
	}
}
