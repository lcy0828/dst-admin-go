package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dont/internal/agents"
	"github.com/gin-gonic/gin"
)

func (h *AgentHandler) softwareStatus(c *gin.Context) { h.softwareRead(c, false) }
func (h *AgentHandler) softwareCheck(c *gin.Context)  { h.softwareRead(c, true) }
func (h *AgentHandler) softwareRead(c *gin.Context, check bool) {
	force, err := strconv.ParseBool(c.DefaultQuery("force", "false"))
	if err != nil {
		agentFailure(c, agents.ErrInvalidInput)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	value, err := h.service.AgentSoftware(ctx, c.Param("agentId"), check, force, c.DefaultQuery("source", "auto"))
	if err != nil {
		agentFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}
func (h *AgentHandler) softwareUpdate(c *gin.Context) {
	var input agents.AgentSoftwareInput
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		agentFailure(c, agents.ErrInvalidInput)
		return
	}
	job, err := h.service.UpdateAgentSoftware(c.Param("agentId"), input)
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, job)
}
func (h *AgentHandler) softwareTransfer(c *gin.Context) {
	auth := strings.Fields(c.GetHeader("Authorization"))
	if len(auth) != 2 || !strings.EqualFold(auth[0], "Bearer") {
		Failure(c, 401, "AGENT_RELEASE_TOKEN_REQUIRED", "Agent 更新凭据无效", nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Minute)
	defer cancel()
	_, err := h.service.AgentSoftwareTransfer(ctx, c.Param("releaseId"), auth[1], func(size int64) io.Writer {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Content-Type", "application/octet-stream")
		c.Header("Content-Length", strconv.FormatInt(size, 10))
		c.Status(http.StatusOK)
		return c.Writer
	})
	if err != nil && !c.Writer.Written() {
		c.Header("Content-Length", "")
		status := http.StatusBadGateway
		if errors.Is(err, agents.ErrDownloadToken) {
			status = http.StatusUnauthorized
		}
		Failure(c, status, "AGENT_RELEASE_TOKEN_INVALID", "Agent 更新传输失败", nil)
	}
}
