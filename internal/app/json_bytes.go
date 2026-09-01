package app

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 线协议 body 的唯一表示是 []byte：gjson 读、sjson 就地写。
//
// 不用 map[string]any 中转不是风格偏好——map 丢键序。一旦把请求解成 map 再整体重编码，
// 网关新增的键只能按字母序落位，被改动的子树还会被 encoding/json 重新转义；为了绕开
// 这两点就得额外维护一层「原始字节 vs 目标 map」的差分渲染。就地改写让未触及的字节
// 原样保留，键序问题从根上不存在。

// setJSONRaw / setJSONValue / deleteJSONPath 是 sjson 的薄封装。路径全是代码里的字面量，
// 唯一可能的错误来源是非法路径语法，所以失败时保留原字节而不是把错误一路传上去。
func setJSONRaw(body []byte, path, raw string) []byte {
	updated, err := sjson.SetRawBytes(body, path, []byte(raw))
	if err != nil {
		return body
	}
	return updated
}

// setJSONValue 写入一个标量（字符串/布尔/数字），转义交给 sjson。
func setJSONValue(body []byte, path string, value any) []byte {
	updated, err := sjson.SetBytes(body, path, value)
	if err != nil {
		return body
	}
	return updated
}

func deleteJSONPath(body []byte, path string) []byte {
	updated, err := sjson.DeleteBytes(body, path)
	if err != nil {
		return body
	}
	return updated
}

// jsonStringValue 只接受 JSON 字符串，数字/布尔不做隐式字符串化——与 map 形态下
// `value.(string)` 的类型断言语义一致。
func jsonStringValue(value gjson.Result) string {
	if value.Type != gjson.String {
		return ""
	}
	return value.String()
}

// jsonIntegerValue 只接受整数字面量。max_tokens/budget_tokens 是整数字段，`1.0`
// 这类写法按 json.Number.Int64() 的语义一样判否。
func jsonIntegerValue(value gjson.Result) (int64, bool) {
	if value.Type != gjson.Number {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value.Raw, 10, 64)
	return parsed, err == nil
}

// jsonMemberCount 返回对象或数组的成员数。调用方必须先确认容器类型：gjson 对标量
// 也会回调一次迭代器。
func jsonMemberCount(value gjson.Result) int {
	count := 0
	value.ForEach(func(_, _ gjson.Result) bool {
		count++
		return true
	})
	return count
}

// jsonEscapedString 返回一个 Go 字符串的 JSON 字面量，供拼接原始片段使用。
func jsonEscapedString(value string) string {
	raw, err := sjson.Set(`{}`, "v", value)
	if err != nil {
		return `""`
	}
	return gjson.Get(raw, "v").Raw
}

// joinJSONRaw 把若干原始 JSON 片段拼成一个数组字面量。
func joinJSONRaw(items []string) string {
	return "[" + strings.Join(items, ",") + "]"
}

// jsonObjectBuilder 按写入顺序拼一个 JSON 对象。键序是线协议契约的一部分（上游按
// body 形态做客户端指纹识别），所以这里显式保序，而不是交给 map 迭代。
type jsonObjectBuilder struct {
	out   strings.Builder
	empty bool
}

func newJSONObjectBuilder() *jsonObjectBuilder {
	builder := &jsonObjectBuilder{empty: true}
	builder.out.WriteByte('{')
	return builder
}

// SetRaw 追加一个成员，值取原始 JSON 片段。keyRaw 必须已经是 JSON 字符串字面量。
func (b *jsonObjectBuilder) SetRaw(keyRaw, valueRaw string) {
	if !b.empty {
		b.out.WriteByte(',')
	}
	b.empty = false
	b.out.WriteString(keyRaw)
	b.out.WriteByte(':')
	b.out.WriteString(valueRaw)
}

// Set 追加一个成员，键是普通 Go 字符串。
func (b *jsonObjectBuilder) Set(key, valueRaw string) {
	b.SetRaw(jsonEscapedString(key), valueRaw)
}

func (b *jsonObjectBuilder) String() string {
	return b.out.String() + "}"
}

// rewriteJSONMembers 递归遍历原始字节，对每个对象成员调用 rewrite，用返回的原始片段
// 替换成员值；返回空字符串表示删除该成员，rewrite 为 nil 的键原样保留并继续下钻。
//
// 重组走 key.Raw + ":" + value.Raw，所以未命中的部分逐字保留——键序、字符串转义形式、
// 数字字面量都不变。这正是解成 map 再整体编码做不到的事。
func rewriteJSONMembers(body []byte, rewrite func(key string, value gjson.Result) (string, bool)) ([]byte, bool) {
	rewritten, changed := rewriteJSONMemberValue(gjson.ParseBytes(body), rewrite)
	if !changed {
		return body, false
	}
	return []byte(rewritten), true
}

func rewriteJSONMemberValue(
	value gjson.Result,
	rewrite func(key string, value gjson.Result) (string, bool),
) (string, bool) {
	switch {
	case value.IsObject():
		builder := newJSONObjectBuilder()
		changed := false
		value.ForEach(func(key, member gjson.Result) bool {
			if replacement, matched := rewrite(key.String(), member); matched {
				changed = true
				if replacement != "" {
					builder.SetRaw(key.Raw, replacement)
				}
				return true
			}
			childRaw, childChanged := rewriteJSONMemberValue(member, rewrite)
			if childChanged {
				changed = true
			} else {
				childRaw = member.Raw
			}
			builder.SetRaw(key.Raw, childRaw)
			return true
		})
		if !changed {
			return value.Raw, false
		}
		return builder.String(), true
	case value.IsArray():
		items := value.Array()
		rendered := make([]string, 0, len(items))
		changed := false
		for _, item := range items {
			itemRaw, itemChanged := rewriteJSONMemberValue(item, rewrite)
			if itemChanged {
				changed = true
			} else {
				itemRaw = item.Raw
			}
			rendered = append(rendered, itemRaw)
		}
		if !changed {
			return value.Raw, false
		}
		return joinJSONRaw(rendered), true
	default:
		return value.Raw, false
	}
}
