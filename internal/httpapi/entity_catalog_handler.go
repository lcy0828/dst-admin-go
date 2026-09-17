package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"dont/internal/entitycatalog"

	"github.com/gin-gonic/gin"
)

type EntityCatalogSearch interface {
	Search(context.Context, entitycatalog.SearchOptions) (entitycatalog.SearchResult, error)
}

type EntityCatalogHandler struct{ catalog EntityCatalogSearch }

func NewEntityCatalogHandler(catalog EntityCatalogSearch) *EntityCatalogHandler {
	return &EntityCatalogHandler{catalog: catalog}
}

func (h *EntityCatalogHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/entity-catalog/entities", h.search)
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
