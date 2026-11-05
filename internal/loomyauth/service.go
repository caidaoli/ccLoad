package loomyauth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

/**
 * @brief 定义桌面客户端公开分发的账号应用身份与签名材料。
 * @note 协议来源及参考实现许可见 UPSTREAM.md，不属于用户私有凭证。
 */
const (
	applicationID = "GM3LOOMY"                         ///< 桌面客户端公开的账号应用标识。
	signKeyID     = "2thryby66wxi53sk"                 ///< 账号服务 HMAC 签名的公开客户端密钥标识。
	signKeySecret = "zsak6eadrbawz683wf5r3m2snrwj868r" ///< 公开客户端协议签名材料，来源与许可见 UPSTREAM.md。
)

/**
 * @brief 封装 Loomy 账号与控制接口，复用调用方的 HTTP 传输策略。
 */
type Service struct {
	Client     *http.Client ///< 调用方提供的 HTTP 客户端，负责代理和超时；nil 时请求失败。
	BaseURL    string       ///< 模型、积分及任务接口根地址，可由测试覆盖。
	AccountURL string       ///< 短信账号接口根地址，可由测试覆盖。
}

/**
 * @brief 创建指向官方端点的 Loomy 协议客户端。
 * @param client 承担代理、超时和传输策略的 HTTP 客户端。
 * @return 返回协议客户端；不立即发起网络请求，nil 客户端会在请求时返回错误。
 */
func NewService(client *http.Client) *Service {
	return &Service{Client: client, BaseURL: BaseURL, AccountURL: AccountURL}
}

/**
 * @brief 标识会话终态失效，调用方应要求重新登录。
 * @note 不存在已知刷新接口；重复使用相同会话不能恢复认证。
 */
var ErrSessionExpired = errors.New("loomy session expired; sign in again")

/**
 * @brief 表示经脱敏的 HTTP 或 Loomy 业务拒绝。
 * @details 仅携带状态码与业务码，避免错误输出包含上游正文、消息或凭证。
 */
type APIError struct {
	Status int    ///< 上游 HTTP 状态码；业务拒绝时仍可能为 2xx。
	Code   string ///< 去除首尾空白的上游业务码；缺失时为空。
}

/**
 * @brief 输出不含响应正文和凭证的协议错误摘要。
 * @return 返回仅包含 HTTP 状态码及业务码的错误文本。
 */
func (e *APIError) Error() string { return fmt.Sprintf("Loomy HTTP %d (code %s)", e.Status, e.Code) }

/**
 * @brief 将确定的会话失效映射为可通过 errors.Is 检测的终态错误。
 * @return HTTP 401 或已知会话失效业务码返回 ErrSessionExpired；其余拒绝返回 nil。
 */
func (e *APIError) Unwrap() error {
	if e.Status == http.StatusUnauthorized || IsSessionCode(e.Code) {
		return ErrSessionExpired
	}
	return nil
}

/**
 * @brief 判断业务码是否明确表示会话失效，避免将 WAF 或额度拒绝误判为认证失败。
 * @param code 可能含首尾空白的上游业务码。
 * @return 归一后为 020002 或 100002 时返回 true，否则返回 false。
 */
func IsSessionCode(code string) bool {
	code = strings.TrimSpace(code)
	return code == "020002" || code == "100002"
}

/**
 * @brief 写入 Loomy 官方请求身份和会话认证头。
 * @param h 调用方分配的非 nil 请求头映射。
 * @param session 经登录验证并从私有凭证取得的会话。
 * @note 原地覆盖内容类型、User-Agent、token 与 Authorization，应在自定义头规则之后调用。
 */
func ApplyHeaders(h http.Header, session string) {
	h.Set("Content-Type", "application/json;charset=utf-8")
	h.Set("User-Agent", ClientUA)
	h.Set("token", session)
	h.Set("Authorization", "Bearer "+session)
}

/**
 * @brief 执行一次有界 Loomy HTTP 请求并归一协议响应。
 * @param ctx 请求取消与截止时间上下文。
 * @param method HTTP 请求方法。
 * @param endpoint 完整目标 URL。
 * @param body 已编码的请求体，可为 nil。
 * @param headers 本次请求使用的头映射。
 * @param requireCode 是否要求成功业务码及非空 data，并提取最多一层嵌套 data。
 * @return 业务数据或完整原始 JSON；失败时为 nil。
 * @return 请求、读取、超过 2 MiB 或 JSON/业务格式错误时返回错误；HTTP/业务拒绝可用 errors.As 检查 *APIError，会话失效可用 errors.Is 检查 ErrSessionExpired；成功为 nil。
 * @note 发起网络请求并关闭响应体；POST 禁用 GetBody，防止传输层自动重放有副作用的请求。
 */
