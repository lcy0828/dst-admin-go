package mods

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDisplayMetadataUsesDetailsNamesAndAuthorsWithoutSlowingRuntimeSummaries(t *testing.T) {
	var communityRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/ISteamRemoteStorage/GetPublishedFileDetails/v1/":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(writer, `{"response":{"publishedfiledetails":[{"publishedfileid":"3760106287","result":1,"consumer_app_id":322330,"title":"Title"},{"publishedfileid":"376333686","result":1,"consumer_app_id":322330,"title":"SHOWTEMPERATURE"}]}}`)
		case "/sharedfiles/filedetails/":
			communityRequests.Add(1)
			if request.URL.Query().Get("l") != "schinese" {
				t.Error("display metadata did not request localized details")
			}
			id := request.URL.Query().Get("id")
			_, _ = fmt.Fprintf(writer, `<html><body><div class="workshopItemTitle">Workshop name %s</div><a href="/id/author/myworkshopfiles/">Display Author</a></body></html>`, id)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	provider := NewSteamProvider("", "322330")
	provider.APIBase, provider.CommunityBase = server.URL, server.URL
	service := &Service{metadata: provider}
	ids := []string{"3760106287", "376333686"}

	summaries, err := service.Describe(context.Background(), ids)
	if err != nil || communityRequests.Load() != 0 || summaries[ids[0]].Name != "Title" {
		t.Fatalf("runtime summaries fetched community details: %v, %d requests, %v", summaries, communityRequests.Load(), err)
	}
	metadata, err := service.Metadata(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	details, err := provider.Details(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if metadata[id].Name != "Workshop name "+id || metadata[id].Author != "Display Author" {
			t.Fatalf("display metadata for %s = %#v", id, metadata[id])
		}
		if metadata[id].Name != details[id].Name || metadata[id].Author != details[id].Author {
			t.Fatalf("list and details disagree for %s: %#v, %#v", id, metadata[id], details[id])
		}
	}
	if _, err := service.Metadata(context.Background(), ids); err != nil {
		t.Fatal(err)
	}
	if communityRequests.Load() != int64(len(ids)) {
		t.Fatalf("list and details did not share existing cache: %d requests", communityRequests.Load())
	}
}

func TestDisplayMetadataTimeoutKeepsBaseMetadataAndReportsWarning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/ISteamRemoteStorage/GetPublishedFileDetails/v1/" {
			_, _ = fmt.Fprint(writer, `{"response":{"publishedfiledetails":[{"publishedfileid":"3760106287","result":1,"consumer_app_id":322330,"title":"Workshop name"}]}}`)
			return
		}
		<-request.Context().Done()
	}))
	defer server.Close()
	provider := NewSteamProvider("", "322330")
	provider.APIBase, provider.CommunityBase = server.URL, server.URL
	service := &Service{metadata: provider}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	items, err := service.Metadata(ctx, []string{"3760106287"})
	if !errors.Is(err, context.DeadlineExceeded) || items["3760106287"].Name != "Workshop name" {
		t.Fatalf("partial display metadata = %#v, error = %v", items, err)
	}
}

func TestDisplayMetadataEmptyIDsSkipsSteam(t *testing.T) {
	service := &Service{}
	if items, err := service.Metadata(context.Background(), nil); err != nil || len(items) != 0 {
		t.Fatalf("empty metadata = %#v, error = %v", items, err)
	}
}

func TestDisplayMetadataDoesNotSilentlyAcceptSteamErrorPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/ISteamRemoteStorage/GetPublishedFileDetails/v1/" {
			_, _ = fmt.Fprint(writer, `{"response":{"publishedfiledetails":[{"publishedfileid":"3760106287","result":1,"consumer_app_id":322330,"title":"Workshop name"}]}}`)
			return
		}
		_, _ = fmt.Fprint(writer, `<html><body>Too many requests. Please try again later.</body></html>`)
	}))
	defer server.Close()
	provider := NewSteamProvider("", "322330")
	provider.APIBase, provider.CommunityBase = server.URL, server.URL
	service := &Service{metadata: provider}
	items, err := service.Metadata(context.Background(), []string{"3760106287"})
	if err == nil || !strings.Contains(err.Error(), "1 个模组的工坊资料未补全") || !strings.Contains(err.Error(), "3760106287") || items["3760106287"].Name != "Workshop name" {
		t.Fatalf("Steam error page lost partial metadata or warning: %#v, %v", items, err)
	}
}

func TestDisplayMetadataRetriesPartialCacheAndKeepsKnownFieldsOnFailure(t *testing.T) {
	var communityRequests atomic.Int64
	var stage atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/ISteamRemoteStorage/GetPublishedFileDetails/v1/" {
			_, _ = fmt.Fprint(writer, `{"response":{"publishedfiledetails":[{"publishedfileid":"376333686","result":1,"consumer_app_id":322330,"title":"SHOWTEMPERATURE"}]}}`)
			return
		}
		communityRequests.Add(1)
		switch stage.Load() {
		case 0:
			_, _ = fmt.Fprint(writer, `<div class="workshopItemTitle">Combined Status</div>`)
		case 1:
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
		default:
			_, _ = fmt.Fprint(writer, `<div class="workshopItemTitle">Combined Status</div><a href="/id/rezecib/myworkshopfiles/">rezecib</a>`)
		}
	}))
	defer server.Close()
	provider := NewSteamProvider("", "322330")
	provider.APIBase, provider.CommunityBase = server.URL, server.URL
	service := &Service{metadata: provider}
	ids := []string{"376333686"}
	for _, phase := range []int64{0, 1, 2} {
		stage.Store(phase)
		// User retries after the short display-only backoff, not immediately.
		provider.cacheMu.Lock()
		if value, ok := provider.communityCache[ids[0]]; ok {
			value.ExpiresAt = time.Now().Add(-time.Second)
			provider.communityCache[ids[0]] = value
		}
		provider.cacheMu.Unlock()
		before := communityRequests.Load()
		items, err := service.Metadata(context.Background(), ids)
		if communityRequests.Load() <= before {
			t.Fatalf("phase %d reused incomplete metadata without retrying", phase)
		}
		if items[ids[0]].Name != "Combined Status" {
			t.Fatalf("phase %d lost the known Workshop name: %#v", phase, items)
		}
		if phase < 2 && (err == nil || !strings.Contains(err.Error(), "工坊资料未补全")) {
			t.Fatalf("phase %d did not report missing author: %v", phase, err)
		}
		if phase == 2 && (err != nil || items[ids[0]].Author != "rezecib") {
			t.Fatalf("retry did not fill the missing author: %#v, %v", items, err)
		}
	}
	before := communityRequests.Load()
	if _, err := service.Metadata(context.Background(), ids); err != nil {
		t.Fatal(err)
	}
	if communityRequests.Load() != before {
		t.Fatal("complete metadata was fetched again within the cache TTL")
	}
}
