package model

// FingerprintStats 描述一次指纹采样的统计摘要（分布的均值/中位数/众数等）。
// 独立于 util 包定义，避免 storage → util 的跨层依赖；job 层负责从 util 的计算结果拷贝字段。
type FingerprintStats struct {
	Mean      float64 `json:"mean"`
	Median    float64 `json:"median"`
	StdDev    float64 `json:"std_dev"`
	Min       int     `json:"min"`
	Max       int     `json:"max"`
	Unique    int     `json:"unique"`
	Mode      int     `json:"mode"`
	ModeCount int     `json:"mode_count"`
}

// ModelFingerprint 持久化的模型指纹基线记录。
// ChannelID 为 nil 表示渠道已被删除但基线保留（DeleteConfig 时清空，不级联删除）。
type ModelFingerprint struct {
	ID            int64            `json:"id"`
	Name          string           `json:"name"`
	ChannelID     *int64           `json:"channel_id"`
	ChannelName   string           `json:"channel_name"`
	Model         string           `json:"model"`
	ActualModel   string           `json:"actual_model,omitempty"`
	ChannelType   string           `json:"channel_type"`
	SampleCount   int              `json:"sample_count"`
	Distribution  []float64        `json:"distribution"`
	Stats         FingerprintStats `json:"stats"`
	RawData       []int            `json:"raw_data,omitempty"`
	PromptVersion string           `json:"prompt_version"`
	CreatedAt     JSONTime         `json:"created_at"`
	UpdatedAt     JSONTime         `json:"updated_at"`
}

// FingerprintTestRecord 持久化的指纹对比结果记录。
type FingerprintTestRecord struct {
	ID           int64     `json:"id"`
	ChannelID    *int64    `json:"channel_id"`
	ChannelName  string    `json:"channel_name"`
	Model        string    `json:"model"`
	SampleCount  int       `json:"sample_count"`
	BestScore    float64   `json:"best_score"`
	Distribution []float64 `json:"distribution"`
	MatchesJSON  string    `json:"-"`
	Matches      []any     `json:"matches,omitempty"`
	CreatedAt    JSONTime  `json:"created_at"`
}
