package entitycatalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearchUsesCommonEntitiesWithoutRemoteRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	service, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Search(context.Background(), SearchOptions{Kind: CapabilityGive, Limit: 10})
	if err != nil || len(result.Items) != 10 || requests != 0 || result.Source != "builtin" {
		t.Fatalf("result=%#v requests=%d err=%v", result, requests, err)
	}
	for _, item := range result.Items {
		if !item.Common || !containsCapability(item.Capabilities, CapabilityGive) {
			t.Fatalf("unexpected common item: %#v", item)
		}
	}
}

func TestCommonCatalogCoversFrequentItemsCreaturesBossesAndStructures(t *testing.T) {
	service, _ := New("", nil)
	result, err := service.Search(context.Background(), SearchOptions{Kind: CapabilitySpawn, Limit: maxCatalogResults})
	if err != nil || len(result.Items) != maxCatalogResults {
		t.Fatalf("common entity count=%d err=%v", len(result.Items), err)
	}

	byID := make(map[string]Entity, len(result.Items))
	for _, item := range result.Items {
		byID[item.ID] = item
	}
	for _, id := range []string{"axe", "perogies", "walrus", "beequeen", "researchlab4"} {
		if _, exists := byID[id]; !exists {
			t.Fatalf("common catalog is missing %q", id)
		}
	}
}

func TestSearchMergesBeaconMetadataAndFiltersCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/search" || request.URL.Query().Get("q") != "木头" {
			t.Fatalf("request URL = %s", request.URL.String())
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"data":[{"key":"dst:log","namespace":"dst","id":"log","type":"item","catalogStatus":"published","names":{"zhCN":"木头","en":"Log"},"components":["inventoryitem"],"artwork":{"path":"/assets/entities/log.webp"}},{"key":"dst:deerclops","namespace":"dst","id":"deerclops","type":"boss","catalogStatus":"published","names":{"zhCN":"独眼巨鹿","en":"Deerclops"},"components":[],"artwork":null}],"meta":{"release":{"id":"release-1"}}}`))
	}))
	defer server.Close()
	service, _ := New(server.URL, server.Client())
	result, err := service.Search(context.Background(), SearchOptions{Query: "木头", Kind: CapabilityGive, Limit: 10})
	if err != nil || len(result.Items) != 1 || result.Items[0].ID != "log" || !result.Items[0].Common || result.Items[0].ArtworkURL == "" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if !result.RemoteAvailable || result.ReleaseID != "release-1" || result.Source != "beacon" {
		t.Fatalf("remote metadata = %#v", result)
	}
	result, err = service.Search(context.Background(), SearchOptions{Query: "木头", Kind: CapabilityRemove, Limit: 10})
	if err != nil || len(result.Items) != 2 || result.Items[0].ID != "log" || result.Items[1].ID != "deerclops" {
		t.Fatalf("remove result=%#v err=%v", result, err)
	}
	for _, item := range result.Items {
		if !containsCapability(item.Capabilities, CapabilityRemove) {
			t.Fatalf("remove capability missing: %#v", item)
		}
	}
}

func TestSearchCommonEntitiesSupportsNearbyRemoval(t *testing.T) {
	service, _ := New("", nil)
	result, err := service.Search(context.Background(), SearchOptions{Query: "beefalo", Kind: CapabilityRemove, Limit: 20})
	if err != nil || len(result.Items) != 1 || result.Items[0].ID != "beefalo" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestSearchFallsBackWhenBeaconIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	service, _ := New(server.URL, server.Client())
	result, err := service.Search(context.Background(), SearchOptions{Query: "pigman", Kind: CapabilitySpawn, Limit: 20})
	if err != nil || result.RemoteAvailable || len(result.Items) != 1 || result.Items[0].ID != "pigman" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestSearchRejectsInvalidOptions(t *testing.T) {
	service, _ := New("", nil)
	if _, err := service.Search(context.Background(), SearchOptions{Kind: "delete", Limit: 20}); !errors.Is(err, ErrInvalidSearch) {
		t.Fatalf("invalid kind error = %v", err)
	}
	if _, err := service.Search(context.Background(), SearchOptions{Limit: maxCatalogResults + 1}); !errors.Is(err, ErrInvalidSearch) {
		t.Fatalf("invalid limit error = %v", err)
	}
}

func TestSearchDropsEntitiesThatCannotBeUsedAsPrefabs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"data":[{"key":"mod:unsafe","namespace":"mod","id":"Unsafe-ID","type":"item","catalogStatus":"published","names":{"zhCN":"无效","en":"Invalid"},"components":["inventoryitem"]}],"meta":{}}`))
	}))
	defer server.Close()
	service, _ := New(server.URL, server.Client())
	result, err := service.Search(context.Background(), SearchOptions{Query: "Unsafe", Kind: CapabilityGive, Limit: 20})
	if err != nil || len(result.Items) != 0 || !result.RemoteAvailable {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
