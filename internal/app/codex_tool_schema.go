package app

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"strings"

	cliproxyutil "ccLoad/internal/protocol/cliproxy/util"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// App-layer port of CLIProxyAPI runtime/executor/helps/codex_tool_schema.go
// (21d26a07). It is executor behavior, so it stays out of the translator
// snapshot manifest.

// codexComplexUnionBranchThreshold is the minimum number of oneOf/anyOf branches
// before a pure constant union is collapsed into enum. MCP servers emit such
// unions for enums with per-value descriptions; the Codex backend aborts on the
// large ones.
const codexComplexUnionBranchThreshold = 8

/**
 * @brief 规整 Codex 上游工具参数 schema。
 * @param body Responses 请求体。
 * @param stripUniqueItems 是否为拒收 uniqueItems 的 Codex OAuth 上游移除该关键字。
 * @return 规整后的请求体，无需改写时返回原文。
 */
func normalizeCodexToolSchemas(body []byte, stripUniqueItems bool) []byte {
	updatedTools, changed := normalizeCodexToolList(gjson.GetBytes(body, "tools"), stripUniqueItems)
	if !changed {
		return body
	}
	out, err := sjson.SetRawBytes(body, "tools", updatedTools)
	if err != nil {
		return body
	}
	return out
}

/**
 * @brief 规整工具数组，保留无需改写的工具原文。
 * @param tools 顶层或命名空间内的工具数组。
 * @param stripUniqueItems 是否移除不兼容的 uniqueItems 关键字。
 * @return 改写后的工具数组，无改写时为 nil。
 * @return 是否改写了工具数组。
 */
func normalizeCodexToolList(tools gjson.Result, stripUniqueItems bool) ([]byte, bool) {
	if !tools.IsArray() {
		return nil, false
	}
	var out []byte
	offset := 0
	tools.ForEach(func(_, tool gjson.Result) bool {
		updated, changed := normalizeCodexTool(tool, stripUniqueItems)
		if !changed {
			return true
		}
		if out == nil {
			out = make([]byte, 0, len(tools.Raw))
		}
		start := tool.Index - tools.Index
		out = append(out, tools.Raw[offset:start]...)
		out = append(out, updated...)
		offset = start + len(tool.Raw)
		return true
	})
	if out == nil {
		return nil, false
	}
	return append(out, tools.Raw[offset:]...), true
}

/**
 * @brief 规整函数、自定义工具或命名空间内的工具参数。
 * @param tool Responses 工具声明。
 * @param stripUniqueItems 是否移除不兼容的 uniqueItems 关键字。
 * @return 工具的改写结果，仅在改写成功时可用。
 * @return 是否成功改写了工具。
 */
func normalizeCodexTool(tool gjson.Result, stripUniqueItems bool) ([]byte, bool) {
	toolType := tool.Get("type").String()
	if toolType == "namespace" {
		updatedTools, changed := normalizeCodexToolList(tool.Get("tools"), stripUniqueItems)
		if !changed {
			return nil, false
		}
		updated, err := sjson.SetRawBytes([]byte(tool.Raw), "tools", updatedTools)
		return updated, err == nil
	}
	if toolType != "function" && toolType != "custom" {
		return nil, false
	}
	params := tool.Get("parameters")
	if !params.IsObject() {
		return nil, false
	}
	updatedParams, changed := normalizeCodexParameters(params, stripUniqueItems)
	if !changed {
		return nil, false
	}
	updated, err := sjson.SetRawBytes([]byte(tool.Raw), "parameters", updatedParams)
	return updated, err == nil
}

/**
 * @brief 清理工具参数中的不兼容关键字，并将符合条件的常量联合折叠为 enum。
 * @param params 工具参数 schema 对象。
 * @param stripUniqueItems 是否移除不兼容的 uniqueItems 关键字。
 * @return 工具参数 schema，无改写时返回原文。
 * @return 是否改写了参数。
 */
