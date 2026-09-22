package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"

	"dont/internal/dstruntime"
	"dont/internal/entitycatalog"

	"github.com/gin-gonic/gin"
)

type EntityCatalogSearch interface {
	Search(context.Context, entitycatalog.SearchOptions) (entitycatalog.SearchResult, error)
}

type EntityCatalogHandler struct {
	artwork   EntityArtworkReader
	catalog   EntityCatalogSearch
	runtime   entitycatalog.RuntimeCommander
	snapshots *entitycatalog.SnapshotCache
}

func NewEntityCatalogHandler(catalog EntityCatalogSearch) *EntityCatalogHandler {
	return &EntityCatalogHandler{catalog: catalog}
}

func (h *EntityCatalogHandler) ConfigureRuntime(runtime entitycatalog.RuntimeCommander) {
	h.runtime = runtime
	h.snapshots = entitycatalog.NewSnapshotCache(runtime)
}

func (h *EntityCatalogHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/entity-catalog/entities", h.search)
	v2.GET("/entity-catalog/directory", func(c *gin.Context) { Success(c, http.StatusOK, entitycatalog.Directory()) })
	v2.GET("/rooms/:roomId/worlds/:worldId/entity-catalog/snapshot", h.snapshot)
	v2.GET("/rooms/:roomId/worlds/:worldId/entity-catalog/artwork/:prefab", h.readArtwork)
	v2.POST("/rooms/:roomId/worlds/:worldId/entity-catalog/search", h.searchRuntime)
}

func (h *EntityCatalogHandler) snapshot(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	refresh, err := strconv.ParseBool(c.DefaultQuery("refresh", "false"))
	if err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_ENTITY_SEARCH", "实体搜索参数无效", nil)
		return
	}
	if h.snapshots == nil {
		Failure(c, http.StatusConflict, "RUNTIME_CATALOG_UNAVAILABLE", "当前世界目录不可用", gin.H{"retryable": false})
		return
	}
	result, err := h.snapshots.Read(c.Request.Context(), c.Param("roomId"), c.Param("worldId"), refresh)
	if err != nil {
		if c.Request.Context().Err() != nil {
			c.Status(499)
			return
		}
		log.Printf("[ENTITY-CATALOG] request_id=%q room=%q world=%q snapshot failed: %v", RequestID(c), c.Param("roomId"), c.Param("worldId"), err)
		code, message, retryable := "RUNTIME_CATALOG_UNAVAILABLE", "暂时未能读取世界目录，仍可浏览原版资源", true
		switch {
		case errors.Is(err, dstruntime.ErrRuntimeCommandBusy):
			code, message = "RUNTIME_BUSY", "世界正在处理其他操作，稍后可重新读取"
		case errors.Is(err, entitycatalog.ErrRuntimeCatalogChanged):
			code, message = "RUNTIME_CATALOG_CHANGED", "世界目录在读取过程中更新，正在重新读取"
		case errors.Is(err, context.DeadlineExceeded):
			code, message = "RUNTIME_CATALOG_TIMEOUT", "世界目录读取超时，可重新读取"
		case errors.Is(err, entitycatalog.ErrRuntimeCatalogLimit):
			code, message, retryable = "RUNTIME_CATALOG_LIMIT", "目录读取达到资源上限，请稍后重试或输入 Prefab", false
		case errors.Is(err, dstruntime.ErrRuntimeNotInstalled):
			code, message, retryable = "RUNTIME_NOT_INSTALLED", "请先在 Runtime 管理中为此世界安装采集组件", false
		}
		Failure(c, http.StatusConflict, code, message, gin.H{"retryable": retryable})
		return
	}
	Success(c, http.StatusOK, result)
}

func (h *EntityCatalogHandler) search(c *gin.Context) {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "120"))
	if err != nil {
		Failure(c, http.StatusUnprocessableEntity, "INVALID_ENTITY_SEARCH", "实体搜索参数无效", nil)
		return
	}
	result, err := h.catalog.Search(c.Request.Context(), entitycatalog.SearchOptions{
		Query: c.Query("q"), Kind: entitycatalog.Capability(c.Query("kind")), Limit: limit,
	})
	if err != nil {
		if errors.Is(err, entitycatalog.ErrInvalidSearch) {
			Failure(c, http.StatusUnprocessableEntity, "INVALID_ENTITY_SEARCH", "实体搜索参数无效", nil)
			return
		}
		Failure(c, http.StatusBadGateway, "ENTITY_SEARCH_FAILED", "实体目录暂时不可用", nil)
		return
	}
	Success(c, http.StatusOK, result)
}

func (h *EntityCatalogHandler) searchRuntime(c *gin.Context) {
	var input entitycatalog.RuntimeSearchOptions
	if c.ShouldBindJSON(&input) != nil {
		Failure(c, http.StatusBadRequest, "INVALID_ENTITY_SEARCH", "实体搜索参数无效", nil)
		return
	}
	result, err := entitycatalog.SearchRuntime(c.Request.Context(), h.runtime, c.Param("roomId"), c.Param("worldId"), input)
	if err != nil {
		log.Printf("[ENTITY-CATALOG] room=%q world=%q read failed: %v", c.Param("roomId"), c.Param("worldId"), err)
		if errors.Is(err, entitycatalog.ErrInvalidSearch) {
			Failure(c, http.StatusUnprocessableEntity, "INVALID_ENTITY_SEARCH", "实体搜索参数无效", nil)
		} else if errors.Is(err, entitycatalog.ErrRuntimeCatalogLimit) {
			Failure(c, http.StatusConflict, "RUNTIME_CATALOG_LIMIT", "实体目录过大或读取超时，可稍后重试或直接输入 Prefab", nil)
		} else if errors.Is(err, dstruntime.ErrRuntimeCommandBusy) {
			Failure(c, http.StatusConflict, "RUNTIME_BUSY", "当前世界正在处理其他操作，请稍后重新读取目录", nil)
		} else {
			Failure(c, http.StatusConflict, "RUNTIME_CATALOG_UNAVAILABLE", "无法读取当前世界实体；请确认世界运行中，并在 Runtime 管理中更新、重载采集组件", nil)
		}
		return
	}
	Success(c, http.StatusOK, result)
}
