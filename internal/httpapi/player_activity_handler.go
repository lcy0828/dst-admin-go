package httpapi

import (
	"dont/internal/playeractivity"
	"dont/internal/rooms"
	"errors"
	"github.com/gin-gonic/gin"
	"net/http"
	"strconv"
	"time"
)

type PlayerActivityHandler struct{ service *playeractivity.Service }

func NewPlayerActivityHandler(service *playeractivity.Service) *PlayerActivityHandler {
	return &PlayerActivityHandler{service}
}
func (h *PlayerActivityHandler) Register(v2 *gin.RouterGroup) {
	base := "/rooms/:roomId/player-activity"
	v2.GET(base+"/settings", h.settings)
	v2.PUT(base+"/settings", h.update)
	v2.GET(base+"/status", h.status)
	v2.GET(base+"/events", h.events)
	v2.GET(base+"/trajectory", h.trajectory)
	v2.GET(base+"/combat", h.combat)
}
func activityResult(c *gin.Context, value any, err error) {
	c.Header("Cache-Control", "no-store")
	switch {
	case err == nil:
		Success(c, http.StatusOK, value)
	case errors.Is(err, playeractivity.ErrInvalid):
		Failure(c, 400, "PLAYER_ACTIVITY_INVALID", "采集参数或筛选条件无效", nil)
	case errors.Is(err, playeractivity.ErrConflict):
		Failure(c, 409, "PLAYER_ACTIVITY_CONFLICT", "采集配置已修改，请刷新后重试", nil)
	case errors.Is(err, playeractivity.ErrNotFound) || errors.Is(err, rooms.ErrRoomNotFound) || errors.Is(err, rooms.ErrWorldNotFound):
		NotFound(c)
	default:
		Failure(c, 503, "PLAYER_ACTIVITY_UNAVAILABLE", "暂时无法读取玩家活动记录，请稍后重试", nil)
	}
}
func (h *PlayerActivityHandler) settings(c *gin.Context) {
	v, e := h.service.Settings(c.Param("roomId"))
	activityResult(c, v, e)
}
func (h *PlayerActivityHandler) update(c *gin.Context) {
	var p playeractivity.SettingsPatch
	if c.ShouldBindJSON(&p) != nil {
		activityResult(c, nil, playeractivity.ErrInvalid)
		return
	}
	v, e := h.service.UpdateSettings(c.Param("roomId"), p)
	activityResult(c, v, e)
}
func (h *PlayerActivityHandler) status(c *gin.Context) {
	v, e := h.service.Status(c.Param("roomId"))
	activityResult(c, v, e)
}
func activityQuery(c *gin.Context) (playeractivity.Query, error) {
	q := playeractivity.Query{UserID: c.Query("userId"), WorldID: c.Query("worldId"), Kind: c.Query("kind"), Before: c.Query("before")}
	var err error
	if v := c.Query("limit"); v != "" {
		q.Limit, err = strconv.Atoi(v)
		if err != nil {
			return q, playeractivity.ErrInvalid
		}
	}
	for _, v := range []struct {
		name string
		to   *time.Time
	}{{"from", &q.From}, {"to", &q.To}} {
		if raw := c.Query(v.name); raw != "" {
			*v.to, err = time.Parse(time.RFC3339, raw)
			if err != nil {
				return q, playeractivity.ErrInvalid
			}
		}
	}
	return q, nil
}
func (h *PlayerActivityHandler) events(c *gin.Context) {
	q, e := activityQuery(c)
	if e != nil {
		activityResult(c, nil, e)
		return
	}
	v, e := h.service.Events(c.Param("roomId"), q)
	activityResult(c, v, e)
}
func (h *PlayerActivityHandler) trajectory(c *gin.Context) {
	q, e := activityQuery(c)
	if e != nil {
		activityResult(c, nil, e)
		return
	}
	v, e := h.service.Trajectory(c.Param("roomId"), q)
	activityResult(c, v, e)
}
func (h *PlayerActivityHandler) combat(c *gin.Context) {
	q, e := activityQuery(c)
	if e != nil {
		activityResult(c, nil, e)
		return
	}
	v, e := h.service.Combat(c.Param("roomId"), q)
	activityResult(c, v, e)
}
