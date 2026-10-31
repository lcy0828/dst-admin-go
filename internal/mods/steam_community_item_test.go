package mods

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func publicWorkshopItem(id, appID, required string) string {
	return fmt.Sprintf(`<html><head><meta property="og:image" content="https://images.steamusercontent.com/ugc/test/preview/"></head><body>
<div class="workshopItemTitle">测试模组 Public Beta</div>
<a href="/id/example/myworkshopfiles/">测试作者 的创意工坊</a>
<div id="highlightContent">公开说明</div>
<a id="SubscribeItemBtn" onclick="SubscribeItem( '%s', '%s' );">Subscribe</a>
%s
<script type="application/json">{"publishedfileid":"999","consumer_appid":730,"file_type":2}</script>
</body></html>`, id, appID, required)
}

func TestWorkshopUnavailableIDUsesVerifiedPageWithAndWithoutKeyOrStore(t *testing.T) {
	for _, key := range []string{"", "test-api-key"} {
		for _, persistent := range []bool{false, true} {
			t.Run(fmt.Sprintf("key=%t/store=%t", key != "", persistent), func(t *testing.T) {
				var apiCalls, htmlCalls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.Contains(r.URL.Path, "/GetPublishedFileDetails/") || strings.Contains(r.URL.Path, "/GetDetails/"):
						apiCalls.Add(1)
						fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":9}]}}`)
					case r.URL.Path == "/prefix/sharedfiles/filedetails/":
						htmlCalls.Add(1)
						if r.URL.Query().Get("id") != "100" || r.URL.Query().Get("l") != "schinese" {
							t.Error("page request lost the exact ID or language")
						}
						fmt.Fprint(w, publicWorkshopItem("100", "322330", `<div id="RequiredItems"><a href="https://steamcommunity.com/workshop/filedetails/?id=200"><div class="requiredItem">依赖</div></a></div>`))
					default:
						t.Errorf("unexpected metadata enrichment: %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				var store *MetadataStore
				if persistent {
					db, opened := openMetadataTestStore(t, ":memory:")
					defer db.Close()
					store = opened
				}
				workshop, err := NewWorkshop(NetworkConfig{APIBase: server.URL + "/prefix", CommunityBase: server.URL + "/prefix", ProxyMode: "direct", APIKey: key}, store, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer workshop.Close()
				options := SearchOptions{Query: "https://steamcommunity.com/sharedfiles/filedetails/?id=100", Page: 1, PageSize: 20}
				result, err := workshop.Search(context.Background(), options)
				if err != nil || len(result.Items) != 1 || result.Source != "community" || result.Warning != workshopCommunityWarning {
					t.Fatalf("fallback search = %+v, %v", result, err)
				}
				item := result.Items[0]
				if item.ID != "100" || item.Name != "测试模组 Public Beta" || item.Author != "测试作者" || item.Description != "公开说明" || item.PreviewURL == "" || fmt.Sprint(item.Dependencies) != "[200]" {
					t.Fatalf("public page metadata = %+v", item)
				}
				if item.Version != "" || item.SteamManifestID != "" || !item.UpdatedAt.IsZero() || item.MetadataWarning == "" {
					t.Fatalf("HTML invented version evidence: %+v", item)
				}
				result.Items[0].Dependencies[0] = "999"
				again, err := workshop.Search(context.Background(), options)
				if err != nil || again.Items[0].Dependencies[0] != "200" || apiCalls.Load() != 1 || htmlCalls.Load() != 1 {
					t.Fatalf("search cache was mutated or made extra requests: %+v, %v, api=%d html=%d", again, err, apiCalls.Load(), htmlCalls.Load())
				}
				// Update checks must remain API-only even when page evidence exists.
				latest, err := workshop.Summaries(context.Background(), []string{"100"})
				if err == nil || len(latest) != 0 || htmlCalls.Load() != 1 {
					t.Fatalf("public display evidence became a version check: %+v, %v", latest, err)
				}
			})
		}
	}
}

func TestWorkshopPageFallbackRejectsUnverifiedOrIncompleteItems(t *testing.T) {
	valid := publicWorkshopItem("100", "322330", "")
	for name, page := range map[string]string{
		"wrong item":                     publicWorkshopItem("200", "322330", ""),
		"wrong game":                     publicWorkshopItem("100", "730", ""),
		"login":                          `<html><body>Please log in to continue</body></html>`,
		"title alone":                    `<html><body><div class="workshopItemTitle">Not enough evidence</div></body></html>`,
		"collection":                     strings.ReplaceAll(valid, "SubscribeItem(", "SubscribeCollection("),
		"disabled":                       strings.Replace(valid, `id="SubscribeItemBtn"`, `id="SubscribeItemBtn" disabled`, 1),
		"truncated":                      strings.TrimSuffix(valid, "</html>"),
		"malformed subscription":         strings.Replace(valid, "'100', '322330'", "'100', '322330' ); RunScript(", 1),
		"mismatched quotes":              strings.Replace(valid, "'100'", "'100&quot;", 1),
		"duplicate primary button":       strings.Replace(valid, "</body>", `<a id="SubscribeItemBtn" onclick="SubscribeItem('100','322330');"></a></body>`, 1),
		"identity inside description":    `<html><body><div class="workshopItemTitle">Other item</div><div id="highlightContent"><a id="SubscribeItemBtn" onclick="SubscribeItem('100','322330');"></a></div></body></html>`,
		"external dependency":            publicWorkshopItem("100", "322330", `<div id="RequiredItems"><a href="https://other.example/workshop/filedetails/?id=200"><div class="requiredItem">bad</div></a></div>`),
		"script dependency URL":          publicWorkshopItem("100", "322330", `<div id="RequiredItems"><a href="javascript:/workshop/filedetails/?id=200">bad</a></div>`),
		"missing dependency link":        publicWorkshopItem("100", "322330", `<div id="RequiredItems"><div class="requiredItem">Missing ID</div></div>`),
		"duplicate dependency parameter": publicWorkshopItem("100", "322330", `<div id="RequiredItems"><a href="/workshop/filedetails/?id=200&amp;id=300">bad</a></div>`),
		"oversized dependency graph":     publicWorkshopItem("100", "322330", `<div id="RequiredItems">`+strings.Repeat(`<a href="/workshop/filedetails/?id=200">dependency</a>`, 101)+`</div>`),
	} {
		t.Run(name, func(t *testing.T) {
			var pages atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/sharedfiles/filedetails/" {
					pages.Add(1)
					fmt.Fprint(w, page)
					return
				}
				fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":9}]}}`)
			}))
			defer server.Close()
			provider := NewSteamProvider("", "322330")
			provider.APIBase, provider.CommunityBase = server.URL, server.URL
			items, err := provider.BasicDetails(context.Background(), []string{"100"})
			if err == nil || len(items) != 0 || pages.Load() != 1 {
				t.Fatalf("invalid page accepted: %+v, %v, pages=%d", items, err, pages.Load())
			}
			// Negative page evidence uses the existing cache/backoff, not retries.
			_, err = provider.BasicDetails(context.Background(), []string{"100"})
			if err == nil || pages.Load() != 1 {
				t.Fatalf("invalid page was retried immediately: %v, pages=%d", err, pages.Load())
			}
		})
	}
}

