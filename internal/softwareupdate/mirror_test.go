package softwareupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestMirrorPrefixValidationAndDefaults(t *testing.T) {
	for raw, want := range map[string]string{"": defaultMirrorPrefix, "https://mirror.example": "https://mirror.example/", " https://mirror.example/gh/ ": "https://mirror.example/gh/", "https://mirror.example:8443/gh": "https://mirror.example:8443/gh/"} {
		got, err := normalizeMirrorPrefix(raw)
		if err != nil || got != want {
			t.Fatal(raw, got, err)
		}
	}
	for _, raw := range []string{"http://mirror.example/", "//mirror.example/", "https://user:secret@mirror.example/", "https://mirror.example/?token=secret", "https://mirror.example/?", "https://mirror.example/#", "https://mirror.example/#fragment", "https://mirror.example/a/../", "https://mirror.example/%2e%2e/", "https://mirror.example/a//b/", "https://mirror.example/a\\b/", "https://mirror.example/a\nb", "https://mirror.example/" + strings.Repeat("a", 1024)} {
		if _, err := normalizeMirrorPrefix(raw); err == nil {
			t.Fatal("invalid prefix accepted", raw)
		}
	}
	s, client := testService(t, nil)
	settings := DefaultSettings()
	settings.MirrorPrefix = "https://mirror.example/gh"
	public, err := s.SaveSettings(settings)
	if err != nil || public.MirrorPrefix != "https://mirror.example/gh/" {
		t.Fatal(public, err)
	}
	if _, err := s.Check(context.Background(), false, "mirror"); err != nil {
		t.Fatal(err)
	}
	settings.MirrorPrefix = "https://another.example/"
	if _, err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Check(context.Background(), false, "mirror"); err != nil {
		t.Fatal(err)
	}
	if client.calls.Load() != 2 {
		t.Fatal("changed prefix reused release cache", client.calls.Load())
	}
	restored, err := New(s.config).Settings()
	if err != nil || restored.MirrorPrefix != settings.MirrorPrefix {
		t.Fatal("custom prefix not persisted", restored, err)
	}
	settings.MirrorPrefix = ""
	if reset, err := s.SaveSettings(settings); err != nil || reset.MirrorPrefix != defaultMirrorPrefix {
		t.Fatal("blank prefix did not restore default", reset, err)
	}
}

func TestConfiguredMirrorCoversReleaseChecksumDownloadAndResume(t *testing.T) {
	for _, kind := range []string{"", "agent"} {
		t.Run(kind, func(t *testing.T) {
			prefix := "https://mirror.example/gh/"
			name := "dst-admin-update-linux-amd64.tar.gz"
			if kind == "agent" {
				name = "dst-admin-agent-update-linux-amd64.tar.gz"
			}
			payload := "verified-release-content"
			archive := Asset{Name: name, URL: "https://github.com/" + Repository + "/releases/download/v1.2.3/" + name, Size: int64(len(payload))}
			checksum := Asset{Name: name + ".sha256", URL: archive.URL + ".sha256", Size: 100}
			metadata, _ := json.Marshal(map[string]any{"tag_name": "v1.2.3", "html_url": "https://github.com/" + Repository + "/releases/tag/v1.2.3", "assets": []Asset{archive, checksum}})
			var calls int
			client := newGitHubClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if !strings.HasPrefix(r.URL.String(), prefix+"https://github.com/"+Repository+"/releases/") || r.Header.Get("Authorization") != "" {
					t.Fatal("mirror route escaped configured prefix", r.URL)
				}
				body := string(metadata)
				header := http.Header{}
				status := http.StatusOK
				switch r.URL.String() {
				case prefix + checksum.URL:
					body = hashBytes([]byte(payload)) + "  " + name + "\n"
				case prefix + archive.URL:
					body = payload
					if r.Header.Get("Range") != "" {
						if r.Header.Get("Range") != "bytes=5-" {
							t.Fatal("invalid resumed offset", r.Header)
						}
						body = payload[5:]
						status = http.StatusPartialContent
						header.Set("Content-Range", fmt.Sprintf("bytes 5-%d/%d", len(payload)-1, len(payload)))
					}
				}
				return &http.Response{StatusCode: status, ContentLength: int64(len(body)), Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}, prefix)
			client.kind = kind
			for _, version := range []string{"", "v1.2.3"} {
				release, err := client.release(context.Background(), "linux-amd64", "mirror", version)
				if err != nil || !release.OnlineUpdate || release.Archive.Name != name {
					t.Fatal(release, err)
				}
			}
			if data, err := client.Checksum(context.Background(), checksum, "mirror"); err != nil || !strings.Contains(string(data), hashBytes([]byte(payload))) {
				t.Fatal(string(data), err)
			}
			var buf bytes.Buffer
			if err := client.Download(context.Background(), archive, "mirror", &buf); err != nil || buf.String() != payload {
				t.Fatal(buf.String(), err)
			}
			file, err := os.CreateTemp(t.TempDir(), "partial")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.WriteString(payload[:5]); err != nil {
				t.Fatal(err)
			}
			var resumed bool
			if err := client.DownloadFile(context.Background(), archive, "mirror", file, func(p DownloadProgress) { resumed = resumed || p.Resumed && p.Source == "mirror" }); err != nil {
				t.Fatal(err)
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(file)
			if err != nil || string(got) != payload || !resumed || calls != 5 {
				t.Fatal("custom mirror resume failed", string(got), resumed, calls, err)
			}
		})
	}
}

func TestCustomMirrorCannotIntroduceUntrustedOriginsOrRedirects(t *testing.T) {
	prefix := "https://mirror.example/gh/"
	for _, destination := range []string{"https://evil.example/update", "http://mirror.example/gh/https://github.com/" + Repository + "/releases/latest/download/dst-admin-release.json", "https://mirror.example.evil/gh/https://github.com/" + Repository + "/releases/latest/download/dst-admin-release.json", prefix + "https://github.com/other/repo/releases/download/v1.0.0/dst-admin-update-x", "https://ghfast.top/https://github.com/" + Repository + "/releases/latest/download/dst-admin-release.json"} {
		t.Run(destination, func(t *testing.T) {
			calls := 0
			client := newGitHubClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{destination}}, Body: io.NopCloser(strings.NewReader(""))}, nil
			})}, prefix)
			if _, err := client.get(context.Background(), prefix+"https://github.com/"+Repository+"/releases/latest/download/dst-admin-release.json"); err == nil || calls != 1 {
				t.Fatal("untrusted redirect followed", calls, err)
			}
		})
	}
	s, _ := testService(t, nil)
	settings := DefaultSettings()
	settings.MirrorPrefix = prefix
	if _, err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []bool{false, true} {
		s.managedClient = true
		resolved, _, _, _, err := s.ResolveNetwork("mirror", agent)
		if err != nil {
			t.Fatal(err)
		}
		client := resolved.(*GitHubClient)
		if client.mirrorPrefix != prefix || agent && client.kind != "agent" {
			t.Fatal("network lost mirror preference", client)
		}
		closeReleaseClient(client)
	}
}
