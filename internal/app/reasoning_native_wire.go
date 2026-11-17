package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"ccLoad/internal/model"

	"github.com/tidwall/gjson"
)

/**
 * @brief 根据实际模型的原生协议编码单评分题或 Jev 双选择题
 * @param provider 实际模型的提供商
 * @param actualModel 最终重定向后的上游模型名
 * @param transcript 已完成隐私遮蔽的判断文本
 * @param levels 按强度升序排列的候选等级
 * @param prompt 已解析的提示词；包含 Depth 时保留 Jev 双题语义
 * @return 上游原生 JSON 请求
 * @return JSON 编码失败时返回错误；成功时为 nil
 */
func reasoningDecisionBody(provider, actualModel, transcript string, levels []string, prompt model.ReasoningPrompt) ([]byte, error) {
	state, _ := json.Marshal(map[string]string{"conversation": transcript})
	questionType := "score"
	if prompt.Depth != nil {
		questionType = "choice"
	}
	questions := make(map[string]systemOneQuestion)
	var openAIQuestions []map[string]any
	names := []string{"effort"}
	if prompt.Depth != nil && reasoningDepthLevels(levels) != nil {
		names = append(names, "depth")
	}
	for _, name := range names {
		instructions, criteria, questionLevels := prompt.Instructions, prompt.Criteria, levels
		if name == "depth" {
			instructions, criteria, questionLevels = prompt.Depth.Instructions, prompt.Depth.Criteria, reasoningDepthLevels(levels)
		}
		if provider == "openai" {
			question := map[string]any{"name": name, "type": questionType, "instructions": instructions}
			optionsField, labelField := "levels", "label"
			if questionType == "choice" {
				optionsField, labelField = "choices", "value"
			}
			options := make([]map[string]string, 0, len(questionLevels))
			for _, level := range questionLevels {
				options = append(options, map[string]string{labelField: level, "description": criteria[level]})
			}
			question[optionsField] = options
			openAIQuestions = append(openAIQuestions, question)
			continue
		}
		var encodedCriteria json.RawMessage
		if questionType == "score" {
			descriptions := make([]string, 0, len(questionLevels))
			for _, level := range questionLevels {
				descriptions = append(descriptions, criteria[level])
			}
			encodedCriteria, _ = json.Marshal(descriptions)
		} else {
			encodedCriteria = reasoningDecisionCriteria(questionLevels, criteria)
		}
		questions[name] = systemOneQuestion{questionType, instructions, encodedCriteria}
	}
	inputField := "state"
	if provider == "gemini" || provider == "openai" {
		inputField = "input"
	}
	body := map[string]any{"model": actualModel, inputField: string(state), "questions": questions}
	if provider == "openai" {
		body["questions"] = openAIQuestions
	}
	return json.Marshal(body)
}

/**
 * @brief 决策请求体压缩门槛（字节）；更小的请求省下的传输不抵压缩开销
 */
const reasoningDecisionCompressMinBytes = 4 << 10

/**
 * @brief 已确认拒收压缩决策请求的目标；同一渠道不同域名或提供商的网关能力互不相同
 */
type reasoningCompressionTarget struct {
	channelID int64  ///< 决策渠道
	host      string ///< 实际请求的域名与端口
	provider  string ///< 实际模型的提供商，决定尝试的编码
}

/**
 * @brief 按实际模型的提供商选择达到门槛的决策请求体编码
 * @param provider 实际模型的提供商
 * @param header 已应用渠道请求规则的请求头
 * @param body 最终发送的明文 JSON
 * @return "zstd"、"gzip"；小请求、阿里云或请求规则已设置 Content-Encoding 时为空
 * @details 2026-11-16 官方端点实测：Jev 与 OpenAI 接受 zstd（OpenAI 拒收 gzip），Gemini 只接受 gzip
 * （zstd 返回 415），阿里云忽略 Content-Encoding 并把压缩字节当 JSON 解析，因此保持明文。
 * 第三方网关由调用方按渠道和域名在拒收后回退明文。
 */
func reasoningDecisionEncoding(provider string, header http.Header, body []byte) string {
	if len(body) < reasoningDecisionCompressMinBytes || header.Get("Content-Encoding") != "" {
		return ""
	}
	switch provider {
	case "jev", "openai":
		return "zstd"
	case "gemini":
		return "gzip"
	}
	return ""
}

/**
 * @brief 拼接原生决策端点，保留已有版本前缀、完整端点和精确 URL
 * @param baseURL 渠道选定的 URL
 * @param provider 实际模型的提供商
 * @return 原生决策请求 URL；无效 URL 留给请求构造阶段报告
 */
