package luajit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type sourceTransport func(*http.Request) (*http.Response, error)

func (f sourceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const testUpstreamAsset = "https://github.com/fesily/DontStarveLuaJIT2/releases/download/v3.0.0/linux_Mod.zip"

func TestGitHubMirrorsOnlyReceivePublicUpstreamURLs(t *testing.T) {
	for _, raw := range []string{
		"https://controller.example/package?token=secret",
		"https://github.com/another/project/releases/download/v1/package.zip",
		testUpstreamAsset + "?token=secret",
		strings.Replace(testUpstreamAsset, "github.com", "github.com.example", 1),
		strings.Replace(testUpstreamAsset, "github.com", "user:secret@github.com", 1),
	} {
		got, err := githubSources(raw, "proxy")
		if err != nil || len(got) != 1 || got[0] != raw {
			t.Fatalf("custom source was proxied: %v %v", got, err)
		}
	}
	for _, raw := range []string{testUpstreamAsset, upstreamReleasesURL} {
		got, err := githubSources(raw, "auto")
		if err != nil || len(got) != 2 || got[0] != raw || got[1] == raw {
			t.Fatalf("missing fallback: %v %v", got, err)
		}
	}
	if _, err := githubSources(upstreamReleasesURL, "invalid"); err == nil {
		t.Fatal("invalid network mode accepted")
	}
}

func TestUpstreamRefreshFallsBackOnNodeWithoutCredentials(t *testing.T) {
	t.Setenv("DST_ADMIN_GITHUB_ACCESS", "auto")
	store, _ := NewStore(t.TempDir())
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	var calls []string
	http.DefaultTransport = sourceTransport(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.String())
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("credentials sent to public download")
		}
		if r.URL.Host == "api.github.com" {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
		}
		body := `{"tag_name":"v3.1.0","assets":[{"name":"linux_Mod.zip","size":1024,"digest":"sha256:` + strings.Repeat("a", 64) + `","browser_download_url":"https://github.com/fesily/DontStarveLuaJIT2/releases/download/v3.1.0/linux_Mod.zip"}]}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	if err := store.RefreshUpstream(context.Background()); err != nil {
		t.Fatal(err)
	}
	releases, err := store.Available()
	if err != nil || releases[0].Version != "3.1.0" || len(calls) != 2 || calls[1] != "https://gh-proxy.com/"+upstreamReleasesURL {
		t.Fatalf("refresh did not use validated mirror metadata: %v %v %v", releases, calls, err)
	}
}

func TestMirroredPackageStillRequiresExactChecksum(t *testing.T) {
	t.Setenv("DST_ADMIN_GITHUB_ACCESS", "auto")
	data := fixtureArchive(t, nil)
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, corrupt := range []bool{true, false} {
		t.Run(map[bool]string{true: "corrupted", false: "valid"}[corrupt], func(t *testing.T) {
			store, _ := NewStore(t.TempDir())
			calls := 0
			http.DefaultTransport = sourceTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host == "github.com" {
					return nil, errors.New("direct connection unavailable")
				}
				if r.URL.String() != "https://ghfast.top/"+testUpstreamAsset {
					t.Fatalf("unexpected mirror: %s", r.URL)
				}
				body := data
				if corrupt {
					body = []byte("corrupt package")
				}
				return &http.Response{StatusCode: 200, ContentLength: int64(len(body)), Body: io.NopCloser(bytes.NewReader(body))}, nil
			})
			_, err := store.ImportURL(context.Background(), testUpstreamAsset, digest)
			cached, listErr := store.List()
			if listErr != nil || calls != 2 || (corrupt && (err == nil || len(cached) != 0)) || (!corrupt && (err != nil || len(cached) != 1)) {
				t.Fatalf("corrupt=%v, err=%v, cached=%v, calls=%d", corrupt, err, cached, calls)
			}
		})
	}
}

func TestDirectModeAndCancellationDoNotContactMirrors(t *testing.T) {
	t.Setenv("DST_ADMIN_GITHUB_ACCESS", "direct")
	calls := 0
	client := &http.Client{Transport: sourceTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("offline")
	})}
	if _, err := fetchPublicSource(context.Background(), upstreamReleasesURL, client); err == nil || calls != 1 {
		t.Fatalf("direct mode retried: calls=%d err=%v", calls, err)
	}
	t.Setenv("DST_ADMIN_GITHUB_ACCESS", "auto")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fetchPublicSource(ctx, upstreamReleasesURL, client); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancelled request contacted a source: calls=%d err=%v", calls, err)
	}
	request, _ := http.NewRequest(http.MethodGet, "http://example.com/package", nil)
	if err := publicSourceClient(time.Minute).CheckRedirect(request, nil); err == nil {
		t.Fatal("HTTP redirect accepted")
	}
}
