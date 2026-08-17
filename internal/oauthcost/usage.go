package oauthcost

import (
	"errors"
	"math"
	"strings"
	"time"
)

const (
	monthlyWindowMinimumSeconds = 28 * 24 * 60 * 60
	monthlyWindowMaximumSeconds = 31 * 24 * 60 * 60
)

// 模型族：上游同一采样里的不同额度窗口可能只覆盖部分模型，
// 累加必须按族归属，否则一个族的消耗会污染另一个族的窗口。
const (
	// FamilyAll 覆盖渠道上的全部模型。
	FamilyAll = ""
	// FamilyGemini 只覆盖 Gemini 系列模型（Antigravity "Gemini Models" 组）。
	FamilyGemini = "gemini"
	// FamilyNonGemini 只覆盖非 Gemini 模型（Antigravity "Claude and GPT models" 组）。
	FamilyNonGemini = "non_gemini"
	// FamilySonnet 只覆盖 Claude Sonnet（Anthropic seven_day_sonnet 窗口）。
	FamilySonnet = "sonnet"
	// FamilyFable 只覆盖 Claude Fable（Anthropic seven_day_fable 窗口）。
	FamilyFable = "fable"
	// FamilySpark 只覆盖 Codex Spark（Codex codex-spark 附加额度窗口）。
	FamilySpark = "spark"
)

// Usage is persisted inside an OAuth credential. Costs come from positive
// standard-cost log entries; channel cost multipliers never apply here.
// 每个上游额度窗口一个槽位，槽位身份是上游的 (limit_name, kind)，而不是窗口时长——
// 同一时长可以对应多个互不相干的上游窗口。
type Usage struct {
	Windows []*Window `json:"windows,omitempty"`
}

// Window is one persisted quota period and its accumulated standard cost.
type Window struct {
	Key                  string `json:"key"`
	Family               string `json:"family,omitempty"`
	WindowSeconds        int64  `json:"window_seconds"`
	StartedAt            int64  `json:"started_at"`
	ResetAt              int64  `json:"reset_at"`
	CountFromAt          int64  `json:"count_from_at,omitempty"`
	ResetDay             int    `json:"reset_day,omitempty"`
	StandardCostMicroUSD int64  `json:"standard_cost_microusd"`
}

// Sample 是一次上游额度采样中的单个窗口边界。
type Sample struct {
	Key           string
	Family        string
	WindowSeconds int64
	ResetAt       time.Time
}

// Key 规范化上游窗口标识，限定为 "limit_name|kind" 形式。
func Key(limitName, kind string) string {
	limitName = strings.ToLower(strings.TrimSpace(limitName))
	kind = strings.ToLower(strings.TrimSpace(kind))
	if limitName == "" && kind == "" {
		return ""
	}
	return limitName + "|" + kind
}

// FamilyMatches 判断某个模型的消耗是否计入该族的额度窗口。
func FamilyMatches(family, modelName string) bool {
	if family == FamilyAll {
		return true
	}
	modelName = strings.ToLower(strings.TrimSpace(modelName))
	switch family {
	// Antigravity 的两组额度按上游模型名前缀划分：gemini-* 归 Gemini Models，
	// 其余（claude-*/gpt-*）归 Claude and GPT models。
	case FamilyGemini:
		return strings.HasPrefix(modelName, "gemini")
	case FamilyNonGemini:
		return modelName != "" && !strings.HasPrefix(modelName, "gemini")
	case FamilySonnet:
		return strings.Contains(modelName, "sonnet")
	case FamilyFable:
		return strings.Contains(modelName, "fable")
	case FamilySpark:
		return strings.Contains(modelName, "spark")
	default:
		return false
	}
}

func validFamily(family string) bool {
	switch family {
	case FamilyAll, FamilyGemini, FamilyNonGemini, FamilySonnet, FamilyFable, FamilySpark:
		return true
	default:
		return false
	}
}

