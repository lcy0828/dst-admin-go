package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"dont/internal/mods"

	"github.com/gin-gonic/gin"
)

type modMetadataReaderFixture struct {
	modHandlerService
	ids       []string
	cachedIDs []string
}

func (f *modMetadataReaderFixture) CachedMetadata(_ context.Context, ids []string) (map[string]mods.SteamMod, error) {
	f.cachedIDs = ids
	return map[string]mods.SteamMod{"100": {ID: "100", Name: "Cached title"}}, nil
}

func TestModMetadataCachedReadNeverCallsWorkshop(t *testing.T) {
	router := gin.New()
	fixture := &modMetadataReaderFixture{}
	NewModHandler(fixture, nil).Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodGet, "/api/v2/mods/metadata?ids=100&cached=true", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if len(fixture.ids) > 0 || len(fixture.cachedIDs) != 1 || !strings.Contains(response.Body.String(), "Cached title") {
		t.Fatalf("wrong cached route: %s", response.Body.String())
	}
}

func (f *modMetadataReaderFixture) Metadata(_ context.Context, ids []string) (map[string]mods.SteamMod, error) {
	f.ids = ids
	return map[string]mods.SteamMod{"100": {ID: "100", Name: "Workshop name", Author: "Workshop author"}}, errors.New("partial Steam failure")
}

func TestModMetadataEndpointBatchesAndKeepsPartialFailuresVisible(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	fixture := &modMetadataReaderFixture{}
	NewModHandler(fixture, nil).Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodGet, "/api/v2/mods/metadata?ids=100,200", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if len(fixture.ids) != 2 || !strings.Contains(response.Body.String(), "Workshop name") || !strings.Contains(response.Body.String(), "Workshop author") || !strings.Contains(response.Body.String(), "partial Steam failure") {
		t.Fatalf("metadata result=%s, IDs=%v", response.Body.String(), fixture.ids)
	}
	for _, ids := range []string{"", "100,invalid", strings.Repeat("100,", 100) + "100"} {
		response := performJSON(router, http.MethodGet, "/api/v2/mods/metadata?ids="+ids, nil, nil, "")
		if response.Code < 400 || response.Code >= 500 {
			t.Fatalf("invalid metadata input status=%d: %s", response.Code, response.Body.String())
		}
	}
}
