package softwareupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOfficialReleaseSelectionAndSourceFallback(t *testing.T) {
	name := "dst-admin-update-linux-amd64.tar.gz"
	value := map[string]any{"tag_name": "v1.10.0", "html_url": "https://github.com/" + Repository + "/releases/tag/v1.10.0", "assets": []Asset{{Name: name, URL: "https://github.com/" + Repository + "/releases/download/v1.10.0/" + name, Size: 100}, {Name: name + ".sha256", URL: "https://github.com/" + Repository + "/releases/download/v1.10.0/" + name + ".sha256", Size: 120}}}
	data, _ := json.Marshal(value)
	var calls []string
	client := NewGitHubClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Hostname())
		if r.URL.Hostname() == "api.github.com" {
			return nil, errors.New("offline")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data))), Header: make(http.Header)}, nil
	})})
	release, err := client.Latest(context.Background(), "linux-amd64", "auto")
	if err != nil || !release.OnlineUpdate || release.Version != "v1.10.0" || len(calls) != 2 || calls[1] != "ghfast.top" {
		t.Fatal(release, calls, err)
	}
	release, err = client.Latest(context.Background(), "darwin-arm64", "proxy")
	if err != nil || release.OnlineUpdate {
		t.Fatal("missing-platform release must be visible but not installable", err)
	}
}

func TestUntrustedReleaseURLsAreRejected(t *testing.T) {
	for _, raw := range []string{"http://github.com/" + Repository + "/releases/latest", "https://example.com/update", "https://github.com/other/repo/releases/download/v1.0.0/dst-admin-update-linux-amd64.tar.gz", "https://github.com:443/" + Repository + "/releases/download/v1.0.0/dst-admin-update-x", "https://ghfast.top/https://127.0.0.1/update", "https://api.github.com/repos/evil/repo/releases/latest"} {
		if _, err := sourceURLs(raw, "auto"); err == nil {
			t.Fatal(raw)
		}
	}
}

func TestAgentOfficialReleaseSelectsItsOwnPlatformBundle(t *testing.T) {
	name := "dst-admin-agent-update-windows-amd64.tar.gz"
	managerName := "dst-admin-update-linux-amd64.tar.gz"
	var assets []Asset
	for _, assetName := range []string{name, name + ".sha256", managerName, managerName + ".sha256"} {
		assets = append(assets, Asset{Name: assetName, URL: "https://github.com/" + Repository + "/releases/download/v1.2.3/" + assetName, Size: 120})
	}
	data, _ := json.Marshal(map[string]any{"tag_name": "v1.2.3", "html_url": "https://github.com/" + Repository + "/releases/tag/v1.2.3", "assets": assets})
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data))), Header: make(http.Header)}, nil
	})}
	release, err := NewAgentGitHubClient(httpClient).Latest(context.Background(), "windows-amd64", "direct")
	if err != nil || !release.OnlineUpdate || release.Archive.Name != name || release.Checksum.Name != name+".sha256" {
		t.Fatal("wrong Agent asset selected", release, err)
	}
	release, err = NewGitHubClient(httpClient).Latest(context.Background(), "windows-amd64", "direct")
	if err != nil || release.OnlineUpdate {
		t.Fatal("Agent bundle exposed as management update", release, err)
	}
}

func TestPartialDownloadIsNeverConcatenatedWithFallback(t *testing.T) {
	var calls int
	client := NewGitHubClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("partial")), Header: make(http.Header)}, nil
	})})
	asset := Asset{URL: "https://github.com/" + Repository + "/releases/download/v1.1.0/dst-admin-update-linux-amd64.tar.gz", Size: 20}
	err := client.Download(context.Background(), asset, "auto", io.Discard)
	if err == nil || calls != 1 {
		t.Fatal("partial archive retried", calls, err)
	}
}
