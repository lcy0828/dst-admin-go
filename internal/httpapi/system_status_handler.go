package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"dont/internal/systemstatus"

	"github.com/gin-gonic/gin"
)

type SystemStatusHandler struct {
	service   *systemstatus.Service
	resources *systemstatus.NodeResourceService
}

func NewSystemStatusHandler(service *systemstatus.Service, resources ...*systemstatus.NodeResourceService) *SystemStatusHandler {
	handler := &SystemStatusHandler{service: service}
	if len(resources) > 0 {
		handler.resources = resources[0]
	}
	return handler
}

func (h *SystemStatusHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/system/status", func(c *gin.Context) { Success(c, http.StatusOK, h.service.Status()) })
	if h.resources != nil {
		v2.GET("/system/resources", h.nodeResources)
	}
}

func (h *SystemStatusHandler) nodeResources(c *gin.Context) {
	refresh, err := strconv.ParseBool(c.DefaultQuery("refresh", "false"))
	if err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_REFRESH", "refresh 必须为布尔值", nil)
		return
	}
	c.Header("Cache-Control", "no-store")
	var value systemstatus.NodeResourceSnapshot
	if refresh {
		value, err = h.resources.Refresh(c.Request.Context(), c.Query("targetId"))
	} else {
		value, err = h.resources.Snapshot(c.Query("targetId"))
	}
	if errors.Is(err, systemstatus.ErrNodeResourceNotFound) {
		Failure(c, http.StatusNotFound, "NODE_RESOURCE_NOT_FOUND", "机器资源不存在", nil)
		return
	}
	if err != nil {
		Failure(c, http.StatusInternalServerError, "NODE_RESOURCE_LOAD_FAILED", "机器资源读取失败", nil)
		return
	}
	Success(c, http.StatusOK, value)
}
