package httpapi

import (
	"context"
	"dont/internal/mods"
	"github.com/gin-gonic/gin"
	"net/http"
	"strings"
	"testing"
)

type localDetailsFixture struct {
	modPlacementReaderFixture
	target, installation, id string
}

func (f *localDetailsFixture) LocalModDetails(_ context.Context, target, installation, id string) (mods.SteamMod, error) {
	f.target, f.installation, f.id = target, installation, id
	return mods.SteamMod{ID: id, Source: "local", DirectoryName: "鱼干", Name: "本地鱼干", Dependencies: []string{}, Tags: []string{}}, nil
}
func TestLocalModDetailsRequireInstallationScopeAndSteamMutationsRejectLocalIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := &localDetailsFixture{}
	handler := NewModHandler(modHandlerService{}, nil)
	handler.ConfigurePlacementReader(fixture)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))
	for _, url := range []string{"/api/v2/mods/local:%E9%B1%BC%E5%B9%B2", "/api/v2/mods/local:%E9%B1%BC%E5%B9%B2?targetId=local"} {
		response := performJSON(router, http.MethodGet, url, nil, nil, "")
		assertStatus(t, response, http.StatusUnprocessableEntity)
	}
	response := performJSON(router, http.MethodGet, "/api/v2/mods/local:%E9%B1%BC%E5%B9%B2?targetId=agent:node&installationId=second", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if fixture.target != "agent:node" || fixture.installation != "second" || fixture.id != "local:鱼干" || !strings.Contains(response.Body.String(), `"source":"local"`) {
		t.Fatalf("wrong scope %+v %s", fixture, response.Body.String())
	}
	response = performJSON(router, http.MethodPost, "/api/v2/runtime-targets/local/installations/default/mods/local:fishdried_custom/actions/download", map[string]any{}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
}
