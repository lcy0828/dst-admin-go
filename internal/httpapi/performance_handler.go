package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"dont/internal/performance"
	"dont/internal/rooms"
	"github.com/gin-gonic/gin"
)

type PerformanceHandler struct{ service *performance.Service }

func NewPerformanceHandler(s *performance.Service) *PerformanceHandler {
	return &PerformanceHandler{service: s}
}
func (h *PerformanceHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/rooms/:roomId/performance", h.report)
	v2.GET("/rooms/:roomId/performance/settings", h.settings)
	v2.PUT("/rooms/:roomId/performance/settings", h.save)
}
func (h *PerformanceHandler) report(c *gin.Context) {
	q := performance.Query{WorldID: c.Query("worldId")}
	var err error
	for _, param := range []struct {
		name string
		dst  *time.Time
	}{{"from", &q.From}, {"to", &q.To}} {
		if value := c.Query(param.name); value != "" {
			*param.dst, err = time.Parse(time.RFC3339, value)
			if err != nil {
				performanceFailure(c, performance.ErrInvalid)
				return
			}
		}
	}
	if value := c.Query("maxPoints"); value != "" {
		q.MaxPoints, err = strconv.Atoi(value)
		if err != nil {
			performanceFailure(c, performance.ErrInvalid)
			return
		}
	}
	v, err := h.service.Report(c.Param("roomId"), q)
	if err != nil {
		performanceFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, v)
}
func (h *PerformanceHandler) settings(c *gin.Context) {
	v, err := h.service.Settings(c.Param("roomId"))
	if err != nil {
		performanceFailure(c, err)
		return
	}
	Success(c, http.StatusOK, v)
}
func (h *PerformanceHandler) save(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var input performance.Settings
	if err := c.ShouldBindJSON(&input); err != nil {
		performanceFailure(c, performance.ErrInvalid)
		return
	}
	v, err := h.service.UpdateSettings(c.Param("roomId"), input)
	if err != nil {
		performanceFailure(c, err)
		return
	}
	Success(c, http.StatusOK, v)
}
func performanceFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, performance.ErrInvalid):
		Failure(c, 422, "PERFORMANCE_INVALID", "性能采集设置或查询范围无效", nil)
	case errors.Is(err, performance.ErrConflict):
		Failure(c, 409, "PERFORMANCE_CONFLICT", "性能采集设置已更改，请刷新后重试", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		NotFound(c)
	case errors.Is(err, rooms.ErrRoomNotManaged):
		Failure(c, 409, "ROOM_NOT_MANAGED", "房间尚未纳入管理", nil)
	default:
		Failure(c, 500, "PERFORMANCE_FAILED", "性能报表读取失败", nil)
	}
}
