package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"dont/internal/aicontrol"
	"dont/internal/authn"
	"dont/internal/rooms"
	"dont/internal/worldquery"
	"github.com/gin-gonic/gin"
)

type WorldQueryHandler struct {
	service *worldquery.Service
	tokens  *aicontrol.Store
}

func NewWorldQueryHandler(s *worldquery.Service, tokens *aicontrol.Store) *WorldQueryHandler {
	return &WorldQueryHandler{s, tokens}
}
func (h *WorldQueryHandler) Register(v2 *gin.RouterGroup) {
	base := "/rooms/:roomId/worlds/:worldId"
	v2.GET(base+"/queries/:queryId", h.query)
	v2.GET(base+"/query-runs/:runId", h.run)
}
func (h *WorldQueryHandler) principal(c *gin.Context) (worldquery.Principal, error) {
	value, exists := c.Get(authn.ContextAdminKey)
	admin, ok := value.(authn.Admin)
	if !exists || !ok || admin.ID < 1 {
		return worldquery.Principal{}, worldquery.ErrDenied
	}
	if header := c.GetHeader("Authorization"); header != "" {
		scheme, secret, ok := strings.Cut(header, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || h.tokens == nil {
			return worldquery.Principal{}, worldquery.ErrDenied
		}
		p, err := h.tokens.Authenticate(secret)
		if err != nil || p.AdminID != admin.ID {
			return worldquery.Principal{}, worldquery.ErrDenied
		}
		return worldquery.Principal{ID: "token:" + p.ID, RoomID: p.RoomID}, nil
	}
	return worldquery.Principal{ID: fmt.Sprintf("admin:%d", admin.ID)}, nil
}
func queryArguments(c *gin.Context, allowed string) (url.Values, error) {
	if c.Request.Method != http.MethodGet || len(c.Request.URL.RawQuery) > 512 {
		return nil, worldquery.ErrInvalid
	}
	// Reject bodies without reading them: a slow chunked GET must not occupy
	// an unbounded handler while we wait to discover its first byte.
	if c.Request.ContentLength != 0 || len(c.Request.TransferEncoding) != 0 || c.Request.Body != nil && c.Request.Body != http.NoBody {
		return nil, worldquery.ErrInvalid
	}
	values, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		return nil, worldquery.ErrInvalid
	}
	for key, v := range values {
		if allowed == "" || key != allowed || len(v) != 1 {
			return nil, worldquery.ErrInvalid
		}
	}
	return values, nil
}
func (h *WorldQueryHandler) query(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	p, err := h.principal(c)
	if err != nil {
		worldQueryResponse(c, nil, err)
		return
	}
	q := worldquery.Request{RoomID: c.Param("roomId"), WorldID: c.Param("worldId"), QueryID: c.Param("queryId")}
	allowed := ""
	if q.QueryID == "entity_count" {
		allowed = "prefab"
	}
	values, err := queryArguments(c, allowed)
	if err != nil {
		worldQueryResponse(c, nil, err)
		return
	}
	q.Prefab = values.Get("prefab")
	v, err := h.service.Query(c.Request.Context(), p, q)
	worldQueryResponse(c, v, err)
}
func (h *WorldQueryHandler) run(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	p, err := h.principal(c)
	if err != nil {
		worldQueryResponse(c, nil, err)
		return
	}
	values, err := queryArguments(c, "refresh")
	if err != nil {
		worldQueryResponse(c, nil, err)
		return
	}
	refresh := values.Get("refresh")
	if _, ok := values["refresh"]; ok && refresh != "true" && refresh != "false" {
		worldQueryResponse(c, nil, worldquery.ErrInvalid)
		return
	}
	v, err := h.service.Get(c.Request.Context(), p, c.Param("roomId"), c.Param("worldId"), c.Param("runId"), refresh == "true")
	worldQueryResponse(c, v, err)
}
func worldQueryResponse(c *gin.Context, v any, err error) {
	switch {
	case err == nil:
		Success(c, 200, v)
	case errors.Is(err, worldquery.ErrDenied):
		Failure(c, 403, "WORLD_QUERY_DENIED", "无权访问此查询", nil)
	case errors.Is(err, worldquery.ErrInvalid), errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, 422, "WORLD_QUERY_INVALID", "查询标识或参数无效；仅支持固定白名单查询", nil)
	case errors.Is(err, worldquery.ErrLimited):
		c.Header("Retry-After", "5")
		Failure(c, 429, "WORLD_QUERY_RATE_LIMITED", "查询过于频繁，请稍后重试", nil)
	case errors.Is(err, worldquery.ErrBusy):
		c.Header("Retry-After", "1")
		Failure(c, 429, "WORLD_QUERY_BUSY", "查询正在执行，请等待原查询结果", nil)
	case errors.Is(err, worldquery.ErrNotFound), errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		NotFound(c)
	default:
		Failure(c, 503, "WORLD_QUERY_UNAVAILABLE", "查询暂不可用，请查看服务日志", nil)
	}
}
