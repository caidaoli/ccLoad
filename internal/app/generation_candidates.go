package app

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

var errGenerationModelDenied = errors.New("model denied by token scope")

type generationRouteInput struct {
	Model          string
	ClientProtocol string
	TokenHash      string
	Multimodal     bool
}

// generationRoutingModel runs before thinking-suffix rewriting in real requests.
// The returned spelling retains the suffix for that rewriting; routing uses its base name.
func (s *Server) generationRoutingModel(input generationRouteInput) string {
	requested := input.Model
	if input.ClientProtocol == string(protocol.Anthropic) {
		requested = trimClaudeCodeLongContextSuffix(requested)
	}
	if fallback := s.multimodalFallbackModel(model.RoutingModelName(requested), input.Multimodal); fallback != "" {
		return fallback
	}
	return requested
}

// resolveGenerationCandidates resolves only ordinary generation candidates. The
// real proxy supplies its already resolved model with Multimodal=false, preserving
// the existing model-rewrite and candidate-selection points.
func (s *Server) resolveGenerationCandidates(ctx context.Context, input generationRouteInput) ([]*model.Config, error) {
	routingModel := model.RoutingModelName(s.generationRoutingModel(input))
	if !s.tokenModelAllowed(input.TokenHash, routingModel) {
		return nil, errGenerationModelDenied
	}
	if channelRestrictionTokenFromContext(ctx) != input.TokenHash {
		ctx = withChannelRestrictionToken(ctx, input.TokenHash)
	}
	return s.selectCandidatesByModelAndClientProtocol(ctx, routingModel, input.ClientProtocol)
}

func isOrdinaryGenerationRequest(method, path string, client protocol.Protocol) bool {
	if method != http.MethodPost {
		return false
	}
	family := protocol.DetectRequestFamily(path)
	switch client {
	case protocol.Anthropic:
		return family == protocol.RequestFamilyMessages
	case protocol.OpenAI:
		return family == protocol.RequestFamilyChatCompletions
	case protocol.Codex:
		return family == protocol.RequestFamilyResponses && !strings.HasSuffix(strings.TrimRight(path, "/"), "/compact")
	case protocol.Gemini:
		return family == protocol.RequestFamilyGenerateContent && (strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent"))
	default:
		return false
	}
}
