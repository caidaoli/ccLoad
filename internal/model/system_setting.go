package model

// SystemSetting 系统配置项
type SystemSetting struct {
	Key          string `json:"key"`           // 配置键(如log_retention_days)
	Value        string `json:"value"`         // 配置值(字符串存储,运行时解析)
	ValueType    string `json:"value_type"`    // 值类型(int/bool/string/duration)
	Description  string `json:"description"`   // 配置说明(用于前端显示)
	DefaultValue string `json:"default_value"` // 默认值(用于重置功能)
	UpdatedAt    int64  `json:"updated_at"`    // 更新时间(Unix秒)
}
