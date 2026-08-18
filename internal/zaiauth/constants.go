// Package zaiauth implements the Z.ai Coding Plan (ZCode) credential contract:
// the ZCode CLI OAuth flow, Coding Plan API key resolution, and the ZCode
// client identity replicated by ccLoad when it forwards Coding Plan traffic.
package zaiauth

import "time"

const (
	// ChannelType is the provider type stored in Z.ai credentials.
	ChannelType = "zai"

	// OAuthAPIBaseURL is the ZCode service root that owns the CLI OAuth flow.
	OAuthAPIBaseURL = "https://zcode.z.ai/api/v1"
	// OAuthProvider selects the Z.ai identity provider inside the CLI OAuth flow.
	OAuthProvider = "zai"

	// BizBaseURL is the Z.ai business API root used to mint Coding Plan API keys.
	BizBaseURL = "https://api.z.ai"
	// CodingPlanAPIBaseURL is the public Coding Plan Anthropic-compatible origin.
	// ZCode never calls it directly: it rewrites the endpoint through
	// AgentConfigsURL, so this value is only the routing lookup key.
	CodingPlanAPIBaseURL = "https://api.z.ai/api/anthropic"
	// CodingPlanProxyBaseURL is the ZCode-routed Coding Plan endpoint. Requests
	// billed through it are the ones ZCode itself issues.
	CodingPlanProxyBaseURL = "https://zcode.z.ai/api/v1/ultra-zai/anthropic"
	// AgentConfigsURL publishes the current ZCode endpoint routing table.
	AgentConfigsURL = "https://zcode.z.ai/api/v1/agent/configs"
	// CodingModelsURL is the Coding Plan model catalog. It is the plan's own
	// endpoint, so it lists models that ship to Coding Plan before the general
	// API exposes them.
	CodingModelsURL = "https://api.z.ai/api/coding/paas/v4/models"
	// ModelsURL is the general API catalog, used as a fallback and to validate
	// a key without spending quota.
	ModelsURL = "https://api.z.ai/api/paas/v4/models"
	// QuotaLimitURL reports the Coding Plan allowance windows. It backs Z.ai's
	// own subscription panel and is undocumented, but takes the Coding Plan key.
	QuotaLimitURL = "https://api.z.ai/api/monitor/usage/quota/limit"

	// CommunityCatalogURL is models.dev, the third-party catalog ccLoad already
	// syncs for pricing. Its Coding Plan provider tracks the plan lineup without
	// an account key, which makes it the keyless fallback for model discovery.
	CommunityCatalogURL = "https://models.dev/api.json"
	// CommunityCatalogProvider is the models.dev provider that mirrors the Z.ai
	// Coding Plan (api.z.ai/api/coding/paas/v4).
	CommunityCatalogProvider = "zai-coding-plan"

	// AppVersion is the ZCode client version ccLoad reports upstream.
	AppVersion = "3.7.7"
	// SourceTitle is ZCode's X-Title value.
	SourceTitle = "Z Code@cli"
	// AgentHeaderValue is ZCode's X-ZCode-Agent value.
	AgentHeaderValue = "glm"
	// RefererValue is ZCode's HTTP-Referer value.
	RefererValue = "https://zcode.z.ai"

	// codingPlanAPIKeyName is the API key ZCode creates inside the account.
	codingPlanAPIKeyName = "zcode-api-key"
	// defaultOrganizationName / defaultProjectName are ZCode's preferred
	// organization and project when an account owns several.
	defaultOrganizationName = "默认机构"
	defaultProjectName      = "默认项目"

	// RequestTimeout bounds one Z.ai control-plane request.
	RequestTimeout = 60 * time.Second
	// PollInterval is the floor ZCode applies to the CLI OAuth poll cadence.
	PollInterval = time.Second
	// PollTimeout bounds one browser authorization.
	PollTimeout = 5 * time.Minute

	maxResponseSize         = 1 << 20
	maxCommunityCatalogSize = 24 << 20
	maxCredentialSize       = 1 << 20
	pollTokenBytes          = 32
)

// DefaultModels is the last-resort lineup, used only when both the account
// catalog and models.dev are unreachable. It is never the source of truth: the
// Coding Plan lineup changes without a ccLoad release (glm-5.3 shipped to the
// plan two months before the general API listed it).
var DefaultModels = []string{"glm-5.3", "glm-5.2", "glm-5.2-highspeed", "glm-5-turbo", "glm-5.1", "glm-4.7"}
