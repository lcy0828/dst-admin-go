package httpapi

import (
	"context"
	"errors"
	"net/http"

	"dont/internal/configuration"
	"dont/internal/jobs"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
)

type ConfigurationHandler struct {
	configuration *configuration.Service
	jobs          *jobs.Service
}

func NewConfigurationHandler(service *configuration.Service, jobService *jobs.Service) *ConfigurationHandler {
	return &ConfigurationHandler{configuration: service, jobs: jobService}
}

func (h *ConfigurationHandler) Register(v2 *gin.RouterGroup) {
	room := v2.Group("/rooms/:roomId")
	room.GET("/configuration", h.roomConfiguration)
	room.POST("/configuration/preview", h.previewRoomConfiguration)
	room.POST("/configuration/actions/apply", h.applyRoomConfiguration)
	room.GET("/access", h.access)
	room.POST("/access/preview", h.previewAccess)
	room.POST("/access/actions/apply", h.applyAccess)
	room.GET("/cluster-token", h.tokenStatus)
	room.POST("/cluster-token/reveal", h.revealToken)
	room.POST("/cluster-token/preview", h.previewToken)
	room.POST("/cluster-token/actions/apply", h.applyToken)
	room.GET("/worlds/:worldId/configuration", h.worldConfiguration)
	room.POST("/worlds/:worldId/configuration/preview", h.previewWorldConfiguration)
	room.POST("/worlds/:worldId/configuration/actions/apply", h.applyWorldConfiguration)
}

