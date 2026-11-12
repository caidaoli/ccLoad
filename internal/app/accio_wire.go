package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/accioauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
)

type accioModelContextKey struct{}

type accioModelCatalog struct {
	models           []accioauth.Model
	expires          time.Time
	credentialDigest [32]byte
}

func (s *Server) accioCatalog(ctx context.Context, cfg *model.Config, c *accioauth.Credential, force bool) ([]accioauth.Model, error) {
	key := fmt.Sprintf("%d/%s/%s", cfg.ID, c.Region, c.UserID)
	digest := sha256.Sum256([]byte(c.AccessToken))
	if !force {
		if value, ok := s.accioModels.Load(key); ok {
			cached := value.(accioModelCatalog)
			if cached.credentialDigest == digest && time.Now().Before(cached.expires) {
				return cached.models, nil
			}
		}
	}
	service := *s.accioService
	service.Client = s.getClientForChannel(cfg)
	models, err := service.FetchModels(ctx, c)
	var apiErr *accioauth.HTTPError
	if cfg.ID > 0 && errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
		refreshed, refreshErr := s.accioCredentials.credential(ctx, cfg, true, c.AccessToken)
		if refreshErr != nil {
			return nil, refreshErr
		}
		*c = *refreshed
		models, err = service.FetchModels(ctx, c)
	}
	if err != nil {
		return nil, err
	}
	s.accioModels.Store(key, accioModelCatalog{models: models, expires: time.Now().Add(time.Hour), credentialDigest: sha256.Sum256([]byte(c.AccessToken))})
	return models, nil
}

func (s *Server) fetchAccioOAuthModels(ctx context.Context, cfg *model.Config, override string) (*FetchModelsResponse, error) {
	if override != "" && util.NormalizeProtocol(override) != util.ProtocolOpenAI {
		return nil, errors.New("accio model discovery requires openai protocol")
	}
	c, err := accioauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, err
	}
	if cfg.ID > 0 {
		c, err = s.accioCredentials.credential(ctx, cfg, false, "")
		if err != nil {
			return nil, err
		}
	}
	catalog, err := s.accioCatalog(ctx, cfg, c, true)
	if err != nil {
		return nil, err
	}
	entries := make([]model.ModelEntry, 0, len(catalog))
	for _, item := range catalog {
		entries = append(entries, model.ModelEntry{Model: item.ID})
	}
	return &FetchModelsResponse{Models: entries, Protocol: util.ProtocolOpenAI, Source: "api"}, nil
}

// prepareAccioWire runs after all generic request body mutations. The logical
// Chat request remains available to the response translator and retry loop.
func (s *Server) prepareAccioWire(cfg *model.Config, req *http.Request) (*http.Request, error) {
	c, err := accioauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, err
	}
	if req.GetBody == nil {
		return nil, errors.New("accio requires a replayable request body")
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
	// Rewrite after channel body rules and before building the native ADK request.
	body, err = rewriteChatSystemMessages(body, func(text string) string {
		text = strings.ReplaceAll(text, "Claude", "Accio")
		text = strings.ReplaceAll(text, "Codex", "Accio")
		if s.antigravityPromptMatcher != nil {
			text = obfuscateAntigravityText(text, s.antigravityPromptMatcher)
		}
		return text
	})
	if err != nil {
		return nil, &protocol.RequestTranslationError{Err: err}
	}
	catalog, err := s.accioCatalog(req.Context(), cfg, c, false)
	if err != nil {
		return nil, err
	}
	name := gjson.GetBytes(body, "model").String()
	for _, item := range catalog {
		if strings.EqualFold(item.ID, name) {
			service := *s.accioService
			service.Client = s.getClientForChannel(cfg)
			signed, buildErr := service.BuildChatRequest(req.Context(), c, body, item)
			if buildErr != nil {
				return nil, &protocol.RequestTranslationError{Err: buildErr}
			}
			// Rules may add operational headers, but cannot replace provider identity.
			signedHeaders := signed.Header.Clone()
			applyHeaderRules(signed.Header, cfg.HeaderRules())
			maps.Copy(signed.Header, signedHeaders)
			return signed.WithContext(context.WithValue(signed.Context(), accioModelContextKey{}, name)), nil
		}
	}
	return nil, &protocol.RequestTranslationError{Err: fmt.Errorf("accio model %q is absent from this account's catalog", name)}
}

