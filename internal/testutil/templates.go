// Package testutil provides testing utilities for channel validation.
package testutil

import (
	"embed"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
)

//go:embed templates/*.json
var templatesFS embed.FS

// loadTemplate 从嵌入的模板文件加载JSON模板
func loadTemplate(name string) (map[string]any, error) {
	data, err := templatesFS.ReadFile("templates/" + name + ".json")
	if err != nil {
		return nil, err
	}
	var tpl map[string]any
	if err := sonic.Unmarshal(data, &tpl); err != nil {
		return nil, err
	}
	return tpl, nil
}

// applyTemplateReplacements 递归替换模板中的占位符
// 支持的占位符: {{MODEL}}, {{STREAM}}, {{CONTENT}}, {{MAX_TOKENS}}, {{USER_ID}}
func applyTemplateReplacements(v any, replacements map[string]any) any {
	switch val := v.(type) {
	case string:
		// 检查是否是纯占位符（如 "{{STREAM}}"）
		if strings.HasPrefix(val, "{{") && strings.HasSuffix(val, "}}") {
			key := val[2 : len(val)-2]
			if replacement, ok := replacements[key]; ok {
				return replacement
			}
		}
		// 检查是否包含占位符（如 "prefix {{MODEL}} suffix"）
		result := val
		for key, replacement := range replacements {
			placeholder := "{{" + key + "}}"
			if strings.Contains(result, placeholder) {
				var replStr string
				switch r := replacement.(type) {
				case string:
					replStr = r
				case bool:
					replStr = strconv.FormatBool(r)
				case int:
					replStr = strconv.Itoa(r)
				case int64:
					replStr = strconv.FormatInt(r, 10)
				case float64:
					replStr = strconv.FormatFloat(r, 'f', -1, 64)
				default:
					replStr = ""
				}
				result = strings.ReplaceAll(result, placeholder, replStr)
			}
		}
		return result
	case map[string]any:
		newMap := make(map[string]any, len(val))
		for k, v := range val {
			newMap[k] = applyTemplateReplacements(v, replacements)
		}
		return newMap
	case []any:
		newSlice := make([]any, len(val))
		for i, v := range val {
			newSlice[i] = applyTemplateReplacements(v, replacements)
		}
		return newSlice
	default:
		return val
	}
}

// buildRequestFromTemplate 从模板构建请求体
func buildRequestFromTemplate(templateName string, replacements map[string]any) ([]byte, error) {
	tpl, err := loadTemplate(templateName)
	if err != nil {
		return nil, err
	}
	result := applyTemplateReplacements(tpl, replacements)
	return sonic.Marshal(result)
}
