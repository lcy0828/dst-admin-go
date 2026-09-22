package httpapi

import (
	"context"
	"dont/internal/dstruntime"
	"net/http"
	"strings"
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

func TestWorldEntityCatalogDoesNotSilentlyUseBuiltinFallback(t *testing.T) {
	router := gin.New()
	NewEntityCatalogHandler(entityCatalogFixture{}).Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodPost, "/api/v2/rooms/remote/worlds/caves/entity-catalog/search", map[string]interface{}{"limit": 60}, nil, "")
	if response.Code != http.StatusConflict {
		t.Fatalf("unavailable world: %d %s", response.Code, response.Body.String())
	}
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/remote/worlds/caves/entity-catalog/search", map[string]interface{}{"limit": 121}, nil, "")
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid limit: %d %s", response.Code, response.Body.String())
	}
}

type busyCatalogCommander struct{}

func (busyCatalogCommander) ExecuteCommand(context.Context, string, string, dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
	return dstruntime.CommandReceipt{}, dstruntime.ErrRuntimeCommandBusy
}
func TestWorldCatalogBusyIsRetryableWithoutRuntimeRepair(t *testing.T) {
	router := gin.New()
	handler := NewEntityCatalogHandler(entityCatalogFixture{})
	handler.ConfigureRuntime(busyCatalogCommander{})
	handler.Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodPost, "/api/v2/rooms/r/worlds/w/entity-catalog/search", map[string]interface{}{"limit": 60}, nil, "")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "RUNTIME_BUSY") {
		t.Fatalf("busy response: %d %s", response.Code, response.Body.String())
	}
}

func TestUnifiedDirectoryRemainsAvailableWithoutAWorldOrArtworkPack(t *testing.T) {
	router := gin.New()
	NewEntityCatalogHandler(entityCatalogFixture{}).Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodGet, "/api/v2/entity-catalog/directory", nil, nil, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"goldnugget"`) || !strings.Contains(response.Body.String(), `"category":"equipment"`) {
		t.Fatalf("directory unavailable: %d", response.Code)
	}
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/remote/worlds/w/entity-catalog/snapshot?refresh=false", nil, nil, "")
	if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), `"id":"goldnugget"`) {
		t.Fatal("unavailable world silently returned local resources")
	}
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/remote/worlds/w/entity-catalog/snapshot?refresh=garbage", nil, nil, "")
	if response.Code != http.StatusBadRequest {
		t.Fatal("invalid refresh accepted", response.Code)
	}
}

func TestWorldSnapshotReportsRetryableBusyAndPermanentLimit(t *testing.T) {
	for _, value := range []struct {
		commander entitycatalog.RuntimeCommander
		code      string
		retryable string
	}{
		{busyCatalogCommander{}, "RUNTIME_BUSY", `"retryable":true`},
		{limitCatalogCommander{}, "RUNTIME_CATALOG_LIMIT", `"retryable":false`},
	} {
		router := gin.New()
		handler := NewEntityCatalogHandler(entityCatalogFixture{})
		handler.ConfigureRuntime(value.commander)
		handler.Register(router.Group("/api/v2"))
		response := performJSON(router, http.MethodGet, "/api/v2/rooms/r/worlds/w/entity-catalog/snapshot", nil, nil, "")
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), value.code) || !strings.Contains(response.Body.String(), value.retryable) {
			t.Fatal(response.Code, response.Body.String())
		}
	}
}

type limitCatalogCommander struct{}

func (limitCatalogCommander) ExecuteCommand(context.Context, string, string, dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
	return dstruntime.CommandReceipt{OK: false, Code: "CATALOG_TOO_LARGE"}, nil
}