func reasoningDecisionURL(baseURL, provider string) string {
	if model.HasExactUpstreamURLMarker(baseURL) {
		return model.StripExactUpstreamURLMarker(baseURL)
	}
	path := "/v1/systemone"
	switch provider {
	case "aliyun":
		path = "/compatible-mode/v1/systemone"
	case "gemini":
		path = "/v1beta/decisions"
	case "openai":
		path = "/v1/decisions"
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return buildUpstreamURL(baseURL, path, "")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	endpoint := path[strings.LastIndex(path, "/"):]
	switch {
	case strings.HasSuffix(parsed.Path, endpoint):
	case strings.HasSuffix(parsed.Path, "/v1"), strings.HasSuffix(parsed.Path, "/v1beta"):
		parsed.Path += endpoint
	case provider == "aliyun" && strings.HasSuffix(parsed.Path, "/compatible-mode"):
		parsed.Path += "/v1/systemone"
	default:
		parsed.Path += path
	}
	parsed.RawPath = ""
	return parsed.String()
}

/**
 * @brief 读取指定题目，兼容原生对象题集与 OpenAI 命名数组
 * @param response 上游 JSON 响应
 * @param provider 实际模型的提供商
 * @param name 题目名称
 * @return 题目原始结果；缺失时返回不存在的结果
 */
func reasoningDecisionAnswer(response []byte, provider, name string) gjson.Result {
	answers := gjson.GetBytes(response, "answers")
	if provider != "openai" {
		return answers.Get(name)
	}
	for _, answer := range answers.Array() {
		if answer.Get("name").String() == name {
			return answer
		}
	}
	return gjson.Result{}
}

/**
 * @brief 按判断模型校准的 Jev 双题概率阈值
 */
type reasoningChoiceThresholds struct {
	high  int ///< 千分比；effort 低于 high 时，high 及以上概率之和达到该值即升为 high
	xhigh int ///< 千分比；effort 为 high 时，depth 的 xhigh 及以上概率之和达到该值判 xhigh，否则为 high
}

/**
 * @brief 已校准判断模型的名称前缀与阈值；未列出的模型直接采用 choice
 * @details 2026-11-17 离线主集回放：Jev 与 D3 按 choice 都把大量 high/xhigh 判成 medium，
 * 改用阈值后 Jev 可接受率 75.9→85.3%、D3 75.0→87.9%。阈值依附模型而非模板：
 * D3 的阈值套到 Jev 上偏高从 10% 升到 24%。
 */
var reasoningChoiceCalibrations = []struct {
	prefix     string                    ///< 去掉思考后缀并转小写后的实际模型名前缀
	thresholds reasoningChoiceThresholds ///< 该模型的阈值
}{
	{prefix: "jev-", thresholds: reasoningChoiceThresholds{high: 400, xhigh: 650}},
	{prefix: "vllm-sr/d3", thresholds: reasoningChoiceThresholds{high: 200, xhigh: 350}},
}

/**
 * @brief 查找实际判断模型的概率阈值
 * @param actualModel 完成重定向后的上游模型名
 * @return 该模型的阈值；未校准时为零值
 * @return 已校准时为 true
 */
func reasoningChoiceCalibration(actualModel string) (reasoningChoiceThresholds, bool) {
	name := strings.ToLower(strings.TrimSpace(model.RoutingModelName(actualModel)))
	for _, calibration := range reasoningChoiceCalibrations {
		if strings.HasPrefix(name, calibration.prefix) {
			return calibration.thresholds, true
		}
	}
	return reasoningChoiceThresholds{}, false
}

/**
 * @brief 汇总 choice 答案中从指定档位起的概率
 * @param answer 已校验 choice 的答案
 * @param question 题目名，用于错误信息
 * @param levels 该题按强度升序排列的档位
 * @param from 起始档位名，须在 levels 中
 * @return 从 from 起各档概率之和，四舍五入为千分比；取整消除浮点累加误差
 *         （0.36+0.04 累加为 0.39999999999999997），使两位小数概率恰好等于阈值时也算达到
 * @return 答案未附概率时为 false
 * @return 概率存在但缺档位或越界时返回错误；成功时为 nil
 */
func reasoningChoiceMass(answer gjson.Result, question string, levels []string, from string) (int, bool, error) {
	probabilities := answer.Get("probabilities")
	if !probabilities.Exists() {
		return 0, false, nil
	}
	if !probabilities.IsObject() {
		return 0, false, fmt.Errorf("system one %s probabilities invalid", question)
	}
	mass, start := 0.0, slices.Index(levels, from)
	for i, level := range levels {
		value := probabilities.Get(level)
		if value.Type != gjson.Number || value.Float() < 0 || value.Float() > 1 {
			return 0, false, fmt.Errorf("system one %s probabilities invalid", question)
		}
		if i >= start {
			mass += value.Float()
		}
	}
	return int(math.Round(mass * 1000)), true, nil
}

/**
 * @brief 保留 Jev 双题置信度与封顶语义，单评分题使用原生概率的最大项
 * @param response 上游 JSON 响应
 * @param provider 实际模型的提供商
 * @param actualModel 完成重定向后的上游模型名，决定双题是否按校准阈值取档
 * @param prompt 本次请求使用的提示词
 * @param levels 按强度升序排列的候选等级
 * @return 建议等级；拒答或无效答案时为空
 * @return Jev 主选择题的置信度；单评分题始终为 nil
 * @return 答案或概率无效时返回错误；成功时为 nil
 * @details 已校准模型的 effort 低于 high 且 high 及以上概率之和达到阈值时升为 high；
 * 进入 depth 后按 xhigh 及以上概率之和判 xhigh 或 high，不再采用 depth 的 choice。
 * 未附概率时沿用 choice；置信度始终取 effort 原始答案。
 */
func parseReasoningDecision(response []byte, provider, actualModel string, prompt model.ReasoningPrompt, levels []string) (string, *float64, error) {
	answer := reasoningDecisionAnswer(response, provider, "effort")
	if prompt.Depth == nil {
		effort, err := reasoningScoreChoice(answer, provider, levels)
		return effort, nil, err
	}
	effort, confidence, err := systemOneChoice(answer, "effort", levels)
	if err != nil {
		return "", nil, err
	}
	thresholds, calibrated := reasoningChoiceCalibration(actualModel)
	if high := slices.Index(levels, "high"); calibrated && high >= 0 && slices.Index(levels, effort) < high {
		mass, ok, err := reasoningChoiceMass(answer, "effort", levels, "high")
		if err != nil {
			return "", nil, err
		}
		if ok && mass >= thresholds.high {
			effort = "high"
		}
	}
	depthLevels := reasoningDepthLevels(levels)
	if depth := reasoningDecisionAnswer(response, provider, "depth"); effort == "high" && depthLevels != nil && depth.Exists() {
		if effort, _, err = systemOneChoice(depth, "depth", depthLevels); err != nil {
			return "", nil, err
		}
		if calibrated {
			mass, ok, err := reasoningChoiceMass(depth, "depth", depthLevels, "xhigh")
			if err != nil {
				return "", nil, err
			}
			if ok {
				effort = "high"
				if mass >= thresholds.xhigh {
					effort = "xhigh"
				}
			}
		}
	}
	if effort == "max" {
		effort = levels[slices.Index(levels, effort)-1]
	}
	return effort, confidence, nil
}

/**
 * @brief 校验完整的评分概率并选择最大项，平票选择较低等级
 * @param answer 单个原生评分题答案
 * @param provider 实际模型的提供商
 * @param levels 按强度升序排列的候选等级
 * @return 概率最大的等级；无效时为空
 * @return 拒答、缺失等级或无效概率分布返回错误；成功时为 nil
 * @details 允许提供商将每项概率舍入到两位小数产生的总和误差
 */
func reasoningScoreChoice(answer gjson.Result, provider string, levels []string) (string, error) {
	invalid := errors.New("reasoning score probabilities missing or invalid")
	if !answer.IsObject() || answer.Get("type").String() != "score" {
		return "", errors.New("reasoning score answer missing or invalid")
	}
	probabilities := answer.Get("probabilities")
	values := make([]float64, len(levels))
	if provider == "openai" {
		items := probabilities.Array()
		if !probabilities.IsArray() || len(items) != len(levels) {
			return "", invalid
		}
		seen := make([]bool, len(levels))
		for _, item := range items {
			index := item.Get("value")
			i := int(index.Int())
			if index.Type != gjson.Number || index.Float() != float64(i) || i < 0 || i >= len(levels) || seen[i] || item.Get("label").String() != levels[i] {
				return "", invalid
			}
			value := item.Get("probability")
			if value.Type != gjson.Number {
				return "", invalid
			}
			values[i], seen[i] = value.Float(), true
		}
	} else {
		if !probabilities.IsObject() || len(probabilities.Map()) != len(levels) {
			return "", invalid
		}
		for i := range levels {
			value := probabilities.Get(strconv.Itoa(i))
			if value.Type != gjson.Number {
				return "", invalid
			}
			values[i] = value.Float()
		}
	}
	best, total := 0, 0.0
	for i, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return "", invalid
		}
		total += value
		if value > values[best] {
			best = i
		}
	}
	if math.Abs(total-1) > 0.005*float64(len(values))+1e-9 {
		return "", invalid
	}
	return levels[best], nil
}

