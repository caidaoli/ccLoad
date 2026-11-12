package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/qoderauth"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

/**
 * @brief 与访问令牌绑定的账号模型目录和最近一次目录恢复时间
 */
type qoderModelCatalog struct {
	catalog          *qoderauth.Catalog
	expires          time.Time
	credentialDigest [32]byte
	refreshedAt      time.Time
}

/**
 * @brief 请求上下文中的工具声明，仅用于解析该次响应
 */
type qoderToolsContextKey struct{}

/**
 * @brief 获取目录并对明确认证错误或免费视图执行受限刷新
 * @param ctx 请求上下文
 * @param cfg 渠道配置
 * @param c 当前凭证，成功刷新时原地更新
 * @param force 是否绕过目录缓存
 * @return 账号目录，失败时为 nil
 * @return 网络、目录或认证刷新错误
 */
func (s *Server) qoderCatalog(ctx context.Context, cfg *model.Config, c *qoderauth.Credential, force bool) (*qoderauth.Catalog, error) {
	key := fmt.Sprintf("%d/%s/%s", cfg.ID, c.Region, c.UserID)
	digest := sha256.Sum256([]byte(c.AccessToken))
	var cached qoderModelCatalog
	if value, ok := s.qoderModels.Load(key); ok {
		cached = value.(qoderModelCatalog)
		if !force && cached.credentialDigest == digest && time.Now().Before(cached.expires) {
			return cached.catalog, nil
		}
	}
	service := *s.qoderService
	service.Client = s.getClientForChannel(cfg)
	catalog, err := service.FetchCatalog(ctx, c)
	var apiErr *qoderauth.APIError
	unauthorized := errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized
	freeView := cfg.ID > 0 && err == nil && catalog.FreeOnly && (c.RefreshToken != "" || c.PAT != "") && time.Since(cached.refreshedAt) >= 5*time.Minute
	if freeView {
		usage, usageErr := service.FetchUsage(ctx, c)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if usageErr == nil && usage.Remaining <= 0 {
			freeView = false
		}
	}
	if cfg.ID > 0 && (unauthorized || freeView) {
		refreshed, refreshErr := s.qoderCredentials.credential(ctx, cfg, true, c.AccessToken)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		cached.refreshedAt = time.Now()
		if refreshErr != nil {
			if unauthorized {
				return nil, refreshErr
			}
		} else {
			*c = *refreshed
			catalog, err = service.FetchCatalog(ctx, c)
		}
	}
	if err != nil {
		return nil, err
	}
	s.qoderModels.Store(key, qoderModelCatalog{catalog: catalog, expires: time.Now().Add(time.Hour), credentialDigest: sha256.Sum256([]byte(c.AccessToken)), refreshedAt: cached.refreshedAt})
	return catalog, nil
}

func (s *Server) fetchQoderOAuthModels(ctx context.Context, cfg *model.Config, override string) (*FetchModelsResponse, error) {
	if override != "" && util.NormalizeProtocol(override) != util.ProtocolOpenAI {
		return nil, errors.New("qoder model discovery requires openai protocol")
	}
	c, err := qoderauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, err
	}
	if cfg.ID > 0 {
		c, err = s.qoderCredentials.credential(ctx, cfg, false, "")
		if err != nil {
			return nil, err
		}
	}
	catalog, err := s.qoderCatalog(ctx, cfg, c, true)
	if err != nil {
		return nil, err
	}
	entries := make([]model.ModelEntry, 0, len(catalog.Models))
	for _, item := range catalog.Models {
		entries = append(entries, model.ModelEntry{Model: item.ID})
	}
	return &FetchModelsResponse{Models: entries, Protocol: util.ProtocolOpenAI, Source: "api"}, nil
}

/**
 * @brief 在通用请求规则之后解析模型和会话，并构造最终签名请求
 * @param cfg 渠道配置
 * @param req 可重放的逻辑 Chat 请求
 * @return 独立的 Qoder 出站请求，失败时为 nil
 * @return 目录错误、读取错误或不可转换请求错误
 */
