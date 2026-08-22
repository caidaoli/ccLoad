// Package cursorauth implements the Cursor credential contract:
// User API Key exchange, Connect JSON control-plane RPCs
// (identity, models, usage), and cursor-sdk-bridge inference.
package cursorauth

import "time"

const (
	// ChannelType is the provider type stored in Cursor credentials.
	ChannelType = "cursor"

	// APIBaseURL is the Cursor control-plane origin used by the CLI.
	APIBaseURL = "https://api2.cursor.sh"
	// ClientVersion is the CLI fingerprint accepted by control-plane JSON RPCs.
	ClientVersion = "2026.08.11-e8db854"
	// ClientType is Cursor's CLI client-type header.
	ClientType = "cli"
	// GhostMode matches the CLI's x-ghost-mode value for unattended calls.
	GhostMode = "true"

	// GetMeRPC is DashboardService.GetMe.
	GetMeRPC = "/aiserver.v1.DashboardService/GetMe"
	// UsageRPC is DashboardService.GetCurrentPeriodUsage.
	UsageRPC = "/aiserver.v1.DashboardService/GetCurrentPeriodUsage"
	// ExchangeAPIKeyPath swaps a Cursor user API key for session tokens.
	ExchangeAPIKeyPath = "/auth/exchange_user_api_key"

	// RequestTimeout bounds one Cursor control-plane request.
	RequestTimeout = 30 * time.Second
	// AgentTimeout bounds one Cursor SDK Agent inference.
	AgentTimeout = 180 * time.Second
	// BridgeVersion pins the standalone companion shipped with ccLoad.
	BridgeVersion = "v1.0.28"
	// BridgeProtocol is the only wire contract this build accepts.
	BridgeProtocol = "sdk.v1"
	// BridgeStartupTimeout bounds spawn, discovery, Ping, and capability checks.
	BridgeStartupTimeout = 30 * time.Second
	// BridgeShutdownGrace is the bridge's total graceful shutdown budget.
	BridgeShutdownGrace = 5 * time.Second
	// BridgeCleanupTimeout bounds cancellation and durable Agent deletion.
	BridgeCleanupTimeout = 5 * time.Second

	maxResponseSize   = 4 << 20
	maxCredentialSize = 1 << 20
)

var requiredBridgeCapabilities = []string{
	"agent.create",
	"agent.send",
	"run.cancel",
	"agent.management",
}

// DefaultModels is the last-resort SDK selector used only when the live model
// catalog is unavailable. Do not invent model variants here.
var DefaultModels = []string{
	"default",
}
