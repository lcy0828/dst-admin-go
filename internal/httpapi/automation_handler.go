package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"dont/internal/automation"
	"dont/internal/jobs"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
)

type AutomationService interface {
	Groups(string) ([]automation.Group, error)
	CreateGroup(string, automation.GroupInput) (automation.Group, error)
	UpdateGroup(string, string, automation.GroupInput) (automation.Group, error)
	DeleteGroup(string, string) error
	Tasks(string) ([]automation.Task, error)
	Task(string, string) (automation.Task, error)
	CreateTask(string, automation.TaskInput) (automation.Task, error)
	UpdateTask(string, string, automation.TaskInput) (automation.Task, error)
	DeleteTask(string, string) error
	Runs(string, automation.RunFilter) (automation.RunList, error)
	Run(string, string) (automation.Run, error)
	ClearRuns(string, automation.ClearRunsInput) (automation.ClearRunsResult, error)
	Stats(string, int) (automation.Stats, error)
	RunTask(string, string, automation.Trigger) (jobs.Job, error)
	Export(string) (automation.Document, error)
	PreviewImport(string, automation.Document) (automation.ImportPreview, error)
	Import(string, automation.ImportRequest) (automation.ImportResult, error)
}

type AutomationScheduler interface {
	Reload() error
}

type AutomationHandler struct {
	service   AutomationService
	scheduler AutomationScheduler
}

func NewAutomationHandler(service AutomationService, scheduler AutomationScheduler) *AutomationHandler {
	return &AutomationHandler{service: service, scheduler: scheduler}
}

func (h *AutomationHandler) Register(v2 *gin.RouterGroup) {
	root := v2.Group("/rooms/:roomId/automation")
	root.GET("/actions", h.actions)
	root.GET("/groups", h.groups)
	root.POST("/groups", h.createGroup)
	root.PUT("/groups/:groupId", h.updateGroup)
	root.DELETE("/groups/:groupId", h.deleteGroup)
	root.GET("/tasks", h.tasks)
	root.POST("/tasks", h.createTask)
	root.GET("/tasks/:taskId", h.task)
	root.PUT("/tasks/:taskId", h.updateTask)
	root.DELETE("/tasks/:taskId", h.deleteTask)
	root.POST("/tasks/:taskId/actions/run", h.runTask)
	root.GET("/runs", h.runs)
	root.GET("/runs/:runId", h.run)
	root.POST("/runs/actions/clear", h.clearRuns)
	root.GET("/stats", h.stats)
	root.GET("/export", h.export)
	root.POST("/imports/preview", h.previewImport)
	root.POST("/imports", h.importDocument)
}

func (h *AutomationHandler) actions(c *gin.Context) {
	Success(c, http.StatusOK, gin.H{"items": automation.ActionDefinitions()})
}

