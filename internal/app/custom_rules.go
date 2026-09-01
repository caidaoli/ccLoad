package app

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"

	"ccLoad/internal/model"

	"github.com/bytedance/sonic"
)

// authHeaderBlacklist 禁止自定义规则改写的认证头（大小写不敏感）
var authHeaderBlacklist = map[string]struct{}{
	"authorization":  {},
	"x-api-key":      {},
	"x-goog-api-key": {},
}

// applyHeaderRules 按配置顺序改写请求头；认证头受黑名单保护，规则被静默忽略并记录警告。
//
// 每条规则先把头名对齐到请求里已存在的同名键：Claude Code CLI 与 ZCode 指纹路径按
// 线上原样大小写写头（如 "anthropic-beta"），而 http.Header 的 Set/Add/Del 只认
// canonical 键，直接调用会让同一个头以两种大小写并存并一起发给上游。
func applyHeaderRules(h http.Header, rules []model.CustomHeaderRule) {
	if h == nil || len(rules) == 0 {
		return
	}
	for idx, rule := range rules {
		name := strings.TrimSpace(rule.Name)
		if name == "" {
			continue
		}
		if _, blocked := authHeaderBlacklist[strings.ToLower(name)]; blocked {
			slog.Warn("custom_request_rules: header rule on auth header ignored",
				"rule_index", idx, "action", rule.Action, "header", name)
			continue
		}
		key, exists := mergeHeaderVariantsToKey(h, name)
		switch rule.Action {
		case model.RuleActionRemove:
			if !exists {
				continue
			}
			if target := strings.TrimSpace(rule.Value); target != "" {
				removeHeaderToken(h, key, target)
			} else {
				delete(h, key)
			}
		case model.RuleActionOverride:
			if exists {
				h[key] = []string{rule.Value}
				continue
			}
			h.Set(name, rule.Value)
		case model.RuleActionAppend:
			if exists {
				h[key] = append(h[key], rule.Value)
				continue
			}
			h.Add(name, rule.Value)
		default:
			slog.Warn("custom_request_rules: unknown header action",
				"rule_index", idx, "action", rule.Action)
		}
	}
}

// mergeHeaderVariantsToKey 返回请求头里与 name 大小写无关相等的真实键，供规则就地改写。
// 名字点出副作用：这不是纯查询——若同一逻辑头以多种大小写并存，会把多余变体的值合并进
// 保留键并删除变体，避免双头发给上游。
func mergeHeaderVariantsToKey(h http.Header, name string) (string, bool) {
	keep := ""
	if _, ok := h[name]; ok {
		keep = name
	}
	extras := make([]string, 0)
	for existing := range h {
		if !strings.EqualFold(existing, name) {
			continue
		}
		if keep == "" {
			keep = existing
			continue
		}
		if existing != keep {
			extras = append(extras, existing)
		}
	}
	if keep == "" {
		return name, false
	}
	for _, extra := range extras {
		h[keep] = append(h[keep], h[extra]...)
		delete(h, extra)
	}
	return keep, true
}

// removeHeaderToken 按逗号 token 精确移除。每条值按 "," 切分、trim 后等值剔除；
// 若某条值所有 token 全部移除则该条值被丢弃；全部为空时整个头被删除。
// 典型用例：从 Anthropic-Beta CSV 头中移除单个 flag，而保留其他 flag。
// key 必须是 h 中真实存在的键（见 mergeHeaderVariantsToKey），内部只做 map 操作。
func removeHeaderToken(h http.Header, key, target string) {
	values := h[key]
	if len(values) == 0 {
		return
	}
	newValues := make([]string, 0, len(values))
	for _, v := range values {
		tokens := strings.Split(v, ",")
		kept := make([]string, 0, len(tokens))
		for _, tok := range tokens {
			t := strings.TrimSpace(tok)
			if t == "" || t == target {
				continue
			}
			kept = append(kept, t)
		}
		if len(kept) > 0 {
			newValues = append(newValues, strings.Join(kept, ", "))
		}
	}
	if len(newValues) == 0 {
		delete(h, key)
		return
	}
	h[key] = newValues
}

// applyBodyRules 尝试对 JSON body 按规则改写；非 JSON body（空/类型不匹配/解析失败）原样返回。
func applyBodyRules(contentType string, body []byte, rules []model.CustomBodyRule) []byte {
	if len(body) == 0 || len(rules) == 0 {
		return body
	}
	if !isJSONContentType(contentType) {
		return body
	}
	root, err := parseOrderedJSON(body)
	if err != nil || (root.kind != '{' && root.kind != '[') {
		return body
	}

	for idx, rule := range rules {
		segs := splitJSONPath(rule.Path)
		if len(segs) == 0 {
			slog.Warn("custom_request_rules: body rule path empty",
				"rule_index", idx, "action", rule.Action)
			continue
		}
		switch rule.Action {
		case model.RuleActionRemove:
			if _, conflict := root.removePath(segs); conflict {
				slog.Warn("custom_request_rules: body remove path conflict",
					"rule_index", idx, "path", rule.Path)
			}
		case model.RuleActionOverride:
			if len(rule.Value) == 0 {
				slog.Warn("custom_request_rules: body override missing value",
					"rule_index", idx, "path", rule.Path)
				continue
			}
			replacement, err := parseOrderedJSON(rule.Value)
			if err != nil {
				slog.Warn("custom_request_rules: body override value not JSON",
					"rule_index", idx, "path", rule.Path)
				continue
			}
			if _, conflict := root.overridePath(segs, replacement); conflict {
				slog.Warn("custom_request_rules: body override path conflict",
					"rule_index", idx, "path", rule.Path)
				continue
			}
		default:
			slog.Warn("custom_request_rules: unknown body action",
				"rule_index", idx, "action", rule.Action)
		}
	}
	if !root.changed() {
		return body
	}
	updated := root.render()
	if !bytes.Equal(updated, body) {
		return updated
	}
	return body
}

// resolveModelAfterBodyRules 返回顶层 body.model 规则生效后的模型身份。
// 非字符串 model 没有可持久化的模型键，返回空字符串让错误响应中的 model 兜底。
func resolveModelAfterBodyRules(modelName string, rules []model.CustomBodyRule) string {
	resolved := strings.TrimSpace(modelName)
	for _, rule := range rules {
		segs := splitJSONPath(rule.Path)
		if len(segs) != 1 || segs[0] != "model" {
			continue
		}
		switch rule.Action {
		case model.RuleActionRemove:
			resolved = ""
		case model.RuleActionOverride:
			var value string
			if err := sonic.Unmarshal(rule.Value, &value); err != nil {
				if sonic.Valid(rule.Value) {
					resolved = ""
				}
				continue
			}
			resolved = strings.TrimSpace(value)
		}
	}
	return resolved
}

// isJSONContentType 判断 Content-Type 是否为 JSON 家族。
func isJSONContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		return false
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return strings.HasSuffix(ct, "/json") || strings.HasSuffix(ct, "+json")
}

// splitJSONPath 按点分切分路径；空段会被丢弃，返回 nil 表示路径无效。
func splitJSONPath(p string) []string {
	p = strings.TrimSpace(p)
	if p == "" {
		return nil
	}
	raw := strings.Split(p, ".")
	segs := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		segs = append(segs, s)
	}
	return segs
}
