package mods

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func openMetadataTestStore(t *testing.T, path string) (*gorm.DB, *MetadataStore) {
	t.Helper()
	db, err := gorm.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	store := NewMetadataStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	return db, store
}

func TestCachedMetadataUsesOldDisplayFieldsWithoutVersionEvidenceOrNetwork(t *testing.T) {
	db, store := openMetadataTestStore(t, ":memory:")
	t.Cleanup(func() { _ = db.Close() })
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := store.save("100", displayLanguage, &SteamMod{ID: "100", Name: "Internal title", PreviewURL: "https://example.test/image", Version: "1", SteamManifestID: "old", UpdatedAt: old}, &communityMetadataCache{Name: "工坊名称", Author: "作者"}, old); err != nil {
		t.Fatal(err)
	}
	provider := &SteamProvider{store: store}
	items, err := provider.CachedMetadata(context.Background(), []string{"100", "200"})
	item := items["100"]
	if err != nil || len(items) != 1 || item.Name != "工坊名称" || item.PreviewURL == "" || item.Version != "" || item.SteamManifestID != "" || !item.UpdatedAt.IsZero() {
		t.Fatalf("display read: %+v %v", items, err)
	}
}

func TestStoredDisplayMetadataSurvivesRestartWithoutHidingUpdates(t *testing.T) {
	var apiCalls, htmlCalls atomic.Int64
	var failAPI atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ISteamRemoteStorage/GetPublishedFileDetails/v1/" {
			manifest := apiCalls.Add(1)
			if failAPI.Load() {
				http.Error(w, "unavailable", 503)
				return
			}
			fmt.Fprintf(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":1,"consumer_app_id":322330,"title":"Internal title","hcontent_file":"%d","tags":[{"tag":"version:1"}]}]}}`, manifest)
			return
		}
		htmlCalls.Add(1)
		if r.Header.Get("Accept") != "text/html" {
			t.Error("missing HTML Accept header")
		}
		fmt.Fprint(w, `<html><body><div class="workshopItemTitle">中文名称</div><a href="/id/author/myworkshopfiles/">作者</a><div id="highlightContent">Description</div></body></html>`)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "metadata.db")
	db, store := openMetadataTestStore(t, path)
	provider := NewSteamProvider("", "322330")
	provider.APIBase, provider.CommunityBase = server.URL, server.URL
	provider.ConfigureMetadataStore(store)
	// Simultaneous panels should share API and HTML work.
	var wait sync.WaitGroup
	for i := 0; i < 4; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			items, err := provider.DisplayMetadata(context.Background(), []string{"100"})
			if err != nil || items["100"].Author != "作者" {
				t.Errorf("display=%#v err=%v", items, err)
			}
		}()
	}
	wait.Wait()
	if apiCalls.Load() != 1 || htmlCalls.Load() != 1 {
		t.Fatalf("duplicate requests: api=%d html=%d", apiCalls.Load(), htmlCalls.Load())
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, store = openMetadataTestStore(t, path)
	defer db.Close()
	provider = NewSteamProvider("", "322330")
	provider.APIBase, provider.CommunityBase = server.URL, server.URL
	provider.ConfigureMetadataStore(store)
	items, err := provider.Details(context.Background(), []string{"100"})
	if err != nil || items["100"].Name != "中文名称" || items["100"].Author != "作者" || apiCalls.Load() != 1 || htmlCalls.Load() != 1 {
		t.Fatalf("restart lost metadata: %#v err=%v api=%d html=%d", items, err, apiCalls.Load(), htmlCalls.Load())
	}
	latest, err := provider.Summaries(context.Background(), []string{"100"})
	if err != nil || latest["100"].SteamManifestID != "2" || latest["100"].Version != "1" || htmlCalls.Load() != 1 {
		t.Fatalf("display cache hid a changed manifest: %#v, %v", latest, err)
	}
	records, err := store.read([]string{"100"}, displayLanguage)
	if err != nil || records["100"].Summary.SteamManifestID != "2" || records["100"].Community.Author != "作者" {
		t.Fatalf("upsert erased fields: %#v, %v", records, err)
	}
	old := records["100"].Summary
	if err := store.save("100", displayLanguage, &old, nil, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	failAPI.Store(true)
	items, err = provider.DisplayMetadata(context.Background(), []string{"100"})
	if err == nil || items["100"].Name != "中文名称" || items["100"].Author != "作者" || items["100"].Version != "" || items["100"].SteamManifestID != "" || !items["100"].UpdatedAt.IsZero() {
		t.Fatalf("failed version query must retain display only: %#v, %v", items, err)
	}
}

func TestOldDisplayMetadataRefreshesOnDetailsAndPreservesKnownAuthor(t *testing.T) {
	db, store := openMetadataTestStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	defer db.Close()
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	old := communityMetadataCache{Name: "Old title", Author: "Known author", Description: "Known description", ExpiresAt: oldTime.Add(steamCommunityCacheTTL)}
	summary := SteamMod{ID: "100", Name: "API name", Version: "1"}
	if err := store.save("100", displayLanguage, &summary, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.save("100", displayLanguage, nil, &old, oldTime); err != nil {
		t.Fatal(err)
	}
	english := communityMetadataCache{Name: "English", Author: "Different"}
	if err := store.save("100", "english", nil, &english, time.Now()); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `<html><body><div class="workshopItemTitle">New title</div></body></html>`)
	}))
	defer server.Close()
	p := NewSteamProvider("", "322330")
	p.APIBase, p.CommunityBase = server.URL, server.URL
	p.ConfigureMetadataStore(store)
	items, err := p.DisplayMetadata(context.Background(), []string{"100"})
	if err != nil || items["100"].Name != "Old title" || calls.Load() != 0 {
		t.Fatalf("list refreshed old known display: %#v, %v", items, err)
	}
	items, err = p.Details(context.Background(), []string{"100"})
	if err != nil || items["100"].Name != "New title" || items["100"].Author != "Known author" || calls.Load() != 1 {
		t.Fatalf("detail refresh lost known fields: %#v, %v", items, err)
	}
	records, err := store.read([]string{"100"}, displayLanguage)
	if err != nil || records["100"].Community.Author != "Known author" || records["100"].Community.Name != "New title" {
		t.Fatalf("bad persisted merge: %#v, %v", records, err)
	}
}

