package gameupdate

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestReadLocalVersionPrefersVersionFileAndFallsBackToManifest(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "steamapps", "appmanifest_343050.acf")
	if err := os.MkdirAll(filepath.Dir(manifest), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`"AppState" { "buildid" "987654" }`), 0640); err != nil {
		t.Fatal(err)
	}
	if value, ok := readLocalVersion(root); !ok || value != "987654" {
		t.Fatalf("manifest version = %q, %t", value, ok)
	}
	if err := os.WriteFile(filepath.Join(root, "version.txt"), []byte("123456\nignored\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if value, ok := readLocalVersion(root); !ok || value != "123456" {
		t.Fatalf("version file = %q, %t", value, ok)
	}
}

func TestSteamVersionCheckerAcceptsNumericAndStringVersions(t *testing.T) {
	responses := []struct {
		body     string
		expected string
		current  bool
	}{
		{body: `{"response":{"success":true,"up_to_date":false,"required_version":234567}}`, expected: "234567"},
		{body: `{"response":{"success":true,"up_to_date":true,"required_version":"345678"}}`, expected: "345678", current: true},
	}
	for _, test := range responses {
		checker := &SteamVersionChecker{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Query().Get("appid") != "343050" || request.URL.Query().Get("version") != "123456" {
				t.Fatalf("unexpected query: %s", request.URL.RawQuery)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(test.body)), Header: make(http.Header)}, nil
		})}}
		version, current, err := checker.Check(context.Background(), "123456")
		if err != nil || version != test.expected || current != test.current {
			t.Fatalf("check = %q, %t, %v", version, current, err)
		}
	}
}

func TestSteamVersionCheckerRejectsUnsuccessfulOrMalformedResponses(t *testing.T) {
	for _, body := range []string{
		`{"response":{"success":false}}`,
		`{"response":{"success":true,"required_version":"../../bad"}}`,
	} {
		checker := &SteamVersionChecker{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}}
		if _, _, err := checker.Check(context.Background(), "1"); err == nil {
			t.Fatalf("response %s was accepted", body)
		}
	}
}