func (s *Service) request(ctx context.Context, method, endpoint string, body []byte, headers http.Header, requireCode bool) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = headers
	// A side-effecting POST must not be replayed automatically by a transport.
	if method == http.MethodPost {
		req.GetBody = nil
	}
	client := s.Client
	if client == nil {
		return nil, errors.New("loomy HTTP client unavailable")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loomy request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("loomy response read failed: %w", err)
	}
	if len(raw) > 2<<20 {
		return nil, errors.New("loomy response exceeds limit")
	}
	var envelope struct {
		Code *string         `json:"code"` ///< 上游业务码；指针 nil 表示缺失，非空值参与成功及会话失效分类。
		Data json.RawMessage `json:"data"` ///< 上游业务数据；根据接口约定解包 JSON 或模型列表。
	}
	parseErr := json.Unmarshal(raw, &envelope)
	code := ""
	if envelope.Code != nil {
		// 与参考客户端一致，业务码按去掉首尾空白后的值分类。
		code = strings.TrimSpace(*envelope.Code)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{Status: resp.StatusCode, Code: code}
	}
	if envelope.Code != nil && code != "000000" {
		return nil, &APIError{Status: resp.StatusCode, Code: code}
	}
	if !requireCode {
		if !json.Valid(raw) {
			return nil, errors.New("invalid Loomy JSON response")
		}
		return raw, nil
	}
	if parseErr != nil || envelope.Code == nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil, errors.New("invalid Loomy business response")
	}
	data := envelope.Data
	var nested struct {
		Data json.RawMessage `json:"data"` ///< 上游业务数据；根据接口约定解包 JSON 或模型列表。
	}
	if json.Unmarshal(data, &nested) == nil && len(nested.Data) > 0 && string(nested.Data) != "null" {
		data = nested.Data
	}
	return data, nil
}

/**
 * @brief 通过会话认证调用 Loomy 控制接口并提取业务数据。
 * @param ctx 请求取消与截止时间上下文。
 * @param method HTTP 请求方法。
 * @param path 以斜杠开头的控制接口路径。
 * @param session 当前账号会话。
 * @param body 已编码请求体，可为 nil。
 * @return 业务 data；失败时为 nil。
 * @return 网络或协议错误；上游拒绝可用 errors.As 检查 *APIError，会话失效可用 errors.Is 检查 ErrSessionExpired；成功为 nil。
 * @note 会发起 HTTP 请求，副作用由目标接口和请求方法决定。
 */
func (s *Service) tokenRequest(ctx context.Context, method, path, session string, body []byte) (json.RawMessage, error) {
	headers := make(http.Header)
	ApplyHeaders(headers, session)
	return s.request(ctx, method, strings.TrimRight(s.BaseURL, "/")+path, body, headers, true)
}

/**
 * @brief 限定归一后的中国大陆手机号为 11 位且使用已知移动号码段。
 */
var phonePattern = regexp.MustCompile(`^1[3-9][0-9]{9}$`)

/**
 * @brief 限定短信验证码为六位 ASCII 数字。
 */
var codePattern = regexp.MustCompile(`^[0-9]{6}$`)

/**
 * @brief 归一中国大陆手机号，确保挑战绑定与限流使用相同身份。
 * @param phone 允许空白、连字符及 +86 或 86 国家码的手机号。
 * @return 归一后的 11 位手机号；校验失败时为空字符串。
 * @return 号码段或长度无效时返回错误；成功为 nil。
 */
func NormalizePhone(phone string) (string, error) {
	phone = strings.Join(strings.Fields(phone), "")
	phone = strings.ReplaceAll(phone, "-", "")
	phone = strings.TrimPrefix(phone, "+86")
	if len(phone) == 13 {
		phone = strings.TrimPrefix(phone, "86")
	}
	if !phonePattern.MatchString(phone) {
		return "", errors.New("请输入有效的中国大陆手机号")
	}
	return phone, nil
}

