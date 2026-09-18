package luajit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Only public upstream requests may use a mirror. Controller transfer tokens
// and user-supplied download URLs never pass through this path's fallbacks.
func githubSources(rawURL, mode string) ([]string, error) {
	proxy := ""
	if rawURL == upstreamReleasesURL {
		proxy = "https://gh-proxy.com/" + rawURL
	} else if u, err := url.Parse(rawURL); err == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && strings.HasPrefix(u.Path, "/fesily/DontStarveLuaJIT2/releases/download/") {
		proxy = "https://ghfast.top/" + rawURL
	}
	if proxy == "" {
		return []string{rawURL}, nil
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "auto":
		return []string{rawURL, proxy}, nil
	case "direct":
		return []string{rawURL}, nil
	case "proxy":
		return []string{proxy}, nil
	default:
		return nil, errors.New("DST_ADMIN_GITHUB_ACCESS 必须为 auto、direct 或 proxy")
	}
}

func publicSourceClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport
	if base, ok := transport.(*http.Transport); ok {
		copy := base.Clone()
		copy.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		copy.TLSHandshakeTimeout = 5 * time.Second
		copy.ResponseHeaderTimeout = 5 * time.Second
		// These requests are explicit and infrequent; do not leave a transport
		// with idle connections behind after each operation.
		copy.DisableKeepAlives = true
		transport = copy
	}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) >= 5 || next.URL.Scheme != "https" || next.URL.User != nil {
			return errors.New("安装包重定向无效")
		}
		return nil
	}}
}

func fetchPublicSource(ctx context.Context, rawURL string, client *http.Client) (*http.Response, error) {
	sources, err := githubSources(rawURL, os.Getenv("DST_ADMIN_GITHUB_ACCESS"))
	if err != nil {
		return nil, err
	}
	var failures []error
	for i, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if i > 0 {
			emit(ctx, "download", 5, "GitHub 直连失败，运行节点正在尝试下载代理")
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("User-Agent", "DST-Admin-LuaJIT")
		if rawURL == upstreamReleasesURL {
			request.Header.Set("Accept", "application/vnd.github+json")
		}
		response, err := client.Do(request)
		if err == nil && response.StatusCode == http.StatusOK {
			return response, nil
		}
		if err == nil {
			response.Body.Close()
			err = fmt.Errorf("HTTP %d", response.StatusCode)
		}
		failures = append(failures, err)
	}
	return nil, fmt.Errorf("运行节点下载失败: %w", errors.Join(failures...))
}
