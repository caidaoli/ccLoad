package app

import (
	"net/http"

	"ccLoad/internal/privacy"

	"github.com/gin-gonic/gin"
)

const (
	maxPrivacyPreviewTextBytes = 64 << 10
	maxPrivacyPreviewBodyBytes = 8 << 20 // 2 MiB config, JSON-escaped, plus text
)

type privacyPreviewRequest struct {
	Config string `json:"config"` // same JSON string as the privacy_redaction setting
	Text   string `json:"text"`
}

type privacyPreviewResponse struct {
	Masked string        `json:"masked"`
	Hits   []privacy.Hit `json:"hits"`
}

// HandlePrivacyRedactionPreview masks sample text with an unsaved draft config.
// It never touches the live privacy engine, the saved setting, or any upstream.
func (s *Server) HandlePrivacyRedactionPreview(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPrivacyPreviewBodyBytes)
	var req privacyPreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Text) > maxPrivacyPreviewTextBytes {
		RespondErrorMsg(c, http.StatusBadRequest, "text exceeds 64 KiB")
		return
	}
	config, err := privacy.ParseConfig(req.Config)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	masked, hits, err := privacy.Preview(config, req.Text)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	RespondJSON(c, http.StatusOK, privacyPreviewResponse{Masked: masked, Hits: hits})
}
