package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/tidwall/gjson"
)

// orderedJSONNode keeps object member order and the original bytes for every
// untouched subtree.  It is deliberately small: request normalization needs
// mutation, not a general-purpose JSON DOM.
//
// raw 是 string 而不是 []byte：它永远只被读，存成 string 才能直接切自 gjson 的
// 零拷贝视图，否则每个节点都要为「原样保留」的子树复制一次字节。
type orderedJSONNode struct {
	raw    string
	kind   byte
	scalar any
	object []orderedJSONMember
	array  []*orderedJSONNode
	dirty  bool
}

type orderedJSONMember struct {
	key   string
	value *orderedJSONNode
}

// parseOrderedJSON 建保序树。语法校验必须和下面的消费者同源：gjson 对残缺输入
// 宽松，先校验才能让遍历不必逐节点重新验证语法。这里刻意用 gjson.ValidBytes 而
// 不是更快的 sonic.Valid——sonic 接受非法 `\u` 转义（`{"a":"\u00"}` 它判 true，
// encoding/json 和 gjson 都判 false），换成 sonic 会让守卫放行一份 gjson 读不出
// 值的字节，等于把本函数从"按 encoding/json 语义可解析"降级成"大致像 JSON"。
// 差价约 8 µs / 28 KB，买的是守卫与消费者语义一致。
func parseOrderedJSON(raw []byte) (*orderedJSONNode, error) {
	if !gjson.ValidBytes(raw) {
		return nil, errors.New("invalid JSON document")
	}
	root := gjson.ParseBytes(raw)
	node, err := parseOrderedJSONValue(root, 0)
	if err != nil {
		return nil, err
	}
	// Keep leading/trailing whitespace byte-for-byte when the tree is untouched.
	// gjson 的顶层 Raw 已经跳过前导空白，长度相等才说明它就是 raw 的零拷贝视图。
	if len(root.Raw) == len(raw) {
		node.raw = root.Raw
	} else {
		node.raw = string(raw)
	}
	return node, nil
}

func parseOrderedJSONValue(value gjson.Result, depth int) (*orderedJSONNode, error) {
	if depth > 1000 {
		return nil, errors.New("JSON nesting exceeds limit")
	}
	node := &orderedJSONNode{raw: value.Raw}
	switch value.Type {
	case gjson.JSON:
		switch {
		case value.IsObject():
			node.kind = '{'
			var err error
			value.ForEach(func(key, child gjson.Result) bool {
				var parsed *orderedJSONNode
				if parsed, err = parseOrderedJSONValue(child, depth+1); err != nil {
					return false
				}
				name := key.String()
				// 重复 key 按 encoding/json 语义后者胜出，就地覆盖以保持原位置。
				// 必须收敛成单个成员：否则 overridePath 只改到第一个，渲染时
				// 却把两个同名 key 都发给上游。标脏是必需的——原始字节里还有
				// 两个成员，不重新渲染就等于没收敛。
				if index, _ := node.objectMember(name); index >= 0 {
					node.object[index].value = parsed
					node.dirty = true
					return true
				}
				node.object = append(node.object, orderedJSONMember{key: name, value: parsed})
				return true
			})
			return node, err
		case value.IsArray():
			node.kind = '['
			var err error
			value.ForEach(func(_, child gjson.Result) bool {
				var parsed *orderedJSONNode
				if parsed, err = parseOrderedJSONValue(child, depth+1); err != nil {
					return false
				}
				node.array = append(node.array, parsed)
				return true
			})
			return node, err
		default:
			return nil, errors.New("unexpected JSON container")
		}
	case gjson.String:
		node.kind = 's'
		node.scalar = value.String()
	case gjson.Number:
		// 数字保留字面量：大整数经 float64 往返会丢精度。
		node.kind = 's'
		node.scalar = json.Number(value.Raw)
	case gjson.True:
		node.kind = 's'
		node.scalar = true
	case gjson.False:
		node.kind = 's'
		node.scalar = false
	case gjson.Null:
		node.kind = 's'
		node.scalar = nil
	default:
		return nil, errors.New("unexpected JSON value")
	}
	return node, nil
}

func (n *orderedJSONNode) changed() bool {
	if n == nil {
		return false
	}
	if n.dirty {
		return true
	}
	switch n.kind {
	case '{':
		for _, member := range n.object {
			if member.value.changed() {
				return true
			}
		}
	case '[':
		for _, item := range n.array {
			if item.changed() {
				return true
			}
		}
	}
	return false
}