// Families 返回持久化窗口里出现过的模型族集合。
func Families(usage *Usage) []string {
	if usage == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(usage.Windows))
	families := make([]string, 0, len(usage.Windows))
	for _, window := range usage.Windows {
		if window == nil {
			continue
		}
		if _, ok := seen[window.Family]; ok {
			continue
		}
		seen[window.Family] = struct{}{}
		families = append(families, window.Family)
	}
	return families
}

// Clone returns a deep copy of persisted OAuth quota cost state.
func Clone(usage *Usage) *Usage {
	if usage == nil {
		return nil
	}
	clone := &Usage{}
	if usage.Windows != nil {
		clone.Windows = make([]*Window, 0, len(usage.Windows))
		for _, window := range usage.Windows {
			clone.Windows = append(clone.Windows, cloneWindow(window))
		}
	}
	return clone
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
	keys := make(map[string]struct{}, len(usage.Windows))
	for _, window := range usage.Windows {
		if window == nil {
			return errors.New("OAuth quota cost window is missing")
		}
		if strings.TrimSpace(window.Key) == "" || window.WindowSeconds <= 0 {
			return errors.New("OAuth quota cost window identity is invalid")
		}
		if _, ok := keys[window.Key]; ok {
			return errors.New("OAuth quota cost window key is duplicated")
		}
		keys[window.Key] = struct{}{}
		if !validFamily(window.Family) {
			return errors.New("OAuth quota cost window family is invalid")
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

// Find 返回指定窗口标识的持久化累计状态。
func Find(usage *Usage, key string) *Window {
	if usage == nil || key == "" {
		return nil
	}
	for _, window := range usage.Windows {
		if window != nil && window.Key == key {
			return window
		}
	}
	return nil
}

// Reconcile aligns persisted counters with freshly sampled upstream window
// boundaries. A changed boundary starts at zero unless a manual count cutoff
// still belongs to the sampled period. 采样里不再出现的窗口直接丢弃。
func Reconcile(current *Usage, samples []Sample, observedAt time.Time) *Usage {
	next := &Usage{}
	seen := make(map[string]struct{}, len(samples))
	for _, sample := range samples {
		key := strings.TrimSpace(sample.Key)
		if key == "" || sample.WindowSeconds <= 0 || sample.ResetAt.IsZero() {
			continue
		}
		if !validFamily(sample.Family) {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		sample.Key = key
		if window := reconcileWindow(Find(current, key), sample, observedAt); window != nil {
			next.Windows = append(next.Windows, window)
		}
	}
	// 采样里一个有效窗口都没有时不销毁已累计数据——拿不到边界是缺信息，
	// 不代表上游窗口消失；真的消失时采样里会有其他窗口，走上面的丢弃分支。
	if len(next.Windows) == 0 {
		return Clone(current)
	}
	return next
}

func reconcileWindow(current *Window, sample Sample, observedAt time.Time) *Window {
	resetAt := sample.ResetAt.UTC()
	next := newWindow(sample, observedAt, 0)
	if isMonthlyWindow(sample.WindowSeconds) && current != nil && current.ResetDay > resetAt.Day() &&
		resetAt.Day() == daysInMonth(resetAt.Year(), resetAt.Month(), resetAt.Location()) {
		next = newWindow(sample, observedAt, current.ResetDay)
	}
	if next == nil || current == nil || current.WindowSeconds != sample.WindowSeconds {
		return next
	}
	current = cloneWindow(current)
	advanceWindow(current, observedAt)
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

func newWindow(sample Sample, observedAt time.Time, resetDay int) *Window {
	if sample.ResetAt.IsZero() || sample.WindowSeconds <= 0 {
		return nil
	}
	resetAt := sample.ResetAt.UTC()
	window := &Window{
		Key:           sample.Key,
		Family:        sample.Family,
		WindowSeconds: sample.WindowSeconds,
		ResetAt:       resetAt.Unix(),
	}
	if isMonthlyWindow(window.WindowSeconds) {
		if resetDay <= 0 {
			resetDay = resetAt.Day()
		}
		window.ResetDay = resetDay
	}
	window.StartedAt = periodStart(window, resetAt).Unix()
	advanceWindow(window, observedAt)
	return window
}

// Reset starts new local counters immediately after an upstream manual reset.
// costByFamily 给出各模型族自 resetAt 起的已落盘成本；缺失的族按零处理。
// The next upstream quota sample reconciles the provisional boundaries.
func Reset(current *Usage, resetAt time.Time, costByFamily map[string]int64) *Usage {
	next := Clone(current)
	if next == nil {
		return nil
	}
	resetAt = resetAt.UTC()
	for _, window := range next.Windows {
		if window == nil {
			continue
		}
		advanceWindow(window, resetAt)
		window.CountFromAt = resetAt.Unix()
		window.StandardCostMicroUSD = costByFamily[window.Family]
	}
	return next
}

// AddStandardCost applies one persisted log to every quota window whose model
// family covers modelName. The half-open period prevents late old logs entering
// a new cycle after another worker has already advanced it.
func AddStandardCost(usage *Usage, at time.Time, modelName string, costMicroUSD int64) (bool, error) {
	if usage == nil || costMicroUSD == 0 {
		return false, nil
	}
	if costMicroUSD < 0 {
		return false, errors.New("OAuth quota standard cost cannot be negative")
	}
	changed := false
	for _, window := range usage.Windows {
		if window == nil || !FamilyMatches(window.Family, modelName) {
			continue
		}
		advanceWindow(window, at)
		countFromAt := window.StartedAt
		if window.CountFromAt > countFromAt {
			countFromAt = window.CountFromAt
		}
		if at.Before(time.Unix(countFromAt, 0)) || !at.Before(time.Unix(window.ResetAt, 0)) {
			continue
		}
		if window.StandardCostMicroUSD > math.MaxInt64-costMicroUSD {
			return false, errors.New("OAuth quota standard cost overflow")
		}
		window.StandardCostMicroUSD += costMicroUSD
		changed = true
	}
	return changed, nil
}

func advanceWindow(window *Window, at time.Time) {
	if window == nil || window.WindowSeconds <= 0 || window.ResetAt <= window.StartedAt {
		return
	}
	resetAt := time.Unix(window.ResetAt, 0).UTC()
	if isMonthlyWindow(window.WindowSeconds) && window.ResetDay == 0 {
		window.ResetDay = resetAt.Day()
	}
	advanced := false
	for !at.Before(resetAt) {
		resetAt = periodEnd(window, resetAt)
		advanced = true
	}
	if !advanced {
		return
	}
	window.StartedAt = periodStart(window, resetAt).Unix()
	window.ResetAt = resetAt.Unix()
	if window.CountFromAt < window.StartedAt {
		window.CountFromAt = 0
	}
	window.StandardCostMicroUSD = 0
}

// isMonthlyWindow 判断窗口是否按自然月推进——月长不固定，按秒推进会漂移。
func isMonthlyWindow(windowSeconds int64) bool {
	return windowSeconds >= monthlyWindowMinimumSeconds && windowSeconds <= monthlyWindowMaximumSeconds
}

func periodStart(window *Window, resetAt time.Time) time.Time {
	if isMonthlyWindow(window.WindowSeconds) {
		return addMonthsClamped(resetAt, -1, window.ResetDay)
	}
	return resetAt.Add(-time.Duration(window.WindowSeconds) * time.Second)
}

func periodEnd(window *Window, startedAt time.Time) time.Time {
	if isMonthlyWindow(window.WindowSeconds) {
		return addMonthsClamped(startedAt, 1, window.ResetDay)
	}
	return startedAt.Add(time.Duration(window.WindowSeconds) * time.Second)
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