/**
 * @brief 按账号服务契约签名并发送短信或登录 POST。
 * @param ctx 请求取消与截止时间上下文。
 * @param path 参与签名的账号接口路径。
 * @param param 可编码为 JSON 的账号业务参数。
 * @return 账号业务 data；失败时为 nil。
 * @return 编码、请求或业务校验失败时返回错误；上游拒绝可用 errors.As 检查 *APIError；成功为 nil。
 * @note 每次调用生成新的追踪标识和 nonce，并执行一次不可自动重放的 POST。
 * @details 使用正文 MD5、GMT 日期、随机 nonce 和九段规范串生成 HMAC-SHA1 签名。
 */
func (s *Service) signedPost(ctx context.Context, path string, param any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"base":  map[string]string{"appid": applicationID, "modelid": "Web", "version": "1.0.0", "devid": "web", "ua": ClientUA, "traceid": strings.ReplaceAll(uuid.NewString(), "-", "")},
		"param": param,
	})
	if err != nil {
		return nil, err
	}
	digest := md5.Sum(body)
	md5Base64 := base64.StdEncoding.EncodeToString(digest[:])
	date, nonce := time.Now().UTC().Format(http.TimeFormat), uuid.NewString()
	canonical := strings.Join([]string{"POST", path, "", md5Base64, "application/json", date, nonce, "", ""}, "\n")
	mac := hmac.New(sha1.New, []byte(signKeySecret))
	_, _ = mac.Write([]byte(canonical))
	headers := make(http.Header)
	headers.Set("Authorization", "account "+signKeyID+":"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	headers.Set("Date", date)
	headers.Set("Nonce", nonce)
	headers.Set("Content-MD5", md5Base64)
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", ClientUA)
	return s.request(ctx, http.MethodPost, strings.TrimRight(s.AccountURL, "/")+path, body, headers, true)
}

/**
 * @brief 向已归一的大陆手机号发送短信并取得上游挑战标识。
 * @param ctx 请求取消与截止时间上下文。
 * @param phone 可带国家码及分隔符的手机号。
 * @return 非空的短信挑战 msgid/msgId；失败时为空字符串。
 * @return 输入无效、上游失败或响应缺少挑战时返回错误；上游拒绝可用 errors.As 检查 *APIError；成功为 nil。
 * @note 真实发送短信；请求的验证码有效期为 300 秒，挑战应仅保存在服务端。
 */
func (s *Service) SendCode(ctx context.Context, phone string) (string, error) {
	phone, err := NormalizePhone(phone)
	if err != nil {
		return "", err
	}
	raw, err := s.signedPost(ctx, "/login/phone/sendMsgCode", map[string]any{"ccode": "86", "phone": phone, "expire": 300})
	if err != nil {
		return "", err
	}
	var result struct {
		MsgID      string `json:"msgid"` ///< 上游小写命名的短信挑战标识。
		MsgIDCamel string `json:"msgId"` ///< 上游驼峰命名的短信挑战标识，仅在 msgid 为空时使用。
	}
	if json.Unmarshal(raw, &result) != nil {
		return "", errors.New("invalid Loomy SMS response")
	}
	if result.MsgID == "" {
		result.MsgID = result.MsgIDCamel
	}
	if result.MsgID == "" {
		return "", errors.New("loomy SMS response has no challenge")
	}
	return result.MsgID, nil
}

/**
 * @brief 消费手机号绑定的短信挑战，取得经上游确认的会话与身份。
 * @param ctx 请求取消与截止时间上下文。
 * @param phone 挑战对应的手机号。
 * @param code 六位短信验证码。
 * @param msgID 发送验证码时上游返回的非空挑战标识。
 * @return 同时包含会话及用户 ID 的私有凭证；失败时为 nil。
 * @return 输入、协议、网络或身份缺失时返回错误；上游拒绝可用 errors.As 检查 *APIError；成功为 nil。
 * @note 调用账号校验接口；请求 expire 不用于推导本地凭证过期时间。
 */