func TestWorkshopFallbackDoesNotOverrideDenialsOrExpandBatchReads(t *testing.T) {
	for name, reply := range map[string]string{
		"permission denied":   `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":15}]}}`,
		"deleted result":      `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":8}]}}`,
		"explicit other game": `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":1,"consumer_app_id":730}]}}`,
		"missing other game":  `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":9,"consumer_app_id":730}]}}`,
		"omitted item":        `{"response":{"publishedfiledetails":[]}}`,
		"invalid envelope":    `{"response":{"error":"denied"}}`,
		"conflicting records": `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":9},{"publishedfileid":"100","result":15}]}}`,
		"duplicate records":   `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":9},{"publishedfileid":"100","result":9}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var pages atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/sharedfiles/filedetails/" {
					pages.Add(1)
					fmt.Fprint(w, publicWorkshopItem("100", "322330", ""))
					return
				}
				fmt.Fprint(w, reply)
			}))
			defer server.Close()
			provider := NewSteamProvider("", "322330")
			provider.APIBase, provider.CommunityBase = server.URL, server.URL
			if items, err := provider.BasicDetails(context.Background(), []string{"100"}); err == nil || len(items) != 0 || pages.Load() != 0 {
				t.Fatalf("API failure incorrectly overridden: %+v, %v, pages=%d", items, err, pages.Load())
			}
		})
	}
	var pages atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sharedfiles/filedetails/" {
			pages.Add(1)
		}
		fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":9},{"publishedfileid":"200","result":9}]}}`)
	}))
	defer server.Close()
	provider := NewSteamProvider("", "322330")
	provider.APIBase, provider.CommunityBase = server.URL, server.URL
	if _, err := provider.BasicDetails(context.Background(), []string{"100", "200"}); err == nil || pages.Load() != 0 {
		t.Fatalf("batch details expanded to per-item requests: %v, pages=%d", err, pages.Load())
	}
}

