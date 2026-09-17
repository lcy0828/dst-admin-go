package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"

	"dont/internal/jobs"
	"dont/internal/modcontrol"
	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/topology"

	"github.com/gin-gonic/gin"
)

type ModPublicationService interface {
	Preview(context.Context, string, modcontrol.Request) (modpublication.Plan, error)
	Publish(context.Context, string, string, modcontrol.Request) (modpublication.Publication, error)
	Retry(context.Context, string, string) (modpublication.Publication, error)
	Activate(context.Context, string, string, modpublication.ActivationPolicy) (modpublication.Publication, error)
	Get(string) (modpublication.Publication, error)
	List(string, int, int) (modcontrol.ListResult, error)
}

type ModPublicationHandler struct {
	service  ModPublicationService
	replicas ModReplicaReader
	jobs     *jobs.Service
}

type ModReplicaReader interface {
	RoomReplicas(string) (modpublication.RoomReplicaState, error)
}

func NewModPublicationHandler(service ModPublicationService, jobService *jobs.Service) (*ModPublicationHandler, error) {
	if service == nil || jobService == nil {
		return nil, errors.New("mod publication handler dependencies are required")
	}
	return &ModPublicationHandler{service: service, jobs: jobService}, nil
}

func (h *ModPublicationHandler) ConfigureReplicaReader(reader ModReplicaReader) {
	h.replicas = reader
}

func (h *ModPublicationHandler) Register(v2 *gin.RouterGroup) {
	v2.POST("/rooms/:roomId/mod-publications/preview", h.preview)
	v2.POST("/rooms/:roomId/mod-publications", h.publish)
	v2.GET("/rooms/:roomId/mod-publications", h.list)
	v2.GET("/rooms/:roomId/mod-replicas", h.roomReplicas)
	v2.GET("/mod-publications/:publicationId", h.get)
	v2.POST("/mod-publications/:publicationId/actions/retry-failed", h.retry)
	v2.POST("/mod-publications/:publicationId/actions/activate", h.activate)
}