func (s *Service) Login(ctx context.Context, phone, code, msgID string) (*Credential, error) {
	phone, err := NormalizePhone(phone)
	if err != nil {
		return nil, err
	}
	if !codePattern.MatchString(code) || msgID == "" {
		return nil, errors.New("请输入六位短信验证码")
	}
	raw, err := s.signedPost(ctx, "/login/phone/checkCode", map[string]any{"ccode": "86", "phone": phone, "mcode": code, "msgid": msgID, "expire": 1209600})
	if err != nil {
		return nil, err
	}
	var result struct {
		Session string `json:"session"` ///< 短信校验成功返回的账号会话。
		UserID  string `json:"userid"`  ///< 账号服务确认的用户标识，禁止由客户端自行声明。
	}
	if json.Unmarshal(raw, &result) != nil || strings.TrimSpace(result.Session) == "" || strings.TrimSpace(result.UserID) == "" {
		return nil, errors.New("loomy login response has no verified identity")
	}
	c := &Credential{Session: result.Session, UserID: result.UserID}
	encoded, err := c.JSON()
	if err != nil {
		return nil, err
	}
	return ParseCredential([]byte(encoded))
}

/**
 * @brief 取得可用于聊天的模型目录并保持上游顺序去重。
 * @param ctx 请求取消与截止时间上下文。
 * @param session 当前账号会话。
 * @return 保持上游顺序的非空聊天模型 ID 列表；失败时为 nil。
 * @return 请求失败、格式无效或没有聊天模型时返回错误；上游拒绝可用 errors.As 检查 *APIError，会话失效可用 errors.Is 检查 ErrSessionExpired；成功为 nil。
 * @details 兼容原始数组与 OpenAI data 包装，允许没有业务码；type 为空或 chat 的条目才保留。
 */
func (s *Service) FetchModels(ctx context.Context, session string) ([]string, error) {
	h := make(http.Header)
	ApplyHeaders(h, session)
	raw, err := s.request(ctx, http.MethodGet, strings.TrimRight(s.BaseURL, "/")+"/api/v1/models", nil, h, false)
	if err != nil {
		return nil, err
	}
	/**
	 * @brief 承载聊天模型标识与能力分类，排除非聊天模型。
	 */
	type entry struct {
		ID   string `json:"id"`   ///< 上游模型标识，去除首尾空白后用于去重。
		Type string `json:"type"` ///< 模型用途；为空或 chat 时允许作为聊天模型。
	}
	var entries []entry
	if len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '[' {
		err = json.Unmarshal(raw, &entries)
	} else {
		var result struct {
			Data []entry `json:"data"` ///< 上游业务数据；根据接口约定解包 JSON 或模型列表。
		}
		err = json.Unmarshal(raw, &result)
		entries = result.Data
	}
	if err != nil {
		return nil, errors.New("invalid Loomy models response")
	}
	names := make([]string, 0, len(entries))
	seen := make(map[string]bool)
	for _, e := range entries {
		id := strings.TrimSpace(e.ID)
		if id != "" && (e.Type == "" || e.Type == "chat") && !seen[id] {
			names = append(names, id)
			seen[id] = true
		}
	}
	if len(names) == 0 {
		return nil, errors.New("loomy returned no chat models")
	}
	return names, nil
}

/**
 * @brief 保存 Loomy 原生积分余额及当前业务日的额度推导结果。
 * @details 指针 nil 表示上游未提供或尚不能确定，不能替换成零；积分不是 token 数。
 */
type Balance struct {
	Permanent      *float64 `json:"balance"`                  ///< 永久积分余额，nil 表示未知。
	Daily          *float64 `json:"dailyBalance"`             ///< 当日剩余积分，nil 表示未知。
	Available      *float64 `json:"availableBalance"`         ///< 上游提供的可用总积分，nil 表示未知。
	DailyQuota     *float64 `json:"dailyQuota,omitempty"`     ///< 签到实测或完整当日流水推导的每日总积分；nil 表示未知。
	DailyConsumed  *float64 `json:"dailyConsumed,omitempty"`  ///< 与 DailyCycleDate 对应的当日已用积分，nil 表示未知。
	DailyCycleDate string   `json:"dailyCycleDate,omitempty"` ///< 每日总额及已用对应的业务日期，格式 YYYY-MM-DD。
}

/**
 * @brief 只读查询 Loomy 原生积分余额，保留缺失字段。
 * @param ctx 请求取消与截止时间上下文。
 * @param session 当前账号会话。
 * @return 至少含一项已知余额的结果；失败时为 nil，未知余额字段保持 nil。
 * @return 请求失败、格式无效或三项余额均缺失时返回错误；上游拒绝可用 errors.As 检查 *APIError，会话失效可用 errors.Is 检查 ErrSessionExpired；成功为 nil。
 */
