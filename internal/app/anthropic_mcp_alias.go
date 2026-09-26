package app

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"ccLoad/internal/model"

	"github.com/tidwall/gjson"
)

// 非 Claude Code 调用方经 OAuth 模拟路径出站时，read_file、apply_patch 这类工具名本身
// 就在说「这不是 Claude Code」。对齐 CLIProxyAPI 的 MCP 别名：每个客户端工具改名为
// Claude Code MCP 扩展的形态 mcp__<词>_<词>__<词>_<语义>——服务器段按账号稳定，工具段
// 由 HMAC 选词再拼上截断的原名，模型仍能按名字区分工具。映射只属于本次请求，响应端按
// 结构（tool_use.name / tool_reference.tool_name）精确还原，从不对正文做字符串替换。

//go:embed anthropic_mcp_alias_words.txt
var anthropicMCPAliasWordsRaw string

// anthropicMCPAliasWords 是 BIP-39 英文词表（2048 词）：常见单词比 Base32 片段更不容易
// 被模型抄错。
var anthropicMCPAliasWords = strings.Fields(anthropicMCPAliasWordsRaw)

const anthropicToolNameMaxLen = 64

// anthropicMCPToolAliases 把本次请求分配的别名映射回调用方原名；nil 表示没有改名。
type anthropicMCPToolAliases map[string]string

// anthropicMCPAliasSecret 按渠道和账号稳定。access token 会轮换，拿它当密钥会让每次
// 刷新都换一套工具名、击穿 prompt cache。
func anthropicMCPAliasSecret(cfg *model.Config, apiKey string) string {
	secret := "ccload:anthropic:mcp-alias\x00" + strconv.FormatInt(cfg.ID, 10)
	if credential := anthropicCredentialForWire(cfg, apiKey); credential != nil {
		secret += "\x00" + anthropicOAuthIdentitySeed(credential)
	}
	return secret
}

// isAnthropicMCPToolName 判断 name 是否已经是 Claude Code MCP 工具名
// （mcp__<server>__<tool>，仅含 Anthropic 工具名允许的字符）。
func isAnthropicMCPToolName(name string) bool {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok || len(name) > anthropicToolNameMaxLen {
		return false
	}
	separator := strings.Index(rest, "__")
	if separator <= 0 || separator+2 >= len(rest) {
		return false
	}
	return strings.IndexFunc(name, func(char rune) bool { return !isAnthropicToolNameChar(char) }) < 0
}

func isAnthropicToolNameChar(char rune) bool {
	return (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
		(char >= '0' && char <= '9') || char == '_' || char == '-'
}

// isAnthropicClientToolDeclaration 只接受调用方自定义工具（无 type 或 type=custom）。
// web_search_*、bash_* 等 Anthropic 定义的工具名字是 API 契约，未知 type 同样原样保留。
func isAnthropicClientToolDeclaration(tool gjson.Result) bool {
	toolType := tool.Get("type")
	return tool.IsObject() && (!toolType.Exists() || jsonStringValue(toolType) == "custom")
}

// aliasAnthropicMCPToolNames 给 tools 中每个客户端工具分配 MCP 别名，并改写 tool_choice
// 与 messages 里引用这些工具的块；已是合法 MCP 名的工具透传。type=custom 一并去掉：
// Claude Code 的 MCP 工具声明不带 type。
func aliasAnthropicMCPToolNames(body []byte, secret string) ([]byte, anthropicMCPToolAliases) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() || len(anthropicMCPAliasWords) == 0 {
		return body, nil
	}
	reserved := make(map[string]bool)
	protected := make(map[string]bool)
	tools.ForEach(func(_, tool gjson.Result) bool {
		name := jsonStringValue(tool.Get("name"))
		reserved[name] = true
		if !isAnthropicClientToolDeclaration(tool) {
			protected[name] = true
		}
		return true
	})

	server := anthropicMCPAliasServer(secret)
	forward := make(map[string]string)
	var edits []jsonValueEdit
	index := -1
	tools.ForEach(func(_, tool gjson.Result) bool {
		index++
		if !isAnthropicClientToolDeclaration(tool) {
			return true
		}
		nameResult := tool.Get("name")
		name := jsonStringValue(nameResult)
		alias, aliased := forward[name]
		if !aliased && name != "" && !protected[name] && !isAnthropicMCPToolName(name) {
			if alias, aliased = allocateAnthropicMCPToolAlias(server, secret, name, reserved); aliased {
				forward[name] = alias
				reserved[alias] = true
			} else {
				log.Printf("[WARN] Anthropic MCP 工具别名已耗尽，保留原名: %q", name)
			}
		}
		path := "tools." + strconv.Itoa(index)
		if tool.Get("type").Exists() {
			raw := deleteJSONPath([]byte(tool.Raw), "type")
			if aliased {
				raw = setJSONRaw(raw, "name", jsonEscapedString(alias))
			}
			edits = append(edits, jsonValueEdit{path: path, value: tool, raw: string(raw)})
		} else if aliased {
			edits = append(edits, jsonValueEdit{path: path + ".name", value: nameResult, raw: jsonEscapedString(alias)})
		}
		return true
	})
	if len(edits) == 0 {
		return body, nil
	}

	rename := func(name string) (string, bool) {
		alias, ok := forward[name]
		return alias, ok
	}
	if toolChoice := gjson.GetBytes(body, "tool_choice"); jsonStringValue(toolChoice.Get("type")) == "tool" {
		edits = appendAnthropicToolNameEdit(edits, "tool_choice.name", toolChoice.Get("name"), rename)
	}
	messageIndex := -1
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		messageIndex++
		edits = appendAnthropicToolBlocksEdits(edits,
			"messages."+strconv.Itoa(messageIndex)+".content", message.Get("content"), rename)
		return true
	})

	body = applyJSONValueEdits(body, edits)
	if len(forward) == 0 {
		return body, nil
	}
	aliases := make(anthropicMCPToolAliases, len(forward))
	for original, alias := range forward {
		aliases[alias] = original
	}
	return body, aliases
}

