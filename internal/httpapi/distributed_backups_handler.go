package httpapi

import (
	"context"
	"errors"
	"net/http"

	"dont/internal/distributedbackup"
	"dont/internal/jobs"

	"github.com/gin-gonic/gin"
)

type DistributedBackupService interface {
	List(string) ([]distributedbackup.Set, error)
	Get(string) (distributedbackup.Set, error)
	Operations(string) ([]distributedbackup.Operation, error)
	Operation(string) (distributedbackup.Operation, error)
	Create(context.Context, string, string, string, string) (distributedbackup.Set, error)
	Restore(context.Context, string, string, string) (distributedbackup.RestoreResult, error)
	RecoverOperation(context.Context, string) (distributedbackup.Operation, error)
}

type DistributedHotBackupService interface {
	CreateWithMode(context.Context, string, string, string, string, string) (distributedbackup.Set, error)
}

type DistributedBackupHandler struct {
	backups DistributedBackupService
	jobs    *jobs.Service
}

func NewDistributedBackupHandler(backups DistributedBackupService, jobs *jobs.Service) (*DistributedBackupHandler, error) {
	if backups == nil || jobs == nil {
		return nil, errors.New("distributed backup handler dependencies are required")
	}
	return &DistributedBackupHandler{backups: backups, jobs: jobs}, nil
}

func (h *DistributedBackupHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/rooms/:roomId/backup-sets", h.list)
	v2.POST("/rooms/:roomId/backup-sets", h.create)
	v2.GET("/rooms/:roomId/backup-operations", h.listOperations)
	v2.GET("/backup-sets/:backupSetId", h.get)
	v2.POST("/backup-sets/:backupSetId/actions/restore", h.restore)
	v2.POST("/backup-operations/:backupOperationId/actions/recover", h.recoverOperation)
}