func (h *AutomationHandler) groups(c *gin.Context) {
	items, err := h.service.Groups(c.Param("roomId"))
	if err != nil {
		automationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": len(items)})
}

func (h *AutomationHandler) createGroup(c *gin.Context) {
	var input automation.GroupInput
	if !bindAutomationJSON(c, &input) {
		return
	}
	value, err := h.service.CreateGroup(c.Param("roomId"), input)
	if err != nil {
		automationFailure(c, err)
		return
	}
	if !h.reload(c) {
		return
	}
	Success(c, http.StatusCreated, value)
}

func (h *AutomationHandler) updateGroup(c *gin.Context) {
	var input automation.GroupInput
	if !bindAutomationJSON(c, &input) {
		return
	}
	value, err := h.service.UpdateGroup(c.Param("roomId"), c.Param("groupId"), input)
	if err != nil {
		automationFailure(c, err)
		return
	}
	if !h.reload(c) {
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *AutomationHandler) deleteGroup(c *gin.Context) {
	if err := h.service.DeleteGroup(c.Param("roomId"), c.Param("groupId")); err != nil {
		automationFailure(c, err)
		return
	}
	if !h.reload(c) {
		return
	}
	Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *AutomationHandler) tasks(c *gin.Context) {
	items, err := h.service.Tasks(c.Param("roomId"))
	if err != nil {
		automationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": len(items)})
}

func (h *AutomationHandler) task(c *gin.Context) {
	value, err := h.service.Task(c.Param("roomId"), c.Param("taskId"))
	if err != nil {
		automationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *AutomationHandler) createTask(c *gin.Context) {
	var input automation.TaskInput
	if !bindAutomationJSON(c, &input) {
		return
	}
	value, err := h.service.CreateTask(c.Param("roomId"), input)
	if err != nil {
		automationFailure(c, err)
		return
	}
	if !h.reload(c) {
		return
	}
	Success(c, http.StatusCreated, value)
}

func (h *AutomationHandler) updateTask(c *gin.Context) {
	var input automation.TaskInput
	if !bindAutomationJSON(c, &input) {
		return
	}
	value, err := h.service.UpdateTask(c.Param("roomId"), c.Param("taskId"), input)
	if err != nil {
		automationFailure(c, err)
		return
	}
	if !h.reload(c) {
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *AutomationHandler) deleteTask(c *gin.Context) {
	if err := h.service.DeleteTask(c.Param("roomId"), c.Param("taskId")); err != nil {
		automationFailure(c, err)
		return
	}
	if !h.reload(c) {
		return
	}
	Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *AutomationHandler) runTask(c *gin.Context) {
	job, err := h.service.RunTask(c.Param("roomId"), c.Param("taskId"), automation.TriggerManual)
	if err != nil {
		automationFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *AutomationHandler) runs(c *gin.Context) {
	limit, limitErr := strconv.Atoi(c.DefaultQuery("limit", "25"))
	offset, offsetErr := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limitErr != nil || offsetErr != nil {
		automationFailure(c, automation.ErrInvalidInput)
		return
	}
	startAt, startErr := parseAutomationDate(c.Query("startDate"), false)
	endAt, endErr := parseAutomationDate(c.Query("endDate"), true)
	if startErr != nil || endErr != nil {
		automationFailure(c, automation.ErrInvalidInput)
		return
	}
	value, err := h.service.Runs(c.Param("roomId"), automation.RunFilter{TaskID: c.Query("taskId"), GroupID: c.Query("groupId"), Status: automation.RunStatus(c.Query("status")), StartAt: startAt, EndAt: endAt, Limit: limit, Offset: offset})
	if err != nil {
		automationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *AutomationHandler) run(c *gin.Context) {
	value, err := h.service.Run(c.Param("roomId"), c.Param("runId"))
	if err != nil {
		automationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *AutomationHandler) clearRuns(c *gin.Context) {
	var input automation.ClearRunsInput
	if !bindAutomationJSON(c, &input) {
		return
	}
	value, err := h.service.ClearRuns(c.Param("roomId"), input)
	if err != nil {
		automationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func parseAutomationDate(value string, exclusiveEnd bool) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return nil, err
	}
	if exclusiveEnd {
		parsed = parsed.AddDate(0, 0, 1)
	}
	return &parsed, nil
}

func (h *AutomationHandler) stats(c *gin.Context) {
	days, err := strconv.Atoi(c.DefaultQuery("days", "7"))
	if err != nil {
		automationFailure(c, automation.ErrInvalidInput)
		return
	}
	value, err := h.service.Stats(c.Param("roomId"), days)
	if err != nil {
		automationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *AutomationHandler) export(c *gin.Context) {
	value, err := h.service.Export(c.Param("roomId"))
	if err != nil {
		automationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *AutomationHandler) previewImport(c *gin.Context) {
	var document automation.Document
	if !bindAutomationJSON(c, &document) {
		return
	}
	value, err := h.service.PreviewImport(c.Param("roomId"), document)
	if err != nil {
		automationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *AutomationHandler) importDocument(c *gin.Context) {
	var request automation.ImportRequest
	if !bindAutomationJSON(c, &request) {
		return
	}
	value, err := h.service.Import(c.Param("roomId"), request)
	if err != nil {
		automationFailure(c, err)
		return
	}
	if !h.reload(c) {
		return
	}
	Success(c, http.StatusOK, value)
}

func bindAutomationJSON(c *gin.Context, value interface{}) bool {
	if err := c.ShouldBindJSON(value); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "自动化内容不是有效 JSON", nil)
		return false
	}
	return true
}

func (h *AutomationHandler) reload(c *gin.Context) bool {
	if h.scheduler == nil {
		return true
	}
	if err := h.scheduler.Reload(); err != nil {
		Failure(c, http.StatusInternalServerError, "AUTOMATION_RELOAD_FAILED", "自动化已保存，但调度器重新加载失败", nil)
		return false
	}
	return true
}

func automationFailure(c *gin.Context, err error) {
	var fieldErr *automation.FieldError
	switch {
	case errors.As(err, &fieldErr):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_AUTOMATION_INPUT", "自动化参数无效", fieldErr.Fields)
	case errors.Is(err, automation.ErrInvalidInput), errors.Is(err, automation.ErrImportInvalid), errors.Is(err, automation.ErrUnsafeAction):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_AUTOMATION_INPUT", "自动化参数无效或动作不允许", nil)
	case errors.Is(err, automation.ErrGroupNotFound), errors.Is(err, automation.ErrTaskNotFound), errors.Is(err, automation.ErrRunNotFound):
		NotFound(c)
	case errors.Is(err, automation.ErrGroupNotEmpty):
		Failure(c, http.StatusConflict, "AUTOMATION_GROUP_NOT_EMPTY", "请先移动或删除组内任务", nil)
	case errors.Is(err, automation.ErrRevisionConflict):
		Failure(c, http.StatusConflict, "AUTOMATION_REVISION_CONFLICT", "自动化内容已被其他请求修改，请刷新后重试", nil)
	case errors.Is(err, automation.ErrTaskRunning):
		Failure(c, http.StatusConflict, "AUTOMATION_TASK_RUNNING", "该任务已有一轮正在执行", nil)
	case errors.Is(err, automation.ErrDependencies):
		Failure(c, http.StatusConflict, "AUTOMATION_DEPENDENCIES", "任务依赖尚未全部成功执行", nil)
	case errors.Is(err, automation.ErrImportDigest):
		Failure(c, http.StatusConflict, "AUTOMATION_IMPORT_CHANGED", "导入内容与预览版本不一致", nil)
	case errors.Is(err, rooms.ErrRoomNotFound):
		NotFound(c)
	default:
		Failure(c, http.StatusInternalServerError, "AUTOMATION_FAILED", "自动化操作失败", nil)
	}
}
