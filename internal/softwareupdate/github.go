package softwareupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

type ReleaseClient interface {
	Latest(context.Context, string, string) (*Release, error)
	Download(context.Context, Asset, string, io.Writer) error
	Checksum(context.Context, Asset, string) ([]byte, error)
}

type GitHubClient struct {
	client       *http.Client
	direct       *http.Client
	kind         string
	mirrorPrefix string
}

const defaultMirrorPrefix = "https://ghfast.top/"

// A mirror prefixes an official release URL; it cannot supply release paths,
// credentials or query parameters of its own.
func normalizeMirrorPrefix(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultMirrorPrefix, nil
	}
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 1024 || strings.ContainsAny(raw, "\\\r\n\t\x00 ") || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || u.Opaque != "" || u.RawPath != "" {
		return "", fmt.Errorf("%w: invalid GitHub mirror prefix", ErrInvalid)
	}
	if u.Path != "" && path.Clean(u.Path)+"/" != u.Path && path.Clean(u.Path) != u.Path {
		return "", fmt.Errorf("%w: invalid GitHub mirror prefix path", ErrInvalid)
	}
	return strings.TrimRight(u.String(), "/") + "/", nil
}

func NewAgentGitHubClient(client *http.Client) *GitHubClient {
	result := NewGitHubClient(client)
	result.kind = "agent"
	return result
}

func NewGitHubClient(client *http.Client) *GitHubClient {
	return newGitHubClient(client, defaultMirrorPrefix)
}

func newGitHubClient(client *http.Client, prefix string) *GitHubClient {
	if client == nil {
		client = &http.Client{}
	}
	copy := *client
	g := &GitHubClient{client: &copy, mirrorPrefix: prefix}
	copy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many release download redirects")
		}
		return g.trustedURL(req.URL.String(), true)
	}
	return g
}

func ValidSource(source string) bool {
	return source == "auto" || source == "direct" || source == "proxy" || source == "mirror"
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
		prefix := "/repos/" + Repository + "/releases/tags/"
		if strings.HasPrefix(u.Path, prefix) {
			if _, ok := NormalizeVersion(strings.TrimPrefix(u.Path, prefix)); ok {
				return nil
			}
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
	case "objects.githubusercontent.com", "release-assets.githubusercontent.com":
		if redirect {
			return nil
		}
	}
	return fmt.Errorf("%w: untrusted release URL", ErrInvalid)
}

func (g *GitHubClient) trustedURL(raw string, redirect bool) error {
	if strings.HasPrefix(raw, g.mirrorPrefix) {
		return trustedURL(strings.TrimPrefix(raw, g.mirrorPrefix), false)
	}
	return trustedURL(raw, redirect)
}

func (g *GitHubClient) sourceURLs(raw, source string) ([]string, error) {
	if !ValidSource(source) {
		return nil, ErrInvalid
	}
	if err := trustedURL(raw, false); err != nil {
		return nil, err
	}
	proxy := g.mirrorPrefix + raw
	if source == "direct" {
		return []string{raw}, nil
	}
	if source == "proxy" || source == "mirror" {
		return []string{proxy}, nil
	}
	return []string{raw, proxy}, nil
}

func (g *GitHubClient) get(ctx context.Context, raw string) (*http.Response, error) {
	if err := g.trustedURL(raw, false); err != nil {
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
		return nil, transportError(err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("release server returned HTTP %d", response.StatusCode)
	}
	return response, nil
}

func (g *GitHubClient) small(ctx context.Context, asset Asset, source string, limit int64) ([]byte, error) {
	urls, err := g.sourceURLs(asset.URL, source)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, route := range g.routes(urls, source) {
		raw := route.url
		attempt, cancel := context.WithTimeout(ctx, 20*time.Second)
		copy := *g
		copy.client = route.client
		response, err := copy.get(attempt, raw)
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
	return g.release(ctx, platform, source, "")
}
func (g *GitHubClient) Release(ctx context.Context, platform, source, version string) (*Release, error) {
	if normalized, ok := NormalizeVersion(version); !ok || version != normalized {
		return nil, ErrInvalid
	}
	return g.release(ctx, platform, source, version)
}
func (g *GitHubClient) release(ctx context.Context, platform, source, version string) (*Release, error) {
	if !ValidSource(source) {
		return nil, ErrInvalid
	}
	apiURL := "https://api.github.com/repos/" + Repository + "/releases/latest"
	indexURL := "https://github.com/" + Repository + "/releases/latest/download/dst-admin-release.json"
	if version != "" {
		apiURL = "https://api.github.com/repos/" + Repository + "/releases/tags/" + version
		indexURL = "https://github.com/" + Repository + "/releases/download/" + version + "/dst-admin-release.json"
	}
	var data []byte
	var err error
	if source != "proxy" && source != "mirror" {
		data, err = g.small(ctx, Asset{URL: apiURL}, source, 2<<20)
	}
	if (source == "proxy" || source == "mirror") || err != nil && source == "auto" {
		// Prefix mirrors support release assets, not api.github.com. The official
		// release index provides bounded metadata without downloading a binary.
		fallback := *g
		if source == "auto" && g.direct != nil {
			fallback.client = g.direct
		}
		data, err = fallback.small(ctx, Asset{URL: indexURL}, "proxy", 2<<20)
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
	selected, valid := NormalizeVersion(value.Version)
	if version != "" && selected != version {
		return nil, ErrReleaseChanged
	}
	version = selected
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
	urls, err := g.sourceURLs(asset.URL, source)
	if err != nil {
		return err
	}
	var lastErr error
	for _, route := range g.routes(urls, source) {
		headCtx, headCancel := context.WithCancel(ctx)
		timer := time.AfterFunc(12*time.Second, headCancel)
		copy := *g
		copy.client = route.client
		response, err := copy.get(headCtx, route.url)
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
