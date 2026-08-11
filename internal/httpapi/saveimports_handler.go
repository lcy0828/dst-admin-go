package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"dont/internal/jobs"
	"dont/internal/saveimport"

	"github.com/gin-gonic/gin"
)

type SaveImportHandler struct {
	imports *saveimport.Service
	jobs    *jobs.Service
}

func NewSaveImportHandler(service *saveimport.Service, jobService *jobs.Service) *SaveImportHandler {
	return &SaveImportHandler{imports: service, jobs: jobService}
}

func (h *SaveImportHandler) Register(v2 *gin.RouterGroup) {
	group := v2.Group("/save-imports")
	group.GET("", h.list)
	group.POST("/upload", h.upload)
	group.GET("/:importId", h.get)
	group.DELETE("/:importId", h.delete)
	group.POST("/:importId/actions/analyze", h.analyze)
	group.POST("/:importId/actions/apply", h.apply)
}

func (h *SaveImportHandler) list(c *gin.Context) {
	items, err := h.imports.List()
	if err != nil {
		saveImportFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": len(items)})
}

func (h *SaveImportHandler) get(c *gin.Context) {
	value, err := h.imports.Get(c.Param("importId"))
	if err != nil {
		saveImportFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *SaveImportHandler) upload(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, saveimport.MaxUploadBytes+1024*1024)
	fileHeader, err := c.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			saveImportFailure(c, saveimport.ErrArchiveTooLarge)
			return
		}
		Failure(c, http.StatusBadRequest, "UPLOAD_REQUIRED", "请选择 ZIP、TAR 或 TAR.GZ 存档文件", nil)
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		Failure(c, http.StatusBadRequest, "UPLOAD_READ_FAILED", "无法读取上传的存档文件", nil)
		return
	}
	defer file.Close()
	value, err := h.imports.Upload(c.Request.Context(), c.PostForm("name"), fileHeader.Filename, file)
	if err != nil {
		saveImportFailure(c, err)
		return
	}
	job, err := h.submitAnalysis(value)
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "文件已保存，但无法创建存档分析任务", map[string]string{"importId": value.ID})
		return
	}
	Success(c, http.StatusAccepted, gin.H{"import": value, "job": job})
}

func (h *SaveImportHandler) analyze(c *gin.Context) {
	value, err := h.imports.Get(c.Param("importId"))
	if err != nil {
		saveImportFailure(c, err)
		return
	}
	job, err := h.submitAnalysis(value)
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建存档分析任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *SaveImportHandler) apply(c *gin.Context) {
	var request saveimport.ApplyRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的存档部署方案", nil)
		return
	}
	value, err := h.imports.Get(c.Param("importId"))
	if err != nil {
		saveImportFailure(c, err)
		return
	}
	roomID := request.TargetRoomID
	targets := []jobs.TargetSpec{{ID: value.ID, Name: value.Name}}
	job, err := h.jobs.SubmitFactory("save-import.apply", roomID, "", targets, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			result, applyErr := h.imports.Apply(ctx, value.ID, job.ID, request)
			if applyErr != nil {
				report(jobs.TargetResult{TargetID: value.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: saveImportErrorCode(applyErr), Message: applyErr.Error()}})
				return nil
			}
			report(jobs.TargetResult{
				TargetID: value.ID, Status: jobs.StatusSucceeded,
				Message: "存档已部署到房间：" + result.RoomName,
			})
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建存档部署任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *SaveImportHandler) submitAnalysis(value saveimport.Session) (jobs.Job, error) {
	targets := []jobs.TargetSpec{{ID: value.ID, Name: value.Name}}
	return h.jobs.Submit("save-import.analyze", "", "", targets, func(ctx context.Context, report func(jobs.TargetResult)) error {
		analyzed, err := h.imports.Analyze(ctx, value.ID)
		if err != nil {
			report(jobs.TargetResult{TargetID: value.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: saveimport.AnalysisErrorCode(err), Message: err.Error()}})
			return nil
		}
		report(jobs.TargetResult{
			TargetID: value.ID, Status: jobs.StatusSucceeded,
			Message: "分析完成，检测到 " + strconv.Itoa(len(analyzed.Manifest.Candidates)) + " 个可识别房间",
		})
		return nil
	})
}

func (h *SaveImportHandler) delete(c *gin.Context) {
	var request saveimport.DeleteRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的删除确认", nil)
		return
	}
	value, err := h.imports.Get(c.Param("importId"))
	if err != nil {
		saveImportFailure(c, err)
		return
	}
	if request.Confirmation != value.Name {
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入完整导入名称确认删除", nil)
		return
	}
	if err := h.imports.Delete(value.ID); err != nil {
		saveImportFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func saveImportFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, saveimport.ErrImportNotFound):
		Failure(c, http.StatusNotFound, "SAVE_IMPORT_NOT_FOUND", "存档导入记录不存在", nil)
	case errors.Is(err, saveimport.ErrInvalidRequest):
		Failure(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "存档导入参数无效", nil)
	case errors.Is(err, saveimport.ErrUnsafeArchive), errors.Is(err, saveimport.ErrInvalidArchive):
		Failure(c, http.StatusUnprocessableEntity, saveImportErrorCode(err), "存档压缩包结构或内容无效", nil)
	case errors.Is(err, saveimport.ErrArchiveTooLarge):
		Failure(c, http.StatusRequestEntityTooLarge, "ARCHIVE_TOO_LARGE", "存档超过上传或解压安全上限", nil)
	case errors.Is(err, saveimport.ErrImportNotReady), errors.Is(err, saveimport.ErrCandidateMissing):
		Failure(c, http.StatusConflict, saveImportErrorCode(err), "存档尚未完成分析或候选房间不存在", nil)
	case errors.Is(err, saveimport.ErrRoomRunning):
		Failure(c, http.StatusConflict, "WORLD_RUNNING", "停止目标房间的全部分片后才能导入", nil)
	case errors.Is(err, saveimport.ErrTargetExists):
		Failure(c, http.StatusConflict, "ROOM_EXISTS", "目标房间目录已经存在", nil)
	case errors.Is(err, saveimport.ErrConfirmation):
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入完整目标房间名称确认替换", nil)
	case errors.Is(err, saveimport.ErrTokenRequired), errors.Is(err, saveimport.ErrMissingMods), errors.Is(err, saveimport.ErrPartialImport):
		Failure(c, http.StatusUnprocessableEntity, saveImportErrorCode(err), "存档部署前仍有必须处理的兼容问题", nil)
	default:
		Failure(c, http.StatusInternalServerError, "SAVE_IMPORT_OPERATION_FAILED", "存档导入操作失败", nil)
	}
}

func saveImportErrorCode(err error) string {
	return saveimport.ErrorCode(err)
}
