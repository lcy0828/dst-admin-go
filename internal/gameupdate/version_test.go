package gameupdate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dstinstall "dont/internal/dstserver"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestReadLocalVersionKeepsSteamBuildSeparateFromGameVersion(t *testing.T) {
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
	if value, ok := readLocalVersion(root); !ok || value != "987654" {
		t.Fatalf("Steam build = %q, %t", value, ok)
	}
	if value, ok := readLocalGameVersion(root); !ok || value != "123456" {
		t.Fatalf("game version = %q, %t", value, ok)
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

func TestReadLocalGameVersionFromEmbeddedExecutable(t *testing.T) {
	gameRoot := filepath.Join(t.TempDir(), "Don't Starve Together")
	executablePath := filepath.Join(gameRoot, "dontstarve_steam.app", "Contents", "MacOS", dstinstall.Binary)
	if err := os.MkdirAll(filepath.Dir(executablePath), 0750); err != nil {
		t.Fatal(err)
	}
	binary := []byte("noise\x00747000\x00PRODUCTION\x00DontStarveTogether\x00SERVER\x00%s %s@r%s cfg:%s\x00747465\x009921\x00release\x00")
	if err := os.WriteFile(executablePath, binary, 0750); err != nil {
		t.Fatal(err)
	}
	if version, ok := readLocalGameVersion(gameRoot); !ok || version != "747465" {
		t.Fatalf("embedded game version = %q, %t", version, ok)
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

func TestSteamVersionCheckerCachesSuccessfulAPILookup(t *testing.T) {
	var requests atomic.Int32
	checker := &SteamVersionChecker{
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"response":{"success":true,"up_to_date":false,"required_version":"24700692"}}`)),
				Header:     make(http.Header),
			}, nil
		})},
		now:   func() time.Time { return time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC) },
		cache: make(map[string]steamBuildCacheEntry),
	}
	for index := 0; index < 2; index++ {
		version, current, err := checker.Check(context.Background(), "322330", "24700691")
		if err != nil || version != "24700692" || current {
			t.Fatalf("lookup %d = %q, %t, %v", index, version, current, err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("Steam API requests = %d, want 1", requests.Load())
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

func TestSteamVersionCheckerCoalescesConcurrentSteamCMDLookups(t *testing.T) {
	var lookups atomic.Int32
	checker := &SteamVersionChecker{
		resolveAppInfo: func(context.Context, string) (string, error) {
			lookups.Add(1)
			time.Sleep(25 * time.Millisecond)
			return "24700372", nil
		},
		cache: make(map[string]steamBuildCacheEntry), inflight: make(map[string]*steamBuildLookup),
	}
	results := make(chan error, 12)
	for index := 0; index < cap(results); index++ {
		go func() {
			version, err := checker.latestFromAppInfo(context.Background(), "343050")
			if err == nil && version != "24700372" {
				err = errors.New("unexpected Steam build " + version)
			}
			results <- err
		}()
	}
	for index := 0; index < cap(results); index++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if actual := lookups.Load(); actual != 1 {
		t.Fatalf("SteamCMD lookups=%d, want 1", actual)
	}
}

func TestSteamVersionCheckerCachesFallbackFailuresBriefly(t *testing.T) {
	now := time.Date(2026, time.August, 27, 0, 0, 0, 0, time.UTC)
	var lookups atomic.Int32
	wantErr := errors.New("Steam is unavailable")
	checker := &SteamVersionChecker{
		resolveAppInfo: func(context.Context, string) (string, error) {
			lookups.Add(1)
			return "", wantErr
		},
		now: func() time.Time { return now }, failureTTL: 30 * time.Second,
		cache: make(map[string]steamBuildCacheEntry),
	}
	for index := 0; index < 2; index++ {
		if _, err := checker.latestFromAppInfo(context.Background(), "343050"); !errors.Is(err, wantErr) {
			t.Fatalf("fallback error=%v, want %v", err, wantErr)
		}
	}
	if actual := lookups.Load(); actual != 1 {
		t.Fatalf("SteamCMD lookups=%d, want 1", actual)
	}
	now = now.Add(31 * time.Second)
	_, _ = checker.latestFromAppInfo(context.Background(), "343050")
	if actual := lookups.Load(); actual != 2 {
		t.Fatalf("SteamCMD lookups after failure expiry=%d, want 2", actual)
	}
}

func TestSteamVersionCheckerBoundsFallbackLookup(t *testing.T) {
	checker := &SteamVersionChecker{
		resolveAppInfo: func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
		lookupTimeout: 10 * time.Millisecond,
		cache:         make(map[string]steamBuildCacheEntry),
	}
	startedAt := time.Now()
	if _, err := checker.latestFromAppInfo(context.Background(), "343050"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fallback error=%v, want deadline exceeded", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("bounded fallback took %s", elapsed)
	}
}

func TestResolveSteamCMDPublicBuildQueriesOnlineEvenWithRecentUnrelatedAppCache(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "steamcmd")
	script := `#!/bin/sh
if [ "$1" != "+login" ]; then
  exit 9
fi
printf '%s\n' '"343050" { "depots" { "branches" { "public" { "buildid" "24700372" } } } }'
`
	if err := os.WriteFile(executable, []byte(script), 0750); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "appcache", "appinfo.vdf")
	if err := os.MkdirAll(filepath.Dir(cache), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte("cached"), 0640); err != nil {
		t.Fatal(err)
	}
	version, err := resolveSteamCMDPublicBuild(context.Background(), executable, "343050")
	if err != nil || version != "24700372" {
		t.Fatalf("cached public build=%q, %v", version, err)
	}
}

func TestSteamCMDVersionTimeoutDoesNotWaitForWrapperChild(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "steamcmd")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nsleep 10 &\nwait\n"), 0750); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := resolveSteamCMDPublicBuild(ctx, executable, "343050")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatalf("timeout duration=%s err=%v", time.Since(started), err)
	}
}

func TestDedicatedServerBuildCheckSkipsUnusableUpToDateAPI(t *testing.T) {
	checker := &SteamVersionChecker{
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unnecessary HTTP version lookup")
			return nil, nil
		})},
		resolveAppInfo: func(context.Context, string) (string, error) { return "24700372", nil },
	}
	build, current, err := checker.Check(context.Background(), "343050", "24700372")
	if err != nil || !current || build != "24700372" {
		t.Fatalf("build=%s current=%t err=%v", build, current, err)
	}
}

func TestParseSteamCMDPublicBuildIgnoresOtherBranches(t *testing.T) {
	output := []byte(`"branches" { "beforemacoschanges" { "buildid" "12576213" } "public" { "buildid" "24700372" } "updatebeta" { "buildid" "23604590" } }`)
	version, err := parseSteamCMDPublicBuild(output)
	if err != nil || version != "24700372" {
		t.Fatalf("public build = %q, %v", version, err)
	}
}
