package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"dont/internal/agents"

	"github.com/gin-gonic/gin"
)

type AgentHandler struct{ service *agents.Service }

func NewAgentHandler(service *agents.Service) *AgentHandler { return &AgentHandler{service: service} }

func (h *AgentHandler) Register(v2 *gin.RouterGroup) {
	group := v2.Group("/agents")
	group.GET("", h.list)
	group.GET("/actions", h.actions)
	group.GET("/commands", h.commands)
	group.GET("/security", h.security)
	group.POST("/security/actions/rotate", h.rotateKey)
	group.GET("/:agentId", h.get)
	group.DELETE("/:agentId", h.forget)
	group.GET("/:agentId/commands", h.agentCommands)
	group.POST("/:agentId/commands", h.runCommand)
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

func (h *AgentHandler) commands(c *gin.Context)      { h.commandList(c, c.Query("agentId")) }
func (h *AgentHandler) agentCommands(c *gin.Context) { h.commandList(c, c.Param("agentId")) }

func (h *AgentHandler) commandList(c *gin.Context, agentID string) {
	limit, limitErr := strconv.Atoi(c.DefaultQuery("limit", "25"))
	offset, offsetErr := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limitErr != nil || offsetErr != nil {
		agentFailure(c, agents.ErrInvalidInput)
		return
	}
	value, err := h.service.Commands(agents.CommandFilter{AgentID: agentID, Status: agents.CommandStatus(c.Query("status")), Limit: limit, Offset: offset})
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
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
	case errors.Is(err, agents.ErrAgentNotFound), errors.Is(err, agents.ErrCommandNotFound):
		NotFound(c)
	case errors.Is(err, agents.ErrAgentOffline):
		Failure(c, http.StatusConflict, "AGENT_OFFLINE", "Agent 当前离线，无法执行命令", nil)
	case errors.Is(err, agents.ErrAgentOnline):
		Failure(c, http.StatusConflict, "AGENT_ONLINE", "在线 Agent 不能从历史中移除", nil)
	case errors.Is(err, agents.ErrUnavailable):
		Failure(c, http.StatusServiceUnavailable, "AGENT_TRANSPORT_UNAVAILABLE", "Agent 通信服务未启用", nil)
	case errors.Is(err, agents.ErrConfirmationRequired):
		Failure(c, http.StatusUnprocessableEntity, "AGENT_KEY_CONFIRMATION_REQUIRED", "请输入 ROTATE AGENT KEY 确认轮换", map[string]string{"confirmation": "确认短语不匹配"})
	case errors.Is(err, agents.ErrInvalidInput), errors.Is(err, agents.ErrUnsupportedAction):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_AGENT_INPUT", "Agent 参数或动作无效", nil)
	default:
		Failure(c, http.StatusInternalServerError, "AGENT_OPERATION_FAILED", "Agent 操作失败", nil)
	}
}
