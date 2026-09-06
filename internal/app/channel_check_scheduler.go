package app

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/testutil"
)

func configuredChannelTestContent(configService *ConfigService) string {
	if configService == nil {
		return config.DefaultChannelTestContent
	}
	content := strings.TrimSpace(configService.GetString("channel_test_content", config.DefaultChannelTestContent))
	if content == "" {
		return config.DefaultChannelTestContent
	}
	return content
}

func (s *Server) startScheduledChannelCheckLoop() {
	log.Print("[INFO] 渠道每日定时检测已启用（服务端本地时间）")
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// Always wait for a future minute; startup never replays missed checks.
		timer := time.NewTimer(time.Until(time.Now().Truncate(time.Minute).Add(time.Minute)))
		defer timer.Stop()
		for {
			select {
			case <-s.shutdownCh:
				return
			case <-timer.C:
				s.triggerScheduledChannelChecks(time.Now())
				timer.Reset(time.Until(time.Now().Truncate(time.Minute).Add(time.Minute)))
			}
		}
	}()
}

func (s *Server) triggerScheduledChannelChecks(now time.Time) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.runScheduledChannelChecks(s.baseCtx, now); err != nil && !isExpectedScheduledCheckStop(err) {
			log.Printf("[WARN] 渠道每日定时检测执行失败: %v", err)
		}
	}()
}
func isExpectedScheduledCheckStop(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (s *Server) runScheduledChannelChecks(ctx context.Context, now time.Time) error {
	if s == nil || s.store == nil {
		return nil
	}

	// Scanning must finish within this minute; delayed scans must not replay a slot.
	scanCtx, cancel := context.WithDeadline(ctx, now.Truncate(time.Minute).Add(time.Minute))
	defer cancel()
	configs, err := s.store.ListConfigs(scanCtx)
	if err != nil {
		return err
	}
	due := configs[:0]
	for _, cfg := range configs {
		if cfg.ScheduledCheckDueAt(now) && cfg.UpdatedAt.Before(now.Truncate(time.Minute)) {
			due = append(due, cfg)
		}
	}
	if len(due) == 0 {
		return nil
	}
	apiKeysByChannel, err := s.store.GetAllAPIKeys(scanCtx)
	if err != nil {
		return err
	}

	content := configuredChannelTestContent(s.configService)
	var running sync.WaitGroup
	defer running.Wait()

	for _, cfg := range due {
		if scanCtx.Err() != nil {
			return scanCtx.Err()
		}
		if _, loaded := s.scheduledChannelChecksRunning.LoadOrStore(cfg.ID, struct{}{}); loaded {
			continue
		}
		running.Add(1)
		go func() {
			defer running.Done()
			defer s.scheduledChannelChecksRunning.Delete(cfg.ID)
			s.runScheduledChannelCheck(ctx, cfg, apiKeysByChannel[cfg.ID], content)
		}()
	}

	return nil
}

func (s *Server) runScheduledChannelCheck(ctx context.Context, cfg *model.Config, apiKeys []*model.APIKey, content string) {
	modelName, skipReason := selectScheduledCheckModel(cfg)
	if skipReason != "" {
		log.Printf("[WARN] [channel-check] 跳过渠道 #%d %s：%s", cfg.ID, cfg.Name, skipReason)
		s.persistDetectionLog(ctx, detectionSkipLog(cfg, model.LogSourceScheduledCheck, modelName, skipReason))
		return
	}

	runtimeCfg, keySelection, err := s.prepareScheduledChannelCheckAuth(ctx, cfg, apiKeys, modelName)
	if err != nil {
		log.Printf("[WARN] [channel-check] 跳过渠道 #%d %s：%v", cfg.ID, cfg.Name, err)
		if !isExpectedScheduledCheckStop(err) {
			s.persistDetectionLog(ctx, detectionSkipLog(cfg, model.LogSourceScheduledCheck, modelName, err.Error()))
		}
		return
	}

	req := &testutil.TestChannelRequest{
		Model:          modelName,
		ClientProtocol: string(protocol.Anthropic),
		Content:        content,
		Stream:         false,
	}
	logModel, logThinking := channelTestLogIdentity(req.Model, req.ThinkingEffort)
	result := s.executeChannelTestWithCooldown(ctx, runtimeCfg, keySelection.keyIndex, keySelection.requestCredential, req, keySelection.updatePersistedCooldown)
	s.persistDetectionLog(ctx, detectionLogFromResult(cfg, model.LogSourceScheduledCheck, logModel, model.RoutingModelName(req.Model), keySelection.apiKey, "", logThinking, result))
	logScheduledChannelCheckResult(cfg, keySelection.keyIndex, req.Model, result)
}

func (s *Server) prepareScheduledChannelCheckAuth(ctx context.Context, cfg *model.Config, apiKeys []*model.APIKey, modelName string) (*model.Config, channelTestKeySelection, error) {
	if runtimeCfg, selection, handled, err := s.prepareOAuthChannelTestAuth(ctx, cfg, oauthCredentialRefreshIfNeeded); handled {
		return runtimeCfg, selection, err
	}

	apiKeys, _ = filterAPIKeysForModel(apiKeys, s.resolveChannelRoutingModel(cfg, modelName))
	if len(apiKeys) == 0 {
		return nil, channelTestKeySelection{}, errors.New("该模型未配置可用 Key")
	}

	// Scheduled checks skip cooled keys; manual tests may probe them via fallback.
	selector := s.keySelector
	if selector == nil {
		selector = NewKeySelector()
	}
	keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)
	return cfg, channelTestKeySelection{
		keyIndex:                keyIndex,
		apiKey:                  apiKey,
		requestCredential:       apiKey,
		updatePersistedCooldown: true,
	}, err
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
