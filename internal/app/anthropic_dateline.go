package app

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// Claude Code 发现 base URL 不是官方地址时，会把 "Today's date is YYYY-MM-DD." 的撇号
// 换成 U+2019/U+02BC/U+02B9、日期分隔符换成 "/"，等于把「经过中转」写进 prompt。
// OAuth 出站前还原成 ASCII 形式（对齐 sub2api anthropicfp.NormalizeDateline）。
// RE2 没有反向引用，两个分隔符是否一致在替换回调里判断；混用分隔符的句子不是
// 客户端生成的，原样保留。
var (
	anthropicDatelinePattern       = regexp.MustCompile(`Today['’ʼʹ]s date is (\d{4})([-/])(\d{2})([-/])(\d{2})\.`)
	anthropicSystemReminderPattern = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
)

func canonicalizeAnthropicDatelines(text string) string {
	if !strings.Contains(text, "s date is ") {
		return text
	}
	return anthropicDatelinePattern.ReplaceAllStringFunc(text, func(sentence string) string {
		parts := anthropicDatelinePattern.FindStringSubmatch(sentence)
		if parts[2] != parts[4] {
			return sentence
		}
		return "Today's date is " + parts[1] + "-" + parts[3] + "-" + parts[5] + "."
	})
}

// canonicalizeAnthropicReminderDatelines 只改 <system-reminder> 内部：消息正文里用户
// 自己写的日期句子不属于客户端指纹。
func canonicalizeAnthropicReminderDatelines(text string) string {
	if !strings.Contains(text, "<system-reminder>") {
		return text
	}
	return anthropicSystemReminderPattern.ReplaceAllStringFunc(text, canonicalizeAnthropicDatelines)
}

// normalizeAnthropicDateline 只扫描客户端放 dateline 的位置：system 文本，以及
// messages 字符串内容或 text 块中的 <system-reminder> 片段。tool_use/tool_result
// 等其他块原样保留；没有改动时返回原字节。
func normalizeAnthropicDateline(body []byte) []byte {
	if !bytes.Contains(body, []byte("s date is ")) || !isAnthropicJSONObject(body) {
		return body
	}
	var patches []anthropicRawPatch
	patchText := func(path string, value gjson.Result, canonicalize func(string) string) {
		if value.Type != gjson.String {
			return
		}
		if text := canonicalize(value.String()); text != value.String() {
			patches = append(patches, anthropicRawPatch{path: path, raw: jsonEscapedString(text)})
		}
	}
	patchTextBlocks := func(prefix string, blocks gjson.Result, canonicalize func(string) string) {
		for index, block := range blocks.Array() {
			if block.IsObject() && jsonStringValue(block.Get("type")) == "text" {
				patchText(prefix+"."+strconv.Itoa(index)+".text", block.Get("text"), canonicalize)
			}
		}
	}

	root := gjson.ParseBytes(body)
	if system := root.Get("system"); system.IsArray() {
		patchTextBlocks("system", system, canonicalizeAnthropicDatelines)
	} else {
		patchText("system", system, canonicalizeAnthropicDatelines)
	}
	if messages := root.Get("messages"); messages.IsArray() {
		for index, message := range messages.Array() {
			path := "messages." + strconv.Itoa(index) + ".content"
			if content := message.Get("content"); content.IsArray() {
				patchTextBlocks(path, content, canonicalizeAnthropicReminderDatelines)
			} else {
				patchText(path, content, canonicalizeAnthropicReminderDatelines)
			}
		}
	}
	for _, patch := range patches {
		body = setJSONRaw(body, patch.path, patch.raw)
	}
	return body
}
