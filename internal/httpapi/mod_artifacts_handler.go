package httpapi

import (
	"mime"
	"net/http"
	"strings"

	"dont/internal/modartifact"

	"github.com/gin-gonic/gin"
)

type ModArtifactHandler struct {
	service *modartifact.Service
}

func NewModArtifactHandler(service *modartifact.Service) *ModArtifactHandler {
	return &ModArtifactHandler{service: service}
}

func (h *ModArtifactHandler) RegisterDownloads(router *gin.Engine) {
	if h == nil || h.service == nil || router == nil {
		return
	}
	router.GET("/mod-artifacts/:workshopId/:treeSha256", h.download)
	router.HEAD("/mod-artifacts/:workshopId/:treeSha256", h.download)
}

func (h *ModArtifactHandler) download(c *gin.Context) {
	authorization := strings.Fields(strings.TrimSpace(c.GetHeader("Authorization")))
	if len(authorization) != 2 || !strings.EqualFold(authorization[0], "Bearer") {
		Failure(c, http.StatusUnauthorized, "MOD_ARTIFACT_TOKEN_REQUIRED", "Mod 制品下载凭据无效", nil)
		return
	}
	descriptor, file, err := h.service.Open(c.Param("workshopId"), c.Param("treeSha256"), authorization[1])
	if err != nil {
		Failure(c, http.StatusUnauthorized, "MOD_ARTIFACT_TOKEN_INVALID", "Mod 制品下载凭据无效或已过期", nil)
		return
	}
	defer file.Close()
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-DST-Mod-Tree-SHA256", descriptor.TreeSHA256)
	c.Header("X-DST-Mod-Bundle-SHA256", descriptor.SHA256)
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": "workshop-" + descriptor.WorkshopID + "-" + descriptor.TreeSHA256 + ".tar",
	}))
	http.ServeContent(c.Writer, c.Request, "", descriptor.CreatedAt, file)
}
