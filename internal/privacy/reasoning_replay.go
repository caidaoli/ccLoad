package privacy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

/**
 * @brief ccLoad 推理签名封套的版本标记
 * @details 版本标记、长度字段和补丁各自对齐到标准 base64 边界，且无填充字符。厂商签名紧随其后，可由 Gemini 客户端整段解码重编码，也支持 Anthropic 分片追加。
 */
const ReasoningSignaturePrefix = "Y2Nsb2FkLXJlYXNvbmluZy12MSUh" // base64("ccload-reasoning-v1%!")

/**
 * @brief 单份推理签名封套允许记录的最大编辑数，供 SSE 累积时执行同一上限
 */
const MaxReasoningEdits = maxMatches

/**
 * @brief 推理签名封套的长度限制
 */
const (
	reasoningPatchLengthWidth = 8                         ///< 补丁 base64 长度的十六进制字符数
	maxReasoningPlaceholder   = 52                        ///< 占位符最长字节数，含最长类型名
	maxReasoningPatchBytes    = 176*MaxReasoningEdits + 4 ///< 每条编辑的 JSON、数组结构和对齐填充的总预算
)

/**
 * @brief 推理签名封套结构、编辑范围或文本长度无效
 */
var ErrInvalidReasoningSignature = errors.New("invalid privacy reasoning signature")

/**
 * @brief 将已还原的推理片段替换回厂商签名前的占位符
 */
type ReasoningEdit struct {
	Offset      int    `json:"offset"`                ///< 片段在客户端可见文本中的起始字节偏移
	Length      int    `json:"length"`                ///< 片段在客户端可见文本中的字节数，跨块的占位符续片可以为零
	Placeholder string `json:"placeholder"`           ///< 完整占位符
	TokenStart  int    `json:"token_start,omitempty"` ///< 签名覆盖的占位符片段起始字节偏移
	TokenEnd    int    `json:"token_end,omitempty"`   ///< 签名覆盖的占位符片段终止字节偏移；起止同为零表示整个占位符
	Field       string `json:"field,omitempty"`       ///< 签名覆盖的 Responses 数组及下标；空字符串表示单字段正文
}

/**
 * @brief 在厂商推理签名前封装客户端重放所需的反向编辑
 * @param signature 完整厂商签名或它的首个非空分片
 * @param edits 按可见文本字节位置递增的非重叠编辑
 * @return 封套签名；无编辑时返回厂商签名
 * @return 编辑或签名无效时返回 ErrInvalidReasoningSignature
 * @note 封套不保存已还原的明文，不依赖进程内映射。
 */
func WrapReasoningSignature(signature string, edits []ReasoningEdit) (string, error) {
	if len(edits) == 0 {
		return signature, nil
	}
	if signature == "" || len(edits) > MaxReasoningEdits {
		return "", ErrInvalidReasoningSignature
	}
	groups, err := reasoningEditsByField(edits)
	if err != nil {
		return "", err
	}
	for _, fieldEdits := range groups {
		if _, err := validateReasoningEdits(fieldEdits, maxTextBytes); err != nil {
			return "", err
		}
	}
	patch, err := json.Marshal(edits)
	if err != nil {
		return "", ErrInvalidReasoningSignature
	}
	for len(patch)%3 != 0 {
		patch = append(patch, ' ')
	}
	encoded := base64.StdEncoding.EncodeToString(patch)
	return fmt.Sprintf("%s%0*x%s%s", ReasoningSignaturePrefix, reasoningPatchLengthWidth, len(encoded), encoded, signature), nil
}

/**
 * @brief 将客户端重放的推理还原为厂商签名所覆盖的文本
 * @param signature 客户端提供的完整签名
 * @param visible 客户端可见的推理文本
 * @return 厂商原签名；无封套时原样返回
 * @return 重新填入占位符的推理文本；无封套时原样返回
 * @return 带标记的封套损坏或超限时返回 ErrInvalidReasoningSignature
 */
func UnwrapReasoningSignature(signature, visible string) (originalSignature, upstreamText string, err error) {
	originalSignature, edits, err := parseReasoningSignature(signature)
	if err != nil {
		return "", "", err
	}
	if len(edits) == 0 {
		return originalSignature, visible, nil
	}
	for _, edit := range edits {
		if edit.Field != "" {
			return "", "", ErrInvalidReasoningSignature
		}
	}
	upstreamText, err = applyReasoningEdits(visible, edits, maxTextBytes)
	if err != nil {
		return "", "", err
	}
	return originalSignature, upstreamText, nil
}

/**
 * @brief 解码签名封套并保留原始厂商签名
 * @param signature 客户端提供的签名
 * @return 厂商原签名
 * @return 含字段定位的编辑，原厂签名返回 nil
 * @return 封套结构损坏或超限时返回 ErrInvalidReasoningSignature
 */
func parseReasoningSignature(signature string) (string, []ReasoningEdit, error) {
	carrier, wrapped := strings.CutPrefix(signature, ReasoningSignaturePrefix)
	if !wrapped {
		return signature, nil, nil
	}
	if len(carrier) <= reasoningPatchLengthWidth {
		return "", nil, ErrInvalidReasoningSignature
	}
	length, err := strconv.ParseUint(carrier[:reasoningPatchLengthWidth], 16, 32)
	if err != nil || length == 0 || length%4 != 0 ||
		length > uint64(base64.StdEncoding.EncodedLen(maxReasoningPatchBytes)) || length >= uint64(len(carrier)-reasoningPatchLengthWidth) {
		return "", nil, ErrInvalidReasoningSignature
	}
	end := reasoningPatchLengthWidth + int(length)
	patch, err := base64.StdEncoding.Strict().DecodeString(carrier[reasoningPatchLengthWidth:end])
	if err != nil || len(patch) != int(length)/4*3 {
		return "", nil, ErrInvalidReasoningSignature
	}
	edits, err := decodeReasoningEdits(patch)
	if err != nil {
		return "", nil, err
	}
	return carrier[end:], edits, nil
}