func (s *Server) prepareQoderWire(cfg *model.Config, req *http.Request) (*http.Request, error) {
	c, err := qoderauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, err
	}
	if req.GetBody == nil {
		return nil, errors.New("qoder requires a replayable request body")
	}
	source, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()
	body, err := io.ReadAll(source)
	if err != nil {
		return nil, err
	}
	// Apply the shared system-prompt word list before encoding and COSY signing.
	body, err = obfuscateChatSystemMessages(body, s.antigravityPromptMatcher)
	if err != nil {
		return nil, &protocol.RequestTranslationError{Err: err}
	}
	catalog, err := s.qoderCatalog(req.Context(), cfg, c, false)
	if err != nil {
		return nil, err
	}
	name := gjson.GetBytes(body, "model").String()
	item, enabled := qoderCatalogModel(catalog, name)
	if item == nil || !enabled {
		catalog, err = s.qoderCatalog(req.Context(), cfg, c, true)
		if err != nil {
			return nil, err
		}
		item, enabled = qoderCatalogModel(catalog, name)
	}
	if item != nil && !enabled {
		service := *s.qoderService
		service.Client = s.getClientForChannel(cfg)
		usage, usageErr := service.FetchUsage(req.Context(), c)
		if usageErr == nil && usage.Remaining <= 0 {
			return nil, &qoderauth.APIError{StatusCode: http.StatusTooManyRequests, Type: "insufficient_quota", Code: "insufficient_quota", Message: fmt.Sprintf("Qoder credits are used up and model %q is disabled", name)}
		}
	}
	if item == nil || !enabled {
		return nil, &protocol.RequestTranslationError{Err: fmt.Errorf("qoder model %q is absent or disabled in this account's catalog", name)}
	}
	session := responsesExecutionSessionID(req.Header)
	if session == "" {
		for _, key := range []string{"X-Deepseek-Harness-Session-Id", "X-Claude-Code-Session-Id", "Session_id", "X-Session-Id"} {
			if session = strings.TrimSpace(req.Header.Get(key)); session != "" {
				break
			}
		}
	}
	if session != "" {
		body, err = sjson.SetBytes(body, "session_id", session)
		if err != nil {
			return nil, err
		}
	}
	var tools []map[string]any
	if raw := gjson.GetBytes(body, "tools"); raw.Exists() && raw.Type != gjson.Null {
		if err := json.Unmarshal([]byte(raw.Raw), &tools); err != nil {
			return nil, &protocol.RequestTranslationError{Err: err}
		}
	}
	if gjson.GetBytes(body, "tool_choice").String() == "none" {
		tools = nil
	}
	service := *s.qoderService
	service.Client = s.getClientForChannel(cfg)
	ctx := context.WithValue(req.Context(), qoderToolsContextKey{}, tools)
	signed, buildErr := service.BuildChatRequest(ctx, c, body, *item)
	if buildErr != nil {
		return nil, &protocol.RequestTranslationError{Err: buildErr}
	}
	signedHeaders := signed.Header.Clone()
	applyHeaderRules(signed.Header, cfg.HeaderRules())
	for key, values := range signedHeaders {
		signed.Header[key] = values
	}
	return signed, nil
}

/**
 * @brief 优先按原生路由 key、其次按历史公开名称定位目录条目
 * @param catalog 上游完整目录
 * @param name 请求模型名
 * @return 匹配条目，未知模型为 nil
 * @return 条目是否启用
 */
func qoderCatalogModel(catalog *qoderauth.Catalog, name string) (*qoderauth.Model, bool) {
	for _, keyed := range []bool{true, false} {
		for _, group := range []struct {
			models  []qoderauth.Model
			enabled bool
		}{{catalog.Models, true}, {catalog.Disabled, false}} {
			for i := range group.models {
				item := &group.models[i]
				if keyed && item.Key == name || !keyed && strings.EqualFold(item.ID, name) {
					return item, group.enabled
				}
			}
		}
	}
	return nil, false
}

