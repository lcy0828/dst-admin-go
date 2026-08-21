package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"dont/internal/chatlogs"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
)

type ChatLogService interface {
	List(context.Context, string, chatlogs.Filter) (chatlogs.List, error)
}

type ChatLogHandler struct{ chatLogs ChatLogService }

func NewChatLogHandler(service ChatLogService) *ChatLogHandler {
	return &ChatLogHandler{chatLogs: service}
}

func (h *ChatLogHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/rooms/:roomId/chat-logs", h.list)
}

func (h *ChatLogHandler) list(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	value, err := h.chatLogs.List(c.Request.Context(), c.Param("roomId"), chatlogs.Filter{
		Query: c.Query("query"), WorldID: c.Query("worldId"), Kind: chatlogs.Kind(c.Query("kind")),
		Limit: limit, Offset: offset,
	})
	if err != nil {
		chatLogFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func chatLogFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, chatlogs.ErrInvalidFilter):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_CHAT_LOG_FILTER", "聊天记录筛选条件无效", nil)
	case errors.Is(err, chatlogs.ErrRoomNotManaged), errors.Is(err, rooms.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_NOT_MANAGED", "接管房间后才能读取聊天记录", nil)
	case errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_RESOURCE_ID", "房间或世界标识无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间或世界不存在", nil)
	case errors.Is(err, context.DeadlineExceeded):
		Failure(c, http.StatusGatewayTimeout, "CHAT_LOG_READ_TIMEOUT", "读取聊天记录超时", nil)
	case errors.Is(err, context.Canceled):
		Failure(c, http.StatusRequestTimeout, "CHAT_LOG_READ_CANCELED", "读取聊天记录已取消", nil)
	default:
		Failure(c, http.StatusInternalServerError, "CHAT_LOG_READ_FAILED", "读取聊天记录失败", nil)
	}
}
