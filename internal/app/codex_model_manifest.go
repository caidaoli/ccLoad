package app

import (
	"context"
	"errors"
	"fmt"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
)

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
		// A poll-detected plan change restarts the epoch before id_token claims
		// catch up; a sample taken before that restart cannot describe the account.
		epoch := credential.QuotaCostUsage.EpochTime()
		if credential.AccessToken != source.AccessToken || !manifest.Matches(credential, manifest.Endpoint) ||
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
