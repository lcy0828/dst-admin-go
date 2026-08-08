package httpapi

import (
	"errors"
	"net/http"

	"dont/internal/gameupdate"
	"dont/internal/jobs"

	"github.com/gin-gonic/gin"
)

type GameUpdateHandler struct {
	updates *gameupdate.Service
	jobs    *jobs.Service
}

func NewGameUpdateHandler(updates *gameupdate.Service, jobService *jobs.Service) *GameUpdateHandler {
	return &GameUpdateHandler{updates: updates, jobs: jobService}
}

func (h *GameUpdateHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/game/version", h.version)
	v2.POST("/game/actions/update", h.update)
	v2.GET("/game/update-runs/:jobId", h.run)
}

func (h *GameUpdateHandler) version(c *gin.Context) {
	Success(c, http.StatusOK, h.updates.Version(c.Request.Context()))
}

func (h *GameUpdateHandler) update(c *gin.Context) {
	var request gameupdate.UpdateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的更新配置", nil)
		return
	}
	targets, factory, release, err := h.updates.Prepare(c.Request.Context(), request)
	if err != nil {
		gameUpdateFailure(c, err)
		return
	}
	job, err := h.jobs.SubmitFactory("game.update", "", "", targets, factory)
	if err != nil {
		release()
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建游戏更新任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *GameUpdateHandler) run(c *gin.Context) {
	value, err := h.updates.Run(c.Param("jobId"))
	if err != nil {
		gameUpdateFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func gameUpdateFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, gameupdate.ErrRunNotFound):
		Failure(c, http.StatusNotFound, "UPDATE_RUN_NOT_FOUND", "更新记录不存在或尚未开始", nil)
	case errors.Is(err, gameupdate.ErrConfirmationNeeded):
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入“更新游戏”确认操作", nil)
	case errors.Is(err, gameupdate.ErrUpdateInProgress):
		Failure(c, http.StatusConflict, "UPDATE_IN_PROGRESS", "已有游戏更新正在执行", nil)
	case errors.Is(err, gameupdate.ErrSteamCMDUnavailable):
		Failure(c, http.StatusConflict, "STEAMCMD_UNAVAILABLE", "SteamCMD 不可用，请先完成部署检查", nil)
	case errors.Is(err, gameupdate.ErrSteamClientManaged):
		Failure(c, http.StatusConflict, "STEAM_CLIENT_MANAGED", "当前游戏由 Steam 客户端管理，请在 Steam 中完成更新", nil)
	default:
		Failure(c, http.StatusInternalServerError, "GAME_UPDATE_FAILED", "游戏更新操作失败", nil)
	}
}