func TestExplicitDownloadMetadataKeepsBatchReadsAndOnlyRecoversEligibleItems(t *testing.T) {
	var manyIDs []string
	for id := 300; id < 401; id++ {
		manyIDs = append(manyIDs, fmt.Sprint(id))
	}
	for _, test := range []struct {
		name      string
		ids       []string
		result    int
		wantError bool
		wantAPI   int32
		wantPages int32
	}{
		{name: "normal batch", ids: []string{"200", "300"}, result: 1, wantAPI: 1},
		{name: "more than one batch", ids: manyIDs, result: 1, wantAPI: 2},
		{name: "single beta", ids: []string{"100"}, result: 9, wantAPI: 2, wantPages: 1},
		{name: "mixed batch", ids: []string{"100", "200"}, result: 9, wantAPI: 2, wantPages: 1},
		{name: "permission denied", ids: []string{"100", "200"}, result: 15, wantError: true, wantAPI: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var api, pages atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/prefix/sharedfiles/filedetails/" {
					pages.Add(1)
					fmt.Fprint(w, publicWorkshopItem("100", "322330", ""))
					return
				}
				api.Add(1)
				_ = r.ParseForm()
				var records []string
				for index := 0; ; index++ {
					id := r.PostForm.Get(fmt.Sprintf("publishedfileids[%d]", index))
					if id == "" {
						break
					}
					result := 1
					if id == "100" {
						result = test.result
					}
					records = append(records, fmt.Sprintf(`{"publishedfileid":%q,"result":%d,"consumer_app_id":322330,"title":"API mod"}`, id, result))
				}
				fmt.Fprintf(w, `{"response":{"publishedfiledetails":[%s]}}`, strings.Join(records, ","))
			}))
			defer server.Close()
			service, _, _ := newConfigTestService(t)
			service.metadata = testWorkshop(t, server, "")
			items, err := service.DescribeForDownload(context.Background(), test.ids)
			if (err != nil) != test.wantError || api.Load() != test.wantAPI || pages.Load() != test.wantPages {
				t.Fatalf("download metadata: items=%+v err=%v api=%d pages=%d", items, err, api.Load(), pages.Load())
			}
			if test.wantPages > 0 {
				item := items["100"]
				if item.Name != "测试模组 Public Beta" || item.MetadataWarning == "" || item.Version != "" || item.SteamManifestID != "" || !item.UpdatedAt.IsZero() {
					t.Fatalf("invalid display-only metadata: %+v", item)
				}
				// The explicit path must not make subsequent update checks succeed.
				if _, err := service.Describe(context.Background(), test.ids); err == nil || pages.Load() != 1 {
					t.Fatalf("download evidence became latest-version evidence: %v pages=%d", err, pages.Load())
				}
			}
		})
	}
}