func (n *orderedJSONNode) render() []byte {
	if !n.changed() {
		return []byte(n.raw)
	}
	var out bytes.Buffer
	n.renderTo(&out)
	return out.Bytes()
}

func (n *orderedJSONNode) renderTo(out *bytes.Buffer) {
	if !n.changed() {
		out.WriteString(n.raw)
		return
	}
	switch n.kind {
	case '{':
		out.WriteByte('{')
		for index, member := range n.object {
			if index > 0 {
				out.WriteByte(',')
			}
			key, _ := json.Marshal(member.key)
			out.Write(key)
			out.WriteByte(':')
			member.value.renderTo(out)
		}
		out.WriteByte('}')
	case '[':
		out.WriteByte('[')
		for index, item := range n.array {
			if index > 0 {
				out.WriteByte(',')
			}
			item.renderTo(out)
		}
		out.WriteByte(']')
	default:
		out.WriteString(n.raw)
	}
}

func (n *orderedJSONNode) objectMember(key string) (int, *orderedJSONNode) {
	for index := range n.object {
		if n.object[index].key == key {
			return index, n.object[index].value
		}
	}
	return -1, nil
}

func orderedJSONArrayIndex(segment string) (int, bool) {
	index, err := strconv.Atoi(segment)
	return index, err == nil && index >= 0 && strconv.Itoa(index) == segment
}

func (n *orderedJSONNode) overridePath(path []string, replacement *orderedJSONNode) (ok, conflict bool) {
	if len(path) == 0 || n == nil {
		return false, true
	}
	segment := path[0]
	if len(path) == 1 {
		switch n.kind {
		case '{':
			if index, _ := n.objectMember(segment); index >= 0 {
				n.object[index].value = replacement
			} else {
				n.object = append(n.object, orderedJSONMember{key: segment, value: replacement})
			}
			n.dirty = true
			return true, false
		case '[':
			index, valid := orderedJSONArrayIndex(segment)
			if !valid || index >= len(n.array) {
				return false, true
			}
			n.array[index] = replacement
			n.dirty = true
			return true, false
		default:
			return false, true
		}
	}
	switch n.kind {
	case '{':
		_, child := n.objectMember(segment)
		if child == nil {
			child = &orderedJSONNode{kind: '{', raw: "{}", dirty: true}
			n.object = append(n.object, orderedJSONMember{key: segment, value: child})
		}
		if child.kind != '{' && child.kind != '[' {
			return false, true
		}
		ok, conflict = child.overridePath(path[1:], replacement)
		if ok {
			n.dirty = true
		}
		return ok, conflict
	case '[':
		index, valid := orderedJSONArrayIndex(segment)
		if !valid || index >= len(n.array) {
			return false, true
		}
		child := n.array[index]
		if child.kind != '{' && child.kind != '[' {
			return false, true
		}
		ok, conflict = child.overridePath(path[1:], replacement)
		if ok {
			n.dirty = true
		}
		return ok, conflict
	default:
		return false, true
	}
}

func (n *orderedJSONNode) removePath(path []string) (ok, conflict bool) {
	if len(path) == 0 || n == nil {
		return false, true
	}
	segment := path[0]
	if len(path) == 1 {
		switch n.kind {
		case '{':
			index, _ := n.objectMember(segment)
			if index < 0 {
				return false, false
			}
			n.object = append(n.object[:index], n.object[index+1:]...)
			n.dirty = true
			return true, false
		case '[':
			index, valid := orderedJSONArrayIndex(segment)
			if !valid || index >= len(n.array) {
				return false, true
			}
			n.array = append(n.array[:index], n.array[index+1:]...)
			n.dirty = true
			return true, false
		default:
			return false, true
		}
	}
	switch n.kind {
	case '{':
		_, child := n.objectMember(segment)
		if child == nil {
			return false, false
		}
		if child.kind != '{' && child.kind != '[' {
			return false, true
		}
		ok, conflict = child.removePath(path[1:])
		if ok {
			n.dirty = true
		}
		return ok, conflict
	case '[':
		index, valid := orderedJSONArrayIndex(segment)
		if !valid || index >= len(n.array) {
			return false, true
		}
		child := n.array[index]
		if child.kind != '{' && child.kind != '[' {
			return false, true
		}
		ok, conflict = child.removePath(path[1:])
		if ok {
			n.dirty = true
		}
		return ok, conflict
	default:
		return false, true
	}
}
