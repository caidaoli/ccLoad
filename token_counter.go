package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

// CountTokensRequest 符合Anthropic官方API规范的请求结构
// 参考: https://docs.claude.com/en/api/messages-count-tokens
type CountTokensRequest struct {
	Model    string         `json:"model" binding:"required"`
	Messages []MessageParam `json:"messages" binding:"required"`
	System   string         `json:"system,omitempty"`
	Tools    []Tool         `json:"tools,omitempty"`
}

// MessageParam 消息参数（简化版本，支持文本内容）
type MessageParam struct {
	Role    string `json:"role" binding:"required"`
	Content any    `json:"content" binding:"required"` // 支持 string 或 []ContentBlock
}

// Tool 工具定义（用于token计数）
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
}

// CountTokensResponse 符合Anthropic官方API规范的响应结构
type CountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// handleCountTokens 本地实现token计数接口
// 设计原则：
// - KISS: 简单高效的估算算法，避免引入复杂的tokenizer库
// - 向后兼容: 支持所有Claude模型和消息格式
// - 性能优先: 本地计算，响应时间<5ms
func (s *Server) handleCountTokens(c *gin.Context) {
	var req CountTokensRequest

	// 解析请求体
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"type":    "invalid_request_error",
				"message": fmt.Sprintf("Invalid request body: %v", err),
			},
		})
		return
	}

	// 验证模型参数（支持所有Claude模型）
	if !isValidClaudeModel(req.Model) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"type":    "invalid_request_error",
				"message": fmt.Sprintf("Invalid model: %s", req.Model),
			},
		})
		return
	}

	// 计算token数量
	tokenCount := estimateTokens(&req)

	// 返回符合官方API格式的响应
	c.JSON(http.StatusOK, CountTokensResponse{
		InputTokens: tokenCount,
	})
}

// estimateTokens 估算消息的token数量
// 算法说明：
// - 基础估算: 英文平均4字符/token，中文平均1.5字符/token
// - 固定开销: 消息角色标记、JSON结构等
// - 工具开销: 每个工具定义约50-200 tokens
//
// 注意：此为快速估算，与官方tokenizer可能有±10%误差
func estimateTokens(req *CountTokensRequest) int {
	totalTokens := 0

	// 1. 系统提示词（system prompt）
	if req.System != "" {
		totalTokens += estimateTextTokens(req.System)
		totalTokens += 5 // 系统提示的固定开销
	}

	// 2. 消息内容（messages）
	for _, msg := range req.Messages {
		// 角色标记开销（"user"/"assistant" + JSON结构）
		totalTokens += 10

		// 消息内容
		switch content := msg.Content.(type) {
		case string:
			// 文本消息
			totalTokens += estimateTextTokens(content)
		case []any:
			// 复杂内容块（文本、图片、文档等）
			for _, block := range content {
				totalTokens += estimateContentBlock(block)
			}
		default:
			// 其他格式：保守估算为JSON长度
			if jsonBytes, err := sonic.Marshal(content); err == nil {
				totalTokens += len(jsonBytes) / 4
			}
		}
	}

	// 3. 工具定义（tools）
	for _, tool := range req.Tools {
		// 工具基础固定开销（根据官方API实测调整）
		// 实测：单个简单工具约400-420 tokens基础开销
		// 包括：tool对象结构、name/description/input_schema字段标记、类型信息、JSON格式开销、API元数据
		baseToolOverhead := 400

		// 工具名称（特殊处理：下划线分词导致token数增加）
		// 例如: "mcp__Playwright__browser_navigate_back" 可能被分为15-20个tokens
		nameTokens := estimateToolName(tool.Name)
		totalTokens += nameTokens

		// 工具描述
		totalTokens += estimateTextTokens(tool.Description)

		// 工具schema（JSON Schema）- 官方tokenizer对schema编码开销极高
		if tool.InputSchema != nil {
			if jsonBytes, err := sonic.Marshal(tool.InputSchema); err == nil {
				// 官方对JSON Schema的token编码极其密集
				// 实测发现：每1.5-1.8个字符约1个token
				// 每个JSON字段、括号、逗号、冒号都可能是独立token
				schemaLen := len(jsonBytes)
				schemaTokens := int(float64(schemaLen) / 1.6)

				// 特殊字段额外开销（$schema等元数据字段）
				if strings.Contains(string(jsonBytes), "$schema") {
					schemaTokens += 15 // $schema字段的URL很长
				}

				if schemaTokens < 80 {
					schemaTokens = 80 // 即使是最简单的schema也有相当高的开销
				}
				totalTokens += schemaTokens
			}
		}

		// 应用基础工具开销
		totalTokens += baseToolOverhead
	}

	// 4. 基础请求开销（API格式固定开销）
	totalTokens += 10

	return totalTokens
}