func (s *Server) tryQoderOAuthChannel(ctx context.Context, cfg *model.Config, reqCtx *proxyRequestContext, w http.ResponseWriter) (*proxyResult, error) {
	return s.tryOAuthChannel(ctx, cfg, reqCtx, w, "Qoder", true, func(force bool, rejected string) (*model.Config, string, error) {
		c, err := s.qoderCredentials.credential(ctx, cfg, force, rejected)
		if c == nil {
			return cfg, "", err
		}
		runtime := cfg.Clone()
		runtime.OAuthCredential, _ = c.JSON()
		return runtime, c.AccessToken, err
	}, func(result *proxyResult) bool {
		return result != nil && !result.succeeded && result.status == http.StatusUnauthorized
	})
}

/**
 * @brief 将原生响应规范化为 Chat SSE 或结构化错误
 * @param resp 待原地更新的 HTTP 响应
 * @param tools 本次请求声明的工具
 * @return 读取或编码响应失败时的错误
 */
func normalizeQoderResponse(resp *http.Response, tools []map[string]any) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		resp.Body = qoderauth.NormalizeStream(resp.Body, tools)
		resp.Header.Set("Content-Type", "text/event-stream")
	} else {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if len(raw) > 1<<20 {
			return errors.New("qoder error response exceeds limit")
		}
		failure := qoderauth.ClassifyError(resp.StatusCode, raw)
		if failure.RetryAfter > 0 {
			resp.Header.Set("Retry-After", strconv.Itoa(failure.RetryAfter))
		}
		encoded, err := json.Marshal(map[string]any{"error": failure, "status": failure.StatusCode})
		if err != nil {
			return err
		}
		resp.StatusCode = failure.StatusCode
		resp.Body = io.NopCloser(bytes.NewReader(encoded))
		resp.Header.Set("Content-Type", "application/json")
	}
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return nil
}

func isQoderChatRequest(cfg *model.Config, upstream protocol.Protocol) bool {
	return cfg != nil && cfg.UsesQoderOAuth() && upstream == protocol.OpenAI
}

// Qoder's provider protocol always streams; aggregation serves non-stream clients.
func (s *Server) handleQoderNonStream(reqCtx *requestContext, resp *http.Response, headers http.Header, w http.ResponseWriter, stats *streamReadStats) (*fwResult, float64, error) {
	return s.handleChatNonStream(reqCtx, resp, headers, w, stats, nil, "Qoder")
}

/**
 * @brief 对上游明确给出的输出上限执行一次同渠道请求修复
 * @param upstreamProtocol 本次上游协议
 * @param cfg 当前渠道
 * @param plan 实际逻辑请求体
 * @param res 尚未提交的转发结果
 * @return 修复后的请求体；不适用时为 nil
 * @return 公共重试循环的去重策略名
 * @return 是否需要重试
 */
func qoderRetryBodyForMaxTokens(upstreamProtocol protocol.Protocol, cfg *model.Config, plan protocol.TransformPlan, res *fwResult) ([]byte, string, bool) {
	if upstreamProtocol != protocol.OpenAI || cfg == nil || !cfg.UsesQoderOAuth() || res == nil || res.ResponseCommitted {
		return nil, "", false
	}
	payload, status := forwardResultErrorPayload(res)
	if status != http.StatusBadRequest || gjson.GetBytes(payload, "error.code").String() != "max_tokens_exceeded" {
		return nil, "", false
	}
	limit := gjson.GetBytes(payload, "error.max_tokens").Int()
	if limit <= 0 {
		return nil, "", false
	}
	key := "max_tokens"
	requested := int64(32000)
	for _, candidate := range []string{"max_completion_tokens", "max_tokens"} {
		var value int64
		if json.Unmarshal([]byte(gjson.GetBytes(plan.TranslatedBody, candidate).Raw), &value) == nil && value > 0 {
			key, requested = candidate, value
			break
		}
	}
	if requested <= limit {
		return nil, "", false
	}
	body, err := sjson.SetBytes(plan.TranslatedBody, key, limit)
	return body, "clamp_qoder_max_tokens", err == nil
}
