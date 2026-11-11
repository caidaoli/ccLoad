package app

import (
	"context"
	"errors"
	"fmt"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
)

/**
 * @brief 条件保存账号模型目录，拒绝凭证或请求来源纪元已变更的旧结果。
 * @param ctx 持久化上下文；取消时终止写入或冲突重试。
 * @param cfg 发起模型发现时的渠道配置。
 * @param source 模型请求实际使用的凭证及额度纪元快照。
 * @param manifest 待保存的账号模型目录。
 * @return 成功返回 nil；渠道、凭证、纪元或更新的目录发生变化，存储、编码或取消失败时返回错误。
 * @note 每次 CAS 重试重新核对身份和纪元，成功后失效凭证与渠道缓存。
 */
func (m *codexCredentialManager) persistModelManifest(ctx context.Context, cfg *model.Config, source *codexauth.Credential, manifest *codexauth.ModelManifest) error {
	for attempt := 0; ; attempt++ {
		current, err := m.store.GetConfig(ctx, cfg.ID)
		if err != nil {
			return err
		}
		if !current.UsesCodexOAuth() {
			return errors.New("model discovery: Codex channel changed provider")
		}
		credential, err := codexauth.ParseCredential([]byte(current.OAuthCredential))
		if err != nil {
			return err
		}
		// 纪元重启与请求开始可能拥有相同墙钟读数，必须比较请求来源纪元，不能只按时间排序。
		epoch := credential.QuotaCostUsage.EpochTime()
		if credential.AccessToken != source.AccessToken || !manifest.Matches(credential, manifest.Endpoint) ||
			!epoch.Equal(source.QuotaCostUsage.EpochTime()) ||
			(!epoch.IsZero() && manifest.SampledAt < epoch.UnixNano()) {
			return errors.New("model discovery: Codex credential changed; retry the refresh")
		}
		if credential.ModelManifest != nil && credential.ModelManifest.SampledAt > manifest.SampledAt {
			return errors.New("a newer Codex model manifest has already been saved")
		}
		credential.ModelManifest = manifest.Clone()
		payload, err := credential.JSON()
		if err != nil {
			return err
		}
		// HTML escaping can expand the snapshot past the credential size limit.
		if _, err := codexauth.ParseCredential([]byte(payload)); err != nil {
			return fmt.Errorf("validate Codex credential with model manifest: %w", err)
		}
		updated, err := m.store.CompareAndSwapOAuthCredential(ctx, cfg.ID, model.AuthTypeCodexOAuth, current.OAuthCredential, payload)
		if err != nil {
			return fmt.Errorf("save Codex model manifest: %w", err)
		}
		if updated {
			m.invalidateCredentialCache(cfg.ID)
			if m.invalidateConfig != nil {
				m.invalidateConfig(cfg.ID)
			}
			return nil
		}
		if err := waitOAuthCASRetry(ctx, attempt); err != nil {
			return err
		}
	}
}