func normalizeCodexParameters(params gjson.Result, stripUniqueItems bool) ([]byte, bool) {
	rawParams := []byte(params.Raw)
	changed := false
	if sanitized, ok := stripIncompatibleCodexSchemaKeywordsJSON(rawParams, stripUniqueItems); ok {
		rawParams = sanitized
		changed = true
		params = gjson.ParseBytes(rawParams)
	}
	properties := params.Get("properties")
	if !properties.IsObject() {
		return rawParams, changed
	}
	for name, prop := range properties.Map() {
		updated, ok := normalizeCodexPropertySchema(prop)
		if !ok {
			continue
		}
		next, err := sjson.SetRawBytes(rawParams, "properties."+escapeCodexSJSONKey(name), updated)
		if err == nil {
			rawParams = next
			changed = true
		}
	}
	return rawParams, changed
}

/**
 * @brief 在工具参数 JSON 中清理上游拒收的 schema 关键字或正则。
 * @param raw 参数 schema 的 JSON 原文。
 * @param stripUniqueItems 是否移除不兼容的 uniqueItems 关键字。
 * @return 改写后的 JSON，无改写时返回原文。
 * @return 是否成功改写了 schema。
 */
func stripIncompatibleCodexSchemaKeywordsJSON(raw []byte, stripUniqueItems bool) ([]byte, bool) {
	// Patterns arrive JSON-escaped: a regex `\0` is `\\0` in the raw bytes, and
	// `\u` covers escaped backslashes or keyword characters.
	hasUniqueItems := stripUniqueItems && bytes.Contains(raw, []byte(`"uniqueItems"`))
	if !bytes.Contains(raw, []byte(`\p{`)) && !bytes.Contains(raw, []byte(`\P{`)) &&
		!bytes.Contains(raw, []byte(`\u`)) && !bytes.Contains(raw, []byte(`\\0`)) && !hasUniqueItems {
		return raw, false
	}
	var root any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil || root == nil {
		return raw, false
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return raw, false
	}
	if !stripIncompatibleCodexSchemaKeywords(root, stripUniqueItems) {
		return raw, false
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		return raw, false
	}
	return bytes.TrimSpace(buf.Bytes()), true
}

/**
 * @brief 清理 schema 节点及子 schema，保留属性名称和默认值等字面数据。
 * @param v 从 JSON 解码的 schema 节点或 schema 数组。
 * @param stripUniqueItems 是否移除不兼容的 uniqueItems 关键字。
 * @return 是否原地改写了任一节点。
 */
func stripIncompatibleCodexSchemaKeywords(v any, stripUniqueItems bool) bool {
	changed := false
	switch schema := v.(type) {
	case map[string]any:
		if pattern, ok := schema["pattern"].(string); ok && cliproxyutil.HasUnsupportedUnicodePropertyEscape(pattern) {
			delete(schema, "pattern")
			changed = true
		}
		if _, ok := schema["uniqueItems"]; stripUniqueItems && ok {
			delete(schema, "uniqueItems")
			changed = true
		}
		if patternProps, ok := schema["patternProperties"].(map[string]any); ok {
			for key, sub := range patternProps {
				if cliproxyutil.HasUnsupportedUnicodePropertyEscape(key) {
					delete(patternProps, key)
					changed = true
				} else if stripIncompatibleCodexSchemaKeywords(sub, stripUniqueItems) {
					changed = true
				}
			}
		}
		for _, keyword := range cliproxyutil.SchemaMapKeywords {
			if keyword == "patternProperties" {
				continue
			}
			if subMap, ok := schema[keyword].(map[string]any); ok {
				for _, sub := range subMap {
					if stripIncompatibleCodexSchemaKeywords(sub, stripUniqueItems) {
						changed = true
					}
				}
			}
		}
		for _, keyword := range cliproxyutil.SchemaValueKeywords {
			switch sub := schema[keyword].(type) {
			case map[string]any:
				if stripIncompatibleCodexSchemaKeywords(sub, stripUniqueItems) {
					changed = true
				}
			case []any:
				for _, item := range sub {
					if stripIncompatibleCodexSchemaKeywords(item, stripUniqueItems) {
						changed = true
					}
				}
			}
		}
	case []any:
		for _, item := range schema {
			if stripIncompatibleCodexSchemaKeywords(item, stripUniqueItems) {
				changed = true
			}
		}
	}
	return changed
}

