package httpapi

import (
	"net/http"

	"dont/internal/systemstatus"

	"github.com/gin-gonic/gin"
)

type SystemStatusHandler struct{ service *systemstatus.Service }

func NewSystemStatusHandler(service *systemstatus.Service) *SystemStatusHandler {
	return &SystemStatusHandler{service: service}
}

func (h *SystemStatusHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/system/status", func(c *gin.Context) { Success(c, http.StatusOK, h.service.Status()) })
}
