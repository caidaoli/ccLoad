package util

import "github.com/bytedance/sonic"

// SerializeJSON 序列化任意类型为JSON字符串，失败时返回默认值
// 自动处理空值：nil返回默认值，空切片/map正常序列化为[]或{}
func SerializeJSON(v any, defaultValue string) (string, error) {
	// 检查空值
	if v == nil {
		return defaultValue, nil
	}

	bytes, err := sonic.Marshal(v)
	if err != nil {
		return defaultValue, err
	}
	return string(bytes), nil
}
