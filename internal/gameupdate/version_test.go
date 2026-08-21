package gameupdate

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dstinstall "dont/internal/dstserver"
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

func TestReadLocalVersionFromMacSteamApplication(t *testing.T) {
	steamApps := filepath.Join(t.TempDir(), "steamapps")
	gameRoot := filepath.Join(steamApps, "common", "Don't Starve Together")
	executablePath := filepath.Join(gameRoot, "dontstarve_steam.app", "Contents", "MacOS", dstinstall.Binary)
	if err := os.MkdirAll(filepath.Dir(executablePath), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executablePath, []byte("test"), 0750); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(steamApps, "appmanifest_322330.acf")
	if err := os.WriteFile(manifest, []byte(`"AppState" { "buildid" "654321" }`), 0640); err != nil {
		t.Fatal(err)
	}
	if actual := installRoot(executablePath); actual != gameRoot {
		t.Fatalf("install root = %q, want %q", actual, gameRoot)
	}
	if value, ok := readLocalVersion(executablePath); !ok || value != "654321" {
		t.Fatalf("macOS Steam manifest version = %q, %t", value, ok)
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
			if request.URL.Query().Get("appid") != "322330" || request.URL.Query().Get("version") != "123456" {
				t.Fatalf("unexpected query: %s", request.URL.RawQuery)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(test.body)), Header: make(http.Header)}, nil
		})}}
		version, current, err := checker.Check(context.Background(), "322330", "123456")
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
		if _, _, err := checker.Check(context.Background(), "343050", "1"); err == nil {
			t.Fatalf("response %s was accepted", body)
		}
	}
}

func TestSteamVersionCheckerFallsBackToCachedSteamCMDPublicBuild(t *testing.T) {
	now := time.Date(2026, time.August, 22, 0, 0, 0, 0, time.UTC)
	lookups := 0
	checker := &SteamVersionChecker{
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"response":{"success":true,"up_to_date":false,"version_is_listable":false}}`)),
				Header:     make(http.Header),
			}, nil
		})},
		resolveAppInfo: func(context.Context, string) (string, error) {
			lookups++
			return "24700372", nil
		},
		now: func() time.Time { return now }, cacheTTL: 5 * time.Minute,
		cache: make(map[string]steamBuildCacheEntry),
	}
	version, current, err := checker.Check(context.Background(), "343050", "24700371")
	if err != nil || version != "24700372" || current {
		t.Fatalf("fallback check = %q, %t, %v", version, current, err)
	}
	version, current, err = checker.Check(context.Background(), "343050", "24700372")
	if err != nil || version != "24700372" || !current || lookups != 1 {
		t.Fatalf("cached check = %q, %t, %v, lookups=%d", version, current, err, lookups)
	}
}

func TestParseSteamCMDPublicBuildIgnoresOtherBranches(t *testing.T) {
	output := []byte(`"branches" { "beforemacoschanges" { "buildid" "12576213" } "public" { "buildid" "24700372" } "updatebeta" { "buildid" "23604590" } }`)
	version, err := parseSteamCMDPublicBuild(output)
	if err != nil || version != "24700372" {
		t.Fatalf("public build = %q, %v", version, err)
	}
}
