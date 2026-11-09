package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/zaiauth"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 付费推理直连账号所属站点，携带渠道 Key 和设备身份；体验请求单独使用 ZCode JWT。

// isZAICodingPlanRequest reports whether this attempt is a Coding Plan
// Anthropic Messages call owned by a Z.ai channel.
func isZAICodingPlanRequest(cfg *model.Config, upstream protocol.Protocol, requestPath string) bool {
	return cfg != nil && cfg.UsesZAIOAuth() && isAnthropicMessagesRequest(upstream, requestPath)
}

// zaiRequestIdentity is ZCode's metadata.user_id payload. Field order matches
// the official client because the value travels as an opaque JSON string.
type zaiRequestIdentity struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

/**
 * @brief 写入 ZCode 身份并在修复快照建立前清理搜索占位工具，保持 tools 下标一致。
 * @param body 转换后的 Messages JSON。
 * @param cfg 当前渠道凭证快照。
 * @return 最终付费请求体；体验包装稍后执行。
 * @return JSON 或身份编码失败时返回错误。
 */
func finalizeZAICodingPlanBody(body []byte, cfg *model.Config) ([]byte, error) {
	if !isMutableJSONObject(body) {
		return nil, errors.New("finalize z.ai Coding Plan request: invalid JSON body")
	}
	if zaiSearchNeedsAlias(body) {
		var tools []json.RawMessage
		for _, tool := range gjson.GetBytes(body, "tools").Array() {
			if tool.Get("name").String() == "DeferredToolPlaceholder" {
				continue
			}
			raw, err := sjson.Delete(tool.Raw, "defer_loading")
			if err != nil {
				return nil, err
			}
			tools = append(tools, json.RawMessage(raw))
		}
		raw, err := json.Marshal(tools)
		if err != nil {
			return nil, err
		}
		body, err = sjson.SetRawBytes(body, "tools", raw)
		if err != nil {
			return nil, err
		}
	}
	identity, err := json.Marshal(zaiRequestIdentity{
		DeviceID:  zaiDeviceID(cfg),
		SessionID: resolveZAISessionID(body, cfg),
	})
	if err != nil {
		return nil, errors.New("finalize z.ai Coding Plan request: invalid identity")
	}
	identityRaw, err := json.Marshal(string(identity))
	if err != nil {
		return nil, errors.New("finalize z.ai Coding Plan request: encode failed")
	}
	metadata := gjson.GetBytes(body, "metadata")
	if !metadata.Exists() || metadata.Type == gjson.Null || !metadata.IsObject() {
		finalized, err := sjson.SetRawBytes(body, "metadata", append([]byte(`{"user_id":`), append(identityRaw, '}')...))
		if err != nil {
			return nil, errors.New("finalize z.ai Coding Plan request: encode failed")
		}
		return finalized, nil
	}
	finalized, err := sjson.SetRawBytes(body, "metadata.user_id", identityRaw)
	if err != nil {
		return nil, errors.New("finalize z.ai Coding Plan request: encode failed")
	}
	return finalized, nil
}

/**
 * @brief 判断搜索工具是否需要别名转换，真实同名工具存在时跳过全部变换。
 * @param body Messages 请求 JSON。
 * @return 声明 ToolSearch 且未声明 ToolSearchTool 时返回 true。
 */
func zaiSearchNeedsAlias(body []byte) bool {
	found := false
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		switch tool.Get("name").String() {
		case "ToolSearchTool":
			return false
		case "ToolSearch":
			found = true
		}
	}
	return found
}

/**
 * @brief 同步工具定义、助手历史及强制选择的名称，不修改参数或正文字符串。
 * @param body 已清理占位符的请求 JSON。
 * @return 改名后的请求；未命中兼容条件时原样返回。
 * @return 是否需要将最终响应中的别名恢复。
 */
func aliasZAISearch(body []byte) ([]byte, bool) {
	if !zaiSearchNeedsAlias(body) {
		return body, false
	}
	rename := func(name string) (string, bool) { return "ToolSearchTool", name == "ToolSearch" }
	var edits []jsonValueEdit
	root := gjson.ParseBytes(body)
	for i, tool := range root.Get("tools").Array() {
		edits = appendAnthropicToolNameEdit(edits, "tools."+strconv.Itoa(i)+".name", tool.Get("name"), rename)
	}
	for i, message := range root.Get("messages").Array() {
		if message.Get("role").String() == "assistant" {
			edits = appendAnthropicToolBlocksEdits(edits, "messages."+strconv.Itoa(i)+".content", message.Get("content"), rename)
		}
	}
	if root.Get("tool_choice.type").String() == "tool" {
		edits = appendAnthropicToolNameEdit(edits, "tool_choice.name", root.Get("tool_choice.name"), rename)
	}
	return applyJSONValueEdits(body, edits), true
}

/**
 * @brief 在协议转换前恢复 JSON 消息或单个 SSE 事件中的搜索工具名称。
 * @param payload 一段完整的协议 JSON。
 * @return 已恢复名称的 JSON；其他工具及参数原样保留。
 */
func restoreZAISearch(payload []byte) []byte {
	rename := func(name string) (string, bool) { return "ToolSearch", name == "ToolSearchTool" }
	root := gjson.ParseBytes(payload)
	edits := appendAnthropicToolBlockEdits(nil, "content_block", root.Get("content_block"), rename)
	edits = appendAnthropicToolBlocksEdits(edits, "content", root.Get("content"), rename)
	edits = appendAnthropicToolBlocksEdits(edits, "message.content", root.Get("message.content"), rename)
	return applyJSONValueEdits(payload, edits)
}

