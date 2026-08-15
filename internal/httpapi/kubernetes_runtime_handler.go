package httpapi

import (
	"context"
	"errors"
	"net/http"

	"dont/internal/kubernetesruntime"

	"github.com/gin-gonic/gin"
)

type KubernetesRuntimeService interface {
	Status() kubernetesruntime.ServiceView
	Observe(context.Context, string, string, string) (kubernetesruntime.Observation, error)
	Preview(context.Context, string, kubernetesruntime.Request) (kubernetesruntime.Preview, error)
}

type KubernetesRuntimeHandler struct{ service KubernetesRuntimeService }

func NewKubernetesRuntimeHandler(service KubernetesRuntimeService) (*KubernetesRuntimeHandler, error) {
	if service == nil {
		return nil, errors.New("kubernetes runtime handler service is required")
	}
	return &KubernetesRuntimeHandler{service: service}, nil
}

func (h *KubernetesRuntimeHandler) Register(v2 *gin.RouterGroup) {
	group := v2.Group("/runtime-providers/kubernetes")
	group.GET("", h.status)
	group.POST("/:providerId/shards/observe", h.observe)
	group.POST("/:providerId/shards/preflight", h.preflight)
}

func (h *KubernetesRuntimeHandler) status(c *gin.Context) {
	Success(c, http.StatusOK, h.service.Status())
}

func (h *KubernetesRuntimeHandler) observe(c *gin.Context) {
	var request struct {
		RoomID  string `json:"roomId" binding:"required"`
		WorldID string `json:"worldId" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "Kubernetes 观察请求不是有效 JSON", nil)
		return
	}
	value, err := h.service.Observe(c.Request.Context(), c.Param("providerId"), request.RoomID, request.WorldID)
	if err != nil {
		kubernetesRuntimeFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *KubernetesRuntimeHandler) preflight(c *gin.Context) {
	var request kubernetesruntime.Request
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "Kubernetes 预检请求不是有效 JSON", nil)
		return
	}
	value, err := h.service.Preview(c.Request.Context(), c.Param("providerId"), request)
	if err != nil {
		kubernetesRuntimeFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func kubernetesRuntimeFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, kubernetesruntime.ErrDisabled):
		Failure(c, http.StatusConflict, "KUBERNETES_EXPERIMENT_DISABLED", "Kubernetes 实验 Provider 未启用", nil)
	case errors.Is(err, kubernetesruntime.ErrUnavailable):
		Failure(c, http.StatusServiceUnavailable, "KUBERNETES_PROVIDER_UNAVAILABLE", "Kubernetes 实验 Provider 配置不可用", nil)
	case errors.Is(err, kubernetesruntime.ErrProviderMissing):
		Failure(c, http.StatusNotFound, "KUBERNETES_PROVIDER_NOT_FOUND", "Kubernetes Provider 不存在", nil)
	case errors.Is(err, kubernetesruntime.ErrObservation):
		Failure(c, http.StatusServiceUnavailable, "KUBERNETES_OBSERVE_FAILED", "无法读取受管 Kubernetes Shard 状态", gin.H{"cause": err.Error()})
	default:
		Failure(c, http.StatusInternalServerError, "KUBERNETES_PROVIDER_FAILED", "Kubernetes 实验 Provider 操作失败", nil)
	}
}
