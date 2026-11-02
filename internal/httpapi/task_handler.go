package httpapi

import (
	"encoding/json"
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
	Success(c, 200, gin.H{"protocolVersions": []int{1, 2}, "preferredVersion": 2, "capabilityContractVersions": []int{1}, "profile": profile, "roomIds": p.RoomIDs, "roomId": p.RoomID, "features": []string{"task-tools", "typed-task-parameters", "prepared-tasks", "original-result-recovery", "capability-revisions"}})
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
