package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"dont/internal/aicontrol"
	"dont/internal/authn"
	"github.com/gin-gonic/gin"
)

func (h *AIHandler) registerTasks(v2 *gin.RouterGroup) {
	v2.GET("/bot/protocol", h.taskProtocol)
	v2.POST("/bot/tasks/queries/:tool", h.task("query"))
	v2.POST("/bot/tasks/preparations/:tool", h.task("prepare"))
	v2.POST("/bot/tasks/:taskRef/execute", h.task("execute"))
	v2.GET("/bot/tasks/:taskRef", h.task("result"))
	v2.POST("/bot/gateway", h.companionGateway)
}
func (h *AIHandler) taskPrincipal(c *gin.Context) (aicontrol.Principal, bool) {
	v, ok := c.Get(authn.ContextAdminKey)
	admin, valid := v.(authn.Admin)
	if !ok || !valid || admin.ID < 1 {
		return aicontrol.Principal{}, false
	}
	p := h.principal(c)
	if c.GetHeader("Authorization") == "" {
		p = aicontrol.Principal{ID: fmt.Sprintf("admin:%d", admin.ID), AdminID: admin.ID}
	}
	return p, p.ID != "" && p.AdminID == admin.ID
}
func (h *AIHandler) taskProtocol(c *gin.Context) {
	p, ok := h.taskPrincipal(c)
	if !ok || !h.service.Enabled() {
		aiFailure(c, aicontrol.ErrDenied)
		return
	}
	profile := p.Profile
	if profile == "" {
		profile = aicontrol.ProfilePanelManage
	}
	c.Header("Cache-Control", "no-store")
	Success(c, 200, gin.H{"protocolVersions": []int{1, 2}, "preferredVersion": 2, "capabilityContractVersions": []int{1}, "profile": profile, "roomIds": p.RoomIDs, "roomId": p.RoomID, "features": []string{"task-tools", "typed-task-parameters", "prepared-tasks", "original-result-recovery", "capability-revisions", "companion-gateway-v1"}})
}

// This trusted plugin transport is intentionally excluded from model/MCP tools.
// Possession of a full service credential authorizes delegation; the plugin
// supplies verified platform identity, while the model supplies only task input.
func (h *AIHandler) companionGateway(c *gin.Context) {
	p, ok := h.taskPrincipal(c)
	if !ok || c.GetHeader("Authorization") == "" || p.Restricted() || aicontrol.IsModelChannel(c.Request.Context()) {
		companionFailure(c, aicontrol.ErrDenied)
		return
	}
	if c.Request.URL.RawQuery != "" {
		companionFailure(c, aicontrol.ErrInvalid)
		return
	}
	raw, e := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10))
	if e != nil {
		companionFailure(c, aicontrol.ErrInvalid)
		return
	}
	if e = aicontrol.StrictGenericJSON(raw); e != nil {
		companionFailure(c, e)
		return
	}
	var in aicontrol.GenericInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&in); e != nil {
		companionFailure(c, aicontrol.ErrInvalid)
		return
	}
	if in.Mode == "resource" {
		delegated, err := h.service.CompanionResource(p, in.Actor, in.TaskRef)
		if err != nil {
			companionFailure(c, err)
			return
		}
		c.Set(aiScopeKey, delegated)
		c.Params = append(c.Params, gin.Param{Key: "resourceId", Value: in.TaskRef})
		c.Request = c.Request.Clone(aicontrol.WithDispatchedPrincipal(c.Request.Context(), delegated))
		c.Request.Method = "GET"
		c.Request.Body = nil
		c.Request.ContentLength = 0
		h.resource(c)
		return
	}
	ctx := aicontrol.WithRequestMetadata(c.Request.Context(), aicontrol.RequestAccess(c.Request, c.ClientIP()))
	out, e := h.service.Generic(ctx, p, in)
	if e != nil {
		companionFailure(c, e)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, 200, out)
}

func companionFailure(c *gin.Context, err error) {
	status, code, message := 422, "AI_INVALID", "请求参数不完整或不符合接口要求"
	switch {
	case errors.Is(err, aicontrol.ErrDenied):
		status, code, message = 403, "AI_DENIED", "当前身份无权访问此操作、目标或回执"
	case errors.Is(err, aicontrol.ErrConflict):
		status, code, message = 409, "AI_CONFLICT", "请求内容或确认条件已变化，请重新核对"
	case errors.Is(err, aicontrol.ErrBusy):
		status, code, message = 429, "AI_BUSY", "请求较多，请稍后再试"
	case errors.Is(err, aicontrol.ErrUnavailable):
		status, code, message = 503, "AI_UNAVAILABLE", "当前连接或功能暂不可用"
	case errors.Is(err, aicontrol.ErrUncertain):
		status, code, message = 409, "AI_RESULT_UNCERTAIN", "结果尚未确认，请查询原任务，不要重复提交"
	}
	Failure(c, status, code, message, gin.H{"reason": err.Error(), "retryable": false})
}
func (h *AIHandler) task(mode string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := h.taskPrincipal(c)
		if !ok {
			aiFailure(c, aicontrol.ErrDenied)
			return
		}
		if c.Request.URL.RawQuery != "" || (mode == "result" && (c.Request.ContentLength > 0 || len(c.Request.TransferEncoding) > 0)) {
			aiFailure(c, aicontrol.ErrInvalid)
			return
		}
		name := c.Param("tool")
		raw := []byte(`{}`)
		if mode != "result" {
			var e error
			raw, e = io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 12<<10))
			if e != nil {
				aiFailure(c, aicontrol.ErrInvalid)
				return
			}
			if len(raw) == 0 {
				raw = []byte(`{}`)
			}
		}
		if mode == "result" || mode == "execute" {
			if mode == "execute" {
				v, e := aicontrol.StrictBotJSON(raw)
				if e != nil || len(v) != 0 {
					aiFailure(c, aicontrol.ErrInvalid)
					return
				}
			}
			name = "dst_result"
			in := map[string]any{"taskRef": c.Param("taskRef")}
			if mode == "execute" {
				name = "dst_run"
				in["mode"] = "execute"
			}
			raw, _ = json.Marshal(in)
		} else {
			write := false
			found := false
			for _, tool := range aicontrol.TaskTools() {
				if tool.Name == name {
					write, found = tool.Mutation, true
					break
				}
			}
			if !found || write != (mode == "prepare") {
				aiFailure(c, aicontrol.ErrDenied)
				return
			}
			if name == "dst_run" {
				v, e := aicontrol.StrictBotJSON(raw)
				if e != nil || v["mode"] != "prepare" {
					aiFailure(c, aicontrol.ErrInvalid)
					return
				}
			}
		}
		ctx := aicontrol.WithRequestMetadata(c.Request.Context(), aicontrol.RequestAccess(c.Request, c.ClientIP()))
		result, e := h.service.InvokeTask(ctx, p, name, raw)
		if e != nil {
			aiFailure(c, e)
			return
		}
		c.Header("Cache-Control", "no-store")
		Success(c, 200, result)
	}
}
