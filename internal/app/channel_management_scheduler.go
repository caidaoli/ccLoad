package app

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"ccLoad/internal/model"
)

const (
	channelManagementScheduleInterval = time.Minute
	channelManagementScheduleWorkers  = 4
)

// managementCheckinLoop owns the daily management check-in scheduler lifecycle.
// It performs an immediate catch-up scan, then scans once per minute until the
// server is shut down.
func (s *Server) managementCheckinLoop() {
	defer s.wg.Done()

	ctx := s.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.runDueManagementCheckins(ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("[WARN] 管理账户每日签到补偿扫描失败: %v", err)
	}

	ticker := time.NewTicker(channelManagementScheduleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := s.runDueManagementCheckins(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[WARN] 管理账户每日签到扫描失败: %v", err)
			}
		}
	}
}

// runDueManagementCheckins claims all channels due at now and waits for their
// check-ins to finish. Claiming the local calendar day before queueing is what
// makes repeated scans and concurrent scans idempotent.
func (s *Server) runDueManagementCheckins(ctx context.Context, now time.Time) error {
	if s == nil || s.store == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return err
	}

	day := now.In(time.Local).Format("2006-01-02")
	channelIDs := make([]int64, 0, len(configs))
	var scanErr error
	for _, cfg := range configs {
		if ctx.Err() != nil {
			scanErr = ctx.Err()
			break
		}
		if cfg == nil || cfg.AuthType != model.AuthTypeAPIKey || !cfg.Enabled {
			continue
		}
		envelope, parseErr := model.ParseChannelManagementEnvelope(cfg.OAuthCredential)
		if parseErr != nil || !isManagementCheckinDue(envelope, now) {
			continue
		}
		claimed, claimErr := s.claimManagementCheckinDay(ctx, cfg, day, now)
		if claimErr != nil {
			if scanErr == nil {
				scanErr = claimErr
			}
			log.Printf("[WARN] 管理账户每日签到渠道 claim 失败（channel=%d）: %v", cfg.ID, claimErr)
			continue
		}
		if claimed {
			channelIDs = append(channelIDs, cfg.ID)
		}
	}
	if len(channelIDs) == 0 {
		return scanErr
	}
	jobs := make(chan int64, len(channelIDs))
	for _, channelID := range channelIDs {
		jobs <- channelID
	}
	workers := channelManagementScheduleWorkers
	if len(channelIDs) < workers {
		workers = len(channelIDs)
	}
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for channelID := range jobs {
				s.runManagementCheckinJob(ctx, channelID)
			}
		}()
	}
	close(jobs)
	wg.Wait()
	if scanErr != nil {
		return scanErr
	}
	return ctx.Err()
}

func (s *Server) claimManagementCheckinDay(
	ctx context.Context,
	cfg *model.Config,
	day string,
	now time.Time,
) (bool, error) {
	if cfg == nil {
		return false, nil
	}
	current := cfg
	// A failed CAS is re-read and re-evaluated. Two attempts are enough to
	// observe the winner of a concurrent claim while avoiding a retry loop on a
	// permanently failing store.
	for attempt := 0; attempt < 2; attempt++ {
		envelope, err := model.ParseChannelManagementEnvelope(current.OAuthCredential)
		if err != nil || current.AuthType != model.AuthTypeAPIKey || !current.Enabled || !isManagementCheckinDue(envelope, now) {
			return false, nil
		}
		next := *envelope
		next.State.LastScheduledDay = day
		nextRaw, err := next.Marshal()
		if err != nil {
			return false, err
		}
		updated, err := s.store.CompareAndSwapChannelManagement(ctx, current.ID, current.OAuthCredential, nextRaw)
		if err != nil {
			return false, err
		}
		if updated {
			return true, nil
		}
		if attempt == 1 {
			return false, nil
		}
		current, err = s.store.GetConfig(ctx, current.ID)
		if err != nil {
			return false, err
		}
	}
	return false, nil
}

func (s *Server) runManagementCheckinJob(ctx context.Context, channelID int64) {
	cfg, err := s.store.GetConfig(ctx, channelID)
	if err != nil {
		if ctx.Err() == nil {
			s.writeManagementCheckinAudit(ctx, &model.Config{ID: channelID}, nil, err)
		}
		return
	}
	if cfg == nil {
		return
	}
	envelope, parseErr := model.ParseChannelManagementEnvelope(cfg.OAuthCredential)
	if parseErr != nil {
		s.writeManagementCheckinAudit(ctx, cfg, nil, parseErr)
		return
	}
	if cfg.AuthType != model.AuthTypeAPIKey || !cfg.Enabled ||
		(envelope.Profile != model.ChannelManagementProfileNewAPI && envelope.Profile != model.ChannelManagementProfileSub2APIPro) ||
		!envelope.Settings.DailyCheckinEnabled {
		s.writeManagementCheckinAudit(ctx, cfg, &channelCheckinResult{Status: newAPICheckinSkippedDisabled}, nil)
		return
	}
	result, checkinErr := s.channelManagement.CheckIn(ctx, channelID)
	if ctx.Err() != nil {
		return
	}
	s.writeManagementCheckinAudit(ctx, cfg, result, checkinErr)
}

func (s *Server) writeManagementCheckinAudit(ctx context.Context, cfg *model.Config, result *channelCheckinResult, err error) {
	if s == nil || s.store == nil || cfg == nil {
		return
	}
	auditCtx := ctx
	if auditCtx == nil || auditCtx.Err() != nil {
		auditCtx = context.Background()
	}
	if auditErr := s.addChannelCheckinAuditLog(auditCtx, cfg, result, err); auditErr != nil {
		log.Printf("[WARN] 管理账户每日签到审计日志写入失败（channel=%d）: %v", cfg.ID, auditErr)
	}
}

// isManagementCheckinDue interprets the configured HH:MM in the server's local
// calendar and intentionally ignores manual check-in state.
func isManagementCheckinDue(envelope *model.ChannelManagementEnvelope, now time.Time) bool {
	if envelope == nil || !envelope.Settings.DailyCheckinEnabled ||
		(envelope.Profile != model.ChannelManagementProfileNewAPI && envelope.Profile != model.ChannelManagementProfileSub2APIPro) {
		return false
	}
	localNow := now.In(time.Local)
	if envelope.State.LastScheduledDay == localNow.Format("2006-01-02") {
		return false
	}
	scheduledClock, err := time.ParseInLocation("15:04", envelope.Settings.DailyCheckinTime, time.Local)
	if err != nil {
		return false
	}
	scheduled := time.Date(
		localNow.Year(), localNow.Month(), localNow.Day(),
		scheduledClock.Hour(), scheduledClock.Minute(), 0, 0, time.Local,
	)
	return !localNow.Before(scheduled)
}
