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

type memoryOfficialReleaseCache struct {
	release OfficialRelease
	found   bool
	saves   int
}

func (c *memoryOfficialReleaseCache) LoadOfficialRelease() (OfficialRelease, bool, error) {
	return c.release, c.found, nil
}

func (c *memoryOfficialReleaseCache) SaveOfficialRelease(release OfficialRelease) error {
	c.release, c.found = release, true
	c.saves++
	return nil
}

func TestParseKleiReleaseFeedFiltersConsoleAndSelectsLatestPCRelease(t *testing.T) {
	feed := rssFeed(
		strings.Replace(rssItem("https://kleiforums.com/game-updates/dst/751622-r2790/", "Sat, 05 Sep 2026 01:30:17 +0000"), ">Release<", ">Test<", 1),
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
	if release.TestRelease == nil || release.TestRelease.Version != "751622" {
		t.Fatalf("test release = %#v", release.TestRelease)
	}
	expected := time.Date(2026, time.August, 13, 0, 0, 0, 0, time.UTC)
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

func TestKleiReleaseCheckerReturnsPersistedReleaseWhenInitialRefreshFails(t *testing.T) {
	base := time.Date(2026, time.August, 28, 10, 0, 0, 0, time.UTC)
	cache := &memoryOfficialReleaseCache{found: true, release: OfficialRelease{
		Version: "747465", ReleaseID: "2783", Source: kleiReleaseSource,
		CheckedAt: base.Add(-time.Hour), PublishedAt: base.Add(-24 * time.Hour),
	}}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	checker := newKleiReleaseChecker(server.Client(), server.URL, 15*time.Minute, func() time.Time { return base }, cache)
	release, err := checker.Check(context.Background())
	if err == nil || release.Version != "747465" || release.ReleaseID != "2783" || !release.Stale {
		t.Fatalf("persisted fallback = %#v, %v", release, err)
	}
}

func TestKleiReleaseCheckerPersistsSuccessfulRefresh(t *testing.T) {
	base := time.Date(2026, time.August, 28, 10, 0, 0, 0, time.UTC)
	cache := &memoryOfficialReleaseCache{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = fmt.Fprint(response, rssFeed(rssItem(
			"https://kleiforums.com/game-updates/dst/747465-r2783/",
			"Thu, 13 Aug 2026 17:11:57 +0000",
		)))
	}))
	defer server.Close()

	checker := newKleiReleaseChecker(server.Client(), server.URL, 15*time.Minute, func() time.Time { return base }, cache)
	release, err := checker.Check(context.Background())
	if err != nil || release.Version != "747465" || cache.saves != 1 || cache.release.Version != "747465" {
		t.Fatalf("persisted refresh = %#v, cache=%#v, %v", release, cache, err)
	}
}

func rssFeed(items ...string) string {
	return `<html><body>` + strings.Join(items, "") + `</body></html>`
}

func rssItem(link, publishedAt string) string {
	if date, err := time.Parse(time.RFC1123Z, publishedAt); err == nil {
		publishedAt = date.Format("01/02/06")
	}
	return `<a class="cRelease" data-releaseid="record" href="` + link + `"><h3><span class="ipsBadge">Release</span></h3><div class="ipsDataItem_meta">Released ` + publishedAt + `...</div></a>`
}

func TestKleiReleaseCheckerIgnoresLegacyRSSCacheAndAllowsExplicitRefresh(t *testing.T) {
	base := time.Now().UTC()
	cache := &memoryOfficialReleaseCache{found: true, release: OfficialRelease{Version: "751622", Source: "klei-forums-rss", CheckedAt: base}}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, rssFeed(rssItem("https://kleiforums.com/game-updates/dst/747465-r2783/", "08/13/26")))
	}))
	defer server.Close()
	checker := newKleiReleaseChecker(server.Client(), server.URL, 15*time.Minute, time.Now, cache)
	first, err := checker.Check(context.Background())
	if err != nil || first.Version != "747465" || requests.Load() != 1 {
		t.Fatalf("first = %#v, %v", first, err)
	}
	if _, err := checker.Refresh(context.Background()); err != nil || requests.Load() != 2 {
		t.Fatalf("refresh requests=%d, %v", requests.Load(), err)
	}
}

func TestKleiListDoesNotInferReleaseFromHotfixOrMissingChannel(t *testing.T) {
	item := rssItem("https://kleiforums.com/game-updates/dst/751622-r2790/", "09/04/26")
	for _, kind := range []string{"Test", "Hotfix", ""} {
		if _, err := parseKleiReleaseFeed([]byte(rssFeed(strings.Replace(item, ">Release<", ">"+kind+"<", 1)))); err == nil {
			t.Fatalf("accepted %q as Release", kind)
		}
	}
}
