package zaiauth

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

/**
 * @brief ZCode 登录所属站点；空值按国际版解释。
 */
type Site string

/**
 * @brief ZCode 支持的账号站点。
 */
const (
	SiteZAI      Site = "zai"      ///< 国际版。
	SiteBigModel Site = "bigmodel" ///< 国内智谱。
)

/**
 * @brief 归一站点，拒绝未知值，防止凭证被发往错误的服务。
 * @param site 用户输入或持久化的站点。
 * @return 已归一的站点，空值为 zai。
 * @return 非 zai/bigmodel 时返回错误。
 */
func NormalizeSite(site Site) (Site, error) {
	site = Site(strings.ToLower(strings.TrimSpace(string(site))))
	switch site {
	case "", SiteZAI:
		return SiteZAI, nil
	case SiteBigModel:
		return SiteBigModel, nil
	default:
		return "", fmt.Errorf("unsupported ZCode site %q", site)
	}
}

/**
 * @brief 两站的业务与推理端点分开配置；国内业务域名不能用于订阅查询。
 */
type siteConfig struct {
	biz       string ///< 账号、项目及 Key 管理。
	api       string ///< 付费订阅、额度及模型查询。
	anthropic string ///< 付费推理的站点直连地址。
	catalog   string ///< models.dev 的 Coding Plan 目录。
	prompt    string ///< 体验提示词中的 provider。
	label     string ///< 渠道默认名称前缀。
}

/**
 * @brief 返回已验证站点的固定配置，零值使用国际版。
 * @return 站点配置。
 */
func (site Site) config() siteConfig {
	if site == SiteBigModel {
		return siteConfig{
			biz: "https://bigmodel.cn", api: "https://open.bigmodel.cn",
			anthropic: "https://open.bigmodel.cn/api/anthropic",
			catalog:   "zhipuai-coding-plan", prompt: "bigmodel-api", label: "BigModel",
		}
	}
	return siteConfig{biz: BizBaseURL, api: BizBaseURL, anthropic: CodingPlanAPIBaseURL,
		catalog: CommunityCatalogProvider, prompt: "zai-api", label: "Z.ai"}
}

/**
 * @brief 返回站点的付费直连地址，付费请求不经过 ZCode 体验服务。
 * @return Anthropic 基础地址，不含 messages 路径。
 */
func (site Site) CodingPlanBaseURL() string { return site.config().anthropic }

/**
 * @brief 将旧官方付费端点归一为站点直连地址，自定义地址及体验入口保持原样。
 * @param raw 已保存或待保存的 URL；可含完整 Messages 路径及精确地址标记。
 * @return 归一后的地址，保留查询参数、尾斜杠及精确地址标记；未命中时返回原值。
 */
func NormalizeCodingPlanURL(raw string) string {
	exact := strings.HasSuffix(raw, "#")
	u, err := url.Parse(strings.TrimSuffix(raw, "#"))
	if err != nil || u.User != nil || !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Host, "zcode.z.ai") {
		return raw
	}
	path := strings.TrimSuffix(u.Path, "/")
	suffix := ""
	if basePath, found := strings.CutSuffix(path, "/v1/messages"); found {
		path = basePath
		suffix = "/v1/messages"
	}
	var base string
	switch path {
	case "/api/v1/ultra/anthropic":
		base = SiteBigModel.CodingPlanBaseURL()
	case "/api/v1/ultra-zai/anthropic":
		base = SiteZAI.CodingPlanBaseURL()
	default:
		return raw
	}
	if strings.HasSuffix(u.Path, "/") {
		suffix += "/"
	}
	target, _ := url.Parse(base) // 站点地址是固定的有效 URL。
	u.Scheme, u.Host, u.Path, u.RawPath = target.Scheme, target.Host, target.Path+suffix, ""
	result := u.String()
	if exact {
		result += "#"
	}
	return result
}

/**
 * @brief 返回可区分账号站点的名称。
 * @return 国际版 Z.ai 或国内版 BigModel。
 */
func (site Site) Label() string { return site.config().label }

/**
 * @brief 创建独立的站点服务副本，统一登录及后续查询的端点和出口。
 * @param site 已在请求或凭证边界验证的站点。
 * @param client 本次出口客户端；nil 时沿用当前客户端。
 * @return 服务副本；同站点保留注入端点，切换站点重设站点专属端点。
 * @note 不修改原服务；ZCode 公共端点及测试时钟始终保留。
 */
func (s *Service) ForSite(site Site, client *http.Client) *Service {
	if s == nil {
		s = NewService(client)
	}
	if site == "" {
		site = SiteZAI
	}
	current := s.Site
	if current == "" {
		current = SiteZAI
	}
	copy := &Service{Client: s.Client, Site: site, OAuthBaseURL: s.OAuthBaseURL,
		BizBaseURL: s.BizBaseURL, SubscriptionBaseURL: s.SubscriptionBaseURL,
		CodingModelsURL: s.CodingModelsURL, ModelsURL: s.ModelsURL,
		CommunityURL:  s.CommunityURL,
		QuotaLimitURL: s.QuotaLimitURL, Now: s.Now}
	if client != nil {
		copy.Client = client
	}
	if current != site {
		cfg := site.config()
		copy.BizBaseURL, copy.SubscriptionBaseURL = cfg.biz, cfg.api
		copy.CodingModelsURL = cfg.api + "/api/coding/paas/v4/models"
		copy.ModelsURL = cfg.api + "/api/paas/v4/models"
		copy.QuotaLimitURL = cfg.api + "/api/monitor/usage/quota/limit"
	}
	return copy
}
