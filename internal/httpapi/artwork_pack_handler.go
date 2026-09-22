package httpapi

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"dont/internal/artworkpack"
	"dont/internal/entitycatalog"
	"github.com/gin-gonic/gin"
)

type ArtworkPackHandler struct{ service *artworkpack.Service }

func NewArtworkPackHandler(service *artworkpack.Service) *ArtworkPackHandler {
	return &ArtworkPackHandler{service: service}
}
func (h *ArtworkPackHandler) Register(v2 *gin.RouterGroup) {
	group := v2.Group("/entity-catalog/artwork-pack")
	group.GET("", func(c *gin.Context) { c.Header("Cache-Control", "no-store"); Success(c, 200, h.service.Status()) })
	group.GET("/index", func(c *gin.Context) {
		revision, ids := h.service.Index()
		c.Header("Cache-Control", "no-store")
		Success(c, 200, gin.H{"revision": revision, "prefabs": ids})
	})
	group.GET("/artwork/:prefab", h.artwork)
	group.GET("/entities", h.search)
	group.POST("/install", h.install)
	group.POST("/upload", h.upload)
	group.POST("/cancel", func(c *gin.Context) { h.service.Cancel(); Success(c, 202, h.service.Status()) })
	group.DELETE("", func(c *gin.Context) {
		if err := h.service.Uninstall(); err != nil {
			h.failure(c, err)
			return
		}
		Success(c, 200, h.service.Status())
	})
}

func (h *ArtworkPackHandler) install(c *gin.Context) {
	var input struct {
		Source string `json:"source"`
	}
	if c.ShouldBindJSON(&input) != nil {
		h.failure(c, artworkpack.ErrSource)
		return
	}
	if input.Source == "" {
		input.Source = "domestic"
	}
	if err := h.service.Download(input.Source); err != nil {
		h.failure(c, err)
		return
	}
	Success(c, http.StatusAccepted, h.service.Status())
}

func (h *ArtworkPackHandler) upload(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, artworkpack.MaxArchiveBytes+(1<<20))
	reader, err := c.Request.MultipartReader()
	if err != nil {
		h.failure(c, artworkpack.ErrInvalid)
		return
	}
	// The API accepts exactly one streamed file, without trusting its filename.
	part, err := reader.NextPart()
	if err != nil || part.FormName() != "file" {
		h.failure(c, artworkpack.ErrInvalid)
		return
	}
	defer part.Close()
	if err = h.service.Import(c.Request.Context(), part); err != nil {
		h.failure(c, err)
		return
	}
	Success(c, 200, h.service.Status())
}

func (h *ArtworkPackHandler) search(c *gin.Context) {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "60"))
	if err != nil {
		h.failure(c, entitycatalog.ErrInvalidSearch)
		return
	}
	offset, err := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if err != nil {
		h.failure(c, entitycatalog.ErrInvalidSearch)
		return
	}
	result, err := h.service.Search(entitycatalog.RuntimeSearchOptions{Query: c.Query("q"), Limit: limit, Offset: offset})
	if err != nil {
		h.failure(c, err)
		return
	}
	Success(c, 200, result)
}

func (h *ArtworkPackHandler) artwork(c *gin.Context) {
	data, revision, err := h.service.Artwork(c.Param("prefab"))
	if err != nil {
		c.Header("Cache-Control", "no-store")
		c.Status(404)
		return
	}
	// Revision query strings invalidate browser caches immediately on replacement.
	if requested := c.Query("v"); requested != "" && requested != revision {
		c.Header("Cache-Control", "no-store")
		c.Status(404)
		return
	}
	c.Header("Cache-Control", "private, max-age=86400")
	c.Header("Vary", "Cookie")
	c.Header("X-Content-Type-Options", "nosniff")
	etag := fmt.Sprintf(`"%x"`, sha256.Sum256(data))
	c.Header("ETag", etag)
	if c.GetHeader("If-None-Match") == etag {
		c.Status(304)
		return
	}
	c.Data(200, "image/webp", data)
}

func (h *ArtworkPackHandler) failure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, artworkpack.ErrBusy):
		Failure(c, 409, "ARTWORK_PACK_BUSY", "图片包正在处理，请稍后重试", nil)
	case errors.Is(err, artworkpack.ErrSource):
		Failure(c, 422, "ARTWORK_PACK_SOURCE", "请选择受支持的下载源", nil)
	case errors.Is(err, artworkpack.ErrInvalid):
		Failure(c, 422, "ARTWORK_PACK_INVALID", "图片包校验失败，请下载当前版本的官方资源包", nil)
	case errors.Is(err, artworkpack.ErrNotInstalled):
		Failure(c, 409, "ARTWORK_PACK_NOT_INSTALLED", "请先安装图片资源包", nil)
	case errors.Is(err, entitycatalog.ErrInvalidSearch):
		Failure(c, 422, "INVALID_ENTITY_SEARCH", "实体搜索参数无效", nil)
	default:
		Failure(c, 500, "ARTWORK_PACK_FAILED", "图片包操作失败，请检查管理服务的数据目录权限和可用空间后重试", nil)
	}
}
