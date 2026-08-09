package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"dont/internal/jobs"
	"dont/internal/logstream"
	"dont/internal/rooms"
	"dont/internal/structuredlogs"

	"github.com/gin-gonic/gin"
)

type StructuredLogService interface {
	List(string, structuredlogs.ListFilter) (structuredlogs.List, error)
	WorldTargets(string) ([]rooms.World, error)
	RefreshWorld(context.Context, string, string) (structuredlogs.RefreshResult, error)
	ClearWorld(string, string) (structuredlogs.ClearResult, error)
	Rules(string) ([]structuredlogs.Rule, error)
	CreateRule(string, structuredlogs.RuleInput) (structuredlogs.Rule, error)
	UpdateRule(string, string, structuredlogs.RuleInput) (structuredlogs.Rule, error)
	DeleteRule(string, string) error
	TestRule(string, structuredlogs.RuleTestInput) (structuredlogs.RuleTestResult, error)
	PreviewRuleMigration(string) (structuredlogs.RuleMigrationPreview, error)
	MigrateLegacyRules(string) (structuredlogs.RuleMigrationResult, error)
}

type StructuredLogHandler struct {
	logs StructuredLogService
	jobs *jobs.Service
}

func NewStructuredLogHandler(service StructuredLogService, jobService *jobs.Service) *StructuredLogHandler {
	return &StructuredLogHandler{logs: service, jobs: jobService}
}

func (h *StructuredLogHandler) Register(v2 *gin.RouterGroup) {
	logs := v2.Group("/rooms/:roomId/structured-logs")
	logs.GET("", h.list)
	logs.POST("/actions/refresh", h.refresh)
	logs.POST("/actions/clear", h.clear)
	rules := v2.Group("/rooms/:roomId/log-rules")
	rules.GET("", h.rules)
	rules.POST("", h.createRule)
	rules.PUT("/:ruleId", h.updateRule)
	rules.DELETE("/:ruleId", h.deleteRule)
	rules.POST("/actions/test", h.testRule)
	rules.GET("/migration-preview", h.previewRuleMigration)
	rules.POST("/actions/migrate-legacy", h.migrateLegacyRules)
}

