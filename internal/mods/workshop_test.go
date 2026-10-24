package mods

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkshopCacheScopesPreserveDisplayAcrossRestart(t *testing.T) {
	db, store := openMetadataTestStore(t, ":memory:")
	defer db.Close()
	for _, scope := range []string{"", "scope-a", "scope-b"} {
		scoped := store.WithScope(scope)
		if err := scoped.save("100", displayLanguage, &SteamMod{ID: "100", Name: scope + " title"}, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
		records, err := scoped.read([]string{"100"}, displayLanguage)
		if err != nil || records["100"].Summary.Name != scope+" title" {
			var rows []metadataRecord
			db.Table(store.table).Find(&rows)
			t.Fatalf("scope=%q records=%+v rows=%+v err=%v", scope, records, rows, err)
		}
	}
}

func TestWorkshopReadsLegacySchemaAsDisplayOnly(t *testing.T) {
	db, store := openMetadataTestStore(t, ":memory:")
	defer db.Close()
	// The pre-upgrade table has no scope column. Leave it in place on upgrade.
	if err := db.Exec(`CREATE TABLE test_workshop_metadata (
		mod_id varchar(32), language varchar(16), summary_json text,
		summary_checked_at datetime, community_json text, community_checked_at datetime,
		PRIMARY KEY (mod_id, language))`).Error; err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := db.Exec(`INSERT INTO test_workshop_metadata VALUES (?, ?, ?, ?, ?, ?)`,
		"100", displayLanguage, `{"id":"100","name":"Old name","version":"old","steamManifestId":"old-manifest"}`,
		old, `{"Name":"旧名称","Author":"旧作者","Description":"已知简介"}`, old).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	records, err := store.WithScope("new-configuration").read([]string{"100"}, displayLanguage)
	item := records["100"]
	if err != nil || item.Summary.Name != "Old name" || item.Community.Name != "旧名称" || item.Community.Author != "旧作者" || !item.SummaryCheckedAt.IsZero() || item.Summary.Version != "" || item.Summary.SteamManifestID != "" {
		t.Fatalf("legacy display: %+v, %v", records, err)
	}
	var count int
	db.Table(store.table).Count(&count)
	if count != 0 {
		t.Fatal("upgrade copied all legacy metadata into the new table")
	}
	if !db.HasTable("test_workshop_metadata") {
		t.Fatal("upgrade removed the previous cache")
	}
}

func TestWorkshopCommunityMemoryStaysBoundedAcrossDifferentDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><body><div class="workshopItemTitle">Name %s</div><a href="/id/author/myworkshopfiles/">Author</a><div id="highlightContent">%s</div></body></html>`, r.URL.Query().Get("id"), strings.Repeat("description ", 6000))
	}))
	defer server.Close()
	p := NewSteamProvider("", "322330")
	p.CommunityBase = server.URL
	for i := 1; i <= 140; i++ {
		if _, ok := p.communityMetadata(context.Background(), fmt.Sprint(i)); !ok {
			t.Fatalf("detail %d unavailable", i)
		}
	}
	if len(p.communityCache) >= 140 || p.communityBytes > 8<<20 || p.communityBytes <= 0 {
		t.Fatalf("community memory budget: entries=%d bytes=%d", len(p.communityCache), p.communityBytes)
	}
}

func BenchmarkWorkshopCachedSearch(b *testing.B) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		items := make([]map[string]string, 20)
		for i := range items {
			items[i] = map[string]string{"publishedfileid": fmt.Sprint(i + 1), "title": "常用模组", "description": strings.Repeat("Mod description. ", 60)}
		}
		json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"total": 20, "publishedfiledetails": items}})
	}))
	defer server.Close()
	w, err := NewWorkshop(NetworkConfig{APIBase: server.URL, APIKey: "benchmark-key", ProxyMode: "direct"}, nil, "")
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	options := SearchOptions{Query: "常用模组", Page: 1, PageSize: 20}
	if _, err := w.Search(context.Background(), options); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := w.Search(context.Background(), options)
		if err != nil || len(result.Items) != 20 {
			b.Fatalf("cached search: %v", err)
		}
	}
	b.StopTimer()
	if requests.Load() != 1 {
		b.Fatalf("cache hits issued %d requests", requests.Load())
	}
}

func testWorkshop(t *testing.T, server *httptest.Server, key string) *Workshop {
	t.Helper()
	w, err := NewWorkshop(NetworkConfig{APIBase: server.URL + "/prefix", CommunityBase: server.URL + "/prefix", ProxyMode: "direct", APIKey: key}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func TestWorkshopSearchDoesNotWaitForAuthorsOrCommunity(t *testing.T) {
	var auxiliary atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/IPublishedFileService/QueryFiles/v1/" {
			auxiliary.Add(1)
			t.Errorf("unexpected auxiliary request: %s", r.URL.Path)
			return
		}
		if r.URL.Query().Get("key") != "private-key" {
			t.Error("missing key")
		}
		fmt.Fprint(w, `{"response":{"total":1,"publishedfiledetails":[{"publishedfileid":"100","title":"Result","creator":"76561198000000000","tags":[{"tag":"utility"}]}]}}`)
	}))
	defer server.Close()
	w := testWorkshop(t, server, "private-key")
	result, err := w.Search(context.Background(), SearchOptions{Page: 1, PageSize: 20, Query: "test"})
	if err != nil || len(result.Items) != 1 || auxiliary.Load() != 0 || result.Source != "api" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	result.Items[0].Name = "mutated"
	result.Items[0].Tags[0] = "mutated"
	second, err := w.Search(context.Background(), SearchOptions{Page: 1, PageSize: 20, Query: "test"})
	if err != nil || second.Items[0].Name != "Result" || second.Items[0].Tags[0] != "utility" {
		t.Fatalf("shared mutable cache: %+v %v", second, err)
	}
}

func TestWorkshopPublicCardsSurviveUnavailableAPI(t *testing.T) {
	var apiCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/workshop/browse/") {
			fmt.Fprint(w, `<html><body><div>1 entries matching filters</div><a href="https://steamcommunity.com/sharedfiles/filedetails/?id=100"><img src="https://images.steamusercontent.com/ugc/a/b/" alt="Known title"></a></body></html>`)
			return
		}
		apiCalls.Add(1)
		http.Error(w, "unavailable", 503)
	}))
	defer server.Close()
	w := testWorkshop(t, server, "")
	result, err := w.Search(context.Background(), SearchOptions{Page: 1, PageSize: 20, Query: "test"})
	if err != nil || len(result.Items) != 1 || result.Items[0].Name != "Known title" || apiCalls.Load() != 0 {
		t.Fatalf("result=%+v err=%v api=%d", result, err, apiCalls.Load())
	}
}

func TestWorkshopRejectsSteamErrorEnvelopesAndCaptcha(t *testing.T) {
	for _, key := range []string{"", "private-key"} {
		t.Run(key, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if key == "" {
					fmt.Fprint(w, `<html><body>Please log in to continue</body></html>`)
				} else {
					fmt.Fprint(w, `{"response":{"error":"denied"}}`)
				}
			}))
			defer server.Close()
			w := testWorkshop(t, server, key)
			_, err := w.Search(context.Background(), SearchOptions{Page: 1, PageSize: 20, Query: "test"})
			var failure *NetworkError
			if !errors.As(err, &failure) || failure.Code != "WORKSHOP_INVALID_RESPONSE" {
				t.Fatalf("invalid response accepted: %v", err)
			}
		})
	}
}

func TestWorkshopExplicitZeroResultsCanOmitTheItemArray(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"response":{"total":0}}`)
	}))
	defer server.Close()
	w := testWorkshop(t, server, "private-key")
	result, err := w.Search(context.Background(), SearchOptions{Query: "no matching items", Page: 1, PageSize: 20})
	if err != nil || result.Total != 0 || result.Items == nil || len(result.Items) != 0 || result.TotalKind != "exact" {
		t.Fatalf("explicit empty result: %+v, %v", result, err)
	}
}