// restore 把上游 Anthropic 响应（非流式 message，或单个 SSE 事件的 data）中本次请求
// 分配的别名还原为原名；映射外的名字原样透传。
func (aliases anthropicMCPToolAliases) restore(payload []byte) []byte {
	if len(aliases) == 0 || !bytes.Contains(payload, []byte("mcp__")) {
		return payload
	}
	rename := func(name string) (string, bool) {
		original, ok := aliases[name]
		return original, ok
	}
	root := gjson.ParseBytes(payload)
	edits := appendAnthropicToolBlockEdits(nil, "content_block", root.Get("content_block"), rename)
	edits = appendAnthropicToolBlocksEdits(edits, "content", root.Get("content"), rename)
	edits = appendAnthropicToolBlocksEdits(edits, "message.content", root.Get("message.content"), rename)
	return applyJSONValueEdits(payload, edits)
}

// prepareAnthropicMCPToolAliasResponse 在任何透传或协议转换之前还原上游响应里的
// 工具名，下游各协议看到的都是调用方原名。
func prepareAnthropicMCPToolAliasResponse(resp *http.Response, aliases anthropicMCPToolAliases, streaming bool) {
	if len(aliases) == 0 || resp == nil || resp.Body == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	var events io.Reader
	if responseIsSSE(resp, streaming) {
		events = resp.Body
	}
	wrapJSONEventRewrite(resp, events, aliases.restore)
}

func appendAnthropicToolBlocksEdits(
	edits []jsonValueEdit, path string, blocks gjson.Result, rename func(string) (string, bool),
) []jsonValueEdit {
	if !blocks.IsArray() {
		return edits
	}
	index := -1
	blocks.ForEach(func(_, block gjson.Result) bool {
		index++
		edits = appendAnthropicToolBlockEdits(edits, path+"."+strconv.Itoa(index), block, rename)
		return true
	})
	return edits
}

// appendAnthropicToolBlockEdits 收集单个 content block 中工具名引用的改写。请求历史与
// 上游响应是同一套块形态，别名和还原共用这里，只是 rename 方向相反。
func appendAnthropicToolBlockEdits(
	edits []jsonValueEdit, path string, block gjson.Result, rename func(string) (string, bool),
) []jsonValueEdit {
	if !block.IsObject() {
		return edits
	}
	references := func(edits []jsonValueEdit, path string, refs gjson.Result) []jsonValueEdit {
		if !refs.IsArray() {
			return edits
		}
		index := -1
		refs.ForEach(func(_, ref gjson.Result) bool {
			index++
			if jsonStringValue(ref.Get("type")) == "tool_reference" {
				edits = appendAnthropicToolNameEdit(edits,
					path+"."+strconv.Itoa(index)+".tool_name", ref.Get("tool_name"), rename)
			}
			return true
		})
		return edits
	}
	switch jsonStringValue(block.Get("type")) {
	case "tool_use":
		edits = appendAnthropicToolNameEdit(edits, path+".name", block.Get("name"), rename)
	case "tool_reference":
		edits = appendAnthropicToolNameEdit(edits, path+".tool_name", block.Get("tool_name"), rename)
	case "tool_result":
		edits = references(edits, path+".content", block.Get("content"))
	case "tool_search_tool_result":
		edits = references(edits, path+".content.tool_references", block.Get("content.tool_references"))
	}
	return edits
}

