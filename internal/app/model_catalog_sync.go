package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"ccLoad/internal/util"
)

const (
	modelsDevCatalogURL          = "https://models.dev/api.json"
	modelCatalogRequestTimeout   = 15 * time.Second
	modelCatalogMaxBodyBytes     = 16 << 20
	defaultModelCatalogSyncHours = 6.0
)

// ModelCatalogSyncStatus 表示一次模型目录同步的结果。
type ModelCatalogSyncStatus string

const (
	// ModelCatalogSyncUpdated 表示已验证并安装新的官方目录。
	ModelCatalogSyncUpdated ModelCatalogSyncStatus = "updated"
	// ModelCatalogSyncNotModified 表示远端确认目录未变化。
	ModelCatalogSyncNotModified ModelCatalogSyncStatus = "not_modified"
	// ModelCatalogSyncSkipped 表示已有同步正在进行。
	ModelCatalogSyncSkipped ModelCatalogSyncStatus = "skipped"
	// ModelCatalogSyncFailed 表示同步未能完成。
	ModelCatalogSyncFailed ModelCatalogSyncStatus = "failed"
)

// ModelCatalogSyncResult 是一次模型目录同步的公开结果。
type ModelCatalogSyncResult struct {
	Status            ModelCatalogSyncStatus
	Stage             string
	Err               error
	PersistenceError  error
	ModelCount        int
	ProviderCount     int
	SkippedModelCount int
	ETag              string
	Duration          time.Duration
}

// ModelCatalogSyncer 获取、验证并持久化 models.dev 的模型目录。
type ModelCatalogSyncer struct {
	client    *http.Client
	endpoint  string
	cachePath string
	running   atomic.Bool
}

// NewModelCatalogSyncer 创建模型目录同步器。
func NewModelCatalogSyncer(client *http.Client, endpoint, cachePath string) *ModelCatalogSyncer {
	if client == nil {
		client = &http.Client{Timeout: modelCatalogRequestTimeout}
	}
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		endpoint = modelsDevCatalogURL
	}
	cachePath = strings.TrimSpace(cachePath)
	if cachePath == "" {
		cachePath = modelCatalogCachePath()
	}

	return &ModelCatalogSyncer{
		client:    client,
		endpoint:  endpoint,
		cachePath: cachePath,
	}
}

func modelCatalogCachePath() string {
	if path := strings.TrimSpace(os.Getenv("CCLOAD_MODEL_CATALOG_CACHE")); path != "" {
		return path
	}
	return filepath.Join("data", "model-catalog.json")
}

func normalizeModelCatalogSyncIntervalHours(hours float64) float64 {
	if hours <= 0 || math.IsNaN(hours) || math.IsInf(hours, 0) {
		return 0
	}
	return hours
}

func newModelCatalogSyncResult(status ModelCatalogSyncStatus, startedAt time.Time) ModelCatalogSyncResult {
	summary := util.CurrentModelCatalogSummary()
	return ModelCatalogSyncResult{
		Status:            status,
		ModelCount:        summary.ModelCount,
		ProviderCount:     summary.ProviderCount,
		SkippedModelCount: summary.SkippedModelCount,
		ETag:              summary.ETag,
		Duration:          time.Since(startedAt),
	}
}

func modelCatalogSyncFailure(startedAt time.Time, stage string, err error) (ModelCatalogSyncResult, error) {
	result := newModelCatalogSyncResult(ModelCatalogSyncFailed, startedAt)
	result.Stage = stage
	result.Err = err
	return result, err
}

// Sync 从远端拉取并安装最新模型目录。
func (s *ModelCatalogSyncer) Sync(ctx context.Context) (ModelCatalogSyncResult, error) {
	startedAt := time.Now()
	if s == nil {
		return modelCatalogSyncFailure(startedAt, "sync", fmt.Errorf("model catalog syncer is nil"))
	}
	if !s.running.CompareAndSwap(false, true) {
		return newModelCatalogSyncResult(ModelCatalogSyncSkipped, startedAt), nil
	}
	defer s.running.Store(false)

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, modelCatalogRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint, nil)
	if err != nil {
		return modelCatalogSyncFailure(startedAt, "request", fmt.Errorf("create model catalog request: %w", err))
	}
	if etag := util.CurrentModelCatalogETag(); etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return modelCatalogSyncFailure(startedAt, "fetch", fmt.Errorf("fetch model catalog: %w", err))
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotModified {
		return newModelCatalogSyncResult(ModelCatalogSyncNotModified, startedAt), nil
	}
	if resp.StatusCode != http.StatusOK {
		return modelCatalogSyncFailure(startedAt, "fetch", fmt.Errorf("fetch model catalog: unexpected HTTP status %d", resp.StatusCode))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, modelCatalogMaxBodyBytes+1))
	if err != nil {
		return modelCatalogSyncFailure(startedAt, "read", fmt.Errorf("read model catalog response: %w", err))
	}
	if len(body) > modelCatalogMaxBodyBytes {
		return modelCatalogSyncFailure(startedAt, "read", fmt.Errorf("model catalog response exceeds %d bytes", modelCatalogMaxBodyBytes))
	}

	snapshot, err := util.ParseModelsDevCatalog(bytes.NewReader(body), resp.Header.Get("ETag"), time.Now().UTC())
	if err != nil {
		return modelCatalogSyncFailure(startedAt, "parse", err)
	}
	if err := util.InstallModelCatalog(snapshot, "models.dev"); err != nil {
		return modelCatalogSyncFailure(startedAt, "install", fmt.Errorf("install model catalog: %w", err))
	}
	result := newModelCatalogSyncResult(ModelCatalogSyncUpdated, startedAt)
	if err := writeModelCatalogCache(s.cachePath, snapshot); err != nil {
		result.PersistenceError = err
	}

	return result, nil
}

