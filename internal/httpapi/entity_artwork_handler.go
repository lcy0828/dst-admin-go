package httpapi

import (
	"context"
	"crypto/sha256"
	"dont/internal/entityart"
	"dont/shared"
	"fmt"
	"github.com/gin-gonic/gin"
	"net/http"
	"time"
)

type EntityArtworkReader interface {
	ReadEntityArtwork(context.Context, string, string, shared.RuntimeEntityArtworkRequest) ([]byte, error)
}

func (h *EntityCatalogHandler) ConfigureArtwork(reader EntityArtworkReader) { h.artwork = reader }
func (h *EntityCatalogHandler) readArtwork(c *gin.Context) {
	input := shared.RuntimeEntityArtworkRequest{Prefab: c.Param("prefab"), ModID: c.Query("modId")}
	if !entityart.Valid(input.Prefab, input.ModID) {
		Failure(c, 422, "INVALID_ENTITY_ARTWORK", "图片参数无效", nil)
		return
	}
	if h.artwork == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	data, err := h.artwork.ReadEntityArtwork(ctx, c.Param("roomId"), c.Param("worldId"), input)
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	c.Header("Cache-Control", "private, max-age=300")
	c.Header("Vary", "Cookie")
	c.Header("X-Content-Type-Options", "nosniff")
	if len(data) == 0 {
		c.Status(http.StatusNoContent)
		return
	}
	etag := fmt.Sprintf(`"%x"`, sha256.Sum256(data))
	c.Header("ETag", etag)
	if c.GetHeader("If-None-Match") == etag {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "image/png", data)
}
