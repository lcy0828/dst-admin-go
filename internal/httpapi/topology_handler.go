package httpapi

import (
	"errors"
	"net/http"

	"dont/internal/rooms"
	"dont/internal/topology"

	"github.com/gin-gonic/gin"
)

type TopologyHandler struct {
	service *topology.Service
}

func NewTopologyHandler(service *topology.Service) *TopologyHandler {
	return &TopologyHandler{service: service}
}

func (h *TopologyHandler) Register(v2 *gin.RouterGroup) {
	room := v2.Group("/rooms/:roomId/topology")
	room.GET("", h.get)
	room.POST("/preview", h.preview)
	room.PUT("", h.update)
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
	value, err := h.service.Update(c.Request.Context(), c.Param("roomId"), request)
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
	var conflict *topology.RevisionConflictError
	var overcommit *topology.OvercommitError
	switch {
	case errors.As(err, &fields):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_TOPOLOGY", "拓扑规划字段无效", gin.H{"fields": fields.Fields})
	case errors.As(err, &conflict):
		Failure(c, http.StatusConflict, "TOPOLOGY_REVISION_CONFLICT", "拓扑已被其他请求修改，请刷新后重试", gin.H{"currentRevision": conflict.CurrentRevision})
	case errors.As(err, &overcommit):
		Failure(c, http.StatusUnprocessableEntity, "TOPOLOGY_OVERCOMMIT_CONFIRMATION_REQUIRED", "计划分片数超过节点建议容量，请确认了解卡顿风险后重试", gin.H{"preview": overcommit.Preview})
	case errors.Is(err, topology.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_NOT_MANAGED", "接管房间后才能规划运行拓扑", nil)
	case errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_RESOURCE_ID", "房间标识无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间不存在", nil)
	default:
		Failure(c, http.StatusInternalServerError, "TOPOLOGY_OPERATION_FAILED", "拓扑规划操作失败", nil)
	}
}