func (h *ConfigurationHandler) roomConfiguration(c *gin.Context) {
	value, err := h.configuration.RoomConfig(c.Param("roomId"))
	if err != nil {
		configurationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ConfigurationHandler) previewRoomConfiguration(c *gin.Context) {
	var request configuration.RoomUpdateRequest
	if !bindConfigurationJSON(c, &request) {
		return
	}
	value, err := h.configuration.PreviewRoom(c.Param("roomId"), request)
	if err != nil {
		configurationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ConfigurationHandler) applyRoomConfiguration(c *gin.Context) {
	var request configuration.RoomUpdateRequest
	if !bindConfigurationJSON(c, &request) {
		return
	}
	roomID := c.Param("roomId")
	if _, err := h.configuration.PreviewRoom(roomID, request); err != nil {
		configurationFailure(c, err)
		return
	}
	h.submitConfiguration(c, "configuration.room.apply", roomID, "", "cluster.ini", func(ctx context.Context, jobID string) (configuration.ApplyResult, error) {
		return h.configuration.ApplyRoom(ctx, jobID, roomID, request)
	})
}

func (h *ConfigurationHandler) worldConfiguration(c *gin.Context) {
	value, err := h.configuration.WorldConfig(c.Param("roomId"), c.Param("worldId"))
	if err != nil {
		configurationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ConfigurationHandler) previewWorldConfiguration(c *gin.Context) {
	var request configuration.WorldUpdateRequest
	if !bindConfigurationJSON(c, &request) {
		return
	}
	value, err := h.configuration.PreviewWorld(c.Param("roomId"), c.Param("worldId"), request)
	if err != nil {
		configurationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ConfigurationHandler) applyWorldConfiguration(c *gin.Context) {
	var request configuration.WorldUpdateRequest
	if !bindConfigurationJSON(c, &request) {
		return
	}
	roomID, worldID := c.Param("roomId"), c.Param("worldId")
	if _, err := h.configuration.PreviewWorld(roomID, worldID, request); err != nil {
		configurationFailure(c, err)
		return
	}
	h.submitConfiguration(c, "configuration.world.apply", roomID, worldID, "server.ini + leveldataoverride.lua", func(ctx context.Context, jobID string) (configuration.ApplyResult, error) {
		return h.configuration.ApplyWorld(ctx, jobID, roomID, worldID, request)
	})
}

func (h *ConfigurationHandler) access(c *gin.Context) {
	value, err := h.configuration.AccessLists(c.Param("roomId"))
	if err != nil {
		configurationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ConfigurationHandler) previewAccess(c *gin.Context) {
	var request configuration.AccessUpdateRequest
	if !bindConfigurationJSON(c, &request) {
		return
	}
	value, err := h.configuration.PreviewAccess(c.Param("roomId"), request)
	if err != nil {
		configurationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ConfigurationHandler) applyAccess(c *gin.Context) {
	var request configuration.AccessUpdateRequest
	if !bindConfigurationJSON(c, &request) {
		return
	}
	roomID := c.Param("roomId")
	if _, err := h.configuration.ValidateAccessApply(roomID, request); err != nil {
		configurationFailure(c, err)
		return
	}
	h.submitConfiguration(c, "configuration.access.apply", roomID, "", "访问名单", func(ctx context.Context, jobID string) (configuration.ApplyResult, error) {
		return h.configuration.ApplyAccess(ctx, jobID, roomID, request)
	})
}

func (h *ConfigurationHandler) tokenStatus(c *gin.Context) {
	value, err := h.configuration.TokenStatus(c.Param("roomId"))
	if err != nil {
		configurationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ConfigurationHandler) revealToken(c *gin.Context) {
	var request configuration.TokenRevealRequest
	if !bindConfigurationJSON(c, &request) {
		return
	}
	value, err := h.configuration.RevealToken(c.Param("roomId"), request.Confirmation)
	if err != nil {
		configurationFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func (h *ConfigurationHandler) previewToken(c *gin.Context) {
	var request configuration.TokenUpdateRequest
	if !bindConfigurationJSON(c, &request) {
		return
	}
	value, err := h.configuration.PreviewToken(c.Param("roomId"), request)
	if err != nil {
		configurationFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func (h *ConfigurationHandler) applyToken(c *gin.Context) {
	var request configuration.TokenUpdateRequest
	if !bindConfigurationJSON(c, &request) {
		return
	}
	roomID := c.Param("roomId")
	if _, err := h.configuration.PreviewToken(roomID, request); err != nil {
		configurationFailure(c, err)
		return
	}
	h.submitConfiguration(c, "configuration.token.apply", roomID, "", "Cluster Token", func(ctx context.Context, jobID string) (configuration.ApplyResult, error) {
		return h.configuration.ApplyToken(ctx, jobID, roomID, request)
	})
}

type configurationApply func(context.Context, string) (configuration.ApplyResult, error)

func (h *ConfigurationHandler) submitConfiguration(c *gin.Context, kind, roomID, worldID, targetName string, apply configurationApply) {
	targetID := roomID
	if worldID != "" {
		targetID = worldID
	}
	targets := []jobs.TargetSpec{{ID: targetID, Name: targetName}}
	job, err := h.jobs.SubmitFactory(kind, roomID, worldID, targets, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			result, applyErr := apply(ctx, job.ID)
			if applyErr != nil {
				report(jobs.TargetResult{TargetID: targetID, Status: jobs.StatusFailed, Error: configurationJobError(applyErr)})
				return nil
			}
			report(jobs.TargetResult{TargetID: targetID, Status: jobs.StatusSucceeded, Message: "配置已应用；保护备份 ID：" + result.ProtectionBackupID})
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建配置任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func configurationJobError(err error) *jobs.Error {
	code := "CONFIGURATION_APPLY_FAILED"
	if errors.Is(err, configuration.ErrRevisionConflict) {
		code = "CONFIG_REVISION_CONFLICT"
	}
	if errors.Is(err, context.Canceled) {
		code = "JOB_CANCELED"
	}
	return &jobs.Error{Code: code, Message: err.Error()}
}

func bindConfigurationJSON(c *gin.Context, target interface{}) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的配置", nil)
		return false
	}
	return true
}

func configurationFailure(c *gin.Context, err error) {
	var fieldError *configuration.FieldError
	var conflict *configuration.RevisionConflictError
	switch {
	case errors.As(err, &fieldError):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_CONFIGURATION", "配置字段无效", gin.H{"fields": fieldError.Fields})
	case errors.As(err, &conflict):
		Failure(c, http.StatusConflict, "CONFIG_REVISION_CONFLICT", "配置已被其他操作修改，请刷新后重试", gin.H{"currentRevision": conflict.CurrentRevision})
	case errors.Is(err, configuration.ErrNoChanges):
		Failure(c, http.StatusConflict, "NO_CONFIGURATION_CHANGES", "配置没有变化", nil)
	case errors.Is(err, configuration.ErrConfirmationNeeded):
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入完整房间名称确认此操作", nil)
	case errors.Is(err, configuration.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_NOT_MANAGED", "接管房间后才能修改配置", nil)
	case errors.Is(err, configuration.ErrUnsafePath), errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_CONFIGURATION_PATH", "配置路径无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间或世界不存在", nil)
	default:
		Failure(c, http.StatusInternalServerError, "CONFIGURATION_OPERATION_FAILED", "配置操作失败", nil)
	}
}