/**
 * @brief 标识已由协议层确认的 ZCode Messages 请求，随克隆保留且不发送到上游。
 */
type zaiPlanRequestKey struct{}

/**
 * @brief 重建 ZCode 请求头并标记套餐请求身份，不依赖实际出站 URL。
 * @param req 已确认属于 Messages 的出站请求，原地更新头和上下文。
 * @param cfg 本次渠道凭证快照。
 * @param apiKey 付费认证 Key。
 * @param body 最终请求体。
 * @param incoming 原始请求头。
 * @note 调用方在重建后重新应用渠道头规则；身份标记随请求重试保留。
 */
func injectZAICodingPlanHeaders(req *http.Request, cfg *model.Config, apiKey string, body []byte, incoming http.Header) {
	if req == nil {
		return
	}
	*req = *req.WithContext(context.WithValue(req.Context(), zaiPlanRequestKey{}, true))
	accept := anthropicHeaderValue(incoming, "Accept")
	if accept == "" {
		accept = "application/json"
	}
	anthropicVersion := anthropicHeaderValue(incoming, "anthropic-version")
	if anthropicVersion == "" {
		anthropicVersion = "2023-06-01"
	}
	for name := range req.Header {
		delete(req.Header, name)
	}
	setRawHeader(req.Header, "Accept", accept)
	setRawHeader(req.Header, "Content-Type", "application/json")
	setRawHeader(req.Header, "anthropic-version", anthropicVersion)
	// 两个认证头使用同一付费 Key，禁止保留调用方的凭证或混入体验 JWT。
	key := strings.TrimSpace(apiKey)
	setRawHeader(req.Header, "x-api-key", key)
	if key != "" {
		setRawHeader(req.Header, "Authorization", "Bearer "+key)
	}
	for _, entry := range zaiauth.SourceHeaders() {
		setRawHeader(req.Header, entry[0], entry[1])
	}
	setRawHeader(req.Header, "x-request-id", uuid.NewString())
	setRawHeader(req.Header, "x-zcode-trace-id", uuid.NewString())
	setRawHeader(req.Header, "x-session-id", resolveZAISessionID(body, cfg))
}

// resolveZAISessionID keeps one conversation on a single session identifier:
// the caller's own session when it sends one, otherwise a value derived from
// the channel fingerprint and the opening user message.
func resolveZAISessionID(body []byte, cfg *model.Config) string {
	if sessionID := anthropicSessionIDFromBody(body); sessionID != "" {
		return sessionID
	}
	if deviceID := zaiDeviceID(cfg); deviceID != "" {
		return anthropicStableSessionID(deviceID, anthropicFirstUserText(gjson.GetBytes(body, "messages")))
	}
	return uuid.NewString()
}

func zaiDeviceID(cfg *model.Config) string {
	if cfg == nil {
		return ""
	}
	if deviceID := strings.TrimSpace(cfg.ZAIDeviceID); deviceID != "" {
		return deviceID
	}
	credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return ""
	}
	return credential.DeviceID
}

// zaiCredentialRejected reports whether an upstream response means the Coding
// Plan key itself was refused. Z.ai answers 401 with its own error envelope.
func zaiCredentialRejected(status int) bool {
	return status == http.StatusUnauthorized
}

/**
 * @brief ZAI 响应保留实际套餐请求和原始修复体，其他渠道原样直通。
 * @param cfg 当前渠道配置。
 * @param resp 发送结果，允许 nil；存在实际请求时从中读取 wire 请求体。
 * @param req 发送前请求，未取得实际请求时原样返回。
 * @param body 改名前的发送体，用于保持修复重试的工具名称边界。
 * @return 实际发送请求；无法取得时返回 req。
 * @return 实际 wire 请求体；无法读取时返回 body。
 * @return 原始修复体，与入参 body 共享底层数组；非 ZAI 或缺少响应请求时为 nil。
 */
func captureZAIForwardRequest(cfg *model.Config, resp *http.Response, req *http.Request, body []byte) (*http.Request, []byte, []byte) {
	if !cfg.UsesZAIOAuth() || resp == nil || resp.Request == nil {
		return req, body, nil
	}
	// 实际 wire 体由 GetBody 重新生成，原发送体不再被改写，可直接作为修复体。
	original := body
	req = resp.Request
	if actual, err := zaiRequestBody(req); err == nil {
		body = actual
	}
	return req, body, original
}

/**
 * @brief 集中标记 ZAI 套餐结果，供现有冷却、计费和重试边界使用。
 * @param cfg 当前渠道配置。
 * @param result 待标注的发送结果，nil 时不操作。
 * @param resp 上游或本地套餐响应，传输失败时允许 nil。
 * @param body 实际 wire 请求体，用于提取计费模型。
 * @param err 发送错误；体验传输错误从 zaiTrialRequestError 取得实际模型。
 * @note 只修改 ZAI 结果的套餐标记与模型，不触发冷却或统计写入。
 */
func annotateZAIForwardResult(cfg *model.Config, result *fwResult, resp *http.Response, body []byte, err error) {
	if !cfg.UsesZAIOAuth() || result == nil {
		return
	}
	if err != nil {
		var trialErr *zaiTrialRequestError
		result.zaiTrial = errors.As(err, &trialErr)
		if result.zaiTrial {
			// 传输失败可能没有响应，请求体仍是路由别名，实际模型由发送边界携带。
			result.zaiModel = trialErr.model
		}
		return
	}
	result.zaiModel = gjson.GetBytes(body, "model").String()
	result.zaiTrial = isZAITrialResponse(resp)
	result.zaiLocalPlanError = isZAILocalPlanResponse(resp)
	result.zaiLocalPlanKind = zaiLocalPlanKind(resp)
}
