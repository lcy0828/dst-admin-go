package httpapi

import (
	"context"
	"dont/internal/jobs"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"dont/internal/chatlogs"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
)

type ChatLogService interface {
	List(context.Context, string, chatlogs.Filter) (chatlogs.List, error)
}

type ChatLogHandler struct {
	chatLogs ChatLogService
	jobs     *jobs.Service
	mu       sync.Mutex
	active   map[string]string
}
type chatHistoryRepairer interface {
	RepairTarget(string) (rooms.Room, error)
	RepairRoom(context.Context, string) (chatlogs.SyncResult, error)
}

func NewChatLogHandler(service ChatLogService, jobServices ...*jobs.Service) *ChatLogHandler {
	h := &ChatLogHandler{chatLogs: service, active: make(map[string]string)}
	if len(jobServices) > 0 {
		h.jobs = jobServices[0]
	}
	return h
}

func (h *ChatLogHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/rooms/:roomId/chat-logs", h.list)
	v2.POST("/rooms/:roomId/chat-logs/actions/repair", h.repair)
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
		Failure(c, http.StatusConflict, "ROOM_UNAVAILABLE", "房间当前不可用，请检查运行节点与拓扑状态", nil)
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

func (h *ChatLogHandler) repair(c *gin.Context) {
	repairer, ok := h.chatLogs.(chatHistoryRepairer)
	if !ok || h.jobs == nil {
		Failure(c, http.StatusServiceUnavailable, "CHAT_REPAIR_UNAVAILABLE", "聊天历史恢复暂不可用", nil)
		return
	}
	roomID := c.Param("roomId")
	room, err := repairer.RepairTarget(roomID)
	if err != nil {
		chatLogFailure(c, err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if id := h.active[roomID]; id != "" {
		if job, err := h.jobs.Get(id); err == nil && (job.Status == jobs.StatusQueued || job.Status == jobs.StatusRunning) {
			Success(c, http.StatusAccepted, job)
			return
		}
	}
	job, err := h.jobs.Submit("log.chat.repair", roomID, "", []jobs.TargetSpec{{ID: roomID, Name: room.Name}}, func(ctx context.Context, report func(jobs.TargetResult)) error {
		result, err := repairer.RepairRoom(ctx, roomID)
		if err != nil {
			report(jobs.TargetResult{TargetID: roomID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "CHAT_REPAIR_FAILED", Message: err.Error()}})
			return err
		}
		target := jobs.TargetResult{TargetID: roomID, Status: jobs.StatusSucceeded, Message: fmt.Sprintf("已补入 %d 条消息来源", result.Imported)}
		if result.ParseErrors > 0 || result.UncertainTimes > 0 || result.UnavailableGenerations > 0 {
			target.Warning = &jobs.Error{Code: "CHAT_REPAIR_NEEDS_REVIEW", Message: fmt.Sprintf("%d 行解析异常，%d 条时间待确认，%d 份历史日志无法重新核验", result.ParseErrors, result.UncertainTimes, result.UnavailableGenerations)}
		}
		report(target)
		return nil
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建聊天历史恢复任务", nil)
		return
	}
	h.active[roomID] = job.ID
	Success(c, http.StatusAccepted, job)
}
