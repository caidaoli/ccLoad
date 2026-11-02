package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"ccLoad/internal/privacy"

	"github.com/gin-gonic/gin"
)

const privacySettingKey = "privacy_redaction"

var errPrivacyMasking = errors.New("privacy masking failed; request was not sent")

type privacyContextKey struct{}

func withPrivacySession(ctx context.Context, session *privacy.Session) context.Context {
	if session == nil {
		return ctx
	}
	return context.WithValue(ctx, privacyContextKey{}, session)
}

// A request keeps its configuration snapshot through retries and protocol changes.
// Token hashes isolate reversible mappings between gateway users.
func (s *Server) privacySession(c *gin.Context) *privacy.Session {
	cfg := s.privacyConfig.Load()
	if cfg == nil || s.privacyEngine == nil {
		return nil
	}
	scope := c.GetString("token_hash")
	if !cfg.Enabled && (scope == "" || !s.privacyEngine.HasMappings(scope)) {
		return nil
	}
	if scope == "" {
		// No authenticated identity must never share a reversible dictionary.
		return privacy.NewEngine().Session("", *cfg)
	}
	return s.privacyEngine.Session(scope, *cfg)
}

func maskPrivacyBody(session *privacy.Session, body []byte, contentType string) ([]byte, error) {
	if session == nil || len(body) == 0 {
		return body, nil
	}
	if !session.Enabled() {
		if json.Valid(body) {
			return session.MaskJSON(body)
		}
		return body, nil
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil && contentType != "" {
		return nil, errors.New("privacy masking requires a valid Content-Type")
	}
	if mediaType != "multipart/form-data" {
		// Generation endpoints carry JSON. Binary media is not decoded or scanned.
		if mediaType == "" || mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
			return session.MaskJSON(body)
		}
		if strings.HasPrefix(mediaType, "text/") {
			masked, maskErr := session.MaskText(string(body))
			return []byte(masked), maskErr
		}
		return nil, errors.New("privacy masking does not support this request Content-Type")
	}
	var output bytes.Buffer
	writer := multipart.NewWriter(&output)
	if err := writer.SetBoundary(params["boundary"]); err != nil {
		return nil, errors.New("privacy masking requires a valid multipart boundary")
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, readErr := reader.NextRawPart()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, errors.New("privacy masking could not read multipart request")
		}
		data, readErr := io.ReadAll(part)
		_ = part.Close()
		if readErr != nil {
			return nil, errors.New("privacy masking could not read multipart field")
		}
		if part.FileName() == "" && (part.FormName() == "prompt" || part.FormName() == "text" || part.FormName() == "input") {
			masked, maskErr := session.MaskText(string(data))
			if maskErr != nil {
				return nil, maskErr
			}
			data = []byte(masked)
		}
		dest, writeErr := writer.CreatePart(part.Header)
		if writeErr != nil {
			return nil, writeErr
		}
		if _, writeErr = dest.Write(data); writeErr != nil {
			return nil, writeErr
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// Keep Gin's status/size bookkeeping on the underlying writer while routing all
// body writes through the same restorer used by the WebSocket SSE bridge.
type privacyGinWriter struct {
	gin.ResponseWriter
	output *privacyResponseWriter
}

func (w *privacyGinWriter) Write(data []byte) (int, error)       { return w.output.Write(data) }
func (w *privacyGinWriter) WriteString(data string) (int, error) { return w.Write([]byte(data)) }
func (w *privacyGinWriter) WriteHeader(status int)               { w.output.WriteHeader(status) }
func (w *privacyGinWriter) Flush()                               { w.output.Flush() }
func (w *privacyGinWriter) Unwrap() http.ResponseWriter          { return w.output }

func maskPrivacyBodyFromContext(ctx context.Context, body []byte, contentType string) ([]byte, error) {
	session, _ := ctx.Value(privacyContextKey{}).(*privacy.Session)
	masked, err := maskPrivacyBody(session, body, contentType)
	if err != nil {
		return nil, errPrivacyMasking
	}
	return masked, nil
}
