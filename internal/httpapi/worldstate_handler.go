package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"

	"dont/internal/dstruntime"
	"dont/internal/requesttiming"
	"dont/internal/rooms"
	"dont/internal/worldstate"

	"github.com/gin-gonic/gin"
)

type WorldStateService interface {
	List(context.Context, string) (worldstate.List, error)
	History(string, string, int) (worldstate.History, error)
	WorldTargets(string) ([]rooms.World, error)
	RefreshWorld(context.Context, string, string) (worldstate.RefreshResult, error)
}

type WorldStateHandler struct {
	states WorldStateService
}

func NewWorldStateHandler(service WorldStateService) *WorldStateHandler {
	return &WorldStateHandler{states: service}
}

func (h *WorldStateHandler) Register(v2 *gin.RouterGroup) {
	states := v2.Group("/rooms/:roomId/world-states")
	states.GET("", h.list)
	states.GET("/history", h.history)
	states.POST("/:worldId/actions/refresh", h.refreshWorld)
	states.POST("/actions/refresh", h.refresh)
}

func (h *WorldStateHandler) list(c *gin.Context) {
	ctx, timing := requesttiming.New(c.Request.Context())
	value, err := h.states.List(ctx, c.Param("roomId"))
	finishReadTiming(c, timing, err)
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

func (h *WorldStateHandler) refreshWorld(c *gin.Context) {
	value, err := h.states.RefreshWorld(c.Request.Context(), c.Param("roomId"), c.Param("worldId"))
	if err != nil {
		worldStateFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *WorldStateHandler) refresh(c *gin.Context) {
	h.list(c)
}

func worldStateFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, worldstate.ErrInvalidFilter):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_WORLD_STATE_FILTER", "世界状态筛选参数无效", nil)
	case errors.Is(err, worldstate.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_UNAVAILABLE", "房间当前不可用，请检查运行节点与拓扑状态", nil)
	case errors.Is(err, worldstate.ErrWorldNotRunning):
		Failure(c, http.StatusConflict, "WORLD_NOT_RUNNING", "分片未运行，无法读取实时状态", nil)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, worldstate.ErrProbeTimedOut):
		Failure(c, http.StatusGatewayTimeout, "WORLD_STATE_PROBE_TIMEOUT", "等待世界状态探针输出超时", nil)
	case errors.Is(err, dstruntime.ErrRuntimeRefreshDeferred):
		Failure(c, http.StatusConflict, "WORLD_STATE_REFRESH_DEFERRED", "房间操作中，世界状态刷新暂缓", gin.H{"reason": err.Error()})
	case errors.Is(err, dstruntime.ErrRuntimeUnavailable):
		Failure(c, http.StatusServiceUnavailable, "WORLD_STATE_RUNTIME_UNAVAILABLE", "世界运行时暂不可用", gin.H{"reason": err.Error()})
	case errors.Is(err, dstruntime.ErrRuntimeRefresh):
		Failure(c, http.StatusBadGateway, "WORLD_STATE_REFRESH_FAILED", "世界状态刷新失败", gin.H{"reason": err.Error()})
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		NotFound(c)
	default:
		log.Printf("[WorldStateHTTP] request_id=%s method=%s path=%s error=%v", RequestID(c), c.Request.Method, c.Request.URL.Path, err)
		Failure(c, http.StatusInternalServerError, "WORLD_STATE_FAILED", "世界状态操作失败", gin.H{"reason": err.Error()})
	}
}
