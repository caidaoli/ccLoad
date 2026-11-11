package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	cliproxysignature "ccLoad/internal/protocol/cliproxy/signature"
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

/**
 * @brief 为未指定思考的 ZAI 请求补 high，并为原输出额度另留思考预算。
 * @param body 转换后的 Messages 正文；渠道规则尚未执行。
 * @param source 调用方正文，用于识别转换时被移除的显式关闭和模型后缀。
 * @param requestModel 实际路由模型，允许带 Trial 后缀。
 * @return 补齐后的正文；显式设置、不支持或未知模型、非法参数保持原处理。
 * @note 仅首次请求准备时调用，修复重放不能恢复已移除的思考或再次增加额度。
 */
func prepareZAIThinkingDefaults(body, source []byte, requestModel string) []byte {
	if !isMutableJSONObject(body) {
		return body
	}
	for _, candidate := range [][]byte{body, source} {
		for _, path := range []string{"thinking", "output_config.effort", "reasoning_effort", "reasoning.effort",
			"generationConfig.thinkingConfig", "generationConfig.thinking_config",
			"generation_config.thinkingConfig", "generation_config.thinking_config"} {
			if gjson.GetBytes(candidate, path).Exists() {
				return body
			}
		}
		if _, _, explicit := model.ParseThinkingSuffix(gjson.GetBytes(candidate, "model").String()); explicit {
			return body
		}
	}
	// 强制工具选择不兼容扩展思考，沿用既有工具契约。
	switch gjson.GetBytes(body, "tool_choice.type").String() {
	case "any", "tool":
		return body
	}
	name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(requestModel)), "-trial")
	budget, ceiling := int64(10240), int64(0)
	switch name {
	case "glm-5.3", "glm-5.3-flash":
		budget, ceiling = 16000, 128000
	case "glm-5.2":
		ceiling = 128000
	case "glm-5.1", "glm-5", "glm-5-turbo":
		ceiling = 64000
	case "glm-4.7", "glm-4.6":
		ceiling = 131072
	case "glm-4.5-air":
		ceiling = 98304
	default:
		return body
	}
	base := int64(1024)
	if value := gjson.GetBytes(body, "max_tokens"); value.Exists() {
		var valid bool
		base, valid = jsonIntegerValue(value)
		if !valid || base <= 0 {
			return body
		}
	}
	if config := gjson.GetBytes(body, "output_config"); config.Exists() && !config.IsObject() {
		return body
	}
	// 先裁剪再相加，既保留正文空间，也避免客户端极大整数造成溢出。
	body = setJSONRaw(body, "max_tokens", strconv.FormatInt(min(base, ceiling-budget)+budget, 10))
	body = setJSONRaw(body, "thinking", `{"type":"enabled","budget_tokens":`+strconv.FormatInt(budget, 10)+`}`)
	if budget == 16000 {
		// GLM-5.3 要求预算与等级配对，仅 effort 不能限制思考消耗。
		body = setJSONRaw(body, "output_config.effort", `"high"`)
	}
	for _, path := range []string{"temperature", "top_p", "top_k"} {
		body = deleteJSONPath(body, path)
	}
	return body
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
	body, err := cleanZAISearchTools(body)
	if err != nil {
		return nil, err
	}
	return writeZAIRequestIdentity(body, cfg)
}

/**
 * @brief 在建立修复基准前清理搜索占位工具，不改写套餐身份或工具名称。
 * @param body Messages JSON。
 * @return 清理后的独立正文；无兼容需求时原样返回。
 * @return JSON 编辑失败时返回错误。
 */
func cleanZAISearchTools(body []byte) ([]byte, error) {
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
	return body, nil
}

/**
 * @brief 保留付费套餐原有的身份覆盖规则。
 * @param body 已归一并清理工具的正文。
 * @param cfg 当前凭证快照。
 * @return 已写入付费身份的正文。
 * @return JSON 编码或编辑失败时返回错误。
 */