// estimateToolName 估算工具名称的token数量
// 工具名称通常包含下划线、驼峰等特殊结构，tokenizer会进行更细粒度的分词
// 例如: "mcp__Playwright__browser_navigate_back"
// 可能被分为: ["mcp", "__", "Play", "wright", "__", "browser", "_", "navigate", "_", "back"]
func estimateToolName(name string) int {
	if name == "" {
		return 0
	}

	// 基础估算：按字符长度
	baseTokens := len(name) / 2 // 工具名称通常极其密集（比普通文本密集2倍）

	// 下划线分词惩罚：每个下划线可能导致额外的token
	underscoreCount := strings.Count(name, "_")
	underscorePenalty := underscoreCount // 每个下划线约1个额外token

	// 驼峰分词惩罚：大写字母可能是分词边界
	camelCaseCount := 0
	for _, r := range name {
		if r >= 'A' && r <= 'Z' {
			camelCaseCount++
		}
	}
	camelCasePenalty := camelCaseCount / 2 // 每2个大写字母约1个额外token

	totalTokens := baseTokens + underscorePenalty + camelCasePenalty
	if totalTokens < 2 {
		totalTokens = 2 // 最少2个token
	}

	return totalTokens
}

// estimateTextTokens 估算纯文本的token数量
// 混合语言处理：
// - 检测中文字符比例
// - 中文: 1.5字符/token（汉字信息密度高）
// - 英文: 4字符/token（标准GPT tokenizer比率）
func estimateTextTokens(text string) int {
	if text == "" {
		return 0
	}

	// 转换为rune数组以正确计算Unicode字符数
	runes := []rune(text)
	runeCount := len(runes)

	if runeCount == 0 {
		return 0
	}

	// 检测中文字符比例（优化：只采样前500字符）
	sampleSize := runeCount
	if sampleSize > 500 {
		sampleSize = 500
	}

	chineseChars := 0
	for i := 0; i < sampleSize; i++ {
		r := runes[i]
		// 中文字符范围（CJK统一汉字）
		if r >= 0x4E00 && r <= 0x9FFF {
			chineseChars++
		}
	}

	// 计算中文比例
	chineseRatio := float64(chineseChars) / float64(sampleSize)

	// 混合语言token估算
	// 纯英文: 4字符/token
	// 纯中文: 1.5字符/token
	// 混合: 线性插值
	charsPerToken := 4.0 - (4.0-1.5)*chineseRatio

	tokens := int(float64(runeCount) / charsPerToken)
	if tokens < 1 {
		tokens = 1 // 最少1个token
	}

	return tokens
}

// estimateContentBlock 估算单个内容块的token数量
// 支持的内容类型：
// - text: 文本块
// - image: 图片（固定1000 tokens估算）
// - document: 文档（根据大小估算）
func estimateContentBlock(block any) int {
	blockMap, ok := block.(map[string]any)
	if !ok {
		return 10 // 未知格式，保守估算
	}

	blockType, _ := blockMap["type"].(string)

	switch blockType {
	case "text":
		// 文本块
		if text, ok := blockMap["text"].(string); ok {
			return estimateTextTokens(text)
		}
		return 10

	case "image":
		// 图片：官方文档显示约1000-2000 tokens
		// 参考: https://docs.anthropic.com/en/docs/build-with-claude/vision
		return 1500

	case "document":
		// 文档：根据大小估算（简化处理）
		return 500

	case "tool_use":
		// 工具调用结果
		if input, ok := blockMap["input"]; ok {
			if jsonBytes, err := sonic.Marshal(input); err == nil {
				return len(jsonBytes) / 4
			}
		}
		return 50

	case "tool_result":
		// 工具执行结果
		if content, ok := blockMap["content"].(string); ok {
			return estimateTextTokens(content)
		}
		return 50

	default:
		// 未知类型：JSON长度估算
		if jsonBytes, err := sonic.Marshal(block); err == nil {
			return len(jsonBytes) / 4
		}
		return 10
	}
}

// isValidClaudeModel 验证是否为有效的Claude模型
// 支持所有Claude系列模型（不限制具体版本号）
func isValidClaudeModel(model string) bool {
	if model == "" {
		return false
	}

	model = strings.ToLower(model)

	// 支持的模型前缀
	validPrefixes := []string{
		"claude-",         // 所有Claude模型
		"gpt-",            // OpenAI兼容模式（codex渠道）
		"gemini-",         // Gemini兼容模式
		"text-",           // 传统completion模型
		"anthropic.claude", // Bedrock格式
	}

	for _, prefix := range validPrefixes {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}

	return false
}
