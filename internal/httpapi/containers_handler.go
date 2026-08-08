package httpapi

import (
	"errors"
	"net/http"

	"dont/internal/containers"

	"github.com/gin-gonic/gin"
)

type ContainerHandler struct{ service *containers.Service }

func NewContainerHandler(service *containers.Service) *ContainerHandler {
	return &ContainerHandler{service: service}
}

func (h *ContainerHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/containers", h.list)
	v2.POST("/containers/:containerId/actions/:action", h.run)
}

func (h *ContainerHandler) list(c *gin.Context) {
	Success(c, http.StatusOK, h.service.List(c.Request.Context()))
}

func (h *ContainerHandler) run(c *gin.Context) {
	var input containers.ActionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "容器操作请求不是有效 JSON", nil)
		return
	}
	job, err := h.service.Run(c.Param("containerId"), containers.Action(c.Param("action")), input)
	if err != nil {
		containerFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func containerFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, containers.ErrNotFound):
		NotFound(c)
	case errors.Is(err, containers.ErrUnavailable):
		Failure(c, http.StatusServiceUnavailable, "DOCKER_UNAVAILABLE", "Docker CLI 不可用", nil)
	case errors.Is(err, containers.ErrConfirmationRequired):
		Failure(c, http.StatusUnprocessableEntity, "CONTAINER_CONFIRMATION_REQUIRED", "请输入容器名确认移除", map[string]string{"confirmation": "确认内容不匹配"})
	case errors.Is(err, containers.ErrConflict):
		Failure(c, http.StatusConflict, "CONTAINER_ACTION_CONFLICT", "容器当前状态不允许该操作或已有操作进行中", nil)
	case errors.Is(err, containers.ErrInvalidInput):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_CONTAINER_ACTION", "容器动作无效", nil)
	default:
		Failure(c, http.StatusInternalServerError, "CONTAINER_ACTION_FAILED", "容器操作失败", nil)
	}
}
