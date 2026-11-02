package mods

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSearchThumbnailSurvivesCachedProgressAndInventoryReads(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/IPublishedFileService/QueryFiles/v1/" {
			fmt.Fprint(w, `{"response":{"total":1,"publishedfiledetails":[{"publishedfileid":"100","title":"Public Beta","preview_url":"https://example.test/preview.jpg"}]}}`)
			return
		}
		if r.URL.Path == "/IPublishedFileService/GetDetails/v1/" {
			fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":9}]}}`)
			return
		}
		t.Errorf("unexpected enrichment request: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	workshop, err := NewWorkshop(NetworkConfig{APIBase: server.URL, CommunityBase: server.URL, APIKey: "test-key", ProxyMode: "direct"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer workshop.Close()
	if result, err := workshop.Search(context.Background(), SearchOptions{Query: "Public Beta", Page: 1, PageSize: 10}); err != nil || len(result.Items) != 1 {
		t.Fatalf("search: %+v %v", result, err)
	}
	for i := 0; i < 2; i++ {
		cached, err := workshop.CachedMetadata(context.Background(), []string{"100", "200"})
		if err != nil || len(cached) != 1 || cached["100"].PreviewURL != "https://example.test/preview.jpg" || requests.Load() != 1 {
			t.Fatalf("progress cache lost thumbnail or fetched upstream: %+v, %v, requests=%d", cached, err, requests.Load())
		}
	}
	service := &Service{metadata: workshop}
	items, err := service.Describe(context.Background(), []string{"100"})
	item := items["100"]
	if err == nil || item.Name != "Public Beta" || item.PreviewURL != "https://example.test/preview.jpg" ||
		item.Version != "" || item.SteamManifestID != "" || !item.UpdatedAt.IsZero() || requests.Load() != 2 {
		t.Fatalf("inventory lost cached display or invented update evidence: %+v, %v, requests=%d", items, err, requests.Load())
	}
}

func TestPersistentCommunityAndSearchThumbnailsAreDisplayOnly(t *testing.T) {
	db, store := openMetadataTestStore(t, ":memory:")
	defer db.Close()
	old := time.Now().Add(-30 * 24 * time.Hour)
	community := communityMetadataCache{Name: "Known name", Author: "Known author", PreviewURL: "https://example.test/community.jpg"}
	if err := store.save("100", displayLanguage, nil, &community, old); err != nil {
		t.Fatal(err)
	}
	withoutCover := communityMetadataCache{Name: "Complete name", Author: "Complete author", Description: "Full description"}
	if err := store.save("300", displayLanguage, nil, &withoutCover, old); err != nil {
		t.Fatal(err)
	}
	provider := &SteamProvider{store: store}
	provider.rememberSearch([]SteamMod{
		{ID: "100", Name: "Partial name", PreviewURL: "https://example.test/partial.jpg"},
		{ID: "200", Name: "Search name", PreviewURL: "https://example.test/search.jpg", Version: "unverified"},
		{ID: "300", Name: "Partial name", Author: "Partial author", PreviewURL: "https://example.test/missing-cover.jpg"},
	}, displayLanguage)
	items, err := provider.CachedMetadata(context.Background(), []string{"100", "200", "300"})
	if err != nil || items["100"].PreviewURL != community.PreviewURL || items["100"].Name != community.Name ||
		items["200"].PreviewURL != "https://example.test/search.jpg" || items["200"].Version != "" ||
		items["300"].PreviewURL != "https://example.test/missing-cover.jpg" || items["300"].Name != withoutCover.Name || items["300"].Author != withoutCover.Author {
		t.Fatalf("persistent thumbnails=%+v err=%v", items, err)
	}
	records, err := store.read([]string{"100", "200"}, displayLanguage)
	if err != nil || !records["100"].CommunityCheckedAt.Equal(old) || !records["200"].CommunityCheckedAt.IsZero() || !records["200"].SummaryCheckedAt.IsZero() {
		t.Fatalf("search postponed refresh or asserted a version check: %+v, %v", records, err)
	}
	summary := SteamMod{PreviewURL: "https://example.test/api.jpg", SteamManifestID: "verified"}
	mergeCommunityMetadata(&summary, community)
	if summary.PreviewURL != "https://example.test/api.jpg" || summary.SteamManifestID != "verified" {
		t.Fatal("fallback overwrote known API evidence")
	}
}
