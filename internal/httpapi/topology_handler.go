package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"

	"dont/internal/roomops"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"

	"github.com/gin-gonic/gin"
)

type TopologyHandler struct {
	service    *topology.Service
	shardLinks shardLinkConfigurationApplier
}

type shardLinkConfigurationApplier interface {
	ApplyDesiredConfiguration(context.Context, string, string) error
}

func NewTopologyHandler(service *topology.Service) *TopologyHandler {
	return &TopologyHandler{service: service}
}

func (h *TopologyHandler) ConfigureShardLinkApplier(applier shardLinkConfigurationApplier) error {
	if applier == nil {
		return errors.New("Shard link configuration applier is required")
	}
	h.shardLinks = applier
	return nil
}

func (h *TopologyHandler) Register(v2 *gin.RouterGroup) {
	room := v2.Group("/rooms/:roomId/topology")
	room.GET("", h.get)
	room.POST("/preview", h.preview)
	room.PUT("", h.update)
	room.POST("/shard-links/actions/discover", h.discoverShardLinks)
	room.POST("/shard-links/actions/apply", h.applyShardLinks)
	infrastructure := v2.Group("/runtime-infrastructure")
	infrastructure.GET("", h.infrastructure)
	infrastructure.PUT("/network-profiles/:profileId", h.updateNetworkProfile)
	infrastructure.POST("/network-profiles/:profileId/actions/detect-egress", h.detectNetworkProfileEgress)
	infrastructure.PUT("/cpu-allocations", h.updateCPUAllocation)
}

