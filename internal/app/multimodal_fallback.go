package app

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"ccLoad/internal/model"
)

const (
	modelMultimodalFallbackSettingKey = "model_multimodal_fallback"
	maxMultimodalFallbackBytes        = 8 * 1024
	maxMultimodalFallbackMappings     = 64
)

// parseMultimodalFallbackModels 解析多模态回退映射。值形态是裸 map
// {"文本模型":"回退模型"}，JSON key 天然去重，无顺序语义。
//
// 运行期查表时请求模型按 RoutingModelName 归一为基名，因此 key 在此处统一
// 小写 + 剥离思考后缀；value 保留管理员原始写法（可带后缀，如 "gemini-3-pro(max)"）。
// 归一后 from == to 或两个原始 key 归一后重复都是配置错误。
func parseMultimodalFallbackModels(value string) (map[string]string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "null" || value == "{}" {
		return nil, nil
	}
	if len(value) > maxMultimodalFallbackBytes {
		return nil, fmt.Errorf("exceeds maximum size of %d bytes", maxMultimodalFallbackBytes)
	}

	var raw map[string]string
	decoder := json.NewDecoder(strings.NewReader(value))
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid multimodal fallback JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("invalid multimodal fallback JSON: trailing data")
	}
	if len(raw) > maxMultimodalFallbackMappings {
		return nil, fmt.Errorf("exceeds maximum of %d mappings", maxMultimodalFallbackMappings)
	}

	mappings := make(map[string]string, len(raw))
	for from, to := range raw {
		from = strings.ToLower(strings.TrimSpace(model.RoutingModelName(from)))
		to = strings.TrimSpace(to)
		if from == "" {
			return nil, fmt.Errorf("mapping model name must not be blank")
		}
		if to == "" {
			return nil, fmt.Errorf("fallback model for %q must not be blank", from)
		}
		normalizedTo := strings.ToLower(strings.TrimSpace(model.RoutingModelName(to)))
		if from == normalizedTo {
			return nil, fmt.Errorf("mapping %q must not fall back to itself", from)
		}
		if _, exists := mappings[from]; exists {
			return nil, fmt.Errorf("duplicate mapping key after normalization: %q", from)
		}
		mappings[from] = to
	}
	if len(mappings) == 0 {
		return nil, nil
	}
	return mappings, nil
}

// multimodalFallbackModel 返回多模态请求应切换到的回退模型名。lookup key 与
// parseMultimodalFallbackModels 的归一规则一致（小写 + 剥思考后缀）；未命中或
// 请求不含非文本内容时返回空串。返回值为管理员配置的原始写法，可能带思考后缀。
func (s *Server) multimodalFallbackModel(requestModel string, hasNonText bool) string {
	if !hasNonText || len(s.multimodalFallbackModels) == 0 {
		return ""
	}
	return s.multimodalFallbackModels[strings.ToLower(model.RoutingModelName(requestModel))]
}
