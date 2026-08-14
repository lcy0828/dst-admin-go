package gameupdate

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	kleiReleaseFeedURL = "https://kleiforums.com/rss/6-dont-starve-together-updates.xml/"
	kleiReleaseSource  = "klei-forums-rss"
	kleiReleaseMaxBody = 4 * 1024 * 1024
)

var kleiPCReleasePath = regexp.MustCompile(`^/game-updates/dst/([0-9]+)-r([0-9]+)/?$`)

type OfficialReleaseChecker interface {
	Check(context.Context) (OfficialRelease, error)
}

type KleiReleaseChecker struct {
	client   *http.Client
	endpoint string
	ttl      time.Duration
	now      func() time.Time

	mu     sync.Mutex
	cached *OfficialRelease
}

func NewKleiReleaseChecker() *KleiReleaseChecker {
	return newKleiReleaseChecker(
		&http.Client{Timeout: 5 * time.Second},
		kleiReleaseFeedURL,
		15*time.Minute,
		time.Now,
	)
}

func newKleiReleaseChecker(client *http.Client, endpoint string, ttl time.Duration, now func() time.Time) *KleiReleaseChecker {
	return &KleiReleaseChecker{client: client, endpoint: endpoint, ttl: ttl, now: now}
}

func (c *KleiReleaseChecker) Check(ctx context.Context) (OfficialRelease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now().UTC()
	if c.cached != nil && now.Sub(c.cached.CheckedAt) < c.ttl {
		return *c.cached, nil
	}

	release, err := c.fetch(ctx)
	if err != nil {
		if c.cached != nil {
			stale := *c.cached
			stale.Stale = true
			return stale, err
		}
		return OfficialRelease{}, err
	}
	release.CheckedAt = now
	release.Stale = false
	c.cached = &release
	return release, nil
}

func (c *KleiReleaseChecker) fetch(ctx context.Context) (OfficialRelease, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return OfficialRelease{}, err
	}
	request.Header.Set("Accept", "application/rss+xml, application/xml;q=0.9, text/xml;q=0.8")
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
	var feed struct {
		Channel struct {
			Items []struct {
				Link    string `xml:"link"`
				PubDate string `xml:"pubDate"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(data, &feed); err != nil {
		return OfficialRelease{}, fmt.Errorf("parse Klei release feed: %w", err)
	}

	var latest OfficialRelease
	for _, item := range feed.Channel.Items {
		link := strings.TrimSpace(item.Link)
		parsedURL, err := url.Parse(link)
		if err != nil || parsedURL.Scheme != "https" || (parsedURL.Hostname() != "kleiforums.com" && parsedURL.Hostname() != "www.kleiforums.com") {
			continue
		}
		match := kleiPCReleasePath.FindStringSubmatch(parsedURL.Path)
		if len(match) != 3 {
			continue
		}
		publishedAt, err := parseKleiReleaseDate(strings.TrimSpace(item.PubDate))
		if err != nil {
			return OfficialRelease{}, fmt.Errorf("parse Klei release %s date: %w", match[1], err)
		}
		if latest.Version == "" || publishedAt.After(latest.PublishedAt) {
			latest = OfficialRelease{
				Version: match[1], ReleaseID: match[2], PublishedAt: publishedAt.UTC(),
				URL: link, Source: kleiReleaseSource,
			}
		}
	}
	if latest.Version == "" {
		return OfficialRelease{}, errors.New("Klei release feed contains no PC DST release")
	}
	return latest, nil
}

func parseKleiReleaseDate(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC1123Z, time.RFC1123} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid RSS date %q", value)
}