/**
 * @brief 读取决策原生用量，Gemini 独立使用 total_input/output_tokens
 * @param response 上游 JSON 响应
 * @param provider 实际模型的提供商
 * @return 已报告的用量；计数缺失时保留其余有效字段
 * @return 精确计数完整时为 true；阿里云不生成输出，允许缺省 output_tokens。
 */
func reasoningDecisionUsage(response []byte, provider string) (*fwResult, bool) {
	if provider != "gemini" {
		known := systemOneUsagePresent(response)
		if provider == "aliyun" && !gjson.GetBytes(response, "usage.output_tokens").Exists() {
			input := gjson.GetBytes(response, "usage.input_tokens")
			known = input.Type == gjson.Number && input.Int() >= 0 && input.Float() == float64(input.Int())
		}
		return systemOneUsage(response, provider), known
	}
	result := &fwResult{ResponseModel: gjson.GetBytes(response, "model").String()}
	known := true
	for _, counter := range []struct {
		name  string
		value *int
	}{{"total_input_tokens", &result.InputTokens}, {"total_output_tokens", &result.OutputTokens}} {
		value := gjson.GetBytes(response, "usage."+counter.name)
		if value.Type != gjson.Number || value.Int() < 0 || value.Float() != float64(value.Int()) {
			known = false
			continue
		}
		*counter.value = int(value.Int())
	}
	return result, known
}
