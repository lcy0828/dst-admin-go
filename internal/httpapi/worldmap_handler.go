package httpapi

import (
	"errors"
	"mime"
	"net/http"
	"strings"

	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/internal/worldmap"

	"github.com/gin-gonic/gin"
)

type WorldMapHandler struct {
	maps *worldmap.Service
	jobs *jobs.Service
}

func NewWorldMapHandler(service *worldmap.Service, jobService *jobs.Service) *WorldMapHandler {
	return &WorldMapHandler{maps: service, jobs: jobService}
}

func (h *WorldMapHandler) Register(v2 *gin.RouterGroup) {
	roomsGroup := v2.Group("/rooms/:roomId")
	roomsGroup.GET("/maps", h.list)
	roomsGroup.POST("/maps/actions/generate", h.generate)
	roomsGroup.GET("/worlds/:worldId/sessions", h.sessions)
	v2.GET("/sessions/:sessionId/download", h.downloadSession)
	v2.GET("/maps/:mapId/images/:layer", h.image)
	v2.GET("/maps/:mapId/manifest", h.artifact(worldmap.ArtifactManifest))
	v2.GET("/maps/:mapId/features", h.artifact(worldmap.ArtifactFeatures))
}

func (h *WorldMapHandler) list(c *gin.Context) {
	items, err := h.maps.List(c.Param("roomId"))
	if err != nil {
		worldMapFailure(c, err)
		return
	}
	renderer := h.maps.RendererStatus()
	Success(c, http.StatusOK, gin.H{
		"items": items, "total": len(items), "renderer": renderer,
		"rendererAvailable": renderer.Available, "rendererPath": renderer.Path,
	})
}

func (h *WorldMapHandler) sessions(c *gin.Context) {
	items, err := h.maps.SessionsContext(c.Request.Context(), c.Param("roomId"), c.Param("worldId"))
	if err != nil {
		worldMapFailure(c, err)
		return
	}
	renderer := h.maps.RendererStatusForWorld(c.Request.Context(), c.Param("roomId"), c.Param("worldId"))
	Success(c, http.StatusOK, gin.H{"items": items, "total": len(items), "renderer": renderer})
}

func (h *WorldMapHandler) generate(c *gin.Context) {
	var request worldmap.GenerateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的地图生成配置", nil)
		return
	}
	targets, factory, release, err := h.maps.PrepareContext(c.Request.Context(), c.Param("roomId"), request)
	if err != nil {
		worldMapFailure(c, err)
		return
	}
	job, err := h.jobs.SubmitFactory("map.generate", c.Param("roomId"), request.WorldID, targets, factory)
	if err != nil {
		release()
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建地图生成任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *WorldMapHandler) downloadSession(c *gin.Context) {
	file, info, session, cleanup, err := h.maps.OpenSessionContext(c.Request.Context(), c.Param("sessionId"))
	if err != nil {
		worldMapFailure(c, err)
		return
	}
	defer file.Close()
	defer cleanup()
	name := "dst-session-" + session.SessionID + "-" + session.FileName + ".bin"
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	c.Header("Content-Type", "application/octet-stream")
	c.Header("X-Content-Type-Options", "nosniff")
	http.ServeContent(c.Writer, c.Request, name, info.ModTime(), file)
}

func (h *WorldMapHandler) image(c *gin.Context) {
	layer := worldmap.Layer(c.Param("layer"))
	file, info, _, err := h.maps.OpenImage(c.Param("mapId"), layer)
	if err != nil {
		worldMapFailure(c, err)
		return
	}
	defer file.Close()
	c.Header("Content-Type", "image/png")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(c.Writer, c.Request, strings.ReplaceAll(string(layer), "World", "-world")+".png", info.ModTime(), file)
}

func (h *WorldMapHandler) artifact(artifact worldmap.Artifact) gin.HandlerFunc {
	return func(c *gin.Context) {
		file, info, _, err := h.maps.OpenArtifact(c.Param("mapId"), artifact)
		if err != nil {
			worldMapFailure(c, err)
			return
		}
		defer file.Close()
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Cache-Control", "private, max-age=31536000, immutable")
		http.ServeContent(c.Writer, c.Request, string(artifact)+".json", info.ModTime(), file)
	}
}

func worldMapFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, worldmap.ErrMapNotFound), errors.Is(err, worldmap.ErrMapImageNotFound), errors.Is(err, worldmap.ErrSessionNotFound):
		Failure(c, http.StatusNotFound, "MAP_RESOURCE_NOT_FOUND", "地图或 Session 资源不存在", nil)
	case errors.Is(err, worldmap.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_UNAVAILABLE", "房间当前不可用，请检查运行节点与拓扑状态", nil)
	case errors.Is(err, worldmap.ErrRendererUnavailable):
		Failure(c, http.StatusConflict, "MAP_RENDERER_UNAVAILABLE", "地图渲染器不可用，请先完成节点配置", nil)
	case errors.Is(err, worldmap.ErrGenerationInProgress):
		Failure(c, http.StatusConflict, "MAP_GENERATION_IN_PROGRESS", "该世界已有地图生成任务", nil)
	case errors.Is(err, worldmap.ErrInvalidLayers):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_MAP_LAYERS", "地图图层配置无效", nil)
	case errors.Is(err, worldmap.ErrUnsafeSessionPath), errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_MAP_RESOURCE", "地图资源或路径无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间或世界不存在", nil)
	default:
		Failure(c, http.StatusInternalServerError, "MAP_OPERATION_FAILED", "地图操作失败", nil)
	}
}
