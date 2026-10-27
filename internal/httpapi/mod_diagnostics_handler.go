package httpapi

import (
	"context"
	"errors"
	"net/http"

	"dont/internal/moddiagnostics"
	"dont/internal/rooms"
	"github.com/gin-gonic/gin"
)

type ModDiagnosticReader interface {
	Analyze(context.Context, string, string) (moddiagnostics.Report, error)
}
type ModDiagnosticsHandler struct{ service ModDiagnosticReader }

func NewModDiagnosticsHandler(service ModDiagnosticReader) *ModDiagnosticsHandler {
	return &ModDiagnosticsHandler{service: service}
}
func (h *ModDiagnosticsHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/rooms/:roomId/worlds/:worldId/mod-diagnostics", h.analyze)
}
func (h *ModDiagnosticsHandler) analyze(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	report, err := h.service.Analyze(c.Request.Context(), c.Param("roomId"), c.Param("worldId"))
	if err != nil {
		if errors.Is(err, rooms.ErrRoomNotFound) || errors.Is(err, rooms.ErrWorldNotFound) {
			NotFound(c)
			return
		}
		Failure(c, http.StatusServiceUnavailable, "MOD_DIAGNOSTICS_UNAVAILABLE", "暂时无法读取兼容性分析证据，请稍后重试", nil)
		return
	}
	Success(c, http.StatusOK, report)
}
