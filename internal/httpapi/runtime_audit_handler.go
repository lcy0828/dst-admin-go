package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"dont/internal/rooms"
	"dont/internal/runtimeaudit"

	"github.com/gin-gonic/gin"
)

type RuntimeAuditHandler struct {
	service *runtimeaudit.Service
}

func NewRuntimeAuditHandler(service *runtimeaudit.Service) *RuntimeAuditHandler {
	return &RuntimeAuditHandler{service: service}
}

func (h *RuntimeAuditHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/rooms/:roomId/runtime-events", h.list)
}

func (h *RuntimeAuditHandler) list(c *gin.Context) {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if err != nil {
		runtimeAuditFailure(c, runtimeaudit.ErrInvalidFilter)
		return
	}
	result, err := h.service.List(c.Param("roomId"), runtimeaudit.ListFilter{
		WorldID: c.Query("worldId"), Type: runtimeaudit.EventType(c.Query("type")), Limit: limit,
	})
	if err != nil {
		runtimeAuditFailure(c, err)
		return
	}
	Success(c, http.StatusOK, result)
}

func runtimeAuditFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, runtimeaudit.ErrInvalidFilter):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_RUNTIME_EVENT_FILTER", "运行事件筛选参数无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		NotFound(c)
	default:
		Failure(c, http.StatusInternalServerError, "RUNTIME_EVENT_FAILED", "读取运行事件失败", nil)
	}
}
