package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dont/internal/agents"

	"github.com/gin-gonic/gin"
)

type AgentHandler struct{ service *agents.Service }

type renameRuntimeTargetInput struct {
	DisplayName string `json:"displayName"`
}

func NewAgentHandler(service *agents.Service) *AgentHandler { return &AgentHandler{service: service} }

func (h *AgentHandler) Register(v2 *gin.RouterGroup) {
	runtimes := v2.Group("/runtime-targets")
	runtimes.GET("", h.runtimeTargets)
	runtimes.PATCH("/:targetId", h.renameRuntimeTarget)
	runtimes.GET("/agents/:agentId", h.runtimeTarget)
	runtimes.PUT("/agents/:agentId", h.saveRuntimeConfig)
	runtimes.DELETE("/agents/:agentId", h.deleteRuntimeConfig)

	group := v2.Group("/agents")
	group.GET("", h.list)
	group.GET("/actions", h.actions)
	group.GET("/commands", h.commands)
	group.GET("/commands/:commandId", h.command)
	group.GET("/security", h.security)
	group.POST("/security/actions/rotate", h.rotateKey)
	group.GET("/:agentId/inventory", h.inventory)
	group.POST("/:agentId/inventory/actions/refresh", h.refreshInventory)
	group.GET("/:agentId", h.get)
	group.DELETE("/:agentId", h.forget)
	group.GET("/:agentId/commands", h.agentCommands)
	group.POST("/:agentId/commands", h.runCommand)
	group.POST("/:agentId/actions/upgrade", h.upgrade)

	releases := v2.Group("/agent-releases")
	releases.GET("", h.releases)
	releases.POST("", h.uploadRelease)
	releases.DELETE("/:releaseId", h.deleteRelease)
}

func (h *AgentHandler) RegisterDownloads(router *gin.Engine) {
	if h != nil && router != nil {
		router.GET("/agent-updates/:releaseId", h.downloadRelease)
	}
}

func (h *AgentHandler) renameRuntimeTarget(c *gin.Context) {
	var input renameRuntimeTargetInput
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "机器名称不是有效 JSON", nil)
		return
	}
	item, err := h.service.RenameRuntimeTarget(c.Param("targetId"), input.DisplayName)
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, item)
}

func (h *AgentHandler) runtimeTargets(c *gin.Context) {
	items, err := h.service.RuntimeTargets()
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": len(items), "defaultTargetId": h.service.DefaultRuntimeTargetID(items)})
}

func (h *AgentHandler) runtimeTarget(c *gin.Context) {
	item, err := h.service.RuntimeTarget(c.Param("agentId"))
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, item)
}

func (h *AgentHandler) saveRuntimeConfig(c *gin.Context) {
	var input agents.RuntimeConfig
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "远程运行时配置不是有效 JSON", nil)
		return
	}
	item, err := h.service.SaveRuntimeConfig(c.Param("agentId"), input)
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, item)
}

func (h *AgentHandler) deleteRuntimeConfig(c *gin.Context) {
	if err := h.service.DeleteRuntimeConfig(c.Param("agentId")); err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *AgentHandler) list(c *gin.Context) {
	items, available, err := h.service.Agents()
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": len(items), "transportAvailable": available})
}

func (h *AgentHandler) get(c *gin.Context) {
	item, err := h.service.Agent(c.Param("agentId"))
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, item)
}

func (h *AgentHandler) inventory(c *gin.Context) {
	value, err := h.service.Inventory(c.Param("agentId"))
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *AgentHandler) refreshInventory(c *gin.Context) {
	job, err := h.service.RefreshInventory(c.Param("agentId"))
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *AgentHandler) forget(c *gin.Context) {
	if err := h.service.Forget(c.Param("agentId")); err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *AgentHandler) actions(c *gin.Context) {
	Success(c, http.StatusOK, gin.H{"items": h.service.Actions()})
}

func (h *AgentHandler) runCommand(c *gin.Context) {
	var input agents.CommandInput
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "Agent 命令不是有效 JSON", nil)
		return
	}
	job, err := h.service.RunCommand(c.Param("agentId"), input)
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *AgentHandler) releases(c *gin.Context) {
	items, err := h.service.Releases()
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": len(items), "maxUploadBytes": agents.MaxAgentReleaseBytes})
}

func (h *AgentHandler) uploadRelease(c *gin.Context) {
	file, err := readUpload(c, h.service.UploadDirectory(), agents.MaxAgentReleaseBytes)
	if err != nil {
		uploadFailure(c, err)
		return
	}
	defer file.cleanup()
	release, err := h.service.SaveRelease(file.Fields["version"], file.Filename, file)
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusCreated, release)
}

func (h *AgentHandler) deleteRelease(c *gin.Context) {
	if err := h.service.DeleteRelease(c.Param("releaseId")); err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *AgentHandler) upgrade(c *gin.Context) {
	var input agents.AgentUpgradeInput
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&input); err != nil {
			Failure(c, http.StatusBadRequest, "INVALID_JSON", "Agent 升级请求不是有效 JSON", nil)
			return
		}
	}
	job, err := h.service.UpgradeAgent(c.Param("agentId"), input)
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *AgentHandler) downloadRelease(c *gin.Context) {
	authorization := strings.Fields(strings.TrimSpace(c.GetHeader("Authorization")))
	if len(authorization) != 2 || !strings.EqualFold(authorization[0], "Bearer") {
		Failure(c, http.StatusUnauthorized, "AGENT_RELEASE_TOKEN_REQUIRED", "Agent 升级下载凭据无效", nil)
		return
	}
	release, file, err := h.service.OpenReleaseDownload(c.Param("releaseId"), authorization[1])
	if err != nil {
		Failure(c, http.StatusUnauthorized, "AGENT_RELEASE_TOKEN_INVALID", "Agent 升级下载凭据无效或已过期", nil)
		return
	}
	defer file.Close()
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Agent-Release-SHA256", release.SHA256)
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": release.FileName}))
	c.DataFromReader(http.StatusOK, release.Size, "application/octet-stream", io.Reader(file), nil)
}

