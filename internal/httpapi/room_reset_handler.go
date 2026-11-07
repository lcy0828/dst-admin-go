package httpapi

import (
	"errors"
	"net/http"

	"dont/internal/roomreset"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
)

// RoomResetHandler exposes the job-based reset of every world in one Room.
type RoomResetHandler struct {
	reset *roomreset.Service
}

func NewRoomResetHandler(service *roomreset.Service) *RoomResetHandler {
	return &RoomResetHandler{reset: service}
}

func (h *RoomResetHandler) Register(v2 *gin.RouterGroup) {
	v2.POST("/rooms/:roomId/actions/reset-worlds", h.submit)
}

func (h *RoomResetHandler) submit(c *gin.Context) {
	var request roomreset.Request
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的重置设置", nil)
		return
	}
	job, err := h.reset.Submit(c.Param("roomId"), request)
	switch {
	case err == nil:
		Success(c, http.StatusAccepted, job)
	case errors.Is(err, roomreset.ErrConfirmation):
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入完整房间名确认重置全部世界", nil)
	case errors.Is(err, roomreset.ErrNoMaster):
		Failure(c, http.StatusConflict, "MASTER_WORLD_REQUIRED", "房间没有主世界，无法重置", nil)
	case errors.Is(err, rooms.ErrRoomNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间不存在", nil)
	case errors.Is(err, rooms.ErrInvalidID):
		Failure(c, http.StatusBadRequest, "INVALID_ROOM", "房间编号无效", nil)
	default:
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建重置任务", nil)
	}
}
