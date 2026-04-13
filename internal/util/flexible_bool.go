package util

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// FlexibleBool 兼容 JSON 布尔值和常见字符串布尔值。
// 用于请求入口的宽松解析，避免上游/客户端把 "true" 当字符串时直接炸掉。
type FlexibleBool bool

// Bool 返回原生布尔值。
func (b FlexibleBool) Bool() bool {
	return bool(b)
}

// UnmarshalJSON 支持 true/false、"true"/"false" 以及 ParseBool 可识别的字符串。
func (b *FlexibleBool) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) == 0, bytes.Equal(trimmed, []byte("null")):
		*b = false
		return nil
	case bytes.Equal(trimmed, []byte("true")):
		*b = true
		return nil
	case bytes.Equal(trimmed, []byte("false")):
		*b = false
		return nil
	}

	var raw string
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return err
	}
	val, ok := ParseBool(raw)
	if !ok {
		return fmt.Errorf("invalid boolean value %q", raw)
	}
	*b = FlexibleBool(val)
	return nil
}
