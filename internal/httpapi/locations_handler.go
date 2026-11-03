package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"dont/internal/aicontrol"
	"dont/internal/authn"
	"dont/internal/locations"
	"github.com/gin-gonic/gin"
)

type LocationsHandler struct {
	service *locations.Service
	tokens  *aicontrol.Store
}

func NewLocationsHandler(s *locations.Service, tokens *aicontrol.Store) *LocationsHandler {
	return &LocationsHandler{s, tokens}
}
func (h *LocationsHandler) Register(v2 *gin.RouterGroup) {
	base := "/rooms/:roomId"
	v2.GET(base+"/locations", h.list)
	v2.GET(base+"/locations/:locationId", h.get)
	v2.POST(base+"/locations", h.save)
	v2.PATCH(base+"/locations/:locationId", h.save)
	v2.DELETE(base+"/locations/:locationId", h.remove)
	v2.POST(base+"/teleport-previews", h.prepare)
	v2.GET(base+"/teleport-previews/:previewId", h.preview)
	v2.POST(base+"/teleport-previews/:previewId/execute", h.execute)
	v2.POST(base+"/teleport-previews/:previewId/cancel", h.cancel)
}
func (h *LocationsHandler) actor(c *gin.Context) (string, error) {
	value, _ := c.Get(authn.ContextAdminKey)
	a, ok := value.(authn.Admin)
	if !ok || a.ID < 1 {
		return "", locations.ErrDenied
	}
	if c.GetHeader("Authorization") != "" {
		p, e := (&WorldQueryHandler{tokens: h.tokens}).principal(c)
		if e != nil {
			return "", locations.ErrDenied
		}
		return p.ID, nil
	}
	return fmt.Sprintf("admin:%d", a.ID), nil
}
func locationBody(c *gin.Context, out any) error {
	raw, e := io.ReadAll(io.LimitReader(c.Request.Body, 4097))
	if e != nil || len(raw) > 4096 {
		return locations.ErrInvalid
	}
	if _, e = aicontrol.StrictBotJSON(raw); e != nil {
		return locations.ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e = d.Decode(out); e != nil {
		return locations.ErrInvalid
	}
	return nil
}
func (h *LocationsHandler) list(c *gin.Context) {
	offset, e := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if e != nil {
		locationResponse(c, nil, locations.ErrInvalid)
		return
	}
	v, e := h.service.List(c.Param("roomId"), c.Query("worldId"), c.Query("search"), offset)
	locationResponse(c, gin.H{"items": v, "offset": offset, "limit": 50, "hasMore": len(v) == 50}, e)
}
func (h *LocationsHandler) get(c *gin.Context) {
	v, e := h.service.Get(c.Param("roomId"), c.Param("locationId"))
	locationResponse(c, v, e)
}
func (h *LocationsHandler) save(c *gin.Context) {
	a, e := h.actor(c)
	if e != nil {
		locationResponse(c, nil, e)
		return
	}
	var input locations.SaveInput
	if e = locationBody(c, &input); e != nil {
		locationResponse(c, nil, e)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	v, e := h.service.Save(ctx, a, c.Param("roomId"), c.Param("locationId"), input)
	locationResponse(c, v, e)
}
func (h *LocationsHandler) remove(c *gin.Context) {
	var in struct {
		Revision int64 `json:"revision"`
	}
	if e := locationBody(c, &in); e != nil || in.Revision < 1 {
		locationResponse(c, nil, locations.ErrInvalid)
		return
	}
	e := h.service.Delete(c.Param("roomId"), c.Param("locationId"), in.Revision)
	locationResponse(c, gin.H{"deleted": e == nil}, e)
}
func (h *LocationsHandler) prepare(c *gin.Context) {
	a, e := h.actor(c)
	if e != nil {
		locationResponse(c, nil, e)
		return
	}
	var in locations.PrepareInput
	if e = locationBody(c, &in); e != nil {
		locationResponse(c, nil, e)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	v, e := h.service.Prepare(ctx, a, c.Param("roomId"), in)
	locationResponse(c, v, e)
}
func (h *LocationsHandler) preview(c *gin.Context) {
	a, e := h.actor(c)
	if e != nil {
		locationResponse(c, nil, e)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	v, e := h.service.GetPreview(ctx, a, c.Param("roomId"), c.Param("previewId"), c.Query("refresh") == "true")
	locationResponse(c, v, e)
}
func (h *LocationsHandler) execute(c *gin.Context) {
	if aicontrol.IsModelChannel(c.Request.Context()) {
		locationResponse(c, nil, locations.ErrDenied)
		return
	}
	a, e := h.actor(c)
	if e != nil {
		locationResponse(c, nil, e)
		return
	}
	var empty struct{}
	if e = locationBody(c, &empty); e != nil {
		locationResponse(c, nil, e)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	v, e := h.service.Execute(ctx, a, c.Param("roomId"), c.Param("previewId"))
	locationResponse(c, v, e)
}
func (h *LocationsHandler) cancel(c *gin.Context) {
	a, e := h.actor(c)
	if e != nil {
		locationResponse(c, nil, e)
		return
	}
	var empty struct{}
	if e = locationBody(c, &empty); e != nil {
		locationResponse(c, nil, e)
		return
	}
	v, e := h.service.Cancel(c.Request.Context(), a, c.Param("roomId"), c.Param("previewId"))
	locationResponse(c, v, e)
}
func locationResponse(c *gin.Context, v any, e error) {
	c.Header("Cache-Control", "no-store")
	switch {
	case e == nil:
		Success(c, 200, v)
	case errors.Is(e, locations.ErrInvalid):
		Failure(c, 422, "LOCATION_INVALID", "请检查名称、玩家及坐标；名称最多 48 个字", nil)
	case errors.Is(e, locations.ErrConflict):
		Failure(c, 409, "LOCATION_CHANGED", "目标不唯一或已变化，请重新选择或检查", nil)
	case errors.Is(e, locations.ErrOldWorld):
		Failure(c, 409, "LOCATION_OLD_WORLD", "此地点属于之前的世界，请重新保存地点", nil)
	case errors.Is(e, locations.ErrNotFound):
		NotFound(c)
	case errors.Is(e, locations.ErrDenied):
		Failure(c, 403, "TELEPORT_CONFIRMATION_REQUIRED", "请在面板或机器人确认消息中确认传送", nil)
	case errors.Is(e, locations.ErrBusy), errors.Is(e, locations.ErrLimit):
		c.Header("Retry-After", "3")
		Failure(c, 429, "LOCATION_BUSY", "操作较多或仍有待确认传送，请稍后重试", nil)
	default:
		Failure(c, 503, "LOCATION_UNAVAILABLE", "暂时无法确认玩家或世界状态，请确认世界正在运行后重试", nil)
	}
}
