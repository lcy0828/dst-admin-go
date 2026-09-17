package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"dont/internal/gamenotifications"
	"dont/internal/jobs"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
)

type GameNotificationService interface {
	Prepare(string, string, gamenotifications.Source) (gamenotifications.SendPlan, error)
	MarkSubmissionFailed(string, error)
	List(gamenotifications.ListFilter) (gamenotifications.List, error)
	Policy(string) (gamenotifications.Policy, error)
	SavePolicy(string, gamenotifications.PolicyInput) (gamenotifications.Policy, error)
	PreviewMaintenance(context.Context, string) (gamenotifications.MaintenancePreview, error)
}

type GameNotificationHandler struct {
	service GameNotificationService
	jobs    *jobs.Service
}

func NewGameNotificationHandler(service GameNotificationService, jobService *jobs.Service) (*GameNotificationHandler, error) {
	if service == nil || jobService == nil {
		return nil, errors.New("game notification handler dependencies are required")
	}
	return &GameNotificationHandler{service: service, jobs: jobService}, nil
}

func (h *GameNotificationHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/game-notifications", h.list)
	v2.POST("/game-notifications", h.send)
	v2.GET("/rooms/:roomId/game-notification-policy", h.policy)
	v2.PUT("/rooms/:roomId/game-notification-policy", h.savePolicy)
	v2.GET("/rooms/:roomId/maintenance-preview", h.maintenancePreview)
}

func (h *GameNotificationHandler) maintenancePreview(c *gin.Context) {
	value, err := h.service.PreviewMaintenance(c.Request.Context(), c.Param("roomId"))
	if err != nil {
		gameNotificationFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func (h *GameNotificationHandler) list(c *gin.Context) {
	limit, limitErr := strconv.Atoi(c.DefaultQuery("limit", "25"))
	offset, offsetErr := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limitErr != nil || offsetErr != nil || limit < 1 || limit > 100 || offset < 0 {
		Failure(c, http.StatusBadRequest, "INVALID_PAGINATION", "分页参数无效", nil)
		return
	}
	result, err := h.service.List(gamenotifications.ListFilter{
		RoomID: strings.TrimSpace(c.Query("roomId")), Limit: limit, Offset: offset,
	})
	if err != nil {
		gameNotificationFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, result)
}

func (h *GameNotificationHandler) send(c *gin.Context) {
	var input gamenotifications.SendInput
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "游戏通知请求不是有效 JSON", nil)
		return
	}
	plan, err := h.service.Prepare(input.RoomID, input.Message, gamenotifications.SourceManual)
	if err != nil {
		gameNotificationFailure(c, err)
		return
	}
	job, err := h.jobs.SubmitFactory("game-notification.send", plan.Notification.RoomID, "", plan.Targets, func(job jobs.Job) jobs.Runner {
		return plan.Runner(job.ID)
	})
	if err != nil {
		h.service.MarkSubmissionFailed(plan.Notification.ID, err)
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建游戏通知任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *GameNotificationHandler) policy(c *gin.Context) {
	value, err := h.service.Policy(c.Param("roomId"))
	if err != nil {
		gameNotificationFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func (h *GameNotificationHandler) savePolicy(c *gin.Context) {
	var input gamenotifications.PolicyInput
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "操作通知策略不是有效 JSON", nil)
		return
	}
	value, err := h.service.SavePolicy(c.Param("roomId"), input)
	if err != nil {
		gameNotificationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func gameNotificationFailure(c *gin.Context, err error) {
	var fieldErr *gamenotifications.FieldError
	switch {
	case errors.As(err, &fieldErr):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_GAME_NOTIFICATION", "游戏通知参数校验失败", fieldErr.Fields)
	case errors.Is(err, gamenotifications.ErrRoomUnmanaged), errors.Is(err, rooms.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_UNAVAILABLE", "房间当前不可用，请检查运行节点与拓扑状态", nil)
	case errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_RESOURCE_ID", "房间标识无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间或世界不存在", nil)
	case errors.Is(err, gamenotifications.ErrNotFound):
		Failure(c, http.StatusNotFound, "GAME_NOTIFICATION_NOT_FOUND", "游戏通知记录不存在", nil)
	default:
		Failure(c, http.StatusInternalServerError, "GAME_NOTIFICATION_OPERATION_FAILED", "游戏通知操作失败", nil)
	}
}
