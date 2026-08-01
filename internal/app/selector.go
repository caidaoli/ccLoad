package app

import (
	"context"
	"net/url"
	"strings"

	modelpkg "ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"
)

func normalizeOptionalProtocol(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return util.NormalizeProtocol(value)
}

// selectCandidatesByClientProtocol 返回所有启用渠道；clientProtocol 仅用于计算客户端协议对应的模型冷却键。
func (s *Server) selectCandidatesByClientProtocol(ctx context.Context, clientProtocol string) ([]*modelpkg.Config, error) {
	channels, err := s.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		return nil, err
	}
	return s.filterCooldownChannels(ctx, channels, "*", clientProtocol)
}

// alphaSearchUpstreamURLs removes exact URLs for other Codex endpoints and
// endpoints that recently proved they do not implement alpha/search.
// Normal base URLs remain eligible because the request path is appended later.
func (s *Server) alphaSearchUpstreamURLs(cfg *modelpkg.Config) []string {
	urls := cfg.GetURLs()
	compatible := make([]string, 0, len(urls))
	for _, rawURL := range urls {
		key := protocolCapabilityKey{
			channelID: cfg.ID, baseURL: rawURL,
			clientProtocol: protocol.Codex, requestFamily: protocol.RequestFamilyAlphaSearch,
		}
		if cached, known := s.protocolCapabilities.get(key); known && cached == protocolUnsupported {
			continue
		}
		if !modelpkg.HasExactUpstreamURLMarker(rawURL) {
			compatible = append(compatible, rawURL)
			continue
		}

		parsed, err := url.Parse(modelpkg.StripExactUpstreamURLMarker(rawURL))
		if err == nil && protocol.DetectRequestFamily(parsed.Path) == protocol.RequestFamilyAlphaSearch {
			compatible = append(compatible, rawURL)
		}
	}
	return compatible
}

func (s *Server) selectAlphaSearchCandidates(ctx context.Context, modelName string) ([]*modelpkg.Config, error) {
	routeModel := modelName
	if routeModel == "" {
		routeModel = "*"
	}
	channels, err := s.GetEnabledChannelsByModel(ctx, routeModel)
	if err != nil {
		return nil, err
	}

	compatible := make([]*modelpkg.Config, 0, len(channels))
	for _, cfg := range channels {
		if cfg == nil {
			continue
		}

		urls := s.alphaSearchUpstreamURLs(cfg)
		if len(urls) == 0 {
			continue
		}
		if len(urls) != len(cfg.GetURLs()) {
			configuredURLs := cfg.URLs
			cfg = cfg.Clone()
			allowed := make(map[string]struct{}, len(urls))
			for _, runtimeURL := range urls {
				allowed[runtimeURL] = struct{}{}
			}
			cfg.URLs = cfg.URLs[:0]
			for _, entry := range configuredURLs {
				if _, ok := allowed[entry.RuntimeURL()]; ok {
					cfg.URLs = append(cfg.URLs, entry)
				}
			}
		}
		compatible = append(compatible, cfg)
	}

	return s.filterCooldownChannels(ctx, compatible, routeModel, string(protocol.Codex))
}

// selectCandidatesByModelAndClientProtocol 按模型选择候选渠道；clientProtocol 仅表示客户端协议，不过滤上游主协议。
func (s *Server) selectCandidatesByModelAndClientProtocol(ctx context.Context, model string, clientProtocol string) ([]*modelpkg.Config, error) {
	normalizedType := normalizeOptionalProtocol(clientProtocol)

	channels, err := s.GetEnabledChannelsByModel(ctx, model)
	if err != nil {
		return nil, err
	}

	// 先做冷却/成本过滤，但不触发“全冷却兜底”，以便后续还能继续做模糊匹配回退。
	filtered, err := s.filterCooldownChannelsStrict(ctx, channels, model, normalizedType)
	if err != nil {
		return nil, err
	}
	if len(filtered) > 0 {
		return filtered, nil
	}

	// 兜底：全量查询（用于“模糊匹配回退”以及最终“全冷却兜底”场景）
	// 注意：此处不能以 len(channels)==0 作为是否回退的条件。
	// 精确候选可能存在但全部在冷却/成本限额下不可用，这时仍需尝试模糊匹配补充候选。
	var allCandidates []*modelpkg.Config
	if model != "*" {
		source, err := s.GetEnabledChannelsByModel(ctx, "*")
		if err != nil {
			return nil, err
		}
		if len(source) == 0 {
			source, err = s.store.ListConfigs(ctx)
			if err != nil {
				return nil, err
			}
		}

		allCandidates = make([]*modelpkg.Config, 0, len(source))
		for _, cfg := range source {
			if cfg == nil || !cfg.Enabled {
				continue
			}
			if s.configSupportsModelWithFuzzyMatch(cfg, model) {
				allCandidates = append(allCandidates, cfg)
			}
		}
	}

	// 再次过滤，但仍不触发“全冷却兜底”：先把可用的候选尽可能找出来。
	filtered, err = s.filterCooldownChannelsStrict(ctx, allCandidates, model, normalizedType)
	if err != nil {
		return nil, err
	}
	if len(filtered) > 0 {
		return filtered, nil
	}

	// 最终兜底：如果候选存在但全部在冷却中，让全冷却兜底逻辑选择“最早恢复”的渠道。
	return s.filterCooldownChannels(ctx, allCandidates, model, normalizedType)
}
