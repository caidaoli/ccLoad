// Package xaiauth implements the fixed xAI OAuth credential lifecycle.
package xaiauth

import "time"

const (
	// ChannelType and the related constants define the fixed xAI OAuth wire contract.
	ChannelType = "xai"
	// OAuthIssuer is the only trusted xAI OAuth issuer origin.
	OAuthIssuer = "https://auth.x.ai"
	// AuthorizeURL is the fixed xAI authorization-code endpoint.
	AuthorizeURL = OAuthIssuer + "/oauth2/authorize"
	// DeviceCodeURL is used only for automated SSO-cookie conversion.
	DeviceCodeURL = OAuthIssuer + "/oauth2/device/code"
	// TokenURL is the fixed xAI OAuth token endpoint.
	TokenURL = OAuthIssuer + "/oauth2/token"
	// ClientID is the public xAI CLI OAuth client identifier.
	ClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	// OAuthScope is the scope used by xAI's interactive PKCE authorization flow.
	OAuthScope = "openid profile email offline_access grok-cli:access api:access"
	// RedirectURI is the loopback URI registered for the public xAI CLI client.
	RedirectURI = "http://127.0.0.1:56121/callback"
	// SSOScope is the exact scope requested during SSO conversion.
	SSOScope = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write"
	// DeviceCodeGrantType is the RFC 8628 device-code grant type.
	DeviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// APIBaseURL is the fixed public xAI API base URL.
	APIBaseURL = "https://api.x.ai/v1"
	// CLIBaseURL is the fixed xAI CLI proxy base URL.
	CLIBaseURL = "https://cli-chat-proxy.grok.com/v1"
	// CLITokenAuthHeader names the CLI token authentication marker.
	CLITokenAuthHeader = "X-XAI-Token-Auth"
	// CLITokenAuthValue is the CLI token authentication marker value.
	CLITokenAuthValue = "xai-grok-cli"
	// CLIClientVersionHeader names the xAI CLI version header.
	CLIClientVersionHeader = "x-grok-client-version"
	// CLIClientVersion is the emulated xAI CLI protocol version.
	CLIClientVersion = "0.2.120"
	// CLIUserAgent is the fixed xAI model-request user agent.
	CLIUserAgent = "xai-grok-workspace/" + CLIClientVersion
	// CLIClientIdentifierHeader identifies Grok CLI requests to chat-proxy.
	CLIClientIdentifierHeader = "x-grok-client-identifier"
	// CLIClientIdentifierValue is the current Grok CLI client identity.
	CLIClientIdentifierValue = "grok-shell"
	// CLIAuthenticateResponseHeader requests chat-proxy response authentication metadata.
	CLIAuthenticateResponseHeader = "x-authenticateresponse"
	// CLIAuthenticateResponseValue enables chat-proxy response authentication metadata.
	CLIAuthenticateResponseValue = "authenticate-response"
	// CLIBillingUserAgent is the fixed xAI CLI billing client identity.
	CLIBillingUserAgent = "grok-pager/" + CLIClientVersion + " grok-shell/" + CLIClientVersion + " (macos; aarch64)"
	// CLIClientModeHeader names the xAI CLI interaction mode header.
	CLIClientModeHeader = "X-Grok-Client-Mode"
	// CLIClientMode identifies normal interactive Grok CLI traffic.
	CLIClientMode = "interactive"

	// SSOAccountsURL is the initial trusted xAI accounts endpoint.
	SSOAccountsURL = "https://accounts.x.ai/"
	// SSODeviceURL is the device endpoint used during SSO conversion.
	SSODeviceURL = DeviceCodeURL
	// SSOVerifyURL is the fixed SSO device verification endpoint.
	SSOVerifyURL = OAuthIssuer + "/oauth2/device/verify"
	// SSOApproveURL is the fixed SSO consent approval endpoint.
	SSOApproveURL = OAuthIssuer + "/oauth2/device/approve"
	// SSOTokenURL is the fixed token endpoint used by SSO conversion.
	SSOTokenURL = TokenURL
	// MaxSSOResponseBytes bounds every SSO response body.
	MaxSSOResponseBytes = 2 << 20
)

const (
	// RefreshLead and the related durations bound refresh and SSO conversion.
	RefreshLead = 5 * time.Minute
	// SSOConversionTimeout is the hard upper bound for SSO conversion.
	SSOConversionTimeout  = 90 * time.Second
	defaultPollInterval   = 5 * time.Second
	requestTimeout        = 30 * time.Second
	maxOAuthResponseBytes = 1 << 20
)
