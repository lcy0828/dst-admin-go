package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"dont/internal/agents"
	"dont/internal/configuration"
	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"

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
	value, err := h.configuration.RoomConfigContext(c.Request.Context(), c.Param("roomId"))
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
	value, err := h.configuration.PreviewRoomContext(c.Request.Context(), c.Param("roomId"), request)
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
	if _, err := h.configuration.PreviewRoomContext(c.Request.Context(), roomID, request); err != nil {
		configurationFailure(c, err)
		return
	}
	h.submitConfiguration(c, "configuration.room.apply", roomID, "", "cluster.ini", func(ctx context.Context, jobID string) (configuration.ApplyResult, error) {
		return h.configuration.ApplyRoom(ctx, jobID, roomID, request)
	})
}

func (h *ConfigurationHandler) worldConfiguration(c *gin.Context) {
	value, err := h.configuration.WorldConfigContext(c.Request.Context(), c.Param("roomId"), c.Param("worldId"))
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
	value, err := h.configuration.PreviewWorldContext(c.Request.Context(), c.Param("roomId"), c.Param("worldId"), request)
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
	if _, err := h.configuration.PreviewWorldContext(c.Request.Context(), roomID, worldID, request); err != nil {
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
	value, err := h.configuration.TokenStatusContext(c.Request.Context(), c.Param("roomId"))
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
	value, err := h.configuration.RevealTokenContext(c.Request.Context(), c.Param("roomId"), request.Confirmation)
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
	value, err := h.configuration.PreviewTokenContext(c.Request.Context(), c.Param("roomId"), request)
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
	if _, err := h.configuration.PreviewTokenContext(c.Request.Context(), roomID, request); err != nil {
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
			message := "配置已写入运行节点并完成磁盘回读校验"
			if result.ProtectionBackupID != "" {
				message += "；配置恢复点：" + result.ProtectionBackupID
			}
			if result.PublishedTargets > 0 {
				message += fmt.Sprintf("；已应用到 %d 个 Runtime 目标", result.PublishedTargets)
			}
			report(jobs.TargetResult{TargetID: targetID, Status: jobs.StatusSucceeded, Message: message})
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
	if agentUpgradeRequired(err) {
		code = "AGENT_UPGRADE_REQUIRED"
	} else if errors.Is(err, configuration.ErrRevisionConflict) {
		code = "CONFIG_REVISION_CONFLICT"
	} else if errors.Is(err, context.Canceled) {
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
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请确认后继续此操作", nil)
	case errors.Is(err, configuration.ErrTokenRevealUnavailable):
		Failure(c, http.StatusConflict, "TOKEN_REVEAL_UNAVAILABLE", "目标 Agent 版本不支持安全读取 Cluster Token，请升级 Agent 后重试", nil)
	case agentUpgradeRequired(err):
		Failure(c, http.StatusConflict, "AGENT_UPGRADE_REQUIRED", "目标 Agent 版本过旧，缺少配置管理所需能力，请升级 Agent 后重试", nil)
	case errors.Is(err, configuration.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_UNAVAILABLE", "房间当前不可用，请检查运行节点与拓扑状态", nil)
	case errors.Is(err, configuration.ErrUnsafePath), errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_CONFIGURATION_PATH", "配置路径无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间或世界不存在", nil)
	default:
		Failure(c, http.StatusInternalServerError, "CONFIGURATION_OPERATION_FAILED", "配置操作失败", nil)
	}
}

func agentUpgradeRequired(err error) bool {
	return errors.Is(err, agents.ErrUnsupportedAction) || errors.Is(err, runtimedriver.ErrCapabilityMissing)
}
