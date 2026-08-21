// Package cursorauth implements the Cursor CLI credential contract:
// the loginDeepControl PKCE flow, Connect JSON control-plane RPCs
// (identity, models, usage), and optional cursor-agent inference.
package cursorauth

import "time"

const (
	// ChannelType is the provider type stored in Cursor credentials.
	ChannelType = "cursor"

	// APIBaseURL is the Cursor control-plane origin used by the CLI.
	APIBaseURL = "https://api2.cursor.sh"
	// WebsiteURL is the Cursor site that hosts loginDeepControl.
	WebsiteURL = "https://cursor.com"

	// ClientVersion is the cursor-agent build ccLoad reports upstream.
	// Control-plane JSON RPCs accept this CLI fingerprint; chat still
	// requires the matching binary on PATH when inference is enabled.
	// Official Docker images pin the same version via CURSOR_AGENT_VERSION
	// in docker/Dockerfile.release and Dockerfile.
	ClientVersion = "2026.08.11-e8db854"
	// ClientType is Cursor's CLI client-type header.
	ClientType = "cli"
	// GhostMode matches the CLI's x-ghost-mode value for unattended calls.
	GhostMode = "true"

	// GetMeRPC is DashboardService.GetMe.
	GetMeRPC = "/aiserver.v1.DashboardService/GetMe"
	// UsageRPC is DashboardService.GetCurrentPeriodUsage.
	UsageRPC = "/aiserver.v1.DashboardService/GetCurrentPeriodUsage"
	// ModelsRPC is AgentService.GetUsableModels.
	ModelsRPC = "/agent.v1.AgentService/GetUsableModels"
	// AuthPollPath is the CLI login poll endpoint.
	AuthPollPath = "/auth/poll"
	// ExchangeAPIKeyPath swaps a Cursor user API key for session tokens.
	ExchangeAPIKeyPath = "/auth/exchange_user_api_key"

	// RequestTimeout bounds one Cursor control-plane request.
	RequestTimeout = 30 * time.Second
	// PollInterval is the CLI's initial login poll cadence.
	PollInterval = time.Second
	// PollTimeout bounds one browser authorization.
	PollTimeout = 5 * time.Minute
	// AgentTimeout bounds one cursor-agent inference.
	AgentTimeout = 180 * time.Second

	maxResponseSize   = 4 << 20
	maxCredentialSize = 1 << 20
	verifierBytes     = 32
)

// DefaultModels is the last-resort lineup used only when GetUsableModels is
// unreachable. Public names omit Cursor's -thinking-* infix; thinking is
// mapped at inference time from the client request.
var DefaultModels = []string{
	"auto",
	"claude-sonnet-5",
	"claude-opus-5",
	"claude-fable-5",
	"gpt-5.3-codex",
	"gpt-5.2",
	"composer-2.5",
	"gemini-3.1-pro",
	"cursor-grok-4.6-high",
}
