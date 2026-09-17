package gameupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const (
	kleiReleaseFeedURL = "https://kleiforums.com/game-updates/dst/"
	kleiReleaseSource  = "klei-forums-release"
	kleiReleaseMaxBody = 4 * 1024 * 1024
)

var kleiPCReleasePath = regexp.MustCompile(`^/game-updates/dst/([0-9]+)-r([0-9]+)/?$`)

type OfficialReleaseChecker interface {
	Check(context.Context) (OfficialRelease, error)
}

type OfficialReleaseCache interface {
	LoadOfficialRelease() (OfficialRelease, bool, error)
	SaveOfficialRelease(OfficialRelease) error
}

type KleiReleaseChecker struct {
	client   *http.Client
	endpoint string
	ttl      time.Duration
	now      func() time.Time

	mu     sync.Mutex
	cached *OfficialRelease
	store  OfficialReleaseCache
	loaded bool
}

func NewKleiReleaseChecker(caches ...OfficialReleaseCache) *KleiReleaseChecker {
	return newKleiReleaseChecker(
		&http.Client{Timeout: 5 * time.Second},
		kleiReleaseFeedURL,
		15*time.Minute,
		time.Now,
		caches...,
	)
}

func newKleiReleaseChecker(client *http.Client, endpoint string, ttl time.Duration, now func() time.Time, caches ...OfficialReleaseCache) *KleiReleaseChecker {
	checker := &KleiReleaseChecker{client: client, endpoint: endpoint, ttl: ttl, now: now}
	if len(caches) > 0 {
		checker.store = caches[0]
	}
	return checker
}

func (c *KleiReleaseChecker) Check(ctx context.Context) (OfficialRelease, error) {
	return c.check(ctx, false)
}

func (c *KleiReleaseChecker) Refresh(ctx context.Context) (OfficialRelease, error) {
	return c.check(ctx, true)
}

func (c *KleiReleaseChecker) check(ctx context.Context, force bool) (OfficialRelease, error) {
	started := c.now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now().UTC()
	c.loadCachedRelease()
	if err := ctx.Err(); err != nil {
		return OfficialRelease{}, err
	}
	if c.cached != nil && !c.cached.Stale && now.Sub(c.cached.CheckedAt) < c.ttl && (!force || c.cached.CheckedAt.After(started)) {
		return *c.cached, nil
	}

	release, err := c.fetch(ctx)
	if err != nil {
		if c.cached != nil {
			c.cached.Stale = true
			stale := *c.cached
			stale.Stale = true
			return stale, err
		}
		return OfficialRelease{}, err
	}
	release.CheckedAt = c.now().UTC()
	release.Stale = false
	if release.TestRelease != nil {
		release.TestRelease.CheckedAt = release.CheckedAt
	}
	c.cached = &release
	if c.store != nil {
		_ = c.store.SaveOfficialRelease(release)
	}
	return release, nil
}

func (c *KleiReleaseChecker) loadCachedRelease() {
	if c.loaded {
		return
	}
	c.loaded = true
	if c.store == nil {
		return
	}
	release, found, err := c.store.LoadOfficialRelease()
	if err == nil && found && release.Source == kleiReleaseSource && releaseVersionPattern.MatchString(strings.TrimSpace(release.Version)) {
		c.cached = &release
	}
}

func (c *KleiReleaseChecker) fetch(ctx context.Context) (OfficialRelease, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return OfficialRelease{}, err
	}
	request.Header.Set("Accept", "text/html")
	request.Header.Set("User-Agent", "dst-admin-go game version checker")
	response, err := c.client.Do(request)
	if err != nil {
		return OfficialRelease{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return OfficialRelease{}, fmt.Errorf("Klei release feed returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, kleiReleaseMaxBody+1))
	if err != nil {
		return OfficialRelease{}, err
	}
	if len(body) > kleiReleaseMaxBody {
		return OfficialRelease{}, errors.New("Klei release feed exceeds the response limit")
	}
	return parseKleiReleaseFeed(body)
}

func parseKleiReleaseFeed(data []byte) (OfficialRelease, error) {
	document, err := goquery.NewDocumentFromReader(strings.NewReader(string(data)))
	if err != nil {
		return OfficialRelease{}, fmt.Errorf("parse Klei release list: %w", err)
	}
	var latest, test OfficialRelease
	document.Find("a.cRelease[data-releaseid]").Each(func(_ int, item *goquery.Selection) {
		link, _ := item.Attr("href")
		parsedURL, err := url.Parse(link)
		if err != nil || parsedURL.Scheme != "https" || (parsedURL.Hostname() != "kleiforums.com" && parsedURL.Hostname() != "www.kleiforums.com") {
			return
		}
		match := kleiPCReleasePath.FindStringSubmatch(parsedURL.Path)
		if len(match) != 3 {
			return
		}
		kind := strings.TrimSpace(item.Find("h3 .ipsBadge").Text())
		if kind != "Release" && kind != "Test" {
			return
		}
		selected := &latest
		if kind == "Test" {
			selected = &test
		}
		// The official list is not always ordered by version or release date.
		if selected.Version == "" || newerGameVersion(match[1], selected.Version) {
			date := releaseListDatePattern.FindString(item.Find(".ipsDataItem_meta").Text())
			publishedAt, err := time.Parse("01/02/06", date)
			if err != nil {
				return
			}
			*selected = OfficialRelease{Version: match[1], ReleaseID: match[2], URL: link, Source: kleiReleaseSource, PublishedAt: publishedAt}
		}
	})
	if latest.Version == "" {
		return OfficialRelease{}, errors.New("Klei release list contains no confirmed PC DST Release")
	}
	if newerGameVersion(test.Version, latest.Version) {
		latest.TestRelease = &test
	}
	return latest, nil
}

var releaseListDatePattern = regexp.MustCompile(`\b[0-9]{2}/[0-9]{2}/[0-9]{2}\b`)

func newerGameVersion(a, b string) bool {
	if !releaseVersionPattern.MatchString(a) || !releaseVersionPattern.MatchString(b) {
		return false
	}
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	return len(a) > len(b) || len(a) == len(b) && a > b
}