func (s *Service) FetchBalance(ctx context.Context, session string) (*Balance, error) {
	raw, err := s.tokenRequest(ctx, http.MethodGet, "/api/v1/points/balance", session, nil)
	if err != nil {
		return nil, err
	}
	var balance Balance
	if json.Unmarshal(raw, &balance) != nil || (balance.Permanent == nil && balance.Daily == nil && balance.Available == nil) {
		return nil, errors.New("invalid Loomy balance response")
	}
	return &balance, nil
}

/**
 * @brief 从完整有界流水汇总指定业务日的每日积分消耗。
 * @param ctx 分页请求的取消与截止时间上下文。
 * @param session 当前账号会话。
 * @param cycleDate 要统计的业务日期，按 YYYY-MM-DD 与上游精确匹配。
 * @return 指定业务日的每日消费积分；无消费或失败时为 0。
 * @return 存在该业务日的 daily 流水时为 true（不限于消费），无匹配记录或失败时为 false。
 * @return 请求失败、分页不完整或数据无效时返回错误；上游拒绝可用 errors.As 检查 *APIError，会话失效可用 errors.Is 检查 ErrSessionExpired；成功为 nil。
 * @details 每页 100 条，最多 6 页；只累加 daily 来源的 debit，拒绝缺失、负数及非有限消费值，避免用部分流水伪造额度。
 */
func (s *Service) FetchDailyConsumed(ctx context.Context, session, cycleDate string) (float64, bool, error) {
	/**
	 * @brief 限定每日流水每页 100 条、最多六页；不允许使用截断结果推导额度。
	 */
	const pageSize, maxPages = 100, 6
	var consumed float64
	var count int
	foundCycle := false
	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("/api/v1/points/records?pageNo=%d&pageSize=%d&recordType=all", page, pageSize)
		raw, err := s.tokenRequest(ctx, http.MethodGet, path, session, nil)
		if err != nil {
			return 0, false, err
		}
		var data struct {
			Total *int `json:"total"` ///< 流水总条数，必须存在且非负，用于判断分页是否完整。
			List  *[]struct {
				Direction     string   `json:"direction"`      ///< 积分变动方向，只统计 debit 消费。
				ConsumeSource string   `json:"consumeSource"`  ///< 积分池来源，只统计 daily。
				CycleDate     string   `json:"dailyCycleDate"` ///< 记录所属的业务日期，与目标日期精确匹配。
				Points        *float64 `json:"pointsActual"`   ///< 实际消费积分；消费记录必须提供有限非负数值。
			} `json:"list"` ///< 当前页流水；nil 表示缺失，空切片仅在总条数已满足时完整。
		}
		if json.Unmarshal(raw, &data) != nil || data.List == nil || data.Total == nil || *data.Total < 0 {
			return 0, false, errors.New("invalid Loomy ledger response")
		}
		for _, row := range *data.List {
			if row.ConsumeSource != "daily" || row.CycleDate != cycleDate {
				continue
			}
			foundCycle = true
			if row.Direction != "debit" {
				continue
			}
			if row.Points == nil || *row.Points < 0 || math.IsNaN(*row.Points) || math.IsInf(*row.Points, 0) {
				return 0, false, errors.New("invalid Loomy daily consumption")
			}
			consumed += *row.Points
		}
		count += len(*data.List)
		if count >= *data.Total {
			return consumed, foundCycle, nil
		}
		if len(*data.List) == 0 {
			break
		}
	}
	return 0, false, errors.New("incomplete Loomy daily ledger")
}

/**
 * @brief 表示上游确认的每日签到结果及业务日期。
 */
type Checkin struct {
	AlreadyProcessed bool     `json:"alreadyProcessed"`     ///< true 表示该业务日已领取，仍属于成功确认。
	Day              string   `json:"dailyCycleDate"`       ///< 上游业务日，格式 YYYY-MM-DD；空值由调用层按北京时间补齐。
	DailyQuota       *float64 `json:"dailyQuota,omitempty"` ///< 上游本次确认的每日积分总额；nil 表示未提供。
}

/**
 * @brief 执行每日签到并确认新领取或当天已处理。
 * @param ctx 请求取消与截止时间上下文。
 * @param session 当前账号会话。
 * @return 签到确认结果；失败时为 nil，已签到由 AlreadyProcessed 表示。
 * @return 请求失败、缺少已处理标志或业务日期无效时返回错误；上游拒绝可用 errors.As 检查 *APIError，会话失效可用 errors.Is 检查 ErrSessionExpired；成功为 nil。
 * @note 调用有副作用的积分领取 POST；不在本层持久化日期或代替调用方安排重试。
 */
