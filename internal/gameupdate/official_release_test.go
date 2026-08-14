package gameupdate

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseKleiReleaseFeedFiltersConsoleAndSelectsLatestPCRelease(t *testing.T) {
	feed := rssFeed(
		rssItem("https://kleiforums.com/game-updates/dst_ps5/999999-r9999/", "Fri, 14 Aug 2026 17:11:57 +0000"),
		rssItem("https://kleiforums.com/game-updates/dst/740477-r2770/", "Thu, 30 Jul 2026 17:11:57 +0000"),
		rssItem("https://kleiforums.com/game-updates/dst/747465-r2783/", "Thu, 13 Aug 2026 17:11:57 +0000"),
		rssItem("https://kleiforums.com/game-updates/dst_xboxone/888888-r8888/", "Sat, 15 Aug 2026 17:11:57 +0000"),
	)

	release, err := parseKleiReleaseFeed([]byte(feed))
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != "747465" || release.ReleaseID != "2783" {
		t.Fatalf("release = %#v", release)
	}
	if release.Source != kleiReleaseSource || release.URL != "https://kleiforums.com/game-updates/dst/747465-r2783/" {
		t.Fatalf("release source = %#v", release)
	}
	expected := time.Date(2026, time.August, 13, 17, 11, 57, 0, time.UTC)
	if !release.PublishedAt.Equal(expected) {
		t.Fatalf("published at = %s, want %s", release.PublishedAt, expected)
	}
}

func TestParseKleiReleaseFeedRejectsMalformedOrMissingPCRelease(t *testing.T) {
	tests := []string{
		`<rss><channel><item>`,
		rssFeed(rssItem("https://kleiforums.com/game-updates/dst_ps4/3450-r2784/", "Thu, 13 Aug 2026 17:11:57 +0000")),
		rssFeed(rssItem("https://kleiforums.com/game-updates/dst/747465-r2783/", "not-a-date")),
	}
	for _, feed := range tests {
		if _, err := parseKleiReleaseFeed([]byte(feed)); err == nil {
			t.Fatalf("feed was accepted: %s", feed)
		}
	}
}

func TestKleiReleaseCheckerCachesSuccessAndReturnsStaleValueAfterRefreshFailure(t *testing.T) {
	var requests atomic.Int32
	fail := atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if fail.Load() {
			response.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		response.Header().Set("Content-Type", "application/rss+xml")
		_, _ = fmt.Fprint(response, rssFeed(rssItem(
			"https://kleiforums.com/game-updates/dst/747465-r2783/",
			"Thu, 13 Aug 2026 17:11:57 +0000",
		)))
	}))
	defer server.Close()

	base := time.Date(2026, time.August, 15, 10, 0, 0, 0, time.UTC)
	now := base
	checker := newKleiReleaseChecker(server.Client(), server.URL, 15*time.Minute, func() time.Time { return now })

	first, err := checker.Check(context.Background())
	if err != nil || first.Version != "747465" || first.Stale || !first.CheckedAt.Equal(base) {
		t.Fatalf("first check = %#v, %v", first, err)
	}
	now = base.Add(10 * time.Minute)
	second, err := checker.Check(context.Background())
	if err != nil || second.Stale || requests.Load() != 1 {
		t.Fatalf("cached check = %#v, %v, requests=%d", second, err, requests.Load())
	}

	fail.Store(true)
	now = base.Add(16 * time.Minute)
	stale, err := checker.Check(context.Background())
	if err == nil || !stale.Stale || stale.Version != "747465" || !stale.CheckedAt.Equal(base) || requests.Load() != 2 {
		t.Fatalf("stale check = %#v, %v, requests=%d", stale, err, requests.Load())
	}
}

func rssFeed(items ...string) string {
	return `<?xml version="1.0"?><rss version="2.0"><channel>` + strings.Join(items, "") + `</channel></rss>`
}

func rssItem(link, publishedAt string) string {
	return `<item><link>` + link + `</link><pubDate>` + publishedAt + `</pubDate></item>`
}
