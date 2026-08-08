package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/internal/worldstate"

	"github.com/gin-gonic/gin"
)

type WorldStateService interface {
	List(string) (worldstate.List, error)
	History(string, string, int) (worldstate.History, error)
	WorldTargets(string) ([]rooms.World, error)
	RefreshWorld(context.Context, string, string) (worldstate.RefreshResult, error)
}

type WorldStateHandler struct {
	states WorldStateService
	jobs   *jobs.Service
}

func NewWorldStateHandler(service WorldStateService, jobService *jobs.Service) *WorldStateHandler {
	return &WorldStateHandler{states: service, jobs: jobService}
}

func (h *WorldStateHandler) Register(v2 *gin.RouterGroup) {
	states := v2.Group("/rooms/:roomId/world-states")
	states.GET("", h.list)
	states.GET("/history", h.history)
	states.POST("/actions/refresh", h.refresh)
}

func (h *WorldStateHandler) list(c *gin.Context) {
	value, err := h.states.List(c.Param("roomId"))
	if err != nil {
		worldStateFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *WorldStateHandler) history(c *gin.Context) {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "120"))
	if err != nil {
		worldStateFailure(c, worldstate.ErrInvalidFilter)
		return
	}
	value, err := h.states.History(c.Param("roomId"), c.Query("worldId"), limit)
	if err != nil {
		worldStateFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *WorldStateHandler) refresh(c *gin.Context) {
	targets, err := h.states.WorldTargets(c.Param("roomId"))
	if err != nil {
		worldStateFailure(c, err)
		return
	}
	jobTargets := make([]jobs.TargetSpec, 0, len(targets))
	for _, target := range targets {
		jobTargets = append(jobTargets, jobs.TargetSpec{ID: target.ID, Name: target.Name})
	}
	roomID := c.Param("roomId")
	job, err := h.jobs.Submit("world.state.refresh", roomID, "", jobTargets, func(ctx context.Context, report func(jobs.TargetResult)) error {
		var result error
		for _, target := range targets {
			refreshed, refreshErr := h.states.RefreshWorld(ctx, roomID, target.ID)
			if refreshErr != nil {
				report(jobs.TargetResult{TargetID: target.ID, Status: jobs.StatusFailed, Error: worldStateJobError(refreshErr)})
				result = errors.Join(result, refreshErr)
				continue
			}
			report(jobs.TargetResult{TargetID: target.ID, Status: jobs.StatusSucceeded, Message: refreshed.Message})
		}
		return result
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建世界状态刷新任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func worldStateJobError(err error) *jobs.Error {
	switch {
	case errors.Is(err, worldstate.ErrWorldNotRunning):
		return &jobs.Error{Code: "WORLD_NOT_RUNNING", Message: "分片未运行，无法读取实时状态"}
	case errors.Is(err, worldstate.ErrProbeTimedOut):
		return &jobs.Error{Code: "WORLD_STATE_PROBE_TIMEOUT", Message: "等待世界状态探针输出超时"}
	case errors.Is(err, context.Canceled):
		return &jobs.Error{Code: "JOB_CANCELED", Message: "任务已取消"}
	default:
		return &jobs.Error{Code: "WORLD_STATE_REFRESH_FAILED", Message: err.Error()}
	}
}

func worldStateFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, worldstate.ErrInvalidFilter):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_WORLD_STATE_FILTER", "世界状态筛选参数无效", nil)
	case errors.Is(err, worldstate.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_NOT_MANAGED", "请先接管房间", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		NotFound(c)
	default:
		Failure(c, http.StatusInternalServerError, "WORLD_STATE_FAILED", "世界状态操作失败", nil)
	}
}
