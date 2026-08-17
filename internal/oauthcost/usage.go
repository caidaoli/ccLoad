package oauthcost

import (
	"errors"
	"math"
	"time"
)

type period string

const (
	weekly  period = "weekly"
	monthly period = "monthly"
)

// Usage is persisted inside an OAuth credential. Costs come from positive
// standard-cost log entries; channel cost multipliers never apply here.
type Usage struct {
	Weekly  *Window `json:"weekly,omitempty"`
	Monthly *Window `json:"monthly,omitempty"`
}

// Window is one persisted quota period and its accumulated standard cost.
type Window struct {
	StartedAt            int64 `json:"started_at"`
	ResetAt              int64 `json:"reset_at"`
	CountFromAt          int64 `json:"count_from_at,omitempty"`
	ResetDay             int   `json:"reset_day,omitempty"`
	StandardCostMicroUSD int64 `json:"standard_cost_microusd"`
}

// Clone returns a deep copy of persisted OAuth quota cost state.
func Clone(usage *Usage) *Usage {
	if usage == nil {
		return nil
	}
	clone := *usage
	clone.Weekly = cloneWindow(usage.Weekly)
	clone.Monthly = cloneWindow(usage.Monthly)
	return &clone
}

func cloneWindow(window *Window) *Window {
	if window == nil {
		return nil
	}
	clone := *window
	return &clone
}

// Validate rejects corrupt quota periods before they can enter a credential.
func Validate(usage *Usage) error {
	if usage == nil {
		return nil
	}
	for _, window := range []*Window{usage.Weekly, usage.Monthly} {
		if window == nil {
			continue
		}
		if window.StartedAt <= 0 || window.ResetAt <= window.StartedAt {
			return errors.New("OAuth quota cost window is invalid")
		}
		if window.CountFromAt < 0 || window.ResetDay < 0 || window.ResetDay > 31 {
			return errors.New("OAuth quota cost window boundary is invalid")
		}
		if window.StandardCostMicroUSD < 0 {
			return errors.New("OAuth quota standard cost cannot be negative")
		}
	}
	return nil
}

// Reconcile aligns persisted counters with freshly sampled upstream reset
// boundaries. A changed boundary starts at zero unless a manual count cutoff
// still belongs to the sampled period.
func Reconcile(
	current *Usage,
	weeklyReset *time.Time,
	monthlyReset *time.Time,
	observedAt time.Time,
) *Usage {
	next := Clone(current)
	if next == nil {
		next = &Usage{}
	}
	if weeklyReset != nil {
		next.Weekly = reconcileWindow(currentWindow(current, weekly), weekly, *weeklyReset, observedAt)
	}
	if monthlyReset != nil {
		next.Monthly = reconcileWindow(currentWindow(current, monthly), monthly, *monthlyReset, observedAt)
	}
	if next.Weekly == nil && next.Monthly == nil {
		return nil
	}
	return next
}

func reconcileWindow(current *Window, period period, resetAt, observedAt time.Time) *Window {
	next := newWindow(period, resetAt, observedAt)
	if period == monthly && current != nil && current.ResetDay > resetAt.Day() &&
		resetAt.Day() == daysInMonth(resetAt.Year(), resetAt.Month(), resetAt.Location()) {
		next = newWindowWithResetDay(period, resetAt, observedAt, current.ResetDay)
	}
	if next == nil || current == nil {
		return next
	}
	current = cloneWindow(current)
	advanceWindow(current, period, observedAt)
	if current.StartedAt == next.StartedAt && current.ResetAt == next.ResetAt {
		next.StandardCostMicroUSD = current.StandardCostMicroUSD
		next.CountFromAt = current.CountFromAt
		return next
	}
	if current.CountFromAt > 0 && current.CountFromAt < next.ResetAt && observedAt.Before(time.Unix(next.ResetAt, 0)) {
		next.StandardCostMicroUSD = current.StandardCostMicroUSD
		next.CountFromAt = current.CountFromAt
	}
	return next
}

func newWindow(period period, resetAt, observedAt time.Time) *Window {
	return newWindowWithResetDay(period, resetAt, observedAt, 0)
}

