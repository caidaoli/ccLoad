package app

import (
	"net/http"
	"time"

	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

type commonModelCatalogResponse struct {
	Models    []string `json:"models"`
	Source    string   `json:"source"`
	FetchedAt string   `json:"fetched_at,omitempty"`
}

// HandleCommonModelCatalog 返回渠道协议对应的常用官方模型目录。
// GET /admin/model-catalog/common?channel_type=...
func (s *Server) HandleCommonModelCatalog(c *gin.Context) {
	channelType := util.NormalizeChannelType(c.Query("channel_type"))
	if !util.IsValidChannelType(channelType) {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel_type")
		return
	}

	models, source, fetchedAt := util.CommonCatalogModels(channelType, 6)
	response := commonModelCatalogResponse{Models: models, Source: source}
	if !fetchedAt.IsZero() {
		response.FetchedAt = fetchedAt.UTC().Format(time.RFC3339)
	}
	RespondJSON(c, http.StatusOK, response)
}
