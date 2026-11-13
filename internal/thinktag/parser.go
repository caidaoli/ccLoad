package thinktag

import "strings"

/**
 * @brief 按原始顺序分离出的正文或思考文本。
 */
type Part struct {
	Text     string ///< 不含已识别标签的文本。
	Thinking bool   ///< 文本位于思考标签内。
}

/**
 * @brief 增量解析 think/thinking 标签，只缓存可能跨分片的标签后缀。
 * @note 零值可用；每条输出流独占实例，不可并发使用。
 */
type Parser struct {
	pending string ///< 尚无法判定为标签或正文的后缀。
	closing string ///< 当前思考段的闭标签；空值表示正文。
}

/**
 * @brief 接收连续文本并按出现顺序返回已确定的正文和思考内容。
 * @param text 下一段增量文本。
 * @return 非空文本片段；只有标签或待判定后缀时返回空切片。
 * @note 不处理结构化思考字段；调用方在语义边界使用 Flush 排空。
 */
func (p *Parser) Push(text string) []Part {
	text = p.pending + text
	p.pending = ""
	var parts []Part
	for text != "" {
		tags := []string{"<think>", "<thinking>"}
		if p.closing != "" {
			tags = []string{p.closing}
		}
		at, found := -1, ""
		for _, tag := range tags {
			if pos := strings.Index(text, tag); pos >= 0 && (at < 0 || pos < at) {
				at, found = pos, tag
			}
		}
		if at >= 0 {
			if at > 0 {
				parts = append(parts, Part{Text: text[:at], Thinking: p.closing != ""})
			}
			text = text[at+len(found):]
			if p.closing == "" {
				p.closing = "</" + found[1:]
			} else {
				p.closing = ""
			}
			continue
		}
		keep := 0
		for _, tag := range tags {
			for n := min(len(text), len(tag)-1); n > keep; n-- {
				if strings.HasSuffix(text, tag[:n]) {
					keep = n
					break
				}
			}
		}
		if len(text) > keep {
			parts = append(parts, Part{Text: text[:len(text)-keep], Thinking: p.closing != ""})
		}
		// A short pending suffix must not retain an entire upstream payload.
		p.pending = strings.Clone(text[len(text)-keep:])
		break
	}
	return parts
}

/**
 * @brief 将残留的未完成标签作为原始文本输出，并重置解析状态。
 * @return 保持原正文/思考归属的尾部片段；没有待发文本时返回空切片。
 */
func (p *Parser) Flush() []Part {
	var parts []Part
	if p.pending != "" {
		parts = []Part{{Text: p.pending, Thinking: p.closing != ""}}
	}
	p.pending, p.closing = "", ""
	return parts
}
