package builtin

// geminiUnsupportedSchemaKeys 是 Gemini functionDeclarations.parameters
// 不识别的 JSON Schema 字段集合；命中即递归删除。
// 触达上游 400 INVALID_ARGUMENT 的字段以及 OpenAPI/JSON Schema 元数据均纳入。
var geminiUnsupportedSchemaKeys = map[string]struct{}{
	"$schema":               {},
	"$id":                   {},
	"$ref":                  {},
	"$defs":                 {},
	"definitions":           {},
	"additionalProperties":  {},
	"propertyNames":         {},
	"patternProperties":     {},
	"unevaluatedProperties": {},
	"format":                {},
	"pattern":               {},
	"minLength":             {},
	"maxLength":             {},
	"minItems":              {},
	"maxItems":              {},
	"uniqueItems":           {},
	"exclusiveMinimum":      {},
	"exclusiveMaximum":      {},
	"multipleOf":            {},
	"default":               {},
	"examples":              {},
	"const":                 {},
	"nullable":              {},
	"title":                 {},
	"deprecated":            {},
	"readOnly":              {},
	"writeOnly":             {},
}

// cleanGeminiSchema 递归剥除 Gemini 不识别的 JSON Schema 字段，返回新值。
// 仅对 schema 节点删除关键字；properties 子键名（即用户字段名）不会被误删。
// 输入应为 sonic.Unmarshal 后的 Go 原生类型（map[string]any / []any / 标量）。
func cleanGeminiSchema(node any) any {
	switch v := node.(type) {
	case map[string]any:
		return cleanGeminiSchemaObject(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cleanGeminiSchema(item)
		}
		return out
	default:
		return v
	}
}

func cleanGeminiSchemaObject(obj map[string]any) map[string]any {
	out := make(map[string]any, len(obj))
	for key, val := range obj {
		if _, drop := geminiUnsupportedSchemaKeys[key]; drop {
			continue
		}
		if len(key) > 2 && key[0] == 'x' && key[1] == '-' {
			// OpenAPI 扩展字段（x-google-*、x-stainless-* 等）Google API 不识别
			continue
		}
		switch key {
		case "properties":
			// properties 是 {fieldName: schema} 的映射；字段名不应被当成关键字处理，
			// 只递归清洗 schema 部分。
			if props, ok := val.(map[string]any); ok {
				cleaned := make(map[string]any, len(props))
				for fieldName, fieldSchema := range props {
					cleaned[fieldName] = cleanGeminiSchema(fieldSchema)
				}
				out[key] = cleaned
				continue
			}
		case "required":
			// required 数组直接透传（cleanupRequiredFields 由调用方决定是否进一步过滤）。
			out[key] = val
			continue
		case "anyOf", "oneOf", "allOf":
			// Gemini 部分版本接受 anyOf；为保守起见仅做递归清洗，不强制扁平化。
			out[key] = cleanGeminiSchema(val)
			continue
		}
		out[key] = cleanGeminiSchema(val)
	}
	return out
}
