package app

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
	System   any            `json:"system,omitempty"` // 支持 string 或 []TextBlock
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
// - 本地计算: 避免引入复杂依赖
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
	// 支持 string 或 []TextBlock 两种格式
	if req.System != nil {
		switch sys := req.System.(type) {
		case string:
			// 字符串格式（旧版本兼容）
			if sys != "" {
				totalTokens += estimateTextTokens(sys)
				totalTokens += 5 // 系统提示的固定开销
			}
		case []any:
			// 数组格式（Beta版本）
			for _, block := range sys {
				totalTokens += estimateContentBlock(block)
			}
			totalTokens += 5 // 系统提示的固定开销
		default:
			// 其他格式：尝试JSON序列化估算
			if jsonBytes, err := sonic.Marshal(sys); err == nil {
				totalTokens += len(jsonBytes) / 4
			}
		}
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
	toolCount := len(req.Tools)
	if toolCount > 0 {
		// 工具开销策略：根据工具数量自适应调整
		// - 少量工具（1-3个）：每个工具高开销（包含大量元数据和结构信息）
		// - 大量工具（10+个）：共享开销 + 小增量（避免线性叠加过高）
		var baseToolsOverhead int
		var perToolOverhead int

		if toolCount == 1 {
			// 单工具场景：高开销（包含tools数组初始化、类型信息等）
			baseToolsOverhead = 0
			perToolOverhead = 400
		} else if toolCount <= 5 {
			// 少量工具：中等开销
			baseToolsOverhead = 150
			perToolOverhead = 150
		} else {
			// 大量工具：共享开销 + 低增量
			baseToolsOverhead = 250
			perToolOverhead = 80
		}

		totalTokens += baseToolsOverhead

		for _, tool := range req.Tools {
			// 工具名称（特殊处理：下划线分词导致token数增加）
			nameTokens := estimateToolName(tool.Name)
			totalTokens += nameTokens

			// 工具描述
			totalTokens += estimateTextTokens(tool.Description)

			// 工具schema（JSON Schema）
			if tool.InputSchema != nil {
				if jsonBytes, err := sonic.Marshal(tool.InputSchema); err == nil {
					// Schema编码密度：根据工具数量自适应
					var schemaCharsPerToken float64
					if toolCount == 1 {
						schemaCharsPerToken = 1.6 // 单工具密集编码
					} else if toolCount <= 5 {
						schemaCharsPerToken = 1.9 // 少量工具
					} else {
						schemaCharsPerToken = 2.2 // 大量工具更宽松
					}

					schemaLen := len(jsonBytes)
					schemaTokens := int(float64(schemaLen) / schemaCharsPerToken)

					// $schema字段URL开销
					if strings.Contains(string(jsonBytes), "$schema") {
						if toolCount == 1 {
							schemaTokens += 15
						} else {
							schemaTokens += 8
						}
					}

					// 最小schema开销
					minSchemaTokens := 80
					if toolCount > 5 {
						minSchemaTokens = 40
					}
					if schemaTokens < minSchemaTokens {
						schemaTokens = minSchemaTokens
					}

					totalTokens += schemaTokens
				}
			}

			totalTokens += perToolOverhead
		}
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
	baseTokens := len(name) / 2 // 工具名称通常比普通文本更密集

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

	totalTokens := max(baseTokens+underscorePenalty+camelCasePenalty, 2) // 最少2个token

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
	sampleSize := min(runeCount, 500)

	chineseChars := 0
	for i := range sampleSize {
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
		"claude-",          // 所有Claude模型
		"gpt-",             // OpenAI GPT系列
		"chatgpt-",         // OpenAI ChatGPT系列（如chatgpt-4o-latest）
		"o1",               // OpenAI o1系列（o1, o1-mini, o1-pro等）
		"o3",               // OpenAI o3系列
		"o4",               // OpenAI o4系列
		"gemini-",          // Gemini兼容模式
		"text-",            // 传统completion模型
		"anthropic.claude", // Bedrock格式
	}

	for _, prefix := range validPrefixes {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}

	return false
}
