package httpapi

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	backupapi "dont/internal/backups"
	"dont/internal/jobs"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
)

type BackupHandler struct {
	backups *backupapi.Service
	jobs    *jobs.Service
}

func NewBackupHandler(service *backupapi.Service, jobService *jobs.Service) *BackupHandler {
	return &BackupHandler{backups: service, jobs: jobService}
}

func (h *BackupHandler) Register(v2 *gin.RouterGroup) {
	roomsGroup := v2.Group("/rooms/:roomId")
	roomsGroup.GET("/backups", h.list)
	roomsGroup.POST("/backups", h.create)
	roomsGroup.POST("/backups/upload", h.upload)
	roomsGroup.POST("/backups/actions/prune", h.prune)
	roomsGroup.GET("/backup-policy", h.policy)
	roomsGroup.PUT("/backup-policy", h.savePolicy)

	backupsGroup := v2.Group("/backups/:backupId")
	backupsGroup.GET("", h.get)
	backupsGroup.PATCH("", h.rename)
	backupsGroup.DELETE("", h.delete)
	backupsGroup.GET("/download", h.download)
	backupsGroup.POST("/actions/restore", h.restore)
}

func (h *BackupHandler) list(c *gin.Context) {
	items, err := h.backups.List(c.Param("roomId"))
	if err != nil {
		backupFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": len(items)})
}

func (h *BackupHandler) get(c *gin.Context) {
	value, err := h.backups.Get(c.Param("backupId"))
	if err != nil {
		backupFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *BackupHandler) create(c *gin.Context) {
	var request backupapi.CreateRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&request); err != nil {
			Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的备份配置", nil)
			return
		}
	}
	roomID := c.Param("roomId")
	if _, err := h.backups.List(roomID); err != nil {
		backupFailure(c, err)
		return
	}
	targets := []jobs.TargetSpec{{ID: roomID, Name: "房间存档"}}
	job, err := h.jobs.SubmitFactory("backup.create", roomID, "", targets, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			value, createErr := h.backups.Create(ctx, roomID, request.Name, backupapi.KindManual, job.ID)
			if createErr != nil {
				report(jobs.TargetResult{TargetID: roomID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "BACKUP_CREATE_FAILED", Message: createErr.Error()}})
				return nil
			}
			report(jobs.TargetResult{TargetID: roomID, Status: jobs.StatusSucceeded, Message: "备份已创建：" + value.Name})
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建备份任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *BackupHandler) upload(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, backupapi.MaxUploadSize+1024*1024)
	fileHeader, err := c.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			backupFailure(c, backupapi.ErrArchiveTooLarge)
			return
		}
		Failure(c, http.StatusBadRequest, "UPLOAD_REQUIRED", "请选择一个 ZIP 备份文件", nil)
		return
	}
	if !strings.EqualFold(filepath.Ext(fileHeader.Filename), ".zip") {
		Failure(c, http.StatusUnprocessableEntity, "INVALID_ARCHIVE", "备份文件必须为 ZIP", nil)
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		Failure(c, http.StatusBadRequest, "UPLOAD_READ_FAILED", "无法读取上传文件", nil)
		return
	}
	defer file.Close()
	value, err := h.backups.Import(c.Request.Context(), c.Param("roomId"), c.PostForm("name"), fileHeader.Filename, file)
	if err != nil {
		backupFailure(c, err)
		return
	}
	Success(c, http.StatusCreated, value)
}

func (h *BackupHandler) rename(c *gin.Context) {
	var request backupapi.RenameRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的备份名称", nil)
		return
	}
	value, err := h.backups.Rename(c.Param("backupId"), request.Name)
	if err != nil {
		backupFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *BackupHandler) delete(c *gin.Context) {
	var request backupapi.DeleteRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的删除确认", nil)
		return
	}
	value, err := h.backups.Get(c.Param("backupId"))
	if err != nil {
		backupFailure(c, err)
		return
	}
	if request.Confirmation != value.Name {
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入完整备份名称确认删除", nil)
		return
	}
	deleted, err := h.backups.Delete(value.ID)
	if err != nil {
		backupFailure(c, err)
		return
	}
	Success(c, http.StatusOK, deleted)
}

func (h *BackupHandler) download(c *gin.Context) {
	file, info, value, err := h.backups.Open(c.Param("backupId"))
	if err != nil {
		backupFailure(c, err)
		return
	}
	defer file.Close()
	name := value.Name + ".zip"
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	c.Header("Content-Type", "application/zip")
	c.Header("X-Content-Type-Options", "nosniff")
	http.ServeContent(c.Writer, c.Request, name, info.ModTime(), file)
}