func writeZAIRequestIdentity(body []byte, cfg *model.Config) ([]byte, error) {
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
 * @brief ZAI 请求私有来源与最后一次实际尝试；仅由该请求的发送协程读写。
 */
type zaiRequestState struct {
	started       time.Time           ///< 首次实际 Trial 尝试时间，容量等待不提前计入。
	retryDeadline time.Time           ///< 当前发送预算的最早截止时间；零表示未限时，仅用于决定是否继续 busy 重试。
	apiKey        string              ///< 已选付费 Key，不进入日志。
	headers       http.Header         ///< 构造套餐所需的调用方白名单头。
	callerBody    []byte              ///< 原调用方正文的只读引用，仅用于 CCH。
	callerWire    anthropicCallerWire ///< 原调用方分类，不按改写后的正文重新判断。
	last          *zaiSendAttempt     ///< 最后一次真实尝试；容量拒绝不覆盖。
	capacityAbort bool                ///< 已收到真实 busy 后因公共容量中止，禁止付费绕过。
	adminTest     bool                ///< 管理检测或聊天；仅付费最终化恢复管理压缩约定。
}

/**
 * @brief 一次最终化套餐的修复基准、实际请求及管理计时归因。
 */
type zaiSendAttempt struct {
	waitsForCapacity bool                ///< 管理回调已接管准入；req 为 nil 表示尚未真正尝试。
	req              *http.Request       ///< 真实尝试的请求，管理回调可替换为发送超时 context。
	repairBody       []byte              ///< 脱敏且未改工具名、未包装日期的修复基准。
	timeout          *channelTestTimeout ///< 仅容量等待批任务拥有的独立发送计时器。
}

/**
 * @brief 通过请求 context 传递 ZAI 私有状态。
 */
type zaiRequestStateKey struct{}

/**
 * @brief 通过请求 context 传递当前套餐尝试。
 */
type zaiSendAttemptKey struct{}

/**
 * @brief 捕获套餐选择前的来源，不提前写入付费身份或重建头。
 * @param req 请求；原地设置私有 context。
 * @param apiKey 已选择的付费 Key。
 * @param incoming 原调用方头，只保留构造必需白名单。
 * @param callerBody 原调用方正文，只读引用。
 * @param callerWire 原调用方分类。
 */
func captureZAISources(req *http.Request, apiKey string, incoming http.Header, callerBody []byte, callerWire anthropicCallerWire) {
	headers := make(http.Header)
	for _, name := range []string{"Accept", "anthropic-version", "x-session-id", "Session-Id", "Session_id", "Thread-Id", "X-Claude-Code-Session-Id", "x-magpie-work-dir", "x-magpie-work-git"} {
		if value := anthropicHeaderValue(incoming, name); value != "" {
			headers.Set(name, value)
		}
	}
	state := &zaiRequestState{apiKey: apiKey, headers: headers, callerBody: callerBody, callerWire: callerWire}
	ctx := context.WithValue(req.Context(), zaiRequestStateKey{}, state)
	*req = *req.WithContext(context.WithValue(ctx, zaiPlanRequestKey{}, true))
}

/**
 * @brief 获取当前 ZAI 私有状态；非 ZAI 请求返回 nil。
 * @param req 当前请求，允许 nil。
 * @return 来源与发送记录，未捕获时为 nil。
 */
func zaiState(req *http.Request) *zaiRequestState {
	if req == nil {
		return nil
	}
	state, _ := req.Context().Value(zaiRequestStateKey{}).(*zaiRequestState)
	return state
}

/**
 * @brief 严格匹配插件接受的 UUID 拼写，避免扩大到 URN 等格式。
 */
var zaiSessionUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

/**
 * @brief 按 ZCode 的 SHA256 规则派生 UUIDv4。
 * @param prefix 会话来源命名空间。
 * @param seed 原始显式头或设备 ID。
 * @return 稳定 UUID。
 */
func zaiSessionHash(prefix, seed string) string {
	digest := sha256.Sum256([]byte(prefix + "\x00" + seed))
	digest[6], digest[8] = digest[6]&0x0f|0x40, digest[8]&0x3f|0x80
	return uuid.UUID(digest[:16]).String()
}

/**
 * @brief 仅为 Trial 选择显式会话，缺失时按设备稳定派生。
 * @param body 公共规则后、身份覆盖前的候选正文。
 * @param headers 原调用方白名单头。
 * @param device 当前账号设备。
 * @return 同时用于 body、header 及 query 派生的会话。
 */
func resolveZAITrialSession(body []byte, headers http.Header, device string) string {
	seed := strings.TrimSpace(headers.Get("x-session-id"))
	if seed == "" {
		for _, name := range []string{"Session-Id", "Session_id"} {
			if value := headers.Get(name); strings.TrimSpace(value) != "" {
				h := http.Header{"Session-Id": {value}, "Thread-Id": {headers.Get("Thread-Id")}}
				seed = responsesExecutionSessionID(h)
				break
			}
		}
	}
	if seed == "" {
		seed = strings.TrimSpace(headers.Get("X-Claude-Code-Session-Id"))
	}
	if seed != "" {
		if zaiSessionUUID.MatchString(seed) {
			return strings.ToLower(seed)
		}
		return zaiSessionHash("zcode-session", seed)
	}
	if session := anthropicSessionIDFromBody(body); session != "" {
		return session
	}
	return zaiSessionHash("zcode-session-device", device)
}

/**
 * @brief 在套餐选择后最终化正文、修复基准及头，复用公共归一与隐私契约。
 * @param cfg 当前渠道快照。
 * @param req 未包装候选请求。
 * @param credential 已解析的 ZAI 凭证。
 * @param trialModel 非空表示 Trial 实际模型，否则付费。
 * @return 独立最终请求，包含尚未提交的尝试记录。
 * @return 准备或隐私错误，必须阻止发送及套餐回退。
 */
func prepareZAIPackageRequest(cfg *model.Config, req *http.Request, credential *zaiauth.Credential, trialModel string) (prepared *http.Request, prepareErr error) {
	defer func() {
		// 未发送的正文错误复用现有本地 400 契约，不伪装成网络失败或触发渠道冷却。
		if prepareErr != nil && !errors.Is(prepareErr, errPrivacyMasking) && req.Context().Err() == nil {
			prepareErr = &requestValidationError{message: prepareErr.Error()}
		}
	}()
	state := zaiState(req)
	body, err := zaiRequestBody(req)
	if err != nil {
		return nil, err
	}
	session := ""
	if trialModel != "" {
		session = resolveZAITrialSession(body, state.headers, credential.DeviceID)
		body, _ = cliproxysignature.RestoreCodexCommentaryMarkers(body)
		body, err = cleanZAISearchTools(body)
	} else {
		body, err = normalizeAnthropicMessagesBody(body)
		if err == nil {
			body, err = finalizeZAICodingPlanBody(body, cfg)
		}
	}
	if err != nil {
		return nil, err
	}
	body, err = maskPrivacyBodyFromContext(req.Context(), body, "application/json")
	if err != nil {
		return nil, err
	}
	if trialModel == "" {
		body, err = refreshAnthropicCallerCCH(body, state.callerBody, state.callerWire)
		if err != nil {
			return nil, err
		}
	}
	attempt := &zaiSendAttempt{repairBody: body}
	body, _ = aliasZAISearch(body)
	if trialModel != "" {
		env := zaiauth.StartEnvironment{}
		if dir, decodeErr := url.PathUnescape(state.headers.Get("x-magpie-work-dir")); decodeErr == nil && utf8.ValidString(dir) {
			env.WorkDir = dir
			env.Git = state.headers.Get("x-magpie-work-git")
		}
		body, err = zaiauth.PrepareStartBody(body, trialModel, credential.DeviceID, session, credential.Site, env)
		if err == nil {
			body, err = maskPrivacyBodyFromContext(req.Context(), body, "application/json")
		}
		if err != nil {
			return nil, err
		}
	}
	outbound := cloneRequestWithBody(req.Clone(context.WithValue(req.Context(), zaiSendAttemptKey{}, attempt)), body)
	if trialModel == "" {
		injectZAICodingPlanHeaders(outbound, cfg, state.apiKey, body, state.headers)
	} else {
		outbound.Header = zaiauth.StartHeaders(credential.JWTToken, session)
		if query := zaiauth.StartQueryID(attempt.repairBody, session); query != "" {
			setRawHeader(outbound.Header, "x-query-id", query)
		}
		setRawHeader(outbound.Header, "x-request-id", uuid.NewString())
		setRawHeader(outbound.Header, "x-zcode-trace-id", uuid.NewString())
	}
	applyHeaderRules(outbound.Header, cfg.HeaderRules())
	if trialModel == "" && state.adminTest {
		// 付费头重建后恢复管理入口的 identity，避免显式 gzip 使检测读到压缩字节。
		setAdminTestAcceptEncoding(cfg, outbound)
	}
	return outbound, nil
}

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
 * @param req 带私有记录的候选请求。
 * @param body 非 ZAI 或旧内部调用的原正文。
 * @return 实际尝试请求；ZAI 尚未发送时返回正文清空的候选克隆。
 * @return 实际 wire 正文；ZAI 尚未发送时为空，禁止把候选计入 Debug/CountSent。
 * @return 最终化后、工具别名前的修复基准；未发送或非 ZAI 为 nil。
 */
func captureZAIForwardRequest(cfg *model.Config, resp *http.Response, req *http.Request, body []byte) (*http.Request, []byte, []byte) {
	if !cfg.UsesZAIOAuth() {
		return req, body, nil
	}
	if state := zaiState(req); state != nil {
		if state.last != nil {
			actual := state.last.req
			if wire, err := zaiRequestBody(actual); err == nil {
				return actual, wire, state.last.repairBody
			}
		}
		// 资格拒绝和准备失败可能尚未扫描候选；它不是出站正文，也不能计入 CountSent。
		return cloneRequestWithBody(req.Clone(req.Context()), nil), nil, nil
	}
	if resp == nil || resp.Request == nil {
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
	if result.zaiTrial && resp != nil && resp.Request != nil {
		result.zaiTrialStarted, _ = resp.Request.Context().Value(zaiTrialStartedKey{}).(time.Time)
	}
	result.zaiLocalPlanError = isZAILocalPlanResponse(resp)
	result.zaiLocalPlanKind = zaiLocalPlanKind(resp)
}
