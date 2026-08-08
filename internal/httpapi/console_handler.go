package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	consoleapi "dont/internal/console"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
)

type ConsoleHandler struct{ console *consoleapi.Service }

func NewConsoleHandler(service *consoleapi.Service) *ConsoleHandler {
	return &ConsoleHandler{console: service}
}

func (h *ConsoleHandler) Register(v2 *gin.RouterGroup) {
	roomsGroup := v2.Group("/rooms/:roomId")
	roomsGroup.GET("/commands", h.definitions)
	roomsGroup.GET("/command-runs", h.runs)
	roomsGroup.GET("/command-runs/:runId", h.run)
	worldGroup := roomsGroup.Group("/worlds/:worldId")
	worldGroup.POST("/commands", h.execute)
	worldGroup.POST("/raw-commands", h.executeRaw)
}

func (h *ConsoleHandler) definitions(c *gin.Context) {
	Success(c, http.StatusOK, gin.H{"items": h.console.Definitions()})
}

func (h *ConsoleHandler) runs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "25"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}
	items, total, err := h.console.Runs(consoleapi.ListFilter{RoomID: c.Param("roomId"), WorldID: c.Query("worldId"), Limit: limit, Offset: offset})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "COMMAND_HISTORY_FAILED", "读取命令历史失败", nil)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": total, "limit": limit, "offset": offset})
}

func (h *ConsoleHandler) run(c *gin.Context) {
	result, err := h.console.Run(c.Param("runId"))
	if err != nil {
		consoleFailure(c, err)
		return
	}
	if result.RoomID != c.Param("roomId") {
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "命令记录不存在", nil)
		return
	}
	Success(c, http.StatusOK, result)
}

func (h *ConsoleHandler) execute(c *gin.Context) {
	var request consoleapi.ExecuteRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的命令", nil)
		return
	}
	result, err := h.console.Execute(c.Request.Context(), c.Param("roomId"), c.Param("worldId"), request)
	if err != nil {
		consoleFailure(c, err)
		return
	}
	Success(c, http.StatusCreated, result)
}

func (h *ConsoleHandler) executeRaw(c *gin.Context) {
	var request consoleapi.RawRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的原始命令", nil)
		return
	}
	result, err := h.console.ExecuteRaw(c.Request.Context(), c.Param("roomId"), c.Param("worldId"), request)
	if err != nil {
		consoleFailure(c, err)
		return
	}
	Success(c, http.StatusCreated, result)
}

func consoleFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, consoleapi.ErrCommandNotFound), errors.Is(err, consoleapi.ErrRunNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "命令或命令记录不存在", nil)
	case errors.Is(err, consoleapi.ErrInvalidArguments), errors.Is(err, consoleapi.ErrRawCommandInvalid):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_COMMAND", "命令参数无效", nil)
	case errors.Is(err, consoleapi.ErrConfirmationNeeded):
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入完整房间名确认该命令", nil)
	case errors.Is(err, consoleapi.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_NOT_MANAGED", "接管房间后才能发送命令", nil)
	case errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_RESOURCE_ID", "房间或世界标识无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间或世界不存在", nil)
	default:
		Failure(c, http.StatusInternalServerError, "COMMAND_SEND_FAILED", "发送命令失败", nil)
	}
}
