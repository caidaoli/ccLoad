package oauthcost

import (
	"errors"
	"math"
	"time"
)

// Rollback preserves evidence of a same-period upstream usage drop. A pending
// rollback keeps the counted interval cut until a later upstream sample resolves it.
type Rollback struct {
	PreviousCountFrom   int64   `json:"previous_count_from"`
	PreviousUsedPercent float64 `json:"previous_used_percent"`
	CutUsedPercent      float64 `json:"cut_used_percent"`
}

func cloneRollback(evidence *Rollback) *Rollback {
	if evidence == nil {
		return nil
	}
	clone := *evidence
	return &clone
}

func validPercent(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 100
}

func recordRollback(current, next *Window, sampledAt time.Time) {
	// The old start must remain available if the low reading proves transient.
	next.StartedAt = min(next.StartedAt, current.StartedAt)
	if current.Rollback != nil {
		next.CountFromAt = current.CountFromAt
		next.Rollback = cloneRollback(current.Rollback)
		return
	}
	cut := sampledAt.Unix()
	next.CountFromAt = cut
	previousCountFrom := CountFrom(current)
	if current.SampledUpstreamUsedPercent == nil || next.SampledUpstreamUsedPercent == nil ||
		previousCountFrom <= 0 || previousCountFrom >= cut {
		return
	}
	next.Rollback = &Rollback{
		PreviousCountFrom:   previousCountFrom,
		PreviousUsedPercent: *current.SampledUpstreamUsedPercent,
		CutUsedPercent:      *next.SampledUpstreamUsedPercent,
	}
}

// Resolution uses upstream percentages only; local prices and model mix can change.
func resolveRollback(window *Window, usedPercent float64) {
	evidence := window.Rollback
	if evidence == nil {
		return
	}
	switch {
	case usedPercent <= evidence.CutUsedPercent+upstreamUsageRollbackEpsilon:
		return
	case usedPercent >= evidence.PreviousUsedPercent-upstreamUsageRollbackEpsilon:
		window.CountFromAt = evidence.PreviousCountFrom
	}
	window.Rollback = nil
}

func validateRollback(window *Window) error {
	evidence := window.Rollback
	if evidence == nil {
		return nil
	}
	if evidence.PreviousCountFrom <= 0 || evidence.PreviousCountFrom >= window.CountFromAt ||
		!validPercent(evidence.PreviousUsedPercent) || !validPercent(evidence.CutUsedPercent) ||
		evidence.CutUsedPercent >= evidence.PreviousUsedPercent {
		return errors.New("OAuth quota rollback evidence is invalid")
	}
	return nil
}
