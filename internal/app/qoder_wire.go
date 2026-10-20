package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/qoderauth"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type qoderModelCatalog struct {
	models  []qoderauth.Model
	expires time.Time
}

func (s *Server) qoderCatalog(ctx context.Context, cfg *model.Config, c *qoderauth.Credential, force bool) ([]qoderauth.Model, error) {
	key := fmt.Sprintf("%d/%s/%s", cfg.ID, c.Region, c.UserID)
	if !force {
		if value, ok := s.qoderModels.Load(key); ok {
			cached := value.(qoderModelCatalog)
			if time.Now().Before(cached.expires) {
				return cached.models, nil
			}
		}
	}
	service := *s.qoderService
	service.Client = s.getClientForChannel(cfg)
	models, err := service.FetchModels(ctx, c)
	var apiErr *qoderauth.APIError
	if cfg.ID > 0 && errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
		refreshed, refreshErr := s.qoderCredentials.credential(ctx, cfg, true, c.AccessToken)
		if refreshErr != nil {
			return nil, refreshErr
		}
		*c = *refreshed
		models, err = service.FetchModels(ctx, c)
	}
	if err != nil {
		return nil, err
	}
	s.qoderModels.Store(key, qoderModelCatalog{models: models, expires: time.Now().Add(time.Hour)})
	return models, nil
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
	entries := make([]model.ModelEntry, 0, len(catalog))
	for _, item := range catalog {
		entries = append(entries, model.ModelEntry{Model: item.ID})
	}
	return &FetchModelsResponse{Models: entries, Protocol: util.ProtocolOpenAI, Source: "api"}, nil
}

// prepareQoderWire runs after all generic request body mutations. The logical
// Chat request remains available to the response translator and retry loop.
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
	if matcher := s.antigravityPromptMatcher; matcher != nil {
		for index, message := range gjson.GetBytes(body, "messages").Array() {
			role := message.Get("role").String()
			if role != "system" && role != "developer" {
				continue
			}
			content := message.Get("content")
			path := fmt.Sprintf("messages.%d.content", index)
			if content.Type == gjson.String {
				body, err = sjson.SetBytes(body, path, obfuscateAntigravityText(content.String(), matcher))
			} else if content.IsArray() {
				for partIndex, part := range content.Array() {
					if part.Get("type").String() != "text" || part.Get("text").Type != gjson.String {
						continue
					}
					body, err = sjson.SetBytes(body, fmt.Sprintf("%s.%d.text", path, partIndex), obfuscateAntigravityText(part.Get("text").String(), matcher))
					if err != nil {
						break
					}
				}
			}
			if err != nil {
				return nil, &protocol.RequestTranslationError{Err: err}
			}
		}
	}
	catalog, err := s.qoderCatalog(req.Context(), cfg, c, false)
	if err != nil {
		return nil, err
	}
	name := gjson.GetBytes(body, "model").String()
	for _, item := range catalog {
		// Catalog IDs are unique case-insensitively; channels may still store mixed-case names.
		if strings.EqualFold(item.ID, name) || item.Key == name {
			service := *s.qoderService
			service.Client = s.getClientForChannel(cfg)
			signed, buildErr := service.BuildChatRequest(req.Context(), c, body, item)
			if buildErr != nil {
				return nil, &protocol.RequestTranslationError{Err: buildErr}
			}
			// Rules may add operational headers, but cannot replace signed identity.
			signedHeaders := signed.Header.Clone()
			applyHeaderRules(signed.Header, cfg.HeaderRules())
			for key, values := range signedHeaders {
				signed.Header[key] = values
			}
			return signed, nil
		}
	}
	return nil, &protocol.RequestTranslationError{Err: fmt.Errorf("qoder model %q is absent from this account's catalog", name)}
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

func normalizeQoderResponse(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		resp.Body = qoderauth.NormalizeStream(resp.Body)
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