func newWindowWithResetDay(period period, resetAt, observedAt time.Time, resetDay int) *Window {
	if resetAt.IsZero() {
		return nil
	}
	resetAt = resetAt.UTC()
	window := &Window{
		ResetAt: resetAt.Unix(),
	}
	if period == monthly {
		if resetDay <= 0 {
			resetDay = resetAt.Day()
		}
		window.ResetDay = resetDay
	}
	window.StartedAt = periodStart(period, resetAt, window.ResetDay).Unix()
	advanceWindow(window, period, observedAt)
	return window
}

// Reset starts new local counters immediately after an upstream manual reset.
// The next upstream quota sample reconciles the provisional boundaries.
func Reset(current *Usage, resetAt time.Time, standardCostMicroUSD int64) *Usage {
	next := Clone(current)
	if next == nil {
		return nil
	}
	resetAt = resetAt.UTC()
	for _, item := range []struct {
		period period
		window *Window
	}{
		{period: weekly, window: next.Weekly},
		{period: monthly, window: next.Monthly},
	} {
		if item.window == nil {
			continue
		}
		advanceWindow(item.window, item.period, resetAt)
		item.window.CountFromAt = resetAt.Unix()
		item.window.StandardCostMicroUSD = standardCostMicroUSD
	}
	return next
}

// AddStandardCost applies one persisted log to every active weekly/monthly
// channel quota. The half-open period prevents late old logs entering a new
// cycle after another worker has already advanced it.
func AddStandardCost(usage *Usage, at time.Time, costMicroUSD int64) (bool, error) {
	if usage == nil || costMicroUSD == 0 {
		return false, nil
	}
	if costMicroUSD < 0 {
		return false, errors.New("OAuth quota standard cost cannot be negative")
	}
	changed := false
	for _, item := range []struct {
		period period
		window *Window
	}{
		{period: weekly, window: usage.Weekly},
		{period: monthly, window: usage.Monthly},
	} {
		if item.window == nil {
			continue
		}
		advanceWindow(item.window, item.period, at)
		countFromAt := item.window.StartedAt
		if item.window.CountFromAt > countFromAt {
			countFromAt = item.window.CountFromAt
		}
		if at.Before(time.Unix(countFromAt, 0)) || !at.Before(time.Unix(item.window.ResetAt, 0)) {
			continue
		}
		if item.window.StandardCostMicroUSD > math.MaxInt64-costMicroUSD {
			return false, errors.New("OAuth quota standard cost overflow")
		}
		item.window.StandardCostMicroUSD += costMicroUSD
		changed = true
	}
	return changed, nil
}

func currentWindow(usage *Usage, period period) *Window {
	if usage == nil {
		return nil
	}
	if period == monthly {
		return usage.Monthly
	}
	return usage.Weekly
}

func advanceWindow(window *Window, period period, at time.Time) {
	if window == nil || window.ResetAt <= window.StartedAt {
		return
	}
	resetAt := time.Unix(window.ResetAt, 0).UTC()
	if period == monthly && window.ResetDay == 0 {
		window.ResetDay = resetAt.Day()
	}
	advanced := false
	for !at.Before(resetAt) {
		resetAt = periodEnd(period, resetAt, window.ResetDay)
		advanced = true
	}
	if !advanced {
		return
	}
	window.StartedAt = periodStart(period, resetAt, window.ResetDay).Unix()
	window.ResetAt = resetAt.Unix()
	if window.CountFromAt < window.StartedAt {
		window.CountFromAt = 0
	}
	window.StandardCostMicroUSD = 0
}

func periodStart(period period, resetAt time.Time, resetDay int) time.Time {
	if period == monthly {
		return addMonthsClamped(resetAt, -1, resetDay)
	}
	return resetAt.Add(-7 * 24 * time.Hour)
}

func periodEnd(period period, startedAt time.Time, resetDay int) time.Time {
	if period == monthly {
		return addMonthsClamped(startedAt, 1, resetDay)
	}
	return startedAt.Add(7 * 24 * time.Hour)
}

func addMonthsClamped(value time.Time, months, anchorDay int) time.Time {
	if anchorDay <= 0 {
		anchorDay = value.Day()
	}
	first := time.Date(value.Year(), value.Month()+time.Month(months), 1,
		value.Hour(), value.Minute(), value.Second(), value.Nanosecond(), value.Location())
	day := min(anchorDay, daysInMonth(first.Year(), first.Month(), first.Location()))
	return time.Date(first.Year(), first.Month(), day,
		value.Hour(), value.Minute(), value.Second(), value.Nanosecond(), value.Location())
}

func daysInMonth(year int, month time.Month, location *time.Location) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, location).Day()
}
