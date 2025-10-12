package util

import "github.com/bytedance/sonic"

// MarshalJSON 使用sonic进行JSON序列化
func MarshalJSON(v any) (string, error) {
	bytes, err := sonic.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

// UnmarshalJSON 使用sonic进行JSON反序列化
func UnmarshalJSON(data []byte, v any) error {
	return sonic.Unmarshal(data, v)
}
