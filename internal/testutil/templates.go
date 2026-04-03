// Package testutil provides testing utilities for channel validation.
package testutil

import (
	"embed"
	"strings"

	"github.com/bytedance/sonic"
)

//go:embed templates/*.json
var templatesFS embed.FS

// loadTemplate 从嵌入的模板文件加载JSON模板文本
func loadTemplate(name string) (string, error) {
	data, err := templatesFS.ReadFile("templates/" + name + ".json")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func marshalTemplateValue(v any) (string, error) {
	data, err := sonic.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func marshalTemplateStringFragment(v any) (string, error) {
	if s, ok := v.(string); ok {
		encoded, err := marshalTemplateValue(s)
		if err != nil {
			return "", err
		}
		return encoded[1 : len(encoded)-1], nil
	}
	return marshalTemplateValue(v)
}

// applyTemplateReplacements 替换模板中的占位符，保留原始 JSON 字段顺序
// 支持的占位符: {{MODEL}}, {{STREAM}}, {{CONTENT}}, {{MAX_TOKENS}}, {{USER_ID}}
func applyTemplateReplacements(tpl string, replacements map[string]any) (string, error) {
	result := tpl

	for key, replacement := range replacements {
		literal, err := marshalTemplateValue(replacement)
		if err != nil {
			return "", err
		}
		result = strings.ReplaceAll(result, `"`+"{{"+key+"}}"+`"`, literal)
	}

	for key, replacement := range replacements {
		fragment, err := marshalTemplateStringFragment(replacement)
		if err != nil {
			return "", err
		}
		result = strings.ReplaceAll(result, "{{"+key+"}}", fragment)
	}

	return result, nil
}

// buildRequestFromTemplate 从模板构建请求体
func buildRequestFromTemplate(templateName string, replacements map[string]any) ([]byte, error) {
	tpl, err := loadTemplate(templateName)
	if err != nil {
		return nil, err
	}
	result, err := applyTemplateReplacements(tpl, replacements)
	if err != nil {
		return nil, err
	}
	return []byte(result), nil
}
