package storage

import (
	"context"
	"fmt"
)

const primaryReconcilePageSize = 25

const (
	reconcileLocalChannels = iota
	reconcilePrimaryChannels
	reconcileLocalAuthTokens
	reconcilePrimaryAuthTokens
	reconcileSettings
	reconcilePhaseCount
)

type primaryReconcileCursor struct {
	active  bool
	started bool
	restart bool
	phase   int
	afterID int64
}

// syncChannelReplica reads one current SQLite aggregate and atomically mirrors it.
func (h *HybridStore) syncChannelReplica(ctx context.Context, channelID int64) error {
	cfg, err := h.sqlite.GetConfig(ctx, channelID)
	if err != nil {
		return err
	}
	keys, err := h.sqlite.GetAPIKeys(ctx, channelID)
	if err != nil {
		return err
	}
	disabledURLs, err := h.sqlite.GetDisabledURLsReplica(ctx, channelID)
	if err != nil {
		return err
	}
	modelCooldowns, err := h.sqlite.GetModelCooldownReplicaState(ctx, channelID)
	if err != nil {
		return err
	}
	return h.primary.SyncChannelAggregateReplica(ctx, cfg, keys, disabledURLs, modelCooldowns)
}

func (h *HybridStore) markPrimaryReconcileDirty() {
	h.primaryReconcileMu.Lock()
	defer h.primaryReconcileMu.Unlock()
	if !h.primaryReconcile.active {
		h.primaryReconcile = primaryReconcileCursor{active: true}
		return
	}
	if h.primaryReconcile.started {
		h.primaryReconcile.restart = true
	}
}

// reconcilePrimary processes one bounded page. A continuation replaces the
// running generation, so a large remote repair never restarts at row zero just
// because it needs more than the normal 30-second entity timeout.
func (h *HybridStore) reconcilePrimary(ctx context.Context) error {
	more, err := h.reconcilePrimaryPage(ctx)
	if err == nil && more {
		h.primarySync.continueReconcile()
	}
	return err
}

func (h *HybridStore) reconcilePrimaryPage(ctx context.Context) (bool, error) {
	h.primaryReconcileMu.Lock()
	if !h.primaryReconcile.active {
		h.primaryReconcile = primaryReconcileCursor{active: true}
	}
	h.primaryReconcile.started = true
	phase, afterID := h.primaryReconcile.phase, h.primaryReconcile.afterID
	h.primaryReconcileMu.Unlock()

	pageDone := false
	lastID := afterID
	switch phase {
	case reconcileLocalChannels:
		ids, err := h.sqlite.ListReplicaIDsPage(ctx, "channels", afterID, primaryReconcilePageSize)
		if err != nil {
			return false, fmt.Errorf("list SQLite channel IDs: %w", err)
		}
		for _, id := range ids {
			if err := h.syncChannelReplica(ctx, id); err != nil {
				return false, fmt.Errorf("sync channel %d: %w", id, err)
			}
		}
		lastID, pageDone = lastReplicaID(ids, afterID), len(ids) < primaryReconcilePageSize

	case reconcilePrimaryChannels:
		ids, err := h.primary.ListReplicaIDsPage(ctx, "channels", afterID, primaryReconcilePageSize)
		if err != nil {
			return false, fmt.Errorf("list primary channel IDs: %w", err)
		}
		for _, id := range ids {
			exists, err := h.sqlite.ReplicaRowExists(ctx, "channels", id)
			if err != nil {
				return false, fmt.Errorf("check SQLite channel %d: %w", id, err)
			}
			if !exists {
				if err := h.primary.DeleteConfig(ctx, id); err != nil {
					return false, fmt.Errorf("delete stale primary channel %d: %w", id, err)
				}
			}
		}
		lastID, pageDone = lastReplicaID(ids, afterID), len(ids) < primaryReconcilePageSize

	case reconcileLocalAuthTokens:
		ids, err := h.sqlite.ListReplicaIDsPage(ctx, "auth_tokens", afterID, primaryReconcilePageSize)
		if err != nil {
			return false, fmt.Errorf("list SQLite auth token IDs: %w", err)
		}
		for _, id := range ids {
			token, err := h.sqlite.GetAuthToken(ctx, id)
			if err != nil {
				return false, fmt.Errorf("load SQLite auth token %d: %w", id, err)
			}
			if err := h.primary.UpsertAuthTokenAllFields(ctx, token); err != nil {
				return false, fmt.Errorf("sync auth token %d: %w", id, err)
			}
		}
		lastID, pageDone = lastReplicaID(ids, afterID), len(ids) < primaryReconcilePageSize

	case reconcilePrimaryAuthTokens:
		ids, err := h.primary.ListReplicaIDsPage(ctx, "auth_tokens", afterID, primaryReconcilePageSize)
		if err != nil {
			return false, fmt.Errorf("list primary auth token IDs: %w", err)
		}
		for _, id := range ids {
			exists, err := h.sqlite.ReplicaRowExists(ctx, "auth_tokens", id)
			if err != nil {
				return false, fmt.Errorf("check SQLite auth token %d: %w", id, err)
			}
			if !exists {
				if err := h.primary.DeleteAuthTokenReplica(ctx, id); err != nil {
					return false, fmt.Errorf("delete stale primary auth token %d: %w", id, err)
				}
			}
		}
		lastID, pageDone = lastReplicaID(ids, afterID), len(ids) < primaryReconcilePageSize

	case reconcileSettings:
		settings, err := h.sqlite.ListAllSettings(ctx)
		if err != nil {
			return false, fmt.Errorf("list SQLite settings: %w", err)
		}
		for _, setting := range settings {
			if setting == nil {
				continue
			}
			if err := h.primary.UpdateSetting(ctx, setting.Key, setting.Value); err != nil {
				return false, fmt.Errorf("sync setting %q: %w", setting.Key, err)
			}
		}
		pageDone = true

	default:
		return false, fmt.Errorf("unknown primary reconciliation phase %d", phase)
	}

	return h.advancePrimaryReconcile(phase, lastID, pageDone), nil
}

func (h *HybridStore) advancePrimaryReconcile(phase int, lastID int64, phaseDone bool) bool {
	h.primaryReconcileMu.Lock()
	defer h.primaryReconcileMu.Unlock()
	if h.primaryReconcile.phase != phase {
		return true
	}
	if phaseDone {
		h.primaryReconcile.phase++
		h.primaryReconcile.afterID = 0
	} else {
		h.primaryReconcile.afterID = lastID
	}
	if h.primaryReconcile.phase < reconcilePhaseCount {
		return true
	}
	if h.primaryReconcile.restart {
		h.primaryReconcile = primaryReconcileCursor{active: true}
		return true
	}
	h.primaryReconcile = primaryReconcileCursor{}
	return false
}

func lastReplicaID(ids []int64, fallback int64) int64 {
	if len(ids) == 0 {
		return fallback
	}
	return ids[len(ids)-1]
}
