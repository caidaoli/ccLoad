package util

import "github.com/bytedance/sonic"

// SerializeModels 序列化模型列表为JSON字符串
func SerializeModels(models []string) (string, error) {
	if len(models) == 0 {
		return "[]", nil
	}

	bytes, err := sonic.Marshal(models)
	if err != nil {
		return "[]", err
	}
	return string(bytes), nil
}

// SerializeModelRedirects 序列化模型重定向映射为JSON字符串
func SerializeModelRedirects(redirects map[string]string) (string, error) {
	if len(redirects) == 0 {
		return "{}", nil
	}

	bytes, err := sonic.Marshal(redirects)
	if err != nil {
		return "{}", err
	}
	return string(bytes), nil
}
