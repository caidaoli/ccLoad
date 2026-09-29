package oauthcost

import (
	"math"
	"strings"
	"time"
)

// LedgerRetention covers the longest quota period plus a full period of recovery.
const LedgerRetention = 64 * 24 * time.Hour

// LedgerModel attributes a log to its upstream model when one was reported.
func LedgerModel(requestModel, actualModel string) string {
	if actual := strings.TrimSpace(actualModel); actual != "" {
		return actual
	}
	return strings.TrimSpace(requestModel)
}

// LedgerTotal is a ledger sum grouped by model and migration baseline key.
type LedgerTotal struct {
	Model        string
	WindowKey    string
	CostMicroUSD int64
}

// Range is a half-open interval of Unix seconds.
type Range struct {
	From  int64
	Until int64
}

// CountedRange returns the half-open interval currently charged to a window.
func CountedRange(window *Window) Range {
	if window == nil {
		return Range{}
	}
	return Range{From: CountFrom(window), Until: window.ResetAt}
}

// CostFromLedger sums rows already restricted to CountedRange by the caller.
func CostFromLedger(window *Window, totals []LedgerTotal) int64 {
	if window == nil {
		return 0
	}
	var total int64
	for _, entry := range totals {
		if entry.CostMicroUSD <= 0 {
			continue
		}
		if entry.WindowKey != "" {
			if entry.WindowKey != window.Key {
				continue
			}
		} else if !WindowMatchesModel(window, entry.Model) {
			continue
		}
		if total > math.MaxInt64-entry.CostMicroUSD {
			return math.MaxInt64
		}
		total += entry.CostMicroUSD
	}
	return total
}

// WindowsAt advances an isolated copy to at without changing persisted state.
func WindowsAt(usage *Usage, at time.Time) []*Window {
	view := Clone(usage)
	if view == nil {
		return nil
	}
	for _, window := range view.Windows {
		advanceWindow(window, at)
	}
	applyEpoch(view)
	return view.Windows
}

// EffectiveUsage bootstraps boundaries from an upstream snapshot only when absent.
func EffectiveUsage(usage *Usage, rawSnapshot []byte) *Usage {
	if usage != nil && len(usage.Windows) > 0 {
		return Clone(usage)
	}
	return BootstrapFromSnapshot(usage, rawSnapshot)
}

// CostView exposes credential boundaries with costs derived from the ledger.
type CostView struct {
	CreditStandardCostMicroUSD int64            `json:"credit_standard_cost_microusd,omitempty"`
	Windows                    []WindowCostView `json:"windows,omitempty"`
}

// WindowCostView is one quota window's ledger-derived standard cost.
type WindowCostView struct {
	Key                  string `json:"key"`
	WindowSeconds        int64  `json:"window_seconds"`
	ResetAt              int64  `json:"reset_at"`
	StandardCostMicroUSD int64  `json:"standard_cost_microusd"`
}

// NewCostView expects totals[i] to cover windows[i]'s counted interval.
func NewCostView(usage *Usage, windows []*Window, totals [][]LedgerTotal) *CostView {
	if usage == nil {
		return nil
	}
	view := &CostView{CreditStandardCostMicroUSD: usage.CreditStandardCostMicroUSD}
	for i, window := range windows {
		if window == nil {
			continue
		}
		var windowTotals []LedgerTotal
		if i < len(totals) {
			windowTotals = totals[i]
		}
		view.Windows = append(view.Windows, WindowCostView{
			Key:                  window.Key,
			WindowSeconds:        window.WindowSeconds,
			ResetAt:              window.ResetAt,
			StandardCostMicroUSD: CostFromLedger(window, windowTotals),
		})
	}
	return view
}

// FindWindow returns the cost view for key, if present.
func (view *CostView) FindWindow(key string) *WindowCostView {
	if view == nil || key == "" {
		return nil
	}
	for i := range view.Windows {
		if view.Windows[i].Key == key {
			return &view.Windows[i]
		}
	}
	return nil
}