func (h *StructuredLogHandler) clear(c *gin.Context) {
	var input struct {
		WorldID string `json:"worldId" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请选择要清空日志的世界", nil)
		return
	}
	value, err := h.logs.ClearWorld(c.Param("roomId"), input.WorldID)
	if err != nil {
		structuredLogFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *StructuredLogHandler) list(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	value, err := h.logs.List(c.Param("roomId"), structuredlogs.ListFilter{
		Query: c.Query("query"), WorldID: c.Query("worldId"), Type: structuredlogs.LogType(c.Query("type")), Limit: limit, Offset: offset,
	})
	if err != nil {
		structuredLogFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *StructuredLogHandler) refresh(c *gin.Context) {
	targets, err := h.logs.WorldTargets(c.Param("roomId"))
	if err != nil {
		structuredLogFailure(c, err)
		return
	}
	jobTargets := make([]jobs.TargetSpec, 0, len(targets))
	for _, target := range targets {
		jobTargets = append(jobTargets, jobs.TargetSpec{ID: target.ID, Name: target.Name})
	}
	roomID := c.Param("roomId")
	job, err := h.jobs.Submit("log.structured.refresh", roomID, "", jobTargets, func(ctx context.Context, report func(jobs.TargetResult)) error {
		var result error
		for _, target := range targets {
			refreshed, refreshErr := h.logs.RefreshWorld(ctx, roomID, target.ID)
			if refreshErr != nil {
				report(jobs.TargetResult{TargetID: target.ID, Status: jobs.StatusFailed, Error: structuredLogJobError(refreshErr)})
				result = errors.Join(result, refreshErr)
				continue
			}
			report(jobs.TargetResult{TargetID: target.ID, Status: jobs.StatusSucceeded, Message: refreshed.Message})
		}
		return result
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建结构化日志刷新任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *StructuredLogHandler) rules(c *gin.Context) {
	items, err := h.logs.Rules(c.Param("roomId"))
	if err != nil {
		structuredLogFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": len(items)})
}

func (h *StructuredLogHandler) createRule(c *gin.Context) {
	var input structuredlogs.RuleInput
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "规则内容不是有效 JSON", nil)
		return
	}
	value, err := h.logs.CreateRule(c.Param("roomId"), input)
	if err != nil {
		structuredLogFailure(c, err)
		return
	}
	Success(c, http.StatusCreated, value)
}

func (h *StructuredLogHandler) updateRule(c *gin.Context) {
	var input structuredlogs.RuleInput
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "规则内容不是有效 JSON", nil)
		return
	}
	value, err := h.logs.UpdateRule(c.Param("roomId"), c.Param("ruleId"), input)
	if err != nil {
		structuredLogFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *StructuredLogHandler) deleteRule(c *gin.Context) {
	if err := h.logs.DeleteRule(c.Param("roomId"), c.Param("ruleId")); err != nil {
		structuredLogFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *StructuredLogHandler) testRule(c *gin.Context) {
	var input structuredlogs.RuleTestInput
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "规则测试内容不是有效 JSON", nil)
		return
	}
	value, err := h.logs.TestRule(c.Param("roomId"), input)
	if err != nil {
		structuredLogFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *StructuredLogHandler) previewRuleMigration(c *gin.Context) {
	value, err := h.logs.PreviewRuleMigration(c.Param("roomId"))
	if err != nil {
		structuredLogFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *StructuredLogHandler) migrateLegacyRules(c *gin.Context) {
	value, err := h.logs.MigrateLegacyRules(c.Param("roomId"))
	if err != nil {
		structuredLogFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func structuredLogFailure(c *gin.Context, err error) {
	var fieldErr *structuredlogs.FieldError
	switch {
	case errors.As(err, &fieldErr):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_LOG_RULE", "日志规则参数无效", fieldErr.Fields)
	case errors.Is(err, structuredlogs.ErrInvalidFilter), errors.Is(err, structuredlogs.ErrInvalidRule):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_STRUCTURED_LOG_INPUT", "结构化日志参数无效", nil)
	case errors.Is(err, structuredlogs.ErrRuleNotFound):
		Failure(c, http.StatusNotFound, "LOG_RULE_NOT_FOUND", "日志规则不存在", nil)
	case errors.Is(err, structuredlogs.ErrBuiltInRule):
		Failure(c, http.StatusConflict, "BUILTIN_LOG_RULE", "内建日志规则不能删除，可以停用或调整", nil)
	case errors.Is(err, structuredlogs.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_NOT_MANAGED", "接管房间后才能使用结构化日志", nil)
	case errors.Is(err, logstream.ErrLogNotFound):
		Failure(c, http.StatusNotFound, "LOG_NOT_FOUND", "该分片还没有生成服务器日志", nil)
	case errors.Is(err, logstream.ErrUnsafeLog), errors.Is(err, rooms.ErrUnsafePath), errors.Is(err, rooms.ErrInvalidID):
		Failure(c, http.StatusBadRequest, "INVALID_LOG_RESOURCE", "日志资源路径或标识无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间或世界不存在", nil)
	default:
		Failure(c, http.StatusInternalServerError, "STRUCTURED_LOG_OPERATION_FAILED", "结构化日志操作失败", nil)
	}
}

func structuredLogJobError(err error) *jobs.Error {
	code := "STRUCTURED_LOG_REFRESH_FAILED"
	if errors.Is(err, logstream.ErrLogNotFound) {
		code = "LOG_NOT_FOUND"
	} else if errors.Is(err, logstream.ErrUnsafeLog) {
		code = "INVALID_LOG_RESOURCE"
	}
	return &jobs.Error{Code: code, Message: err.Error()}
}
