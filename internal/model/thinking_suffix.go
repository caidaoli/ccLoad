package model

import (
	"strings"

	"ccLoad/internal/protocol/cliproxy/thinking"
)

// 思考后缀（gpt-5.6-luna(max)）是请求期的等级修饰，不是模型身份。渠道条目、模型列表、
// 选路索引和发往上游的模型名一律使用基名；只有请求体里的思考参数会带上等级。
// 后缀词汇表在这里定义一次，internal/app 与索引构建共用，避免两处判定不一致。

// ParseThinkingSuffix 解析模型名尾部的思考后缀。
// 只有括号内容是已知等级、特殊值或非负整数预算时才算识别成功；其余带括号的模型名
// （例如上游真的叫 foo(bar)）原样返回，ok=false。
func ParseThinkingSuffix(model string) (base string, cfg thinking.ThinkingConfig, ok bool) {
	parsed := thinking.ParseSuffix(model)
	if !parsed.HasSuffix {
		return model, thinking.ThinkingConfig{}, false
	}
	base = strings.TrimSpace(parsed.ModelName)
	if base == "" {
		return model, thinking.ThinkingConfig{}, false
	}
	cfg, ok = parseThinkingSuffixValue(parsed.RawSuffix)
	if !ok {
		return model, thinking.ThinkingConfig{}, false
	}
	return base, cfg, true
}

// RoutingModelName 返回去掉思考后缀的模型名，用于选路、鉴权、冷却与上游请求。
func RoutingModelName(model string) string {
	base, _, ok := ParseThinkingSuffix(model)
	if ok {
		return base
	}
	return model
}

func parseThinkingSuffixValue(rawSuffix string) (thinking.ThinkingConfig, bool) {
	rawSuffix = strings.TrimSpace(rawSuffix)
	if rawSuffix == "" {
		return thinking.ThinkingConfig{}, false
	}
	if mode, ok := thinking.ParseSpecialSuffix(rawSuffix); ok {
		switch mode {
		case thinking.ModeNone:
			return thinking.ThinkingConfig{Mode: thinking.ModeNone}, true
		case thinking.ModeAuto:
			return thinking.ThinkingConfig{Mode: thinking.ModeAuto, Budget: -1}, true
		}
	}
	if level, ok := thinking.ParseLevelSuffix(rawSuffix); ok {
		return thinking.ThinkingConfig{Mode: thinking.ModeLevel, Level: level}, true
	}
	if budget, ok := thinking.ParseNumericSuffix(rawSuffix); ok {
		if budget == 0 {
			return thinking.ThinkingConfig{Mode: thinking.ModeNone}, true
		}
		return thinking.ThinkingConfig{Mode: thinking.ModeBudget, Budget: budget}, true
	}
	return thinking.ThinkingConfig{}, false
}