func (h *DistributedBackupHandler) listOperations(c *gin.Context) {
	values, err := h.backups.Operations(c.Param("roomId"))
	if err != nil {
		distributedBackupFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": values, "total": len(values)})
}

func (h *DistributedBackupHandler) list(c *gin.Context) {
	values, err := h.backups.List(c.Param("roomId"))
	if err != nil {
		distributedBackupFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": values, "total": len(values)})
}

func (h *DistributedBackupHandler) get(c *gin.Context) {
	value, err := h.backups.Get(c.Param("backupSetId"))
	if err != nil {
		distributedBackupFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *DistributedBackupHandler) create(c *gin.Context) {
	var request distributedbackup.CreateRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&request); err != nil {
			Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的备份集配置", nil)
			return
		}
	}
	roomID := c.Param("roomId")
	mode := request.Mode
	if mode == "" {
		mode = distributedbackup.ModeCold
	}
	if mode != distributedbackup.ModeCold && mode != distributedbackup.ModeHot {
		Failure(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "备份模式无效", nil)
		return
	}
	job, err := h.jobs.SubmitFactory("backup-set.create", roomID, "", []jobs.TargetSpec{{ID: roomID, Name: "全部分片"}}, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			var value distributedbackup.Set
			var createErr error
			if mode == distributedbackup.ModeHot {
				service, ok := h.backups.(DistributedHotBackupService)
				if !ok {
					createErr = distributedbackup.ErrHotUnavailable
				} else {
					value, createErr = service.CreateWithMode(ctx, roomID, request.Name, "manual", job.ID, mode)
				}
			} else {
				value, createErr = h.backups.Create(ctx, roomID, request.Name, "manual", job.ID)
			}
			if createErr != nil {
				report(jobs.TargetResult{TargetID: roomID, Status: jobs.StatusFailed, Error: distributedBackupJobError(createErr)})
				return nil
			}
			report(jobs.TargetResult{TargetID: roomID, Status: jobs.StatusSucceeded, Message: "一致性备份集已创建：" + value.Name})
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建备份集任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *DistributedBackupHandler) restore(c *gin.Context) {
	var request distributedbackup.RestoreRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的恢复确认", nil)
		return
	}
	setID := c.Param("backupSetId")
	value, err := h.backups.Get(setID)
	if err != nil {
		distributedBackupFailure(c, err)
		return
	}
	if request.Confirmation != value.RoomName {
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入完整房间名确认恢复", nil)
		return
	}
	job, err := h.jobs.SubmitFactory("backup-set.restore", value.RoomID, "", []jobs.TargetSpec{{ID: setID, Name: value.Name}}, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			result, restoreErr := h.backups.Restore(ctx, setID, request.Confirmation, job.ID)
			if restoreErr != nil {
				report(jobs.TargetResult{TargetID: setID, Status: jobs.StatusFailed, Error: distributedBackupJobError(restoreErr)})
				return nil
			}
			message := "备份集恢复完成；保护备份集：" + result.ProtectionSetID
			if len(result.Warnings) > 0 {
				message += "；部分清理将在后台重试"
			}
			report(jobs.TargetResult{TargetID: setID, Status: jobs.StatusSucceeded, Message: message})
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建备份集恢复任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *DistributedBackupHandler) recoverOperation(c *gin.Context) {
	operationID := c.Param("backupOperationId")
	operation, err := h.backups.Operation(operationID)
	if err != nil {
		distributedBackupFailure(c, err)
		return
	}
	job, err := h.jobs.SubmitFactory("backup-set.recover", operation.RoomID, "", []jobs.TargetSpec{{ID: operation.ID, Name: operation.Kind}}, func(jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			recovered, recoverErr := h.backups.RecoverOperation(ctx, operation.ID)
			if recoverErr != nil {
				report(jobs.TargetResult{TargetID: operation.ID, Status: jobs.StatusFailed, Error: distributedBackupJobError(recoverErr)})
				return nil
			}
			report(jobs.TargetResult{TargetID: operation.ID, Status: jobs.StatusSucceeded, Message: "备份恢复操作已完成：" + string(recovered.Status)})
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建备份恢复任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func distributedBackupJobError(err error) *jobs.Error {
	code := "BACKUP_SET_OPERATION_FAILED"
	switch {
	case errors.Is(err, distributedbackup.ErrTopologyChanged):
		code = "BACKUP_TOPOLOGY_CHANGED"
	case errors.Is(err, distributedbackup.ErrTargetUnavailable):
		code = "BACKUP_TARGET_UNAVAILABLE"
	case errors.Is(err, distributedbackup.ErrIntegrity):
		code = "BACKUP_INTEGRITY_FAILED"
	case errors.Is(err, distributedbackup.ErrSharedFilesDiffer):
		code = "BACKUP_SHARED_FILES_DIFFER"
	case errors.Is(err, distributedbackup.ErrIncomplete):
		code = "BACKUP_SET_INCOMPLETE"
	case errors.Is(err, distributedbackup.ErrRecoveryIncomplete):
		code = "BACKUP_RECOVERY_REQUIRED"
	case errors.Is(err, distributedbackup.ErrHotUnavailable):
		code = "BACKUP_HOT_UNAVAILABLE"
	case errors.Is(err, distributedbackup.ErrBarrierFailed):
		code = "BACKUP_SNAPSHOT_BARRIER_FAILED"
	}
	return &jobs.Error{Code: code, Message: err.Error()}
}

func distributedBackupFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, distributedbackup.ErrNotFound):
		Failure(c, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "备份集不存在", nil)
	case errors.Is(err, distributedbackup.ErrInvalidInput):
		Failure(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "备份集请求无效", nil)
	case errors.Is(err, distributedbackup.ErrTopologyChanged), errors.Is(err, distributedbackup.ErrTargetUnavailable):
		Failure(c, http.StatusConflict, "BACKUP_TOPOLOGY_UNAVAILABLE", err.Error(), nil)
	case errors.Is(err, distributedbackup.ErrIntegrity), errors.Is(err, distributedbackup.ErrIncomplete):
		Failure(c, http.StatusUnprocessableEntity, "BACKUP_SET_INVALID", err.Error(), nil)
	case errors.Is(err, distributedbackup.ErrHotUnavailable), errors.Is(err, distributedbackup.ErrBarrierFailed):
		Failure(c, http.StatusConflict, "BACKUP_HOT_UNAVAILABLE", err.Error(), nil)
	default:
		Failure(c, http.StatusInternalServerError, "BACKUP_SET_OPERATION_FAILED", "备份集操作失败", nil)
	}
}