func (h *AgentHandler) commands(c *gin.Context)      { h.commandList(c, c.Query("agentId")) }
func (h *AgentHandler) agentCommands(c *gin.Context) { h.commandList(c, c.Param("agentId")) }

func (h *AgentHandler) commandList(c *gin.Context, agentID string) {
	limit, limitErr := strconv.Atoi(c.DefaultQuery("limit", "25"))
	offset, offsetErr := strconv.Atoi(c.DefaultQuery("offset", "0"))
	startAt, startErr := parseAgentCommandDate(c.Query("startDate"), false)
	endAt, endErr := parseAgentCommandDate(c.Query("endDate"), true)
	if limitErr != nil || offsetErr != nil || startErr != nil || endErr != nil {
		agentFailure(c, agents.ErrInvalidInput)
		return
	}
	value, err := h.service.Commands(agents.CommandFilter{
		AgentID: agentID, Status: agents.CommandStatus(c.Query("status")), Query: c.Query("query"),
		StartAt: startAt, EndAt: endAt, Limit: limit, Offset: offset,
	})
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *AgentHandler) command(c *gin.Context) {
	value, err := h.service.Command(c.Param("commandId"))
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func parseAgentCommandDate(value string, exclusiveEnd bool) (*time.Time, error) {
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

func (h *AgentHandler) security(c *gin.Context) {
	value, err := h.service.Security()
	if err != nil {
		agentFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func (h *AgentHandler) rotateKey(c *gin.Context) {
	var input agents.RotateKeyInput
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "密钥轮换请求不是有效 JSON", nil)
		return
	}
	value, err := h.service.RotateKey(c.Request.Context(), input)
	if err != nil {
		agentFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func agentFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, agents.ErrAgentNotFound), errors.Is(err, agents.ErrCommandNotFound), errors.Is(err, agents.ErrRuntimeNotConfigured), errors.Is(err, agents.ErrRuntimeTargetNotFound), errors.Is(err, agents.ErrInventoryNotFound), errors.Is(err, agents.ErrReleaseNotFound):
		NotFound(c)
	case errors.Is(err, agents.ErrAgentOffline):
		Failure(c, http.StatusConflict, "AGENT_OFFLINE", "Agent 当前离线，无法执行命令", nil)
	case errors.Is(err, agents.ErrAgentOnline):
		Failure(c, http.StatusConflict, "AGENT_ONLINE", "在线 Agent 不能从历史中移除", nil)
	case errors.Is(err, agents.ErrUnavailable):
		Failure(c, http.StatusServiceUnavailable, "AGENT_TRANSPORT_UNAVAILABLE", "Agent 通信服务未启用", nil)
	case errors.Is(err, agents.ErrConfirmationRequired):
		Failure(c, http.StatusUnprocessableEntity, "AGENT_KEY_CONFIRMATION_REQUIRED", "请输入 ROTATE AGENT KEY 确认轮换", map[string]string{"confirmation": "确认短语不匹配"})
	case errors.Is(err, agents.ErrRuntimeInstallationNotRegistered):
		Failure(c, http.StatusUnprocessableEntity, "RUNTIME_INSTALLATION_NOT_REGISTERED", "所选 DST 安装未在 Agent 上登记，或路径与 Agent 受信配置不一致", nil)
	case errors.Is(err, agents.ErrUpgradeUnsupported):
		Failure(c, http.StatusConflict, "AGENT_UPGRADE_UNSUPPORTED", "当前 Agent 安装方式不支持页面内升级", nil)
	case errors.Is(err, agents.ErrUpgradeNotAvailable):
		Failure(c, http.StatusConflict, "AGENT_UPGRADE_NOT_AVAILABLE", "没有适用于该机器的新版本 Agent 包", nil)
	case errors.Is(err, agents.ErrUpgradeInProgress):
		Failure(c, http.StatusConflict, "AGENT_UPGRADE_IN_PROGRESS", "该 Agent 已有升级任务正在执行", nil)
	case errors.Is(err, agents.ErrReleaseInUse):
		Failure(c, http.StatusConflict, "AGENT_RELEASE_IN_USE", "该 Agent 包正在用于升级，暂时不能删除", nil)
	case errors.Is(err, agents.ErrReleaseTooLarge):
		Failure(c, http.StatusRequestEntityTooLarge, "AGENT_RELEASE_TOO_LARGE", "Agent 二进制文件超过 128 MiB 上传上限", nil)
	case errors.Is(err, agents.ErrInvalidRelease):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_AGENT_RELEASE", err.Error(), nil)
	case errors.Is(err, agents.ErrInvalidInput), errors.Is(err, agents.ErrUnsupportedAction):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_AGENT_INPUT", "Agent 参数或动作无效", nil)
	default:
		Failure(c, http.StatusInternalServerError, "AGENT_OPERATION_FAILED", "Agent 操作失败", nil)
	}
}
