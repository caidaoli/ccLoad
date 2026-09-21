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
	// upstreamUsageRollbackEpsilon 是判定上游用量「回退」所需的最小降幅
	// （绝对百分点，开区间）。上游用量在同一个额度周期内只会单调增加，
	// 小数级下降一律是采样噪声：Google remaining_fraction 的浮点抖动、
	// Codex 整数百分比的取整闪烁。真实的上游提前重置下降几十个百分点，
	// 远高于此容差。有意取舍：已用 ≤1% 时发生的真重置（如 1.0→0）会漏检，
	// 由本地时钟越过 reset_at 后的自然滚动兜底。
	upstreamUsageRollbackEpsilon = 1.0
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
	// FamilyCodex 覆盖 Codex 主额度窗口，但不覆盖单独计量的 Spark。
	FamilyCodex = "codex"
	// FamilyCodexReserve 是 Codex 的 gpt-reserve 窗口。
	// 上游没有提供请求级归属，不能把普通 Codex 请求计入该窗口。
	FamilyCodexReserve = "codex_reserve"
)

// Usage is persisted inside an OAuth credential. Costs come from positive
// standard-cost log entries; channel cost multipliers never apply here.
// 每个上游额度窗口一个槽位，槽位身份是上游的 (limit_name, kind)，而不是窗口时长——
// 同一时长可以对应多个互不相干的上游窗口。
type Usage struct {
	// 套餐观察可以暂时未知，但账号主体不能随之丢失。
	Identity  string `json:"identity,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	EpochAt   int64  `json:"epoch_at,omitempty"`
	// 成本窗口以秒计数；事件屏障保留纳秒，避免同秒旧采样越过重置。
	EpochAtUnixNano int64 `json:"epoch_at_unix_nano,omitempty"`
	// 独立购买额度的标准成本，不属于任何周期窗口。
	CreditStandardCostMicroUSD int64     `json:"credit_standard_cost_microusd,omitempty"`
	Windows                    []*Window `json:"windows,omitempty"`
}

// Window is one persisted quota period and its accumulated standard cost.
//
// StartedAt 是周期起点，CountFromAt 是手动重置截止点；计数起点取两者较大值，
// 见 CountFrom。周期切换时若要保留真实计数起点，StartedAt 会被一并前移到
// CountFromAt 之前，所以 StartedAt 不总是名义周期起点。
type Window struct {
	Key                        string   `json:"key"`
	Family                     string   `json:"family,omitempty"`
	WindowSeconds              int64    `json:"window_seconds"`
	StartedAt                  int64    `json:"started_at"`
	ResetAt                    int64    `json:"reset_at"`
	CountFromAt                int64    `json:"count_from_at,omitempty"`
	ResetDay                   int      `json:"reset_day,omitempty"`
	SampledUpstreamUsedPercent *float64 `json:"sampled_upstream_used_percent,omitempty"`
	SampledUpstreamAtUnixNano  int64    `json:"sampled_upstream_at_unix_nano,omitempty"`
	StandardCostMicroUSD       int64    `json:"standard_cost_microusd"`

	// AccountedFrom/AccountedUntil 是 StandardCostMicroUSD 已经计入的日志时间
	// 区间（半开，Unix 秒）。不变式：区间非空时，累计值等于该区间内本族日志
	// 成本之和——包括日志已被保留期清理掉的那部分，这正是边界变化时只能核对
	// 对称差、不能全量重算的原因（日志保留期可以短于周/月额度周期）。
	// 零值表示未知，存储层会按当前边界重算一次并写回，重算结果只取较大值。
	AccountedFrom  int64 `json:"accounted_from,omitempty"`
	AccountedUntil int64 `json:"accounted_until,omitempty"`

	// LocallyAdvanced 标记这个周期是本地按截止时间滚出来的、还没被任何上游采样
	// 确认。此时 StartedAt/ResetAt 都只是暂定值：真实重置可能漂移到旧截止点的
	// 另一侧，采样确认时必须允许边界被改写（见 reconcileWindow）。
	// 显式记录而不是从其他字段反推——否则 advanceWindow 的任何赋值调整都会
	// 让这个判断静默失效，额度数字慢慢跑偏且没有任何编译或测试信号。
	LocallyAdvanced bool `json:"locally_advanced,omitempty"`
}

// CountFrom 返回窗口的计数起点：周期起点与手动重置截止点中较晚的一个。
func CountFrom(window *Window) int64 {
	if window == nil {
		return 0
	}
	return max(window.StartedAt, window.CountFromAt)
}

// Accounted 报告窗口是否带有可信的已计入区间。
func Accounted(window *Window) bool {
	return window != nil && window.AccountedUntil > window.AccountedFrom
}

// MarkAccounted 记录累计成本所覆盖的日志时间区间；空区间表示未知。
func MarkAccounted(window *Window, from, until int64) {
	if window == nil {
		return
	}
	if from <= 0 || until <= from {
		window.AccountedFrom, window.AccountedUntil = 0, 0
		return
	}
	window.AccountedFrom, window.AccountedUntil = from, until
}

// Sample 是一次上游额度采样中的单个窗口状态。
type Sample struct {
	Key           string
	Family        string
	WindowSeconds int64
	ResetAt       time.Time
	UsedPercent   *float64
	SampledAt     time.Time
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
	case FamilyCodex:
		// Reserve and Spark consume independent quotas; response model names
		// cannot determine this, so callers must use the actual request model.
		return modelName != "gpt-reserve" && !strings.Contains(modelName, "spark")
	case FamilyCodexReserve:
		return modelName == "gpt-reserve"
	default:
		return false
	}
}

func validFamily(family string) bool {
	switch family {
	case FamilyAll, FamilyGemini, FamilyNonGemini, FamilySonnet, FamilyFable, FamilySpark, FamilyCodex, FamilyCodexReserve:
		return true
	default:
		return false
	}
}

// WindowMatchesModel 判断一个持久化额度窗口是否应累计指定模型。
// 旧版本把 Codex 主窗口持久化为 FamilyAll；按 key 识别并按新的 Codex
// 族规则匹配，避免历史窗口在下一次刷新前继续吞掉 Spark 成本。
func WindowMatchesModel(window *Window, modelName string) bool {
	if window == nil {
		return false
	}
	return FamilyMatches(WindowModelFamily(window), modelName)
}

// WindowModelFamily resolves historical family values from the quota identity.
func WindowModelFamily(window *Window) string {
	if isCodexReserveKey(window.Key) {
		return FamilyCodexReserve
	}
	family := window.Family
	if family == FamilyAll && strings.EqualFold(strings.TrimSpace(strings.SplitN(window.Key, "|", 2)[0]), ProviderCodex) {
		family = FamilyCodex
	}
	return family
}

func isCodexReserveKey(key string) bool {
	return strings.EqualFold(strings.TrimSpace(strings.SplitN(key, "|", 2)[0]), "gpt-reserve")
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
		family := WindowModelFamily(window)
		if _, ok := seen[family]; ok {
			continue
		}
		seen[family] = struct{}{}
		families = append(families, family)
	}
	return families
}

// Clone returns a deep copy of persisted OAuth quota cost state.
func Clone(usage *Usage) *Usage {
	if usage == nil {
		return nil
	}
	clone := &Usage{CreditStandardCostMicroUSD: usage.CreditStandardCostMicroUSD, Identity: usage.Identity, AccountID: usage.AccountID, EpochAt: usage.EpochAt, EpochAtUnixNano: usage.EpochAtUnixNano}
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
	clone.SampledUpstreamUsedPercent = cloneFloat64(window.SampledUpstreamUsedPercent)
	return &clone
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

// Validate rejects corrupt quota periods before they can enter a credential.
func Validate(usage *Usage) error {
	if usage != nil && usage.CreditStandardCostMicroUSD < 0 {
		return errors.New("OAuth credit standard cost cannot be negative")
	}
	if usage == nil {
		return nil
	}
	if usage.EpochAt < 0 || usage.EpochAtUnixNano < 0 ||
		(usage.EpochAtUnixNano != 0 && usage.EpochAtUnixNano/int64(time.Second) != usage.EpochAt) {
		return errors.New("OAuth quota epoch is invalid")
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
		if window.CountFromAt < 0 || window.ResetDay < 0 || window.ResetDay > 31 ||
			window.SampledUpstreamAtUnixNano < 0 {
			return errors.New("OAuth quota cost window boundary is invalid")
		}
		if usedPercent := window.SampledUpstreamUsedPercent; usedPercent != nil &&
			(math.IsNaN(*usedPercent) || math.IsInf(*usedPercent, 0) || *usedPercent < 0 || *usedPercent > 100) {
			return errors.New("OAuth quota sampled usage is invalid")
		}
		if window.StandardCostMicroUSD < 0 {
			return errors.New("OAuth quota standard cost cannot be negative")
		}
		// 已计入区间只有两种合法状态：整体为零（未知）或半开区间 [from, until)。
		// 半开状态 from>0 && until==0 会被 Accounted 判为未知、却又留着一个看似
		// 可信的起点，后续对账按它做差集就会漏算，必须在入库前拦下。
		if window.AccountedFrom < 0 || window.AccountedUntil < 0 ||
			(window.AccountedFrom == 0) != (window.AccountedUntil == 0) ||
			(window.AccountedUntil > 0 && window.AccountedUntil <= window.AccountedFrom) {
			return errors.New("OAuth quota accounted range is invalid")
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

// Reconcile aligns persisted counters with a complete upstream window
// snapshot. A changed boundary starts at zero unless a manual count cutoff
// still belongs to the sampled period. Windows missing from a complete
// snapshot are retired unless they were sampled after that snapshot. A usage
// rollback is scoped to the sampled window, so a rolling 5-hour reset cannot
// clear the independent weekly counter.
func Reconcile(current *Usage, samples []Sample, observedAt time.Time) *Usage {
	return reconcile(current, samples, observedAt, false)
}

// ReconcilePartial applies a partial upstream sample. It updates only windows
// present in samples and retains omitted siblings: their absence is not proof
// of an upstream reset, so their old cost remains until their own sample or
// local boundary. Usage rollback uses the same per-window scope as Reconcile.
func ReconcilePartial(current *Usage, samples []Sample, observedAt time.Time) *Usage {
	return reconcile(current, samples, observedAt, true)
}

func reconcile(current *Usage, samples []Sample, observedAt time.Time, partial bool) *Usage {
	current, staleCodexLayout := reconcileCodexWeeklyKey(current, samples, observedAt)
	next := Clone(current)
	if next == nil {
		next = &Usage{}
	}
	next.Windows = nil
	seen := make(map[string]struct{}, len(samples))
	snapshotAtNano := sampleTimeUnixNano(observedAt)
	for _, sample := range samples {
		key := strings.TrimSpace(sample.Key)
		if key == "" || sample.WindowSeconds <= 0 || sample.ResetAt.IsZero() {
			continue
		}
		sample.Key = key
		if isCodexReserveKey(key) {
			sample.Family = FamilyCodexReserve
		}
		if !validFamily(sample.Family) {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		// Quota may have been sampled before an auxiliary metadata request.
		// That later request's completion time cannot authorize deletions.
		if sampledAtNano := sampleTimeUnixNano(sample.SampledAt); sampledAtNano > 0 &&
			(snapshotAtNano == 0 || sampledAtNano < snapshotAtNano) {
			snapshotAtNano = sampledAtNano
		}
		if staleCodexLayout && isCodexMainWindowKey(key) {
			continue
		}
		seen[key] = struct{}{}
		sample.UsedPercent = normalizedUsedPercent(sample.UsedPercent)
		if window := reconcileWindow(Find(current, key), sample, observedAt); window != nil {
			next.Windows = append(next.Windows, window)
		}
	}
	if !partial && !staleCodexLayout && len(next.Windows) == 0 {
		// An empty/invalid snapshot has no authority to retire any window,
		// including when only some of the current windows have newer samples.
		return Clone(current)
	}
	if current != nil {
		for _, window := range current.Windows {
			if window == nil {
				continue
			}
			// A complete snapshot cannot retire a sibling learned from a newer
			// passive sample while that snapshot was waiting to be persisted.
			newerSample := snapshotAtNano > 0 && window.SampledUpstreamAtUnixNano > snapshotAtNano
			keepOmitted := partial || newerSample || (staleCodexLayout && isCodexMainWindowKey(window.Key))
			if !keepOmitted {
				continue
			}
			if _, ok := seen[window.Key]; ok {
				continue
			}
			retained := cloneWindow(window)
			advanceWindow(retained, observedAt)
			next.Windows = append(next.Windows, retained)
		}
	}
	// 采样里一个有效窗口都没有时不销毁已累计数据——拿不到边界是缺信息，
	// 不代表上游窗口消失；真的消失时采样里会有其他窗口，走上面的丢弃分支。
	if len(next.Windows) == 0 {
		return Clone(current)
	}
	applyEpoch(next)
	return next
}

// Codex can move its weekly quota between primary and secondary when the 5h
// limit appears or disappears. Move that counter before reconciling periods;
// neither the old 5h counter nor another quota group is a weekly cost source.
// A late snapshot of the previous layout must not move or duplicate it again.
func reconcileCodexWeeklyKey(current *Usage, samples []Sample, observedAt time.Time) (*Usage, bool) {
	for _, sample := range samples {
		key := strings.TrimSpace(sample.Key)
		if !isCodexMainWindowKey(key) || sample.WindowSeconds != weeklyWindowSeconds || sample.ResetAt.IsZero() ||
			(sample.Family != FamilyAll && sample.Family != FamilyCodex) {
			continue
		}
		target := Find(current, key)
		if target != nil && target.WindowSeconds == weeklyWindowSeconds {
			continue
		}
		otherKey := "codex|primary"
		if key == otherKey {
			otherKey = "codex|secondary"
		}
		source := Find(current, otherKey)
		if source == nil || source.WindowSeconds != weeklyWindowSeconds ||
			(source.Family != FamilyAll && source.Family != FamilyCodex) {
			continue
		}
		for _, other := range samples {
			if strings.TrimSpace(other.Key) == otherKey && other.WindowSeconds == weeklyWindowSeconds && !other.ResetAt.IsZero() {
				// Two explicitly reported weekly windows are independent counters.
				return current, false
			}
		}
		sampledAt := sampleTimeUnixNano(firstNonZeroTime(sample.SampledAt, observedAt))
		for _, window := range []*Window{source, target} {
			if window != nil && window.SampledUpstreamAtUnixNano > 0 &&
				(sampledAt < window.SampledUpstreamAtUnixNano ||
					(sampledAt == window.SampledUpstreamAtUnixNano && window.SampledUpstreamUsedPercent != nil)) {
				return current, true
			}
		}
		next := Clone(current)
		next.Windows = make([]*Window, 0, len(current.Windows))
		for _, window := range current.Windows {
			if window != nil && window.Key == key {
				continue
			}
			window = cloneWindow(window)
			if window != nil && window.Key == otherKey {
				window.Key = key
			}
			next.Windows = append(next.Windows, window)
		}
		return next, false
	}
	return current, false
}

func isCodexMainWindowKey(key string) bool {
	return key == "codex|primary" || key == "codex|secondary"
}

func reconcileWindow(current *Window, sample Sample, observedAt time.Time) *Window {
	resetAt := sample.ResetAt.UTC()
	next := newWindow(sample, observedAt, 0)
	if isMonthlyWindow(sample.WindowSeconds) && current != nil && current.ResetDay > resetAt.Day() &&
		resetAt.Day() == daysInMonth(resetAt.Year(), resetAt.Month(), resetAt.Location()) {
		next = newWindow(sample, observedAt, current.ResetDay)
	}
	if next == nil {
		return nil
	}
	if current == nil {
		return next
	}
	current = cloneWindow(current)
	usageSampledAt := firstNonZeroTime(sample.SampledAt, observedAt)
	sampledAtUnixNano := sampleTimeUnixNano(usageSampledAt)
	// Correct a same-period deadline before advancing, or the old deadline can
	// destroy cost before the upstream period ends. Keep the accounting start
	// and manual cutoff so already-counted logs remain in the counted interval.
	if sample.UsedPercent != nil &&
		(sampledAtUnixNano > current.SampledUpstreamAtUnixNano ||
			(sampledAtUnixNano == current.SampledUpstreamAtUnixNano && current.SampledUpstreamUsedPercent == nil)) &&
		current.WindowSeconds == sample.WindowSeconds && sample.ResetAt.After(observedAt) &&
		next.ResetAt > current.StartedAt && sameQuotaPeriod(current, next) &&
		!upstreamUsageRolledBack(current.SampledUpstreamUsedPercent, sample.UsedPercent) {
		current.ResetAt = next.ResetAt
		current.ResetDay = next.ResetDay
	}
	advanceWindow(current, observedAt)
	usageSampleIsNewer := sample.UsedPercent != nil &&
		(current.SampledUpstreamAtUnixNano == 0 || sampledAtUnixNano > current.SampledUpstreamAtUnixNano ||
			(current.SampledUpstreamUsedPercent == nil && sampledAtUnixNano == current.SampledUpstreamAtUnixNano))
	usageSampleIsStale := sample.UsedPercent != nil && current.SampledUpstreamAtUnixNano > 0 &&
		(sampledAtUnixNano < current.SampledUpstreamAtUnixNano ||
			(sampledAtUnixNano == current.SampledUpstreamAtUnixNano && current.SampledUpstreamUsedPercent != nil))
	if usageSampleIsStale {
		// 主动刷新与被动队列会并发更新同一槽位。旧百分比样本的 reset_at 同样是旧的，
		// 必须整条丢弃；只忽略百分比仍可能让旧边界清空新周期成本。
		return current
	}
	if current.WindowSeconds != sample.WindowSeconds {
		return next
	}
	if usageSampleIsNewer && upstreamUsageRolledBack(current.SampledUpstreamUsedPercent, sample.UsedPercent) {
		// 上游可以在原 reset_at 到期前直接恢复额度。使用率在同一额度周期内只会
		// 单调增加；只有超过 upstreamUsageRollbackEpsilon 的显著回退才会切断旧成本，
		// 小数级抖动按噪声保留累计，不能再用 reset_at 的位移猜测。
		next.CountFromAt = usageSampledAt.Unix()
		return next
	}
	if sameQuotaPeriod(current, next) {
		// 同周期保留累计；有效新采样的 reset 边界已在本地滚动前校正。
		// 无用量基线的边界快照仍不能覆盖已确认的重置时间。
		current.Family = next.Family
		if usageSampleIsNewer {
			// A locally advanced period has no upstream baseline. Its start is
			// provisional too: retaining it can exclude logs when the actual
			// reset drifted across the old deadline. The storage transaction
			// reconciles costs against the newly confirmed interval.
			if current.LocallyAdvanced {
				current.StartedAt = next.StartedAt
				current.ResetAt = next.ResetAt
				current.ResetDay = next.ResetDay
			}
			current.SampledUpstreamUsedPercent = cloneFloat64(sample.UsedPercent)
			current.SampledUpstreamAtUnixNano = sampledAtUnixNano
			// 采样确认了边界，暂定状态结束。
			current.LocallyAdvanced = false
		}
		return current
	}
	if current.CountFromAt > 0 && current.CountFromAt < next.ResetAt && observedAt.Before(time.Unix(next.ResetAt, 0)) {
		next.StandardCostMicroUSD = current.StandardCostMicroUSD
		next.CountFromAt = current.CountFromAt
		// 周期切换不得抬高计数起点，否则手动重置后已计入的日志会被排除。
		next.StartedAt = min(next.StartedAt, current.StartedAt)
		// 成本沿用，已计入区间必须一同沿用，否则存储层会按新边界全量重算，
		// 把保留期外的历史成本抹掉。
		next.AccountedFrom, next.AccountedUntil = current.AccountedFrom, current.AccountedUntil
	}
	return next
}

func normalizedUsedPercent(usedPercent *float64) *float64 {
	if usedPercent == nil || math.IsNaN(*usedPercent) || math.IsInf(*usedPercent, 0) ||
		*usedPercent < 0 || *usedPercent > 100 {
		return nil
	}
	return cloneFloat64(usedPercent)
}

func upstreamUsageRolledBack(previous, current *float64) bool {
	return previous != nil && current != nil && *current < *previous-upstreamUsageRollbackEpsilon
}

func sampleTimeUnixNano(sampledAt time.Time) int64 {
	if sampledAt.IsZero() {
		return 0
	}
	return sampledAt.UnixNano()
}

// sameQuotaPeriod 用半窗口容差识别同周期的边界修正或采样抖动；它只决定
// 是否保留累计，不决定采用哪个重置时间。整窗口推进仍视为周期切换。
func sameQuotaPeriod(current, next *Window) bool {
	delta := next.ResetAt - current.ResetAt
	if delta < 0 {
		delta = -delta
	}
	return delta*2 < current.WindowSeconds
}

func newWindow(sample Sample, observedAt time.Time, resetDay int) *Window {
	if sample.ResetAt.IsZero() || sample.WindowSeconds <= 0 {
		return nil
	}
	resetAt := sample.ResetAt.UTC()
	window := &Window{
		Key:                        sample.Key,
		Family:                     sample.Family,
		WindowSeconds:              sample.WindowSeconds,
		ResetAt:                    resetAt.Unix(),
		SampledUpstreamUsedPercent: cloneFloat64(sample.UsedPercent),
		SampledUpstreamAtUnixNano:  sampleTimeUnixNano(firstNonZeroTime(sample.SampledAt, observedAt)),
	}
	if isMonthlyWindow(window.WindowSeconds) {
		if resetDay <= 0 {
			resetDay = resetAt.Day()
		}
		window.ResetDay = resetDay
	}
	window.StartedAt = periodStart(window, resetAt).Unix()
	advanceWindow(window, observedAt)
	// 全新窗口的零成本只是缺省值：该周期内可能已有日志落盘，
	// 已计入区间必须标记为未知，交给存储层按当前边界重算一次。
	window.AccountedFrom, window.AccountedUntil = 0, 0
	return window
}

func firstNonZeroTime(primary, fallback time.Time) time.Time {
	if !primary.IsZero() {
		return primary
	}
	return fallback
}

// Reset starts new local counters immediately after an upstream manual reset.
// costByFamily 按 WindowModelFamily 给出各族自 resetAt 起的已落盘成本；缺失的族按零处理。
// The next upstream quota sample reconciles the provisional boundaries.
func Reset(current *Usage, resetAt time.Time, costByFamily map[string]int64) *Usage {
	next := Clone(current)
	if next == nil {
		next = &Usage{}
	}
	resetAt = resetAt.UTC()
	if resetAt.Before(next.EpochTime()) {
		return next
	}
	next.EpochAt, next.EpochAtUnixNano = resetAt.Unix(), resetAt.UnixNano()
	for _, window := range next.Windows {
		if window == nil {
			continue
		}
		advanceWindow(window, resetAt)
		window.CountFromAt = resetAt.Unix()
		window.SampledUpstreamUsedPercent = nil
		window.SampledUpstreamAtUnixNano = sampleTimeUnixNano(resetAt)
		// 手动重置是已确认的计数起点，不是本地按截止时间猜出来的边界：
		// 后续采样不得改写 StartedAt，否则会把重置后已计入的日志排除在外。
		window.LocallyAdvanced = false
		window.Family = WindowModelFamily(window)
		window.StandardCostMicroUSD = costByFamily[window.Family]
		// costByFamily 正是自计数起点起的已落盘成本，已计入区间随之确定。
		MarkAccounted(window, CountFrom(window), window.ResetAt)
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
		if window == nil || !WindowMatchesModel(window, modelName) {
			continue
		}
		advanceWindow(window, at)
		if at.Before(time.Unix(CountFrom(window), 0)) || !at.Before(time.Unix(window.ResetAt, 0)) {
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
	window.SampledUpstreamUsedPercent = nil
	window.SampledUpstreamAtUnixNano = sampleTimeUnixNano(time.Unix(window.StartedAt, 0).UTC())
	window.StandardCostMicroUSD = 0
	// 本地按截止时间滚出的新周期，边界都是暂定值，等采样确认。
	window.LocallyAdvanced = true
	// 新周期的零成本是确定的：落在新周期内的日志都会先推进窗口再累计，
	// 所以此刻整个新区间「已计入」且为零，后续日志由增量累计维持该不变式。
	MarkAccounted(window, CountFrom(window), window.ResetAt)
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

// EpochTime 保留事件的先后顺序；仅有秒级纪元的状态仍可读。
func (usage *Usage) EpochTime() time.Time {
	if usage == nil || usage.EpochAt == 0 {
		return time.Time{}
	}
	if usage.EpochAtUnixNano != 0 {
		return time.Unix(0, usage.EpochAtUnixNano).UTC()
	}
	return time.Unix(usage.EpochAt, 0).UTC()
}

func applyEpoch(usage *Usage) {
	if usage == nil || usage.EpochAt <= 0 {
		return
	}
	for _, window := range usage.Windows {
		if window != nil && window.StartedAt < usage.EpochAt {
			window.CountFromAt = max(window.CountFromAt, usage.EpochAt)
		}
	}
}
