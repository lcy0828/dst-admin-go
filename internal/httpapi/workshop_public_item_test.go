package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"dont/internal/mods"

	"github.com/gin-gonic/gin"
)

type publicWorkshopModService struct {
	modHandlerService
	workshop *mods.Workshop
}

func (s publicWorkshopModService) Search(ctx context.Context, options mods.SearchOptions) (mods.SearchResult, error) {
	return s.workshop.Search(ctx, options)
}

func TestModHTTPPublicWorkshopFallbackKeepsWarningAndRejectsWrongIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sharedfiles/filedetails/" {
			fmt.Fprint(w, `<html><body><div class="workshopItemTitle">Public Beta</div><div id="highlightContent">Description</div><a id="SubscribeItemBtn" onclick="SubscribeItem('100','322330');">Subscribe</a></body></html>`)
			return
		}
		_ = r.ParseForm()
		fmt.Fprintf(w, `{"response":{"publishedfiledetails":[{"publishedfileid":%q,"result":9}]}}`, r.PostForm.Get("publishedfileids[0]"))
	}))
	defer upstream.Close()
	workshop, err := mods.NewWorkshop(mods.NetworkConfig{APIBase: upstream.URL, CommunityBase: upstream.URL, ProxyMode: "direct"}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer workshop.Close()
	router := gin.New()
	NewModHandler(publicWorkshopModService{workshop: workshop}, nil).Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodGet, "/api/v2/mods/search?query=100", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	data := responseData(t, response)
	if data["source"] != "community" || data["total"] != float64(1) || data["warning"] == nil || data["warning"] == "" {
		t.Fatalf("fallback search lost provenance: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodGet, "/api/v2/mods/100", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	data = responseData(t, response)
	if data["id"] != "100" || data["metadataWarning"] == nil || data["metadataWarning"] == "" || data["steamManifestId"] != nil || data["version"] != nil {
		t.Fatalf("details lost warning or invented version: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodGet, "/api/v2/mods/200", nil, nil, "")
	assertAPIError(t, response, http.StatusNotFound, "WORKSHOP_ITEM_UNAVAILABLE")
}
