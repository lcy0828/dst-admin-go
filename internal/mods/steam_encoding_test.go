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

// The affected Steam edge serves the gzip-only representation as a 429 while
// the uncompressed public page remains readable. Exercise the real transport,
// not a RoundTripper stub that bypasses net/http's automatic gzip negotiation.
func TestWorkshopCommunityReadsUseAvailableRepresentation(t *testing.T) {
	for _, query := range []string{"example", "100"} {
		t.Run(query, func(t *testing.T) {
			var pages atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/GetPublishedFileDetails/") {
					if r.Header.Get("Accept-Encoding") != "gzip" {
						t.Error("community compatibility changed the JSON API transport")
					}
					fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":9}]}}`)
					return
				}
				pages.Add(1)
				if r.Header.Get("Accept-Encoding") != "identity" {
					http.Error(w, "representation unavailable", http.StatusTooManyRequests)
					return
				}
				if strings.Contains(r.URL.Path, "/workshop/browse/") {
					fmt.Fprint(w, `<html><body><div>1 entries matching filters</div><a href="https://steamcommunity.com/sharedfiles/filedetails/?id=100"><img alt="Public result"></a></body></html>`)
					return
				}
				fmt.Fprint(w, publicWorkshopItem("100", "322330", ""))
			}))
			defer server.Close()
			workshop := testWorkshop(t, server, "")
			options := SearchOptions{Query: query, Page: 1, PageSize: 20}
			for range 2 {
				result, err := workshop.Search(context.Background(), options)
				if err != nil || len(result.Items) != 1 || result.Items[0].ID != "100" || result.Source != "community" {
					t.Fatalf("query = %+v, %v", result, err)
				}
			}
			if pages.Load() != 1 {
				t.Fatalf("cache hit repeated the public page request: %d", pages.Load())
			}
		})
	}
}

func TestWorkshopGenuineCommunityRateLimitStillStopsRetries(t *testing.T) {
	var pages atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/GetPublishedFileDetails/") {
			fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":1,"title":"API result"}]}}`)
			return
		}
		pages.Add(1)
		w.Header().Set("Retry-After", "120")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()
	workshop := testWorkshop(t, server, "")
	for _, query := range []string{"first", "different"} {
		_, err := workshop.Search(context.Background(), SearchOptions{Query: query, Page: 1, PageSize: 20})
		var failure *NetworkError
		if !errors.As(err, &failure) || failure.Code != "WORKSHOP_RATE_LIMITED" || failure.RetryAfter < 118 || failure.RetryAfter > 120 {
			t.Fatalf("lost real rate limit or wait time: %v", err)
		}
	}
	if pages.Load() != 1 {
		t.Fatalf("retried a real 429: %d", pages.Load())
	}
	result, err := workshop.Search(context.Background(), SearchOptions{Query: "100", Page: 1, PageSize: 20})
	if err != nil || len(result.Items) != 1 || result.Items[0].Name != "API result" {
		t.Fatalf("community cooldown blocked independent ID API: %+v, %v", result, err)
	}
}

func TestWorkshopQueuedCommunityRequestHonorsNewRateLimit(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
			<-release
		}
		w.Header().Set("Retry-After", "120")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()
	limits := newNetworkLimits()
	config, _ := (NetworkConfig{ProxyMode: "direct"}).Normalize()
	client := newWorkshopClient(config, limits)
	defer client.CloseIdleConnections()
	results := make(chan error, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request := func() {
		req, _ := http.NewRequestWithContext(context.WithValue(ctx, networkKindKey{}, 1), http.MethodGet, server.URL, nil)
		response, err := client.Do(req)
		if response != nil {
			response.Body.Close()
		}
		results <- err
	}
	go request()
	select {
	case <-started:
	case <-ctx.Done():
		close(release)
		t.Fatal("first request did not reach the server")
	}
	go request()
	// Confirm the second request is waiting for the shared HTML slot.
	for len(limits.waiting) == 0 && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	close(release)
	for range 2 {
		var failure *NetworkError
		if err := <-results; !errors.As(err, &failure) || failure.Code != "WORKSHOP_RATE_LIMITED" {
			t.Fatalf("queued result: %v", err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("queued request ignored new cooldown: %d", requests.Load())
	}
}