func (s *Service) DailyCheckin(ctx context.Context, session string) (*Checkin, error) {
	raw, err := s.tokenRequest(ctx, http.MethodPost, "/api/v1/points/first-login", session, []byte(`{}`))
	if err != nil {
		return nil, err
	}
	var result struct {
		Already    *bool    `json:"alreadyProcessed"` ///< 是否当日已处理；指针用于区分 false 与协议字段缺失。
		Day        string   `json:"dailyCycleDate"`   ///< 上游业务日，格式 YYYY-MM-DD；空值由调用层按北京时间补齐。
		DailyQuota *float64 `json:"dailyQuota"`       ///< 上游本次确认的每日积分总额；nil 表示未提供。
	}
	if json.Unmarshal(raw, &result) != nil || result.Already == nil {
		return nil, errors.New("invalid Loomy check-in response")
	}
	if result.Day != "" {
		if _, err := time.Parse("2006-01-02", result.Day); err != nil {
			return nil, errors.New("invalid Loomy check-in date")
		}
	}
	return &Checkin{AlreadyProcessed: *result.Already, Day: result.Day, DailyQuota: result.DailyQuota}, nil
}

/**
 * @brief 提供已经核对的八个新手任务标识。
 * @return 返回新的任务切片，调用方修改不会污染后续调用。
 */
func OnboardingKeys() []string {
	return []string{"first_message", "pick_skill", "generate_ppt", "set_schedule", "install_skill", "configure_remote", "create_soul", "share_soul"}
}

/**
 * @brief 读取并验证全部已知新手任务的完成状态。
 * @param ctx 请求取消与截止时间上下文。
 * @param session 当前账号会话。
 * @return 八个已知任务的状态映射，false 表示未完成；失败时为 nil。
 * @return 请求失败、已知任务缺失、null 或格式错误时返回错误；上游拒绝可用 errors.As 检查 *APIError，会话失效可用 errors.Is 检查 ErrSessionExpired；成功为 nil。
 * @details 完整性校验防止把部分响应误记为账号级任务全部完成。
 */
func (s *Service) FetchTasks(ctx context.Context, session string) (map[string]bool, error) {
	raw, err := s.tokenRequest(ctx, http.MethodGet, "/api/v1/onboarding/tasks", session, nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Tasks map[string]*bool `json:"tasks"` ///< 任务状态映射；已知任务的值必须存在且非 null。
	}
	if json.Unmarshal(raw, &result) != nil {
		return nil, errors.New("invalid Loomy tasks response")
	}
	tasks := make(map[string]bool)
	for _, key := range OnboardingKeys() {
		value := result.Tasks[key]
		if value == nil {
			return nil, errors.New("incomplete Loomy tasks response")
		}
		tasks[key] = *value
	}
	return tasks, nil
}

/**
 * @brief 向上游报告一个已知新手任务的完成请求。
 * @param ctx 请求取消与截止时间上下文。
 * @param session 当前账号会话。
 * @param key OnboardingKeys 中的任务标识。
 * @return 上游返回有效完成确认时为 nil；未知任务、请求失败或确认格式无效时返回错误；上游拒绝可用 errors.As 检查 *APIError，会话失效可用 errors.Is 检查 ErrSessionExpired。
 * @note 可能领取任务积分；调用方必须重新读取任务状态后才能持久化全部完成标记。
 */
func (s *Service) CompleteTask(ctx context.Context, session, key string) error {
	if !slices.Contains(OnboardingKeys(), key) {
		return errors.New("unknown Loomy task")
	}
	body, err := json.Marshal(map[string]string{"key": key})
	if err != nil {
		return err
	}
	raw, err := s.tokenRequest(ctx, http.MethodPost, "/api/v1/onboarding/tasks/complete", session, body)
	if err != nil {
		return err
	}
	var result struct {
		Already *bool `json:"alreadyCompleted"` ///< 是否已完成该新手任务；指针用于拒绝缺失的上游确认。
	}
	if json.Unmarshal(raw, &result) != nil || result.Already == nil {
		return errors.New("invalid Loomy task completion response")
	}
	return nil
}
