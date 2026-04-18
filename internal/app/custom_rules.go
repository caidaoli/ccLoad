package app

import (
	"log/slog"
	"net/http"
	"strconv"
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
		switch rule.Action {
		case model.RuleActionRemove:
			h.Del(name)
		case model.RuleActionOverride:
			h.Set(name, rule.Value)
		case model.RuleActionAppend:
			h.Add(name, rule.Value)
		default:
			slog.Warn("custom_request_rules: unknown header action",
				"rule_index", idx, "action", rule.Action)
		}
	}
}

// applyBodyRules 尝试对 JSON body 按规则改写；非 JSON body（空/类型不匹配/解析失败）原样返回。
func applyBodyRules(contentType string, body []byte, rules []model.CustomBodyRule) []byte {
	if len(body) == 0 || len(rules) == 0 {
		return body
	}
	if !isJSONContentType(contentType) {
		return body
	}
	var root any
	if err := sonic.Unmarshal(body, &root); err != nil {
		return body
	}
	// 根必须为对象或数组；字面量无法寻址
	switch root.(type) {
	case map[string]any, []any:
	default:
		return body
	}

	changed := false
	for idx, rule := range rules {
		segs := splitJSONPath(rule.Path)
		if len(segs) == 0 {
			slog.Warn("custom_request_rules: body rule path empty",
				"rule_index", idx, "action", rule.Action)
			continue
		}
		switch rule.Action {
		case model.RuleActionRemove:
			if next, ok := removeJSONPath(root, segs); ok {
				root = next
				changed = true
			}
		case model.RuleActionOverride:
			var parsed any
			if len(rule.Value) == 0 {
				slog.Warn("custom_request_rules: body override missing value",
					"rule_index", idx, "path", rule.Path)
				continue
			}
			if err := sonic.Unmarshal(rule.Value, &parsed); err != nil {
				slog.Warn("custom_request_rules: body override value not JSON",
					"rule_index", idx, "path", rule.Path, "error", err.Error())
				continue
			}
			if next, ok := setJSONPath(root, segs, parsed); ok {
				root = next
				changed = true
			} else {
				slog.Warn("custom_request_rules: body override path conflict",
					"rule_index", idx, "path", rule.Path)
			}
		default:
			slog.Warn("custom_request_rules: unknown body action",
				"rule_index", idx, "action", rule.Action)
		}
	}
	if !changed {
		return body
	}
	marshaled, err := sonic.Marshal(root)
	if err != nil {
		slog.Warn("custom_request_rules: re-marshal body failed, falling back to original",
			"error", err.Error())
		return body
	}
	return marshaled
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

// setJSONPath 设置嵌套路径的值；中间节点类型冲突时返回 ok=false。
// 不存在的中间节点按对象创建（即便下一段是数字，也创建对象而非数组——避免歧义）。
func setJSONPath(root any, segs []string, value any) (any, bool) {
	if len(segs) == 0 {
		return value, true
	}
	seg := segs[0]
	rest := segs[1:]

	switch node := root.(type) {
	case map[string]any:
		if len(rest) == 0 {
			node[seg] = value
			return node, true
		}
		child, exists := node[seg]
		if !exists {
			child = map[string]any{}
		}
		newChild, ok := setJSONPath(child, rest, value)
		if !ok {
			return root, false
		}
		node[seg] = newChild
		return node, true
	case []any:
		idx, ok := parseArrayIndex(seg)
		if !ok || idx < 0 || idx >= len(node) {
			return root, false
		}
		if len(rest) == 0 {
			node[idx] = value
			return node, true
		}
		newChild, ok := setJSONPath(node[idx], rest, value)
		if !ok {
			return root, false
		}
		node[idx] = newChild
		return node, true
	default:
		return root, false
	}
}

// removeJSONPath 删除嵌套路径上的节点；路径不存在时 ok=false（静默忽略）。
func removeJSONPath(root any, segs []string) (any, bool) {
	if len(segs) == 0 {
		return root, false
	}
	seg := segs[0]
	rest := segs[1:]

	switch node := root.(type) {
	case map[string]any:
		if len(rest) == 0 {
			if _, exists := node[seg]; !exists {
				return root, false
			}
			delete(node, seg)
			return node, true
		}
		child, exists := node[seg]
		if !exists {
			return root, false
		}
		newChild, ok := removeJSONPath(child, rest)
		if !ok {
			return root, false
		}
		node[seg] = newChild
		return node, true
	case []any:
		idx, ok := parseArrayIndex(seg)
		if !ok || idx < 0 || idx >= len(node) {
			return root, false
		}
		if len(rest) == 0 {
			return append(node[:idx], node[idx+1:]...), true
		}
		newChild, ok := removeJSONPath(node[idx], rest)
		if !ok {
			return root, false
		}
		node[idx] = newChild
		return node, true
	default:
		return root, false
	}
}

// parseArrayIndex 解析段为非负整数。
func parseArrayIndex(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	i, err := strconv.Atoi(s)
	if err != nil || i < 0 {
		return 0, false
	}
	return i, true
}