func normalizeCodexPropertySchema(prop gjson.Result) ([]byte, bool) {
	if !prop.IsObject() {
		return nil, false
	}
	hasOneOf := prop.Get("oneOf").Exists()
	hasAnyOf := prop.Get("anyOf").Exists()
	// Both keywords together form a compound constraint; leave it alone.
	if hasOneOf == hasAnyOf {
		return nil, false
	}
	unionName := "anyOf"
	if hasOneOf {
		unionName = "oneOf"
	}
	union := prop.Get(unionName)
	if !union.IsArray() {
		return nil, false
	}
	branches := union.Array()
	if len(branches) < codexComplexUnionBranchThreshold {
		return nil, false
	}
	rawValues := make([]string, 0, len(branches))
	keys := make([]string, 0, len(branches))
	seen := make(map[string]struct{}, len(branches))
	for _, branch := range branches {
		key, raw, ok := codexPureConstBranch(branch)
		if !ok {
			return nil, false
		}
		// A duplicate value breaks oneOf exclusivity; the union is not an enum.
		if _, dup := seen[key]; dup {
			return nil, false
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
		rawValues = append(rawValues, raw)
	}

	rawProp := []byte(prop.Raw)
	if existing := prop.Get("enum"); existing.IsArray() {
		existingKeys := make([]string, 0, len(keys))
		for _, value := range existing.Array() {
			key, ok := canonicalCodexJSONValueKey(value)
			if !ok {
				return nil, false
			}
			existingKeys = append(existingKeys, key)
		}
		// Drop the union only when the enum already states the same set.
		if !equalCodexCanonicalSets(existingKeys, keys) {
			return nil, false
		}
		rawProp, _ = sjson.DeleteBytes(rawProp, unionName)
		return rawProp, true
	}
	// Raw JSON tokens keep numeric values exact.
	rawProp, err := sjson.SetRawBytes(rawProp, "enum", []byte("["+strings.Join(rawValues, ",")+"]"))
	if err != nil {
		return nil, false
	}
	rawProp, _ = sjson.DeleteBytes(rawProp, unionName)
	return rawProp, true
}

func codexPureConstBranch(branch gjson.Result) (key, raw string, ok bool) {
	if !branch.IsObject() {
		return "", "", false
	}
	value := branch.Get("const")
	if !value.Exists() {
		return "", "", false
	}
	for name := range branch.Map() {
		if name != "const" && name != "description" && name != "title" {
			return "", "", false
		}
	}
	key, ok = canonicalCodexJSONValueKey(value)
	return key, value.Raw, ok
}

func canonicalCodexJSONValueKey(value gjson.Result) (string, bool) {
	switch value.Type {
	case gjson.String:
		return "s:" + value.String(), true
	case gjson.Number:
		raw := strings.TrimSpace(value.Raw)
		var rat big.Rat
		if _, ok := rat.SetString(raw); ok {
			return "n:" + rat.RatString(), true
		}
		return "n:" + raw, true
	case gjson.True:
		return "b:true", true
	case gjson.False:
		return "b:false", true
	case gjson.Null:
		return "null", true
	default:
		return "", false
	}
}

func equalCodexCanonicalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, value := range a {
		set[value] = struct{}{}
	}
	for _, value := range b {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return len(set) == len(a)
}

// escapeCodexSJSONKey makes sjson treat a property name containing '.', ':'
// or '\' as one literal key instead of a nested path.
func escapeCodexSJSONKey(key string) string {
	key = strings.ReplaceAll(key, `\`, `\\`)
	key = strings.ReplaceAll(key, `.`, `\.`)
	return strings.ReplaceAll(key, `:`, `\:`)
}