func (h *ModPublicationHandler) roomReplicas(c *gin.Context) {
	if h.replicas == nil {
		Failure(c, http.StatusServiceUnavailable, "MOD_REPLICA_STATE_UNAVAILABLE", "Mod 节点状态服务尚未就绪", nil)
		return
	}
	value, err := h.replicas.RoomReplicas(c.Param("roomId"))
	if err != nil {
		modPublicationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ModPublicationHandler) preview(c *gin.Context) {
	var request modcontrol.Request
	if !bindModJSON(c, &request) {
		return
	}
	policy, err := modpublication.NormalizeActivationPolicy(request.Activation)
	if err != nil {
		modPublicationFailure(c, err)
		return
	}
	request.Activation = policy
	plan, err := h.service.Preview(c.Request.Context(), c.Param("roomId"), request)
	if err != nil {
		modPublicationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, plan)
}

func (h *ModPublicationHandler) publish(c *gin.Context) {
	var request modcontrol.Request
	if !bindModJSON(c, &request) {
		return
	}
	policy, err := modpublication.NormalizeActivationPolicy(request.Activation)
	if err != nil {
		modPublicationFailure(c, err)
		return
	}
	request.Activation = policy
	roomID := c.Param("roomId")
	plan, err := h.service.Preview(c.Request.Context(), roomID, request)
	if err != nil {
		modPublicationFailure(c, err)
		return
	}
	if request.PlanHash != plan.PlanHash {
		modPublicationFailure(c, modpublication.ErrPlanChanged)
		return
	}
	if request.Confirmation != plan.PlanHash {
		modPublicationFailure(c, modcontrol.ErrConfirmation)
		return
	}
	if !plan.Ready {
		modPublicationFailure(c, modpublication.ErrPreviewBlocked)
		return
	}
	targets := make([]jobs.TargetSpec, 0, len(plan.Targets))
	for _, target := range plan.Targets {
		targets = append(targets, jobs.TargetSpec{ID: publicationJobTarget(target.TargetID, target.InstallationID), Name: target.TargetID + " / " + target.InstallationID})
		if request.Activation.Mode == modpublication.ActivationModeRestart {
			for _, world := range target.Worlds {
				targets = append(targets, jobs.TargetSpec{ID: publicationActivationJobTarget(world.RoomID, world.WorldID), Name: "激活 · " + world.RoomDirectory + " / " + world.WorldDirectory})
			}
		}
	}
	job, err := h.jobs.SubmitFactory("mod.publication", roomID, "", targets, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			ctx = attachModJobProgress(ctx, h.jobs, job.ID)
			publication, publishErr := h.service.Publish(ctx, job.ID, roomID, request)
			for _, target := range plan.Targets {
				report(publicationTargetJobResult(publication, target.TargetID, target.InstallationID, publishErr, false, "Mod 已原子发布；重启分片后生效"))
			}
			if request.Activation.Mode == modpublication.ActivationModeRestart {
				for _, target := range plan.Targets {
					for _, world := range target.Worlds {
						report(publicationActivationJobResult(publication, world.RoomID, world.WorldID, publishErr))
					}
				}
			}
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建 Mod 发布任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *ModPublicationHandler) activate(c *gin.Context) {
	var policy modpublication.ActivationPolicy
	if !bindModJSON(c, &policy) {
		return
	}
	policy, err := modpublication.NormalizeActivationPolicy(policy)
	if err != nil || policy.Mode != modpublication.ActivationModeRestart {
		modPublicationFailure(c, modpublication.ErrInvalidInput)
		return
	}
	publicationID := c.Param("publicationId")
	current, err := h.service.Get(publicationID)
	if err != nil {
		modPublicationFailure(c, err)
		return
	}
	targets := make([]jobs.TargetSpec, 0)
	for _, target := range current.Plan.Targets {
		for _, world := range target.Worlds {
			targets = append(targets, jobs.TargetSpec{ID: publicationActivationJobTarget(world.RoomID, world.WorldID), Name: "激活 · " + world.RoomDirectory + " / " + world.WorldDirectory})
		}
	}
	job, err := h.jobs.SubmitFactory("mod.publication.activation", current.RoomID, "", targets, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			ctx = attachModJobProgress(ctx, h.jobs, job.ID)
			publication, activationErr := h.service.Activate(ctx, job.ID, publicationID, policy)
			for _, target := range current.Plan.Targets {
				for _, world := range target.Worlds {
					report(publicationActivationJobResult(publication, world.RoomID, world.WorldID, activationErr))
				}
			}
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建 Mod 激活任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *ModPublicationHandler) retry(c *gin.Context) {
	publicationID := c.Param("publicationId")
	current, err := h.service.Get(publicationID)
	if err != nil {
		modPublicationFailure(c, err)
		return
	}
	targets := make([]jobs.TargetSpec, 0, len(current.Plan.Targets))
	for _, target := range current.Plan.Targets {
		targets = append(targets, jobs.TargetSpec{ID: publicationJobTarget(target.TargetID, target.InstallationID), Name: target.TargetID + " / " + target.InstallationID})
	}
	job, err := h.jobs.SubmitFactory("mod.publication.retry", current.RoomID, "", targets, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			ctx = attachModJobProgress(ctx, h.jobs, job.ID)
			publication, retryErr := h.service.Retry(ctx, job.ID, publicationID)
			for _, target := range current.Plan.Targets {
				report(publicationTargetJobResult(publication, target.TargetID, target.InstallationID, retryErr, true, "Mod 发布恢复已完成"))
			}
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建 Mod 发布恢复任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *ModPublicationHandler) get(c *gin.Context) {
	value, err := h.service.Get(c.Param("publicationId"))
	if err != nil {
		modPublicationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ModPublicationHandler) list(c *gin.Context) {
	limit, limitErr := strconv.Atoi(c.DefaultQuery("limit", "10"))
	offset, offsetErr := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limitErr != nil || offsetErr != nil || limit < 1 || limit > 100 || offset < 0 {
		Failure(c, http.StatusUnprocessableEntity, "INVALID_PAGINATION", "分页参数无效", nil)
		return
	}
	value, err := h.service.List(c.Param("roomId"), limit, offset)
	if err != nil {
		modPublicationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func modPublicationFailure(c *gin.Context, err error) {
	status, code, message := http.StatusInternalServerError, "MOD_PUBLICATION_FAILED", "Mod 发布操作失败"
	switch {
	case errors.Is(err, modpublication.ErrNotFound):
		status, code, message = http.StatusNotFound, "MOD_PUBLICATION_NOT_FOUND", "Mod 发布记录不存在"
	case errors.Is(err, modcontrol.ErrInvalidRequest), errors.Is(err, modpublication.ErrInvalidInput), errors.Is(err, mods.ErrInvalidRequest):
		status, code, message = http.StatusUnprocessableEntity, "INVALID_MOD_PUBLICATION", "Mod 发布请求无效"
	case errors.Is(err, modcontrol.ErrConfirmation):
		status, code, message = http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "必须使用当前 planHash 确认发布"
	case errors.Is(err, modcontrol.ErrTopologyChanged), errors.Is(err, modpublication.ErrTopologyChanged), errors.Is(err, topology.ErrRevisionConflict):
		status, code, message = http.StatusConflict, "TOPOLOGY_CHANGED", "运行拓扑已变化，请重新预览"
	case errors.Is(err, modpublication.ErrPlanChanged):
		status, code, message = http.StatusConflict, "PLAN_CHANGED", "Mod 发布计划已变化，请重新预览"
	case errors.Is(err, modpublication.ErrPreviewBlocked):
		status, code, message = http.StatusConflict, "PREVIEW_BLOCKED", "Mod 发布预检存在阻断项"
	case errors.Is(err, modpublication.ErrIdempotencyConflict):
		status, code, message = http.StatusConflict, "MOD_PUBLICATION_IDEMPOTENCY_CONFLICT", "Mod 发布幂等键已用于其他计划"
	case errors.Is(err, modpublication.ErrRecoveryRequired):
		status, code, message = http.StatusConflict, "MOD_PUBLICATION_RECOVERY_REQUIRED", "Mod 发布需要继续恢复"
	case errors.Is(err, modpublication.ErrActivationState):
		status, code, message = http.StatusConflict, "MOD_ACTIVATION_STATE_INVALID", "当前 Mod 发布状态不允许激活"
	case errors.Is(err, modpublication.ErrActivationFailed):
		status, code, message = http.StatusConflict, "MOD_ACTIVATION_FAILED", "Mod 已发布，但分片激活未全部完成"
	case errors.Is(err, modpublication.ErrVersionConflict):
		status, code, message = http.StatusConflict, "MOD_VERSION_CONFLICT", "同一安装中的模组版本要求冲突"
	case errors.Is(err, modpublication.ErrConflict), errors.Is(err, operationlease.ErrBusy), errors.Is(err, modcontrol.ErrPublicationState):
		status, code, message = http.StatusConflict, "MOD_PUBLICATION_CONFLICT", "另一个操作正在修改相同房间或安装"
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		status, code, message = http.StatusNotFound, "MOD_TARGET_NOT_FOUND", "房间或世界不存在"
	case errors.Is(err, mods.ErrRevisionConflict):
		status, code, message = http.StatusConflict, "CONFIG_REVISION_CONFLICT", "Mod 配置已变化，请重新加载"
	}
	Failure(c, status, code, message, gin.H{"reason": err.Error()})
}

func publicationJobTarget(targetID, installationID string) string {
	digest := sha256.Sum256([]byte(targetID + "\x00" + installationID))
	return "mod-target-" + hex.EncodeToString(digest[:])
}

func publicationActivationJobTarget(roomID, worldID string) string {
	digest := sha256.Sum256([]byte(roomID + "\x00" + worldID))
	return "mod-activation-" + hex.EncodeToString(digest[:])
}

func publicationJobError(err error) *jobs.Error {
	if err == nil {
		return &jobs.Error{Code: "MOD_PUBLICATION_FAILED", Message: "Mod 发布未成功完成"}
	}
	code := "MOD_PUBLICATION_FAILED"
	switch {
	case errors.Is(err, modcontrol.ErrConfirmation):
		code = "CONFIRMATION_REQUIRED"
	case errors.Is(err, modcontrol.ErrTopologyChanged), errors.Is(err, modpublication.ErrTopologyChanged), errors.Is(err, topology.ErrRevisionConflict):
		code = "TOPOLOGY_CHANGED"
	case errors.Is(err, modpublication.ErrPlanChanged):
		code = "PLAN_CHANGED"
	case errors.Is(err, modpublication.ErrPreviewBlocked):
		code = "PREVIEW_BLOCKED"
	case errors.Is(err, modpublication.ErrIdempotencyConflict):
		code = "MOD_PUBLICATION_IDEMPOTENCY_CONFLICT"
	case errors.Is(err, modpublication.ErrRecoveryRequired):
		code = "MOD_PUBLICATION_RECOVERY_REQUIRED"
	case errors.Is(err, modpublication.ErrActivationFailed):
		code = "MOD_ACTIVATION_FAILED"
	case errors.Is(err, modpublication.ErrActivationState):
		code = "MOD_ACTIVATION_STATE_INVALID"
	case errors.Is(err, modpublication.ErrVersionConflict):
		code = "MOD_VERSION_CONFLICT"
	case errors.Is(err, modpublication.ErrConflict), errors.Is(err, operationlease.ErrBusy), errors.Is(err, modcontrol.ErrPublicationState):
		code = "MOD_PUBLICATION_CONFLICT"
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		code = "MOD_TARGET_NOT_FOUND"
	case errors.Is(err, mods.ErrRevisionConflict):
		code = "CONFIG_REVISION_CONFLICT"
	case errors.Is(err, modcontrol.ErrInvalidRequest), errors.Is(err, modpublication.ErrInvalidInput), errors.Is(err, mods.ErrInvalidRequest):
		code = "INVALID_MOD_PUBLICATION"
	}
	return &jobs.Error{Code: code, Message: err.Error()}
}

func publicationTargetJobResult(publication modpublication.Publication, targetID, installationID string, operationErr error, allowRolledBack bool, successMessage string) jobs.TargetResult {
	id := publicationJobTarget(targetID, installationID)
	for _, target := range publication.Targets {
		if target.TargetID != targetID || target.InstallationID != installationID {
			continue
		}
		targetSucceeded := target.Status == modpublication.StatusSucceeded ||
			(allowRolledBack && publication.Status == modpublication.StatusRolledBack && target.Status == modpublication.StatusRolledBack)
		if targetSucceeded && (operationErr == nil || errors.Is(operationErr, modpublication.ErrActivationFailed) || publicationHasIncompleteTarget(publication)) {
			return jobs.TargetResult{TargetID: id, Status: jobs.StatusSucceeded, Message: successMessage}
		}
		resultError := publicationJobError(operationErr)
		if code := firstNonEmpty(target.ErrorCode, publication.ErrorCode); code != "" {
			resultError.Code = code
		}
		if message := firstNonEmpty(target.ErrorMessage, publication.ErrorMessage); message != "" {
			resultError.Message = message
		}
		return jobs.TargetResult{TargetID: id, Status: jobs.StatusFailed, Error: resultError}
	}
	return jobs.TargetResult{TargetID: id, Status: jobs.StatusFailed, Error: publicationJobError(operationErr)}
}

func publicationActivationJobResult(publication modpublication.Publication, roomID, worldID string, operationErr error) jobs.TargetResult {
	id := publicationActivationJobTarget(roomID, worldID)
	for _, shard := range publication.Activation.Shards {
		if shard.RoomID != roomID || shard.WorldID != worldID {
			continue
		}
		switch shard.Status {
		case modpublication.ActivationStatusSucceeded:
			return jobs.TargetResult{TargetID: id, Status: jobs.StatusSucceeded, Message: "分片已重启并确认加载完成"}
		case modpublication.ActivationStatusSkipped:
			return jobs.TargetResult{TargetID: id, Status: jobs.StatusSucceeded, Message: "分片原本未运行，无需重启"}
		default:
			resultError := publicationJobError(operationErr)
			if shard.ErrorCode != "" {
				resultError.Code = shard.ErrorCode
			}
			if shard.ErrorMessage != "" {
				resultError.Message = shard.ErrorMessage
			}
			return jobs.TargetResult{TargetID: id, Status: jobs.StatusFailed, Error: resultError}
		}
	}
	return jobs.TargetResult{TargetID: id, Status: jobs.StatusFailed, Error: publicationJobError(operationErr)}
}

func publicationHasIncompleteTarget(publication modpublication.Publication) bool {
	for _, target := range publication.Targets {
		if target.Status != modpublication.StatusSucceeded {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
