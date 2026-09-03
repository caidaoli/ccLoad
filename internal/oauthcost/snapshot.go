package oauthcost

import (
	"encoding/json"
	"strings"
	"time"
)

// 上游 provider 标识，取值与各 auth 包的 ChannelType 一致。
// 常量放在这里而不是引用 auth 包：auth 包的凭证结构已经依赖本包，反向引用会成环。
const (
	ProviderCodex       = "codex"
	ProviderAnthropic   = "anthropic"
	ProviderAntigravity = "antigravity"
	ProviderXAI         = "xai"
)

const (
	// xAI 的额度不在 windows 数组里，按固定标识合成窗口槽位。
	xaiWindowLimitName      = "xai"
	xaiMonthlyWindowSeconds = 30 * 24 * 60 * 60
	weeklyWindowSeconds     = 7 * 24 * 60 * 60
)

// SnapshotWindow 是持久化额度采样里的单个上游窗口。UsedPercent 用于识别
// 不依赖 reset_at 的上游提前重置；旧快照缺失该字段时只建立边界，不猜重置。
type SnapshotWindow struct {
	LimitName          string    `json:"limit_name"`
	Kind               string    `json:"kind"`
	UsedPercent        *float64  `json:"used_percent,omitempty"`
	LimitWindowSeconds int64     `json:"limit_window_seconds"`
	ResetAt            int64     `json:"reset_at"`
	SampledAt          time.Time `json:"-"`
}

// SnapshotBilling 承载 xAI 独有的周/月额度边界。
type SnapshotBilling struct {
	WeeklyPresent     bool     `json:"weekly_present"`
	WeeklyUsedPercent *float64 `json:"weekly_usage_percent,omitempty"`
	WeeklyResetAt     string   `json:"weekly_reset_at,omitempty"`
	MonthlyPresent    bool     `json:"monthly_present"`
	MonthlyLimitCents *float64 `json:"monthly_limit_cents,omitempty"`
	MonthlyUsedCents  *float64 `json:"included_used_cents,omitempty"`
	MonthlyResetAt    string   `json:"monthly_reset_at,omitempty"`
}

// SnapshotSummary 是重建窗口边界所需的采样字段子集。
type SnapshotSummary struct {
	Provider   string           `json:"provider"`
	Windows    []SnapshotWindow `json:"windows"`
	XAIBilling *SnapshotBilling `json:"xai_billing,omitempty"`
}

// Snapshot 是凭证 oauth_usage 字段的持久化形状（只取重建窗口边界所需字段）。
type Snapshot struct {
	SampledAt string          `json:"sampled_at"`
	Summary   SnapshotSummary `json:"summary"`
}

// Samples 把一次上游额度采样转成持久化槽位。槽位身份是上游的
// (limit_name, kind)，同一时长可以对应多个互不相干的窗口（Antigravity 的
// gemini/3p 周额度、Anthropic 的三个 7 天窗口），只按时长归并必然错位。
func (s *SnapshotSummary) Samples() []Sample {
	if s == nil {
		return nil
	}
	samples := make([]Sample, 0, len(s.Windows)+2)
	for _, window := range s.Windows {
		key := Key(window.LimitName, window.Kind)
		if key == "" || window.ResetAt <= 0 || window.LimitWindowSeconds <= 0 {
			continue
		}
		sample := Sample{
			Key:           key,
			Family:        WindowFamily(s.Provider, window.LimitName, window.Kind),
			WindowSeconds: window.LimitWindowSeconds,
			ResetAt:       time.Unix(window.ResetAt, 0).UTC(),
		}
		if supportsSampledUpstreamReset(s.Provider) {
			sample.UsedPercent = cloneFloat64(window.UsedPercent)
			sample.SampledAt = window.SampledAt
		}
		samples = append(samples, sample)
	}
	if s.XAIBilling == nil {
		return samples
	}
	if s.XAIBilling.WeeklyPresent {
		if resetAt := parseSnapshotTime(s.XAIBilling.WeeklyResetAt); !resetAt.IsZero() {
			samples = append(samples, Sample{
				Key:           Key(xaiWindowLimitName, "weekly"),
				WindowSeconds: weeklyWindowSeconds,
				ResetAt:       resetAt,
				UsedPercent:   cloneFloat64(s.XAIBilling.WeeklyUsedPercent),
			})
		}
	}
	if s.XAIBilling.MonthlyPresent {
		if resetAt := parseSnapshotTime(s.XAIBilling.MonthlyResetAt); !resetAt.IsZero() {
			samples = append(samples, Sample{
				Key:           Key(xaiWindowLimitName, "monthly"),
				WindowSeconds: xaiMonthlyWindowSeconds,
				ResetAt:       resetAt,
				UsedPercent:   percentOf(s.XAIBilling.MonthlyUsedCents, s.XAIBilling.MonthlyLimitCents),
			})
		}
	}
	return samples
}

func supportsSampledUpstreamReset(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case ProviderCodex, ProviderAnthropic, ProviderAntigravity, ProviderXAI:
		return true
	default:
		return false
	}
}

func percentOf(used, limit *float64) *float64 {
	if used == nil || limit == nil || *limit <= 0 {
		return nil
	}
	value := *used * 100 / *limit
	return &value
}

// WindowFamily 把 provider 的窗口标识映射到模型族：只覆盖部分模型的
// 窗口不能累加其他模型的消耗。
func WindowFamily(provider, limitName, kind string) string {
	limitName = strings.ToLower(strings.TrimSpace(limitName))
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch provider {
	case ProviderAntigravity:
		// Antigravity 的 bucketId 带族前缀（gemini-* / 3p-*）。
		switch {
		case strings.HasPrefix(kind, "gemini"):
			return FamilyGemini
		case strings.HasPrefix(kind, "3p"):
			return FamilyNonGemini
		}
	case ProviderAnthropic:
		switch kind {
		case "seven_day_sonnet":
			return FamilySonnet
		case "seven_day_fable":
			return FamilyFable
		}
	case ProviderCodex:
		// codex-spark 是独立额度窗口，只覆盖 Spark 模型；主 codex 窗口不包含 Spark。
		// gpt-reserve 也是独立保留额度，不对应任何请求模型。
		if limitName == "gpt-reserve" {
			return FamilyCodexReserve
		}
		if strings.Contains(limitName, "spark") {
			return FamilySpark
		}
		return FamilyCodex
	}
	return FamilyAll
}

// BootstrapFromSnapshot 从凭证里已持久化的 oauth_usage 采样重建窗口边界。
// 窗口边界只能来自上游额度采样，但采样一旦落盘就必须立刻可用于累加——
// 否则渠道要等下一次人工刷新才开始计数，中间的消耗全部静默丢失。
// 采样缺失或无有效窗口时返回 nil，调用方按"没有窗口"处理。
func BootstrapFromSnapshot(rawSnapshot []byte) *Usage {
	if len(rawSnapshot) == 0 {
		return nil
	}
	var snapshot Snapshot
	if err := json.Unmarshal(rawSnapshot, &snapshot); err != nil {
		return nil
	}
	samples := snapshot.Summary.Samples()
	if len(samples) == 0 {
		return nil
	}
	// sampledAt 缺失时按零值观测：窗口保持采样周期，后续 AddStandardCost
	// 会按日志时间推进，不会把成本记进错误的周期。
	usage := Reconcile(nil, samples, parseSnapshotTime(snapshot.SampledAt))
	if usage == nil || len(usage.Windows) == 0 {
		return nil
	}
	return usage
}

func parseSnapshotTime(raw string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}