func (h *TopologyHandler) applyShardLinks(c *gin.Context) {
	if h.shardLinks == nil {
		Failure(c, http.StatusServiceUnavailable, "SHARD_LINK_APPLIER_UNAVAILABLE", "世界互联线路应用服务不可用", nil)
		return
	}
	request := struct {
		ExpectedRevision string `json:"expectedRevision"`
	}{}
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "世界互联线路应用请求不是有效 JSON", nil)
		return
	}
	roomID := c.Param("roomId")
	ctx, release, err := roomops.Acquire(c.Request.Context(), roomID)
	if err != nil {
		topologyFailure(c, err)
		return
	}
	defer release()
	if err := h.shardLinks.ApplyDesiredConfiguration(ctx, roomID, request.ExpectedRevision); err != nil {
		topologyFailure(c, err)
		return
	}
	value, err := h.service.Topology(ctx, roomID)
	if err != nil {
		topologyFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *TopologyHandler) discoverShardLinks(c *gin.Context) {
	var request topology.ShardLinkDiscoveryRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "Shard 互联候选探测请求不是有效 JSON", nil)
		return
	}
	value, err := h.service.DiscoverShardLinks(c.Request.Context(), c.Param("roomId"), request)
	if err != nil {
		topologyFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *TopologyHandler) detectNetworkProfileEgress(c *gin.Context) {
	request := struct {
		Region shared.RuntimeNetworkRegion `json:"region"`
	}{}
	if err := c.ShouldBindJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "公网出口探测请求不是有效 JSON", nil)
		return
	}
	value, err := h.service.DetectNetworkProfileEgress(c.Request.Context(), c.Param("profileId"), request.Region)
	if err != nil {
		topologyFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *TopologyHandler) infrastructure(c *gin.Context) {
	value, err := h.service.Infrastructure(c.Request.Context())
	if err != nil {
		topologyFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *TopologyHandler) updateNetworkProfile(c *gin.Context) {
	var request topology.NetworkProfileUpdate
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "网络配置请求不是有效 JSON", nil)
		return
	}
	value, err := h.service.UpdateNetworkProfile(c.Request.Context(), c.Param("profileId"), request)
	if err != nil {
		topologyFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *TopologyHandler) updateCPUAllocation(c *gin.Context) {
	var request topology.CPUAllocationUpdate
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "CPU 分配请求不是有效 JSON", nil)
		return
	}
	value, err := h.service.UpdateCPUAllocation(c.Request.Context(), request)
	if err != nil {
		topologyFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *TopologyHandler) get(c *gin.Context) {
	value, err := h.service.Topology(c.Request.Context(), c.Param("roomId"))
	if err != nil {
		topologyFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *TopologyHandler) preview(c *gin.Context) {
	request, ok := bindTopologyRequest(c)
	if !ok {
		return
	}
	value, err := h.service.Preview(c.Request.Context(), c.Param("roomId"), request)
	if err != nil {
		topologyFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *TopologyHandler) update(c *gin.Context) {
	request, ok := bindTopologyRequest(c)
	if !ok {
		return
	}
	roomID := c.Param("roomId")
	ctx, release, err := roomops.Acquire(c.Request.Context(), roomID)
	if err != nil {
		topologyFailure(c, err)
		return
	}
	defer release()
	value, err := h.service.Update(ctx, roomID, request)
	if err != nil {
		topologyFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func bindTopologyRequest(c *gin.Context) (topology.UpdateRequest, bool) {
	var request topology.UpdateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "拓扑规划请求不是有效 JSON", nil)
		return topology.UpdateRequest{}, false
	}
	return request, true
}

func topologyFailure(c *gin.Context, err error) {
	var fields *topology.FieldError
	var resourceFields *topology.ResourceFieldError
	var conflict *topology.RevisionConflictError
	var overcommit *topology.OvercommitError
	var resourceConflict *topology.ResourceConflictError
	var execution *topology.ExecutionError
	switch {
	case errors.As(err, &fields):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_TOPOLOGY", "拓扑规划字段无效", gin.H{"fields": fields.Fields})
	case errors.As(err, &resourceFields):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_RUNTIME_RESOURCE", "运行资源字段无效", gin.H{"fields": resourceFields.Fields})
	case errors.As(err, &conflict):
		Failure(c, http.StatusConflict, "TOPOLOGY_REVISION_CONFLICT", "拓扑已被其他请求修改，请刷新后重试", gin.H{"currentRevision": conflict.CurrentRevision})
	case errors.As(err, &overcommit):
		Failure(c, http.StatusUnprocessableEntity, "TOPOLOGY_OVERCOMMIT_CONFIRMATION_REQUIRED", "计划分片数超过节点建议容量，请确认了解卡顿风险后重试", gin.H{"preview": overcommit.Preview})
	case errors.As(err, &resourceConflict):
		Failure(c, http.StatusConflict, "RUNTIME_RESOURCE_CONFLICT", "运行资源预检发现冲突", gin.H{"preflight": resourceConflict.Preflight})
	case errors.Is(err, topology.ErrCPUNotSupported):
		Failure(c, http.StatusUnprocessableEntity, "CPU_POLICY_NOT_SUPPORTED", "当前执行环境不支持该 CPU 分配策略", nil)
	case errors.Is(err, topology.ErrResourceNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "运行资源不存在", nil)
	case errors.Is(err, topology.ErrEgressDetectionUnavailable):
		Failure(c, http.StatusServiceUnavailable, "EGRESS_DETECTION_UNAVAILABLE", "当前运行目标不支持公网出口探测", nil)
	case errors.Is(err, topology.ErrEgressDetectionFailed):
		Failure(c, http.StatusBadGateway, "EGRESS_DETECTION_FAILED", "无法从运行节点探测公网出口 IP", nil)
	case errors.As(err, &execution):
		Failure(c, http.StatusConflict, execution.Code, execution.Message, nil)
	case errors.Is(err, topology.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_UNAVAILABLE", "房间当前不可用，请检查运行节点与拓扑状态", nil)
	case errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_RESOURCE_ID", "房间标识无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间不存在", nil)
	case errors.Is(err, rooms.ErrWorldNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "世界不存在", nil)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		Failure(c, http.StatusRequestTimeout, "TOPOLOGY_OPERATION_CANCELED", "拓扑操作等待期间已取消", nil)
	default:
		Failure(c, http.StatusInternalServerError, "TOPOLOGY_OPERATION_FAILED", "拓扑规划操作失败", nil)
	}
}
