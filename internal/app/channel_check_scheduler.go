package app

import (
	"context"
	"log"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
)

const defaultChannelCheckIntervalHours = 0

func normalizeChannelCheckIntervalHours(hours int) int {
	if hours < 0 {
		return 0
	}
	return hours
}

func (s *Server) startScheduledChannelCheckLoop(interval time.Duration) {
	if s == nil || interval <= 0 {
		return
	}

	log.Printf("[INFO] 渠道定时检测已启用：间隔=%s（启动后完整周期才会首次执行）", interval)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-s.shutdownCh:
				log.Print("[INFO] 渠道定时检测已停止")
				return
			case <-ticker.C:
				s.triggerScheduledChannelChecks()
			}
		}
	}()
}

func (s *Server) triggerScheduledChannelChecks() bool {
	if s == nil {
		return false
	}
	if !s.scheduledChannelChecksRunning.CompareAndSwap(false, true) {
		log.Print("[WARN] 跳过本轮渠道定时检测：上一轮仍在执行")
		return false
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.scheduledChannelChecksRunning.Store(false)

		ctx := s.baseCtx
		if ctx == nil {
			ctx = context.Background()
		}
		if err := s.runScheduledChannelChecks(ctx); err != nil && !isExpectedScheduledCheckStop(err) {
			log.Printf("[WARN] 渠道定时检测执行失败: %v", err)
		}
	}()

	return true
}

func isExpectedScheduledCheckStop(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

func (s *Server) runScheduledChannelChecks(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}

	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return err
	}
	apiKeysByChannel, err := s.store.GetAllAPIKeys(ctx)
	if err != nil {
		return err
	}

	content := "sonnet 4.0的发布日期是什么"
	if s.configService != nil {
		content = s.configService.GetString("channel_test_content", content)
	}

	for _, cfg := range configs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !shouldRunScheduledChannelCheck(cfg) {
			continue
		}
		modelName, skipReason := selectScheduledCheckModel(cfg)
		if skipReason != "" {
			log.Printf("[WARN] [channel-check] 跳过渠道 #%d %s：%s", cfg.ID, cfg.Name, skipReason)
			s.persistDetectionLog(ctx, detectionSkipLog(cfg, model.LogSourceScheduledCheck, modelName, skipReason))
			continue
		}

		apiKeys := apiKeysByChannel[cfg.ID]
		if len(apiKeys) == 0 {
			log.Printf("[WARN] [channel-check] 跳过渠道 #%d %s：未配置可用 Key", cfg.ID, cfg.Name)
			s.persistDetectionLog(ctx, detectionSkipLog(cfg, model.LogSourceScheduledCheck, modelName, "未配置可用 Key"))
			continue
		}

		selector := s.keySelector
		if selector == nil {
			selector = NewKeySelector()
		}
		keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)
		if err != nil {
			log.Printf("[WARN] [channel-check] 跳过渠道 #%d %s：%v", cfg.ID, cfg.Name, err)
			if !isExpectedScheduledCheckStop(err) {
				s.persistDetectionLog(ctx, detectionSkipLog(cfg, model.LogSourceScheduledCheck, modelName, err.Error()))
			}
			continue
		}

		req := &testutil.TestChannelRequest{
			Model:       modelName,
			ChannelType: cfg.GetChannelType(),
			Content:     content,
			Stream:      false,
		}
		requestedModel := req.Model
		result := s.executeChannelTest(ctx, cfg, keyIndex, apiKey, req)
		s.persistDetectionLog(ctx, detectionLogFromResult(cfg, model.LogSourceScheduledCheck, requestedModel, req.Model, apiKey, "", 0, result))
		logScheduledChannelCheckResult(cfg, keyIndex, req.Model, result)
	}

	return nil
}

func shouldRunScheduledChannelCheck(cfg *model.Config) bool {
	return cfg != nil && cfg.Enabled && cfg.ScheduledCheckEnabled
}

func logScheduledChannelCheckResult(cfg *model.Config, keyIndex int, modelName string, result map[string]any) {
	if cfg == nil {
		return
	}

	if success, _ := result["success"].(bool); success {
		log.Printf("[INFO] [channel-check] 渠道 #%d %s 检测成功 model=%s key_index=%d", cfg.ID, cfg.Name, modelName, keyIndex)
		return
	}

	msg, _ := result["error"].(string)
	if strings.TrimSpace(msg) == "" {
		msg = "unknown error"
	}
	log.Printf("[WARN] [channel-check] 渠道 #%d %s 检测失败 model=%s key_index=%d error=%s", cfg.ID, cfg.Name, modelName, keyIndex, msg)
}