func (s *Server) tryAccioOAuthChannel(ctx context.Context, cfg *model.Config, reqCtx *proxyRequestContext, w http.ResponseWriter) (*proxyResult, error) {
	return s.tryOAuthChannel(ctx, cfg, reqCtx, w, "Accio", true, func(force bool, rejected string) (*model.Config, string, error) {
		c, err := s.accioCredentials.credential(ctx, cfg, force, rejected)
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

func normalizeAccioResponse(resp *http.Response, modelName string) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	punished := strings.TrimSpace(resp.Header.Get("Bxpunish")) == "1"
	gatewayBlocked := punished || resp.StatusCode == http.StatusForbidden && contentType == "text/html"
	successful := resp.StatusCode >= 200 && resp.StatusCode < 300
	if successful && !gatewayBlocked && (contentType == "" || contentType == "text/event-stream") {
		resp.Body = accioauth.NormalizeStream(resp.Body, modelName)
		resp.Header.Set("Content-Type", "text/event-stream")
	} else {
		var failure *accioauth.APIError
		if gatewayBlocked {
			// Gateway challenges are not credential failures. Never parse or retain
			// their HTML, cookies, or redirect URLs in client/admin diagnostics.
			_ = resp.Body.Close()
			message := "Accio gateway rejected request"
			if punished {
				message += " (Bxpunish: 1)"
			}
			failure = &accioauth.APIError{StatusCode: http.StatusBadGateway, Type: "upstream_error", Code: "accio_gateway_blocked", Message: message}
			resp.Header.Del("Set-Cookie")
			resp.Header.Del("Location")
		} else {
			raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
			_ = resp.Body.Close()
			if err != nil {
				return err
			}
			if len(raw) > 1<<20 {
				return errors.New("accio error response exceeds limit")
			}
			failure = accioauth.ClassifyError(resp.StatusCode, raw)
			if successful {
				// A successful inference must be SSE. JSON can instead carry a
				// business rejection; classify it without echoing arbitrary messages.
				failure = &accioauth.APIError{StatusCode: http.StatusBadGateway, Type: "upstream_error", Code: "invalid_upstream_response", Message: "Accio returned a non-SSE inference response"}
				if json.Valid(raw) && (contentType == "application/json" || strings.HasSuffix(contentType, "+json")) {
					root := gjson.ParseBytes(raw)
					code := root.Get("errorCode")
					if !code.Exists() {
						code = root.Get("error_code")
					}
					if !code.Exists() {
						code = root.Get("code")
					}
					businessError := root.Get("success").Type == gjson.False ||
						root.Get("error").Exists() && root.Get("error").Type != gjson.Null ||
						code.Exists() && code.String() != "0" && code.String() != "200" && code.String() != "" ||
						root.Get("errorMessage").String() != "" || root.Get("error_message").String() != ""
					if businessError {
						failure = accioauth.ClassifyError(resp.StatusCode, raw)
						failure.Message = fmt.Sprintf("Accio upstream rejected request (HTTP %d)", resp.StatusCode)
						if n, err := strconv.Atoi(code.String()); err == nil && n >= 0 && n <= 999999 {
							failure.Message = fmt.Sprintf("Accio upstream rejected request (HTTP %d, business code %d)", resp.StatusCode, n)
						}
					}
				}
				resp.Header.Del("Set-Cookie")
				resp.Header.Del("Location")
			}
		}
		encoded, err := json.Marshal(map[string]any{"error": failure, "status": failure.StatusCode})
		if err != nil {
			return err
		}
		resp.StatusCode = failure.StatusCode
		resp.Body = io.NopCloser(bytes.NewReader(encoded))
		resp.Header.Set("Content-Type", "application/json")
		resp.Header.Del("Content-Encoding")
	}
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return nil
}

func isAccioChatRequest(cfg *model.Config, upstream protocol.Protocol) bool {
	return cfg != nil && cfg.UsesAccioOAuth() && upstream == protocol.OpenAI
}

// Accio's provider protocol always streams; aggregation serves non-stream clients.
func (s *Server) handleAccioNonStream(reqCtx *requestContext, resp *http.Response, headers http.Header, w http.ResponseWriter, stats *streamReadStats) (*fwResult, float64, error) {
	return s.handleChatNonStream(reqCtx, resp, headers, w, stats, nil, "Accio")
}