func (h *BackupHandler) restore(c *gin.Context) {
	var request backupapi.RestoreRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的恢复确认", nil)
		return
	}
	backupID := c.Param("backupId")
	value, err := h.backups.Get(backupID)
	if err != nil {
		backupFailure(c, err)
		return
	}
	roomID := value.RoomID
	_, value, err = h.backups.ValidateRestore(c.Request.Context(), roomID, backupID, request.Confirmation)
	if err != nil {
		backupFailure(c, err)
		return
	}
	targets := []jobs.TargetSpec{{ID: backupID, Name: value.Name}}
	job, err := h.jobs.SubmitFactory("backup.restore", roomID, "", targets, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			protection, restoreErr := h.backups.Restore(ctx, roomID, backupID, request.Confirmation, job.ID)
			if restoreErr != nil {
				report(jobs.TargetResult{TargetID: backupID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "BACKUP_RESTORE_FAILED", Message: restoreErr.Error()}})
				return nil
			}
			report(jobs.TargetResult{TargetID: backupID, Status: jobs.StatusSucceeded, Message: "恢复完成；保护性备份：" + protection.Name})
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建恢复任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *BackupHandler) policy(c *gin.Context) {
	value, err := h.backups.Policy(c.Param("roomId"))
	if err != nil {
		backupFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *BackupHandler) savePolicy(c *gin.Context) {
	var request backupapi.PolicyRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的快照策略", nil)
		return
	}
	value, err := h.backups.SavePolicy(c.Param("roomId"), request)
	if err != nil {
		backupFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *BackupHandler) prune(c *gin.Context) {
	roomID := c.Param("roomId")
	policy, err := h.backups.Policy(roomID)
	if err != nil {
		backupFailure(c, err)
		return
	}
	targets := []jobs.TargetSpec{{ID: roomID, Name: "自动快照"}}
	job, err := h.jobs.Submit("backup.prune", roomID, "", targets, func(_ context.Context, report func(jobs.TargetResult)) error {
		bytes, count, pruneErr := h.backups.PruneSnapshots(roomID, policy.MaxSnapshots)
		if pruneErr != nil {
			report(jobs.TargetResult{TargetID: roomID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "BACKUP_PRUNE_FAILED", Message: pruneErr.Error()}})
			return nil
		}
		report(jobs.TargetResult{TargetID: roomID, Status: jobs.StatusSucceeded, Message: fmt.Sprintf("已清理 %d 个快照，释放 %s", count, formatByteCount(bytes))})
		return nil
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建清理任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func backupFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, backupapi.ErrBackupNotFound):
		Failure(c, http.StatusNotFound, "BACKUP_NOT_FOUND", "备份不存在", nil)
	case errors.Is(err, backupapi.ErrInvalidName), errors.Is(err, backupapi.ErrSnapshotPolicyInvalid):
		Failure(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "备份名称或快照策略无效", nil)
	case errors.Is(err, backupapi.ErrInvalidArchive), errors.Is(err, backupapi.ErrUnsafeArchive):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_ARCHIVE", "ZIP 结构或存档内容无效", nil)
	case errors.Is(err, backupapi.ErrArchiveTooLarge):
		Failure(c, http.StatusRequestEntityTooLarge, "ARCHIVE_TOO_LARGE", "备份超过上传或解压安全上限", nil)
	case errors.Is(err, backupapi.ErrInsufficientSpace):
		Failure(c, http.StatusInsufficientStorage, "INSUFFICIENT_SPACE", "磁盘空间不足，无法完成备份操作", nil)
	case errors.Is(err, backupapi.ErrConfirmationNeeded):
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入完整房间名确认恢复", nil)
	case errors.Is(err, backupapi.ErrWorldRunning):
		Failure(c, http.StatusConflict, "WORLD_RUNNING", "停止全部分片后才能恢复备份", nil)
	case errors.Is(err, backupapi.ErrConsistentSaveMissing):
		Failure(c, http.StatusConflict, "CONSISTENT_SAVE_UNAVAILABLE", "运行中的房间缺少 Master，无法创建一致性备份", nil)
	case errors.Is(err, backupapi.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_NOT_MANAGED", "接管房间后才能管理备份", nil)
	case errors.Is(err, backupapi.ErrBackupRoomMismatch), errors.Is(err, backupapi.ErrUnsafeBackupPath), errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_BACKUP_RESOURCE", "备份资源或路径无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间不存在", nil)
	default:
		Failure(c, http.StatusInternalServerError, "BACKUP_OPERATION_FAILED", "备份操作失败", nil)
	}
}

func formatByteCount(value int64) string {
	if value < 1024 {
		return strconv.FormatInt(value, 10) + " B"
	}
	if value < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(value)/1024)
	}
	if value < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(value)/(1024*1024))
	}
	return fmt.Sprintf("%.1f GB", float64(value)/(1024*1024*1024))
}