func TestWorkshopPageFallbackSharesConcurrentRequestsAndPreservesRateLimit(t *testing.T) {
	var pages atomic.Int32
	var limited atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sharedfiles/filedetails/" {
			pages.Add(1)
			if limited.Load() {
				w.Header().Set("Retry-After", "30")
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}
			time.Sleep(15 * time.Millisecond)
			fmt.Fprint(w, publicWorkshopItem("100", "322330", ""))
			return
		}
		fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":9}]}}`)
	}))
	defer server.Close()
	provider := NewSteamProvider("", "322330")
	provider.APIBase, provider.CommunityBase = server.URL, server.URL
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			items, err := provider.BasicDetails(context.Background(), []string{"100"})
			if err != nil || items["100"].Dependencies == nil {
				t.Errorf("concurrent item unavailable: %+v, %v", items, err)
			}
		}()
	}
	wait.Wait()
	if pages.Load() != 1 {
		t.Fatalf("duplicate page fetches: %d", pages.Load())
	}
	limited.Store(true)
	provider = NewSteamProvider("", "322330")
	provider.APIBase, provider.CommunityBase = server.URL, server.URL
	for i := 0; i < 2; i++ {
		_, err := provider.BasicDetails(context.Background(), []string{"100"})
		var failure *NetworkError
		if !errors.As(err, &failure) || failure.Code != "WORKSHOP_RATE_LIMITED" || failure.RetryAfter < 1 {
			t.Fatalf("rate limit was hidden by item unavailable: %v", err)
		}
	}
	if pages.Load() != 2 {
		t.Fatalf("rate-limited page retried: %d", pages.Load())
	}
}

func TestWorkshopFallbackRefreshesOldDisplayEvidence(t *testing.T) {
	var pages atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sharedfiles/filedetails/" {
			pages.Add(1)
			fmt.Fprint(w, publicWorkshopItem("100", "322330", `<div id="RequiredItems"><a href="/workshop/filedetails/?id=300">new dependency</a></div>`))
			return
		}
		fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":9}]}}`)
	}))
	defer server.Close()
	for _, old := range []communityMetadataCache{
		{Name: "Legacy display", Author: "Known author", ExpiresAt: time.Now().Add(24 * time.Hour)},
		{Name: "Old page", ItemID: "100", AppID: "322330", Dependencies: []string{"200"}, ItemCheckedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(24 * time.Hour)},
	} {
		provider := NewSteamProvider("", "322330")
		provider.APIBase, provider.CommunityBase = server.URL, server.URL
		provider.cacheCommunityLocked("100", old)
		items, err := provider.BasicDetails(context.Background(), []string{"100"})
		if err != nil || fmt.Sprint(items["100"].Dependencies) != "[300]" {
			t.Fatalf("stale display became current dependency evidence: %+v, %v", items, err)
		}
	}
	if pages.Load() != 2 {
		t.Fatalf("old eligibility was not refreshed: %d", pages.Load())
	}
}

func TestPublicWorkshopItemCanDownloadWithDeclaredDependencies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prefix/sharedfiles/filedetails/" {
			fmt.Fprint(w, publicWorkshopItem("100", "322330", `<div id="RequiredItems"><a href="/workshop/filedetails/?id=200">dependency</a></div>`))
			return
		}
		r.ParseForm()
		id := r.PostForm.Get("publishedfileids[0]")
		if id == "100" {
			fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":9}]}}`)
		} else {
			fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"200","result":1,"consumer_app_id":322330,"title":"Dependency"}]}}`)
		}
	}))
	defer server.Close()
	service, _, _ := newConfigTestService(t)
	service.metadata = testWorkshop(t, server, "")
	runner := &countingLifecycleRunner{root: service.config.WorkshopContentRoot}
	service.runner = runner
	result, err := service.Download(context.Background(), DownloadRequest{ModID: "100", IncludeDependencies: true}, io.Discard)
	if err != nil || fmt.Sprint(result.ModIDs) != "[100 200]" || runner.calls != 1 {
		t.Fatalf("dependency download was blocked by missing API metadata: %+v, %v, calls=%d", result, err, runner.calls)
	}
	for _, id := range result.ModIDs {
		if !directoryExists(service.downloadedPath(id)) {
			t.Fatalf("missing downloaded content for %s", id)
		}
	}
	// The direct installation download also verifies and links files without
	// requiring unavailable API version metadata.
	if _, err := service.DownloadLibraryMods(context.Background(), result.ModIDs, io.Discard); err != nil || runner.calls != 2 {
		t.Fatalf("direct download failed: %v calls=%d", err, runner.calls)
	}
	denied := errors.New("SteamCMD access denied")
	service.runner = lifecycleRunner{root: service.config.WorkshopContentRoot, err: denied}
	if _, err := service.Download(context.Background(), DownloadRequest{ModID: "100", IncludeDependencies: true}, io.Discard); !errors.Is(err, denied) {
		t.Fatalf("public metadata concealed SteamCMD failure: %v", err)
	}
}
