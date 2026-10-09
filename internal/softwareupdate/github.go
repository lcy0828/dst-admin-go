package softwareupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ReleaseClient interface {
	Latest(context.Context, string, string) (*Release, error)
	Download(context.Context, Asset, string, io.Writer) error
	Checksum(context.Context, Asset, string) ([]byte, error)
}

type GitHubClient struct {
	client *http.Client
	kind   string
}

func NewAgentGitHubClient(client *http.Client) *GitHubClient {
	result := NewGitHubClient(client)
	result.kind = "agent"
	return result
}

func NewGitHubClient(client *http.Client) *GitHubClient {
	if client == nil {
		client = &http.Client{}
	}
	copy := *client
	copy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many release download redirects")
		}
		return trustedURL(req.URL.String(), true)
	}
	return &GitHubClient{client: &copy}
}

func ValidSource(source string) bool {
	return source == "auto" || source == "direct" || source == "proxy"
}

func trustedURL(raw string, redirect bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" {
		return ErrInvalid
	}
	switch u.Hostname() {
	case "api.github.com":
		if u.Path == "/repos/"+Repository+"/releases/latest" {
			return nil
		}
	case "github.com":
		if u.Path == "/"+Repository+"/releases/latest/download/dst-admin-release.json" && u.RawQuery == "" {
			return nil
		}
		prefix := "/" + Repository + "/releases/download/"
		if strings.HasPrefix(u.Path, prefix) && !strings.Contains(u.Path, "..") && u.RawQuery == "" {
			parts := strings.Split(strings.TrimPrefix(u.Path, prefix), "/")
			if len(parts) == 2 {
				if _, ok := NormalizeVersion(parts[0]); ok && (strings.HasPrefix(parts[1], "dst-admin-update-") || strings.HasPrefix(parts[1], "dst-admin-agent-update-") || parts[1] == "dst-admin-release.json") {
					return nil
				}
			}
		}
	case "ghfast.top":
		if strings.HasPrefix(u.Path, "/https://") {
			return trustedURL(strings.TrimPrefix(u.Path, "/"), false)
		}
	case "objects.githubusercontent.com", "release-assets.githubusercontent.com":
		if redirect {
			return nil
		}
	}
	return fmt.Errorf("%w: untrusted release URL", ErrInvalid)
}

func sourceURLs(raw, source string) ([]string, error) {
	if !ValidSource(source) {
		return nil, ErrInvalid
	}
	if err := trustedURL(raw, false); err != nil {
		return nil, err
	}
	proxy := "https://ghfast.top/" + raw
	if source == "direct" {
		return []string{raw}, nil
	}
	if source == "proxy" {
		return []string{proxy}, nil
	}
	return []string{raw, proxy}, nil
}

func (g *GitHubClient) get(ctx context.Context, raw string) (*http.Response, error) {
	if err := trustedURL(raw, false); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "dst-admin-go/software-update")
	response, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("release server returned HTTP %d", response.StatusCode)
	}
	return response, nil
}

func (g *GitHubClient) small(ctx context.Context, asset Asset, source string, limit int64) ([]byte, error) {
	urls, err := sourceURLs(asset.URL, source)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, raw := range urls {
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		response, err := g.get(attempt, raw)
		if err == nil {
			var data []byte
			data, err = io.ReadAll(io.LimitReader(response.Body, limit+1))
			response.Body.Close()
			if err == nil && int64(len(data)) > limit {
				err = ErrInvalid
			}
			if err == nil {
				cancel()
				return data, nil
			}
		}
		cancel()
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

func (g *GitHubClient) Latest(ctx context.Context, platform, source string) (*Release, error) {
	if !ValidSource(source) {
		return nil, ErrInvalid
	}
	var data []byte
	var err error
	if source != "proxy" {
		data, err = g.small(ctx, Asset{URL: "https://api.github.com/repos/" + Repository + "/releases/latest"}, "direct", 2<<20)
	}
	if source == "proxy" || err != nil && source == "auto" {
		// GHFast supports release assets, not api.github.com. The official
		// release index provides bounded metadata without downloading a binary.
		data, err = g.small(ctx, Asset{URL: "https://github.com/" + Repository + "/releases/latest/download/dst-admin-release.json"}, "proxy", 2<<20)
	}
	if err != nil {
		return nil, err
	}
	var value struct {
		Version     string    `json:"tag_name"`
		Name        string    `json:"name"`
		Notes       string    `json:"body"`
		URL         string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Draft       bool      `json:"draft"`
		Prerelease  bool      `json:"prerelease"`
		Assets      []Asset   `json:"assets"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	version, valid := NormalizeVersion(value.Version)
	if !valid || value.Draft || value.Prerelease || value.URL != "https://github.com/"+Repository+"/releases/tag/"+version {
		return nil, ErrInvalid
	}
	if len(value.Notes) > 128<<10 {
		value.Notes = value.Notes[:128<<10]
	}
	release := &Release{Version: version, Name: value.Name, Notes: value.Notes, URL: value.URL, PublishedAt: value.PublishedAt}
	name := "dst-admin-update-" + platform + ".tar.gz"
	if g.kind == "agent" {
		name = "dst-admin-agent-update-" + platform + ".tar.gz"
	}
	for _, asset := range value.Assets {
		if asset.Name != name && asset.Name != name+".sha256" {
			continue
		}
		if asset.URL != "https://github.com/"+Repository+"/releases/download/"+version+"/"+asset.Name {
			return nil, ErrInvalid
		}
		if asset.Name == name {
			release.Archive = asset
		} else {
			release.Checksum = asset
		}
	}
	if release.Archive.Size > MaxArchiveBytes {
		return nil, ErrInvalid
	}
	release.OnlineUpdate = release.Archive.Size > 0 && release.Checksum.Size > 0
	return release, nil
}

func (g *GitHubClient) Checksum(ctx context.Context, asset Asset, source string) ([]byte, error) {
	return g.small(ctx, asset, source, 4096)
}

// Downloads can be retried only before any bytes have been delivered. A partial
// download fails safely; it is never concatenated with bytes from another mirror.
func (g *GitHubClient) Download(ctx context.Context, asset Asset, source string, destination io.Writer) error {
	urls, err := sourceURLs(asset.URL, source)
	if err != nil {
		return err
	}
	var lastErr error
	for _, raw := range urls {
		headCtx, headCancel := context.WithCancel(ctx)
		timer := time.AfterFunc(12*time.Second, headCancel)
		response, err := g.get(headCtx, raw)
		timer.Stop()
		if err != nil {
			headCancel()
			lastErr = err
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		if response.ContentLength > MaxArchiveBytes {
			response.Body.Close()
			headCancel()
			return ErrInvalid
		}
		n, copyErr := io.Copy(destination, io.LimitReader(response.Body, MaxArchiveBytes+1))
		response.Body.Close()
		headCancel()
		if copyErr != nil {
			return copyErr
		}
		if n > MaxArchiveBytes || n != asset.Size {
			return fmt.Errorf("%w: release archive size mismatch", ErrInvalid)
		}
		return nil
	}
	return lastErr
}
