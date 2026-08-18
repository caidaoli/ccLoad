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
	// ModelsURL validates a Coding Plan API key without consuming quota.
	ModelsURL = "https://api.z.ai/api/paas/v4/models"

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

	maxResponseSize   = 1 << 20
	maxCredentialSize = 1 << 20
	pollTokenBytes    = 32
)

// DefaultModels are the Coding Plan models ZCode configures out of the box.
var DefaultModels = []string{"glm-5.1", "glm-4.7"}