func TestWorkshopIDLinkUsesOnlyConfiguredEndpoint(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/prefix/ISteamRemoteStorage/GetPublishedFileDetails/v1/" {
			t.Errorf("path=%s", r.URL.Path)
		}
		r.ParseForm()
		if r.Form.Get("publishedfileids[0]") != "100" {
			t.Error("invalid ID")
		}
		fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"100","title":"ID result"}]}}`)
	}))
	defer server.Close()
	w := testWorkshop(t, server, "")
	result, err := w.Search(context.Background(), SearchOptions{Page: 1, PageSize: 20, Query: "https://steamcommunity.com/sharedfiles/filedetails/?id=100"})
	if err != nil || len(result.Items) != 1 || calls.Load() != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls.Load())
	}
}

func TestWorkshopHotApplyKeepsOldResultsOutOfNewGeneration(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		if key == "old-key" {
			close(started)
			<-release
		}
		fmt.Fprintf(w, `{"response":{"total":1,"publishedfiledetails":[{"publishedfileid":"100","title":%q}]}}`, key)
	}))
	defer server.Close()
	w := testWorkshop(t, server, "old-key")
	oldVersion := w.Version()
	done := make(chan SearchResult, 1)
	go func() {
		value, _ := w.Search(context.Background(), SearchOptions{Page: 1, PageSize: 20, Query: "test"})
		done <- value
	}()
	<-started
	config := w.snapshot().config
	config.APIKey = "new-key"
	commit, discard, err := w.Prepare(config)
	if err != nil {
		t.Fatal(err)
	}
	defer discard()
	commit()
	close(release)
	old := <-done
	if old.ConfigVersion != oldVersion {
		t.Fatal("inflight request changed its identity")
	}
	_, err = w.Search(context.Background(), SearchOptions{Page: 1, PageSize: 20, Query: "test", ConfigVersion: oldVersion})
	var failure *NetworkError
	if !errors.As(err, &failure) || failure.Code != "WORKSHOP_CONFIG_CHANGED" {
		t.Fatalf("stale page accepted: %v", err)
	}
	result, err := w.Search(context.Background(), SearchOptions{Page: 1, PageSize: 20, Query: "test"})
	if err != nil || result.Items[0].Name != "new-key" || result.ConfigVersion == oldVersion {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestWorkshopCoalescingSurvivesOneCallerCancellation(t *testing.T) {
	w, err := NewWorkshop(NetworkConfig{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	first, cancel := context.WithCancel(context.Background())
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fn := func(ctx context.Context) (any, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return "ok", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	done1, done2 := make(chan error, 1), make(chan error, 1)
	go func() { _, err := w.coalesce(first, "shared", time.Second, fn); done1 <- err }()
	<-started
	go func() { _, err := w.coalesce(context.Background(), "shared", time.Second, fn); done2 <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		w.mu.Lock()
		count := w.calls["shared"].waiters
		w.mu.Unlock()
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second caller did not join")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if <-done1 == nil {
		t.Fatal("cancellation lost")
	}
	close(release)
	if err := <-done2; err != nil || calls.Load() != 1 {
		t.Fatalf("remaining caller=%v calls=%d", err, calls.Load())
	}
}

func TestWorkshopStaleSearchIsLabeledAndRateLimitStopsRepeatedCalls(t *testing.T) {
	var limited atomic.Bool
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if limited.Load() {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, `{"response":{"total":1,"publishedfiledetails":[{"publishedfileid":"100","title":"Known"}]}}`)
	}))
	defer server.Close()
	w := testWorkshop(t, server, "private-key")
	_, err := w.Search(context.Background(), SearchOptions{Page: 1, PageSize: 20, Query: "test"})
	if err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	for key, entry := range w.searches {
		entry.result.ObservedAt = time.Now().Add(-time.Hour)
		w.searches[key] = entry
	}
	w.mu.Unlock()
	limited.Store(true)
	result, err := w.Search(context.Background(), SearchOptions{Page: 1, PageSize: 20, Query: "test"})
	if err != nil || !result.Stale || result.Warning == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	_, err = w.Search(context.Background(), SearchOptions{Page: 1, PageSize: 20, Query: "other"})
	var failure *NetworkError
	if !errors.As(err, &failure) || failure.RetryAfter < 100 || calls.Load() != 2 {
		t.Fatalf("rate limit=%v requests=%d", err, calls.Load())
	}
}

func TestWorkshopErrorsDoNotDiscloseCredentials(t *testing.T) {
	for _, err := range []error{&urlErrorForTest{}, fmt.Errorf("wrapped: %w", &NetworkError{Code: "WORKSHOP_TIMEOUT"})} {
		text := safeNetworkError(err).Error()
		if strings.Contains(text, "secret") || strings.Contains(text, "http") {
			t.Fatal(text)
		}
	}
	for _, raw := range []string{"http://127.0.0.1/private", "https://images.steamusercontent.com.evil.test/a", "https://images.steamusercontent.com@127.0.0.1/a", "https://images.steamusercontent.com:444/a", "file:///etc/passwd"} {
		if _, err := allowedImageURL(raw); err == nil {
			t.Fatalf("accepted image source %q", raw)
		}
	}
	if _, err := thumbnail([]byte("<svg onload=alert(1)>")); err == nil {
		t.Fatal("accepted active image")
	}
}

type urlErrorForTest struct{}

func (*urlErrorForTest) Error() string {
	return "Get https://example.test?key=secret: proxy http://user:secret@proxy.test failed"
}

func TestWorkshopNoIdleNetworkWork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()
	w := testWorkshop(t, server, "")
	for i := 0; i < 5; i++ {
		config := w.snapshot().config
		commit, discard, err := w.Prepare(config)
		if err != nil {
			t.Fatal(err)
		}
		commit()
		discard()
	}
	if calls.Load() != 0 {
		t.Fatal("initialization or settings apply performs network work")
	}
}