/**
 * @brief 按字段引用连续的编辑，避免复制编辑数组
 * @param edits 签名封套中的编辑
 * @return 按单字段或数组位置分组的编辑
 * @return 同一字段被重复分组或字段定位超限时返回 ErrInvalidReasoningSignature
 */
func reasoningEditsByField(edits []ReasoningEdit) (map[string][]ReasoningEdit, error) {
	groups := make(map[string][]ReasoningEdit)
	for start := 0; start < len(edits); {
		field := edits[start].Field
		if _, duplicate := groups[field]; duplicate || len(field) > len("summary.")+8 {
			return nil, ErrInvalidReasoningSignature
		}
		end := start + 1
		for end < len(edits) && edits[end].Field == field {
			end++
		}
		groups[field] = edits[start:end]
		start = end
	}
	return groups, nil
}

/**
 * @brief 在一个可见字段中精确重放厂商签名覆盖的占位符
 * @param visible 客户端回传的字段值
 * @param edits 此字段的编辑
 * @param maxBytes 此字段可使用的重放字节数
 * @return 还原给厂商的正文
 * @return 文本或编辑无效、超过剩余容量时返回 ErrInvalidReasoningSignature
 */
func applyReasoningEdits(visible string, edits []ReasoningEdit, maxBytes int) (string, error) {
	if len(visible) > maxTextBytes {
		return "", ErrInvalidReasoningSignature
	}
	delta, err := validateReasoningEdits(edits, len(visible))
	if err != nil || len(visible)+delta > maxBytes || !utf8.ValidString(visible) {
		return "", ErrInvalidReasoningSignature
	}
	for _, edit := range edits {
		end := edit.Offset + edit.Length
		if edit.Offset < len(visible) && !utf8.RuneStart(visible[edit.Offset]) || end < len(visible) && !utf8.RuneStart(visible[end]) {
			return "", ErrInvalidReasoningSignature
		}
	}
	var out strings.Builder
	out.Grow(len(visible) + delta)
	last := 0
	for _, edit := range edits {
		start, end := edit.tokenBounds()
		out.WriteString(visible[last:edit.Offset])
		out.WriteString(edit.Placeholder[start:end])
		last = edit.Offset + edit.Length
	}
	out.WriteString(visible[last:])
	return out.String(), nil
}

/**
 * @brief 读取受条目数限制的编辑数组
 * @param patch 包含可选尾部对齐空格的 JSON 补丁
 * @return 编辑列表
 * @return 数组损坏或超限时返回 ErrInvalidReasoningSignature
 */
func decodeReasoningEdits(patch []byte) ([]ReasoningEdit, error) {
	decoder := json.NewDecoder(bytes.NewReader(patch))
	decoder.DisallowUnknownFields()
	start, err := decoder.Token()
	if err != nil || start != json.Delim('[') {
		return nil, ErrInvalidReasoningSignature
	}
	var edits []ReasoningEdit
	for decoder.More() {
		if len(edits) == MaxReasoningEdits {
			return nil, ErrInvalidReasoningSignature
		}
		var edit ReasoningEdit
		if err := decoder.Decode(&edit); err != nil {
			return nil, ErrInvalidReasoningSignature
		}
		edits = append(edits, edit)
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim(']') || len(edits) == 0 || len(bytes.TrimSpace(patch[decoder.InputOffset():])) != 0 {
		return nil, ErrInvalidReasoningSignature
	}
	return edits, nil
}

/**
 * @brief 校验编辑的顺序、位置和占位符格式
 * @param edits 待校验的编辑
 * @param visibleBytes 可见文本的实际字节数或容量上限
 * @return 重放文本相对可见文本的字节数变化
 * @return 编辑为空、超限、顺序不合法或占位符无效时返回 ErrInvalidReasoningSignature
 */
func validateReasoningEdits(edits []ReasoningEdit, visibleBytes int) (int, error) {
	if len(edits) == 0 || len(edits) > MaxReasoningEdits {
		return 0, ErrInvalidReasoningSignature
	}
	last, delta := 0, 0
	for _, edit := range edits {
		if edit.Offset < last || edit.Offset > visibleBytes || edit.Length < 0 || edit.Length > visibleBytes-edit.Offset ||
			len(edit.Placeholder) > maxReasoningPlaceholder {
			return 0, ErrInvalidReasoningSignature
		}
		span := placeholderPattern.FindStringIndex(edit.Placeholder)
		if span == nil || span[0] != 0 || span[1] != len(edit.Placeholder) {
			return 0, ErrInvalidReasoningSignature
		}
		start, end := edit.tokenBounds()
		if start < 0 || end <= start || end > len(edit.Placeholder) ||
			edit.Length == 0 && start == 0 && end == len(edit.Placeholder) {
			return 0, ErrInvalidReasoningSignature
		}
		last = edit.Offset + edit.Length
		delta += end - start - edit.Length
	}
	if last+delta > maxTextBytes {
		return 0, ErrInvalidReasoningSignature
	}
	return delta, nil
}

/**
 * @brief 取反向编辑要重放的占位符字节范围
 * @return 起始字节偏移
 * @return 终止字节偏移
 */
func (edit ReasoningEdit) tokenBounds() (start, end int) {
	if edit.TokenStart == 0 && edit.TokenEnd == 0 {
		return 0, len(edit.Placeholder)
	}
	return edit.TokenStart, edit.TokenEnd
}