// LoadCache 加载并安装最后一次成功的归一化模型目录快照。
func (s *ModelCatalogSyncer) LoadCache() error {
	if s == nil {
		return fmt.Errorf("model catalog syncer is nil")
	}
	cachePath := strings.TrimSpace(s.cachePath)
	if cachePath == "" {
		return fmt.Errorf("model catalog cache path is empty")
	}

	cacheFile, err := os.Open(cachePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open model catalog cache: %w", err)
	}
	defer func() {
		_ = cacheFile.Close()
	}()

	data, err := io.ReadAll(io.LimitReader(cacheFile, modelCatalogMaxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read model catalog cache: %w", err)
	}
	if len(data) > modelCatalogMaxBodyBytes {
		return fmt.Errorf("model catalog cache exceeds %d bytes", modelCatalogMaxBodyBytes)
	}

	var snapshot util.ModelCatalogSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("decode model catalog cache: %w", err)
	}
	if snapshot.Version != util.ModelCatalogSchemaVersion {
		return fmt.Errorf("unsupported model catalog cache version %d", snapshot.Version)
	}
	if err := util.InstallModelCatalog(&snapshot, "cache"); err != nil {
		return fmt.Errorf("install model catalog cache: %w", err)
	}
	return nil
}

func writeModelCatalogCache(cachePath string, snapshot *util.ModelCatalogSnapshot) (err error) {
	cachePath = strings.TrimSpace(cachePath)
	if cachePath == "" {
		return fmt.Errorf("model catalog cache path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return fmt.Errorf("create model catalog cache directory: %w", err)
	}

	tempFile, err := os.CreateTemp(filepath.Dir(cachePath), ".model-catalog-*")
	if err != nil {
		return fmt.Errorf("create model catalog cache temp file: %w", err)
	}
	tempPath := tempFile.Name()
	defer func() {
		if tempFile != nil {
			_ = tempFile.Close()
		}
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()

	if err = tempFile.Chmod(0o600); err != nil {
		return fmt.Errorf("set model catalog cache permissions: %w", err)
	}
	if err = json.NewEncoder(tempFile).Encode(snapshot); err != nil {
		return fmt.Errorf("encode model catalog cache: %w", err)
	}
	if err = tempFile.Sync(); err != nil {
		return fmt.Errorf("sync model catalog cache: %w", err)
	}
	if err = tempFile.Close(); err != nil {
		return fmt.Errorf("close model catalog cache: %w", err)
	}
	tempFile = nil
	if err = os.Rename(tempPath, cachePath); err != nil {
		return fmt.Errorf("replace model catalog cache: %w", err)
	}
	return nil
}

func (s *Server) runModelCatalogSyncLoop(syncer *ModelCatalogSyncer, interval time.Duration) {
	defer s.wg.Done()

	ctx := s.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	s.syncModelCatalog(ctx, syncer)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncModelCatalog(ctx, syncer)
		}
	}
}

func (s *Server) syncModelCatalog(ctx context.Context, syncer *ModelCatalogSyncer) {
	result, err := syncer.Sync(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[WARN] model_catalog_sync status=%s stage=%s error=%v duration=%s", result.Status, result.Stage, result.Err, result.Duration)
		}
		return
	}
	log.Printf("[INFO] model_catalog_sync status=%s models=%d providers=%d skipped_models=%d duration=%s etag=%q persistence_error=%v", result.Status, result.ModelCount, result.ProviderCount, result.SkippedModelCount, result.Duration, result.ETag, result.PersistenceError)
}