func appendAnthropicToolNameEdit(
	edits []jsonValueEdit, path string, value gjson.Result, rename func(string) (string, bool),
) []jsonValueEdit {
	if value.Type != gjson.String {
		return edits
	}
	if renamed, ok := rename(value.String()); ok && renamed != value.String() {
		edits = append(edits, jsonValueEdit{path: path, value: value, raw: jsonEscapedString(renamed)})
	}
	return edits
}

// jsonValueEdit 用 raw 替换原 body 中的 value。value 必须取自同一份 body 的 gjson 结果。
type jsonValueEdit struct {
	path  string
	value gjson.Result
	raw   string
}

// applyJSONValueEdits 按原 body 偏移一次拼接全部改写：长会话里数百个 tool_use 逐条
// sjson 会反复拷贝整个 body。偏移对不上（gjson Index 未知）时退回按路径逐条写入。
func applyJSONValueEdits(body []byte, edits []jsonValueEdit) []byte {
	if len(edits) == 0 {
		return body
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].value.Index < edits[j].value.Index })
	size, cursor := len(body), 0
	for _, edit := range edits {
		start, end := edit.value.Index, edit.value.Index+len(edit.value.Raw)
		if edit.value.Raw == "" || start < cursor || end > len(body) || string(body[start:end]) != edit.value.Raw {
			for _, edit := range edits {
				body = setJSONRaw(body, edit.path, edit.raw)
			}
			return body
		}
		size += len(edit.raw) - len(edit.value.Raw)
		cursor = end
	}
	out := make([]byte, 0, size)
	cursor = 0
	for _, edit := range edits {
		out = append(out, body[cursor:edit.value.Index]...)
		out = append(out, edit.raw...)
		cursor = edit.value.Index + len(edit.value.Raw)
	}
	return append(out, body[cursor:]...)
}

// allocateAnthropicMCPToolAlias 从 HMAC 选中的词开始线性探测未占用的别名；尝试次数以
// 词表大小为上限，净化后语义相同的名字不会无限循环。
func allocateAnthropicMCPToolAlias(server, secret, original string, reserved map[string]bool) (string, bool) {
	words := anthropicMCPAliasWords
	digest := anthropicMCPAliasDigest(secret, "tool", original)
	base := int(binary.BigEndian.Uint16(digest[:2]))
	for attempt := range words {
		prefix := "mcp__" + server + "__" + words[(base+attempt)%len(words)] + "_"
		alias := prefix + anthropicMCPToolSemanticSuffix(original, max(anthropicToolNameMaxLen-len(prefix), 1))
		if !reserved[alias] {
			return alias, true
		}
	}
	return "", false
}

// anthropicMCPAliasServer 是同一密钥下所有别名共享的两词虚拟 MCP 服务器名。
func anthropicMCPAliasServer(secret string) string {
	digest := anthropicMCPAliasDigest(secret, "server", "")
	word := func(offset int) string {
		return anthropicMCPAliasWords[int(binary.BigEndian.Uint16(digest[offset:offset+2]))%len(anthropicMCPAliasWords)]
	}
	return word(0) + "_" + word(2)
}

// anthropicMCPToolSemanticSuffix 保留原名中的合法字符，非法字符段折叠为单个 "_"，
// 截断到 maxLength；净化后为空时用 "tool"。
func anthropicMCPToolSemanticSuffix(original string, maxLength int) string {
	var semantic strings.Builder
	pendingSeparator := false
	for _, char := range original {
		if !isAnthropicToolNameChar(char) {
			pendingSeparator = semantic.Len() > 0
			continue
		}
		if pendingSeparator && semantic.Len()+1 < maxLength {
			semantic.WriteByte('_')
		}
		pendingSeparator = false
		if semantic.Len() >= maxLength {
			break
		}
		semantic.WriteRune(char)
	}
	if result := strings.Trim(semantic.String(), "_-"); result != "" {
		return result
	}
	return "tool"
}

func anthropicMCPAliasDigest(secret, purpose, original string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("ccload-anthropic-mcp-alias-v1\x00" + purpose + "\x00" + original))
	var digest [sha256.Size]byte
	copy(digest[:], mac.Sum(nil))
	return digest
}
