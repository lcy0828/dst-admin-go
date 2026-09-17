package httpapi

import (
	"context"
	"net/http"
	"testing"

	"dont/internal/entitycatalog"

	"github.com/gin-gonic/gin"
)

type entityCatalogFixture struct{}

func (entityCatalogFixture) Search(_ context.Context, options entitycatalog.SearchOptions) (entitycatalog.SearchResult, error) {
	if options.Limit < 1 || options.Kind != entitycatalog.CapabilityGive {
		return entitycatalog.SearchResult{}, entitycatalog.ErrInvalidSearch
	}
	return entitycatalog.SearchResult{Query: options.Query, Source: "builtin", Items: []entitycatalog.Entity{{ID: "log"}}}, nil
}

func TestEntityCatalogHandler(t *testing.T) {
	router := gin.New()
	NewEntityCatalogHandler(entityCatalogFixture{}).Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodGet, "/api/v2/entity-catalog/entities?q=log&kind=give&limit=10", nil, nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("search: %d %s", response.Code, response.Body.String())
	}
	response = performJSON(router, http.MethodGet, "/api/v2/entity-catalog/entities?kind=delete", nil, nil, "")
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid search: %d %s", response.Code, response.Body.String())
	}
}
