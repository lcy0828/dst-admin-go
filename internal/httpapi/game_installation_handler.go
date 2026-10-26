package httpapi

import (
	"dont/internal/gameinstall"
	"dont/shared"
	"github.com/gin-gonic/gin"
	"net/http"
)

type GameInstallationHandler struct {
	service  *gameinstall.Service
	releases GameReleaseService
}

func (h *GameInstallationHandler) ConfigureReleases(releases GameReleaseService) {
	h.releases = releases
}

func NewGameInstallationHandler(service *gameinstall.Service) *GameInstallationHandler {
	return &GameInstallationHandler{service: service}
}
func (h *GameInstallationHandler) Register(v2 *gin.RouterGroup) {
	if h.releases != nil {
		v2.POST("/runtime-targets/game-installations/preview", h.preview)
	}
	v2.GET("/runtime-targets/game-installations", func(c *gin.Context) {
		items, err := h.service.Catalog(c.Request.Context(), c.Query("targetId"))
		if err != nil {
			gameInstallationFailure(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		Success(c, http.StatusOK, items)
	})
	v2.POST("/runtime-targets/game-installations/probe", func(c *gin.Context) {
		var input gameInstallationInput
		if c.ShouldBindJSON(&input) != nil || input.Path == "" {
			Failure(c, 400, "INVALID_GAME_INSTALLATION", "请选择机器、安装位置并填写已有目录", nil)
			return
		}
		r, err := h.service.Probe(c.Request.Context(), input.TargetID, input.InstallationID, input.Path)
		if err != nil {
			gameInstallationFailure(c, err)
			return
		}
		Success(c, 200, r)
	})
	v2.POST("/runtime-targets/game-installations/actions/install", h.submit(false))
	v2.POST("/runtime-targets/game-installations/actions/adopt", h.submit(true))
	v2.GET("/runtime-targets/game-installations/steam-login", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		status, err := h.service.SteamLogin(c.Request.Context(), c.Query("targetId"), c.Query("installationId"), shared.SteamLoginRequest{Action: "status"})
		if err != nil {
			gameInstallationFailure(c, err)
			return
		}
		Success(c, 200, status)
	})
	v2.POST("/runtime-targets/game-installations/steam-login", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		var input struct {
			TargetID       string `json:"targetId" binding:"required"`
			InstallationID string `json:"installationId" binding:"required"`
			shared.SteamLoginRequest
		}
		if c.ShouldBindJSON(&input) != nil || input.Action == "status" {
			Failure(c, 400, "INVALID_STEAM_LOGIN", "Steam 登录请求无效", nil)
			return
		}
		status, err := h.service.SteamLogin(c.Request.Context(), input.TargetID, input.InstallationID, input.SteamLoginRequest)
		if err != nil {
			gameInstallationFailure(c, err)
			return
		}
		Success(c, 200, status)
	})
}

type gameInstallationInput struct {
	TargetID       string `json:"targetId" binding:"required"`
	InstallationID string `json:"installationId" binding:"required"`
	Path           string `json:"path"`
	Fingerprint    string `json:"fingerprint"`
}

func (h *GameInstallationHandler) submit(adopt bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input gameInstallationInput
		if c.ShouldBindJSON(&input) != nil {
			Failure(c, 400, "INVALID_GAME_INSTALLATION", "请选择机器和安装位置", nil)
			return
		}
		job, err := h.service.Submit(input.TargetID, input.InstallationID, adopt, shared.GameInstallationRequest{Path: input.Path, Fingerprint: input.Fingerprint})
		if err != nil {
			gameInstallationFailure(c, err)
			return
		}
		Success(c, http.StatusAccepted, job)
	}
}
func gameInstallationFailure(c *gin.Context, err error) {
	Failure(c, http.StatusUnprocessableEntity, "GAME_INSTALLATION_FAILED", err.Error(), nil)
}