func TestCommunityRateLimitBackoffDoesNotBlockVersionAPI(t *testing.T) {
	for _, status := range []int{200, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var htmlCalls, apiCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/ISteamRemoteStorage/GetPublishedFileDetails/v1/" {
					apiCalls.Add(1)
					fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":1,"consumer_app_id":322330,"title":"name","hcontent_file":"123"}]}}`)
					return
				}
				htmlCalls.Add(1)
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(status)
				fmt.Fprint(w, `<html><body>您最近作出的请求太多了。请稍候，然后重试。</body></html>`)
			}))
			defer server.Close()
			p := NewSteamProvider("", "322330")
			p.APIBase, p.CommunityBase = server.URL, server.URL
			for _, id := range []string{"100", "100", "200"} {
				metadata, ok := p.communityMetadata(context.Background(), id)
				if ok || metadata.Err == nil {
					t.Fatalf("rate limit hidden: %#v", metadata)
				}
			}
			if _, _, err := p.searchCommunityPage(context.Background(), normalizeSearchOptions(SearchOptions{}), 1); err == nil {
				t.Fatal("browse ignored cooldown")
			}
			if htmlCalls.Load() != 1 || time.Until(p.communityCooldown) < 119*time.Second {
				t.Fatalf("backoff ignored: calls=%d", htmlCalls.Load())
			}
			for i := 0; i < 2; i++ {
				items, err := p.Summaries(context.Background(), []string{"100"})
				if err != nil || items["100"].SteamManifestID != "123" {
					t.Fatalf("version API blocked: %#v, %v", items, err)
				}
			}
			if apiCalls.Load() != 2 {
				t.Fatal("version check incorrectly cached")
			}
		})
	}
}
