package app

import (
	"strings"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

// withOAuthBaseURLOverride returns a request-private channel config whose sole
// URL comes from the matching global OAuth setting. Cached channel snapshots
// remain immutable.
func (s *Server) withOAuthBaseURLOverride(cfg *model.Config) *model.Config {
	if s == nil || s.configService == nil || cfg == nil {
		return cfg
	}

	var settingKey string
	entry := model.ChannelURL{}
	switch {
	case cfg.UsesCodexOAuth():
		settingKey = config.CodexBaseURLSettingKey
		entry.Exact = true
		entry.Protocols = []string{string(protocol.Codex)}
	case cfg.UsesXAIOAuth():
		settingKey = config.XAIBaseURLSettingKey
		entry.Protocols = []string{string(protocol.Codex)}
	case cfg.UsesAntigravityOAuth():
		settingKey = config.AntigravityURLSettingKey
		entry.Protocols = []string{string(protocol.Gemini)}
	case cfg.UsesAnthropicOAuth():
		settingKey = config.AnthropicBaseURLSettingKey
		entry.Protocols = []string{string(protocol.Anthropic)}
	default:
		return cfg
	}

	entry.URL = strings.TrimSpace(s.configService.GetString(settingKey, ""))
	if entry.URL == "" {
		return cfg
	}

	runtimeCfg := cfg.Clone()
	runtimeCfg.URLs = model.ChannelURLs{entry}
	return runtimeCfg
}
