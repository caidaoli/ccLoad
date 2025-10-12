package util

import "github.com/bytedance/sonic"

// SerializeJSON 序列化任意类型为JSON字符串，失败时返回默认值
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

// SerializeModels 序列化模型列表为JSON字符串
func SerializeModels(models []string) (string, error) {
	if len(models) == 0 {
		return "[]", nil
	}
	return SerializeJSON(models, "[]")
}

// SerializeModelRedirects 序列化模型重定向映射为JSON字符串
func SerializeModelRedirects(redirects map[string]string) (string, error) {
	if len(redirects) == 0 {
		return "{}", nil
	}
	return SerializeJSON(redirects, "{}")
}
