package gameupdate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	dstinstall "dont/internal/dstserver"
	"dont/internal/steamvdf"
)

var buildIDPattern = regexp.MustCompile(`(?m)"buildid"\s+"([0-9]+)"`)
var embeddedGameVersionPattern = regexp.MustCompile(`DontStarveTogether\x00SERVER\x00[^\x00]{1,256}\x00([0-9]{1,64})\x00`)

const (
	steamBuildCacheTTL        = 15 * time.Minute
	steamBuildFailureTTL      = 30 * time.Second
	steamBuildLookupTimeout   = 30 * time.Second
	steamOnlineAppInfoTimeout = 30 * time.Second
	maxEmbeddedVersionBinary  = 64 * 1024 * 1024
)

type LatestChecker interface {
	Check(context.Context, string, string) (string, bool, error)
}

type steamBuildCacheEntry struct {
	version   string
	err       error
	expiresAt time.Time
}

type steamBuildLookup struct {
	done    chan struct{}
	version string
	err     error
}

type SteamVersionChecker struct {
	client         *http.Client
	resolveAppInfo func(context.Context, string) (string, error)
	now            func() time.Time
	cacheTTL       time.Duration
	failureTTL     time.Duration
	lookupTimeout  time.Duration
	mu             sync.Mutex
	cache          map[string]steamBuildCacheEntry
	inflight       map[string]*steamBuildLookup
}

func NewSteamVersionChecker(steamCMDPaths ...string) *SteamVersionChecker {
	configuredPath := ""
	if len(steamCMDPaths) > 0 {
		configuredPath = steamCMDPaths[0]
	}
	checker := &SteamVersionChecker{
		client: &http.Client{Timeout: 8 * time.Second}, now: time.Now, cacheTTL: steamBuildCacheTTL,
		failureTTL: steamBuildFailureTTL, lookupTimeout: steamBuildLookupTimeout,
		cache: make(map[string]steamBuildCacheEntry), inflight: make(map[string]*steamBuildLookup),
	}
	checker.resolveAppInfo = func(ctx context.Context, appID string) (string, error) {
		return resolveSteamCMDPublicBuild(ctx, configuredPath, appID)
	}
	return checker
}

func (c *SteamVersionChecker) Check(ctx context.Context, appID, localVersion string) (string, bool, error) {
	if !releaseAppIDPattern.MatchString(appID) {
		return "", false, errors.New("invalid Steam application ID")
	}
	if cached, ok := c.cachedSuccess(appID); ok {
		return cached, strings.TrimSpace(localVersion) == cached, nil
	}
	if appID == dstinstall.AppIDDedicatedServer {
		latest, err := c.latestFromAppInfo(ctx, appID)
		return latest, err == nil && strings.TrimSpace(localVersion) == latest, err
	}
	required, upToDate, apiErr := c.checkSteamAPI(ctx, appID, localVersion)
	if apiErr == nil {
		if required != "" {
			c.rememberSuccess(appID, required)
			return required, upToDate, nil
		}
		localVersion = strings.TrimSpace(localVersion)
		if upToDate && releaseVersionPattern.MatchString(localVersion) && localVersion != "0" {
			c.rememberSuccess(appID, localVersion)
			return localVersion, true, nil
		}
	}
	latest, fallbackErr := c.latestFromAppInfo(ctx, appID)
	if fallbackErr != nil {
		if apiErr != nil {
			return "", false, errors.Join(apiErr, fallbackErr)
		}
		return "", false, fallbackErr
	}
	return latest, strings.TrimSpace(localVersion) == latest, nil
}

func (c *SteamVersionChecker) cachedSuccess(appID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[appID]
	return entry.version, ok && entry.err == nil && c.cacheNow().Before(entry.expiresAt) && releaseVersionPattern.MatchString(entry.version)
}

func (c *SteamVersionChecker) rememberSuccess(appID, version string) {
	version = strings.TrimSpace(version)
	if !releaseVersionPattern.MatchString(version) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = make(map[string]steamBuildCacheEntry)
	}
	ttl := c.cacheTTL
	if ttl <= 0 {
		ttl = steamBuildCacheTTL
	}
	c.cache[appID] = steamBuildCacheEntry{version: version, expiresAt: c.cacheNow().Add(ttl)}
}

func (c *SteamVersionChecker) checkSteamAPI(ctx context.Context, appID, localVersion string) (string, bool, error) {
	endpoint := "https://api.steampowered.com/ISteamApps/UpToDateCheck/v1/"
	query := url.Values{"appid": {appID}, "version": {localVersion}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return "", false, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return "", false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("Steam version API returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Response struct {
			Success         bool            `json:"success"`
			UpToDate        bool            `json:"up_to_date"`
			RequiredVersion json.RawMessage `json:"required_version"`
		} `json:"response"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&payload); err != nil {
		return "", false, err
	}
	if !payload.Response.Success {
		return "", false, errors.New("Steam version API reported an unsuccessful check")
	}
	required, err := parseRequiredVersion(payload.Response.RequiredVersion)
	if err != nil {
		return "", false, err
	}
	return required, payload.Response.UpToDate, nil
}

func (c *SteamVersionChecker) latestFromAppInfo(ctx context.Context, appID string) (string, error) {
	if c.resolveAppInfo == nil {
		return "", errors.New("Steam version API omitted the required build and SteamCMD fallback is unavailable")
	}
	c.mu.Lock()
	now := c.cacheNow()
	if cached, ok := c.cache[appID]; ok && now.Before(cached.expiresAt) {
		c.mu.Unlock()
		return cached.version, cached.err
	}
	if lookup := c.inflight[appID]; lookup != nil {
		c.mu.Unlock()
		select {
		case <-lookup.done:
			return lookup.version, lookup.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	lookup := &steamBuildLookup{done: make(chan struct{})}
	if c.inflight == nil {
		c.inflight = make(map[string]*steamBuildLookup)
	}
	c.inflight[appID] = lookup
	lookupTimeout := c.lookupTimeout
	if lookupTimeout <= 0 {
		lookupTimeout = steamBuildLookupTimeout
	}
	c.mu.Unlock()

	lookupContext, cancel := context.WithTimeout(ctx, lookupTimeout)
	version, err := c.resolveAppInfo(lookupContext, appID)
	if lookupErr := lookupContext.Err(); lookupErr != nil {
		err = lookupErr
	}
	cancel()
	version = strings.TrimSpace(version)
	if err == nil && !releaseVersionPattern.MatchString(version) {
		err = errors.New("SteamCMD returned an invalid public build")
		version = ""
	}

	c.mu.Lock()
	if c.cache == nil {
		c.cache = make(map[string]steamBuildCacheEntry)
	}
	ttl := c.cacheTTL
	if ttl <= 0 {
		ttl = steamBuildCacheTTL
	}
	if err != nil {
		ttl = c.failureTTL
		if ttl <= 0 {
			ttl = steamBuildFailureTTL
		}
	}
	if err == nil || ctx.Err() == nil {
		c.cache[appID] = steamBuildCacheEntry{version: version, err: err, expiresAt: c.cacheNow().Add(ttl)}
	}
	lookup.version, lookup.err = version, err
	delete(c.inflight, appID)
	close(lookup.done)
	c.mu.Unlock()
	return version, err
}

func (c *SteamVersionChecker) cacheNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func resolveSteamCMDPublicBuild(ctx context.Context, configuredPath, appID string) (string, error) {
	executable := findSteamCMD(configuredPath)
	if executable == "" {
		return "", errors.New("SteamCMD is unavailable for the latest build lookup")
	}
	started := time.Now()
	defer func() {
		log.Printf("[GameVersion] stage=steam_appinfo app_id=%s duration_ms=%d", appID, time.Since(started).Milliseconds())
	}()
	commandContext, cancel := context.WithTimeout(ctx, steamOnlineAppInfoTimeout)
	defer cancel()
	command := exec.CommandContext(
		commandContext, executable,
		"+login", "anonymous", "+app_info_update", "1", "+app_info_print", appID, "+quit",
	)
	configureVersionCommand(command)
	output := &boundedBuffer{limit: 4 * 1024 * 1024}
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	if commandContext.Err() != nil {
		return "", commandContext.Err()
	}
	if err != nil {
		return "", fmt.Errorf("SteamCMD app info lookup failed: %w", err)
	}
	return parseSteamCMDPublicBuild([]byte(output.String()), appID)
}

func parseSteamCMDPublicBuild(output []byte, appIDs ...string) (string, error) {
	key := "branches"
	if len(appIDs) > 0 {
		key = appIDs[0]
	}
	start, end := bytes.Index(output, []byte(`"`+key+`"`)), bytes.LastIndexByte(output, '}')
	if start < 0 || end < start {
		return "", errors.New("SteamCMD app info omitted the requested application")
	}
	root, err := steamvdf.Parse(output[start : end+1])
	if err != nil {
		return "", fmt.Errorf("parse SteamCMD app info: %w", err)
	}
	if len(appIDs) > 0 {
		app, _ := root[key].(map[string]interface{})
		root, _ = app["depots"].(map[string]interface{})
	}
	branches, _ := root["branches"].(map[string]interface{})
	public, _ := branches["public"].(map[string]interface{})
	version, _ := public["buildid"].(string)
	if !releaseVersionPattern.MatchString(version) {
		return "", errors.New("SteamCMD app info omitted the public build")
	}
	return version, nil
}

func parseRequiredVersion(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	var value string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", err
		}
	} else {
		value = string(raw)
	}
	value = strings.TrimSpace(value)
	if value == "" || !regexp.MustCompile(`^[0-9]{1,64}$`).MatchString(value) {
		return "", errors.New("Steam version API returned an invalid required version")
	}
	return value, nil
}

func readLocalVersion(serverPath string, requestedAppIDs ...string) (string, bool) {
	root := installRoot(serverPath)
	appIDs := uniqueAppIDs(requestedAppIDs)
	if len(appIDs) == 0 {
		if layout, ok := dstinstall.Resolve(serverPath, "64"); ok {
			appIDs = []string{layout.AppID}
		} else {
			appIDs = []string{dstinstall.AppIDDedicatedServer}
		}
	}
	candidates := steamManifestCandidates(root, appIDs...)
	candidates = append(candidates, filepath.Join(root, "version.txt"), filepath.Join(serverPath, "version.txt"))
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		data, err := os.ReadFile(candidate)
		if err != nil || len(data) > 1024*1024 {
			continue
		}
		if filepath.Base(candidate) == "version.txt" {
			scanner := bufio.NewScanner(strings.NewReader(string(data)))
			if scanner.Scan() {
				version := strings.TrimSpace(scanner.Text())
				if version != "" && len(version) <= 64 {
					return version, true
				}
			}
			continue
		}
		match := buildIDPattern.FindSubmatch(data)
		if len(match) == 2 {
			return string(match[1]), true
		}
	}
	return "", false
}

func readLocalGameVersion(serverPath string) (string, bool) {
	root := installRoot(serverPath)
	seen := make(map[string]bool, 2)
	for _, candidate := range []string{filepath.Join(root, "version.txt"), filepath.Join(serverPath, "version.txt")} {
		candidate = filepath.Clean(candidate)
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		data, err := os.ReadFile(candidate)
		if err != nil || len(data) > 1024*1024 {
			continue
		}
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		if scanner.Scan() {
			version := strings.TrimSpace(scanner.Text())
			if releaseVersionPattern.MatchString(version) {
				return version, true
			}
		}
	}
	if layout, ok := dstinstall.Resolve(serverPath, "64"); ok {
		if version, found := readEmbeddedGameVersion(layout.Executable); found {
			return version, true
		}
	}
	return "", false
}

func readEmbeddedGameVersion(executable string) (string, bool) {
	info, err := os.Stat(executable)
	if err != nil || info.IsDir() || info.Size() <= 0 || info.Size() > maxEmbeddedVersionBinary {
		return "", false
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		return "", false
	}
	match := embeddedGameVersionPattern.FindSubmatch(data)
	if len(match) != 2 {
		return "", false
	}
	version := string(match[1])
	return version, releaseVersionPattern.MatchString(version)
}

func steamManifestCandidates(root string, appIDs ...string) []string {
	values := make([]string, 0, 24)
	for current, depth := filepath.Clean(root), 0; depth < 6; depth++ {
		for _, appID := range uniqueAppIDs(appIDs) {
			name := "appmanifest_" + appID + ".acf"
			values = append(values, filepath.Join(current, name), filepath.Join(current, "steamapps", name))
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return values
}

func uniqueAppIDs(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func installRoot(serverPath string) string {
	serverPath = filepath.Clean(strings.TrimSpace(serverPath))
	if serverPath == "." || serverPath == "" {
		return serverPath
	}
	if layout, ok := dstinstall.Resolve(serverPath, "64"); ok {
		return layout.InstallRoot
	}
	if runtime.GOOS == "darwin" || strings.Contains(serverPath, ".app"+string(os.PathSeparator)) {
		parts := strings.Split(serverPath, string(os.PathSeparator))
		for index := len(parts) - 1; index >= 0; index-- {
			if strings.HasSuffix(strings.ToLower(parts[index]), ".app") {
				appPath := strings.Join(parts[:index+1], string(os.PathSeparator))
				if strings.HasPrefix(serverPath, string(os.PathSeparator)) && !strings.HasPrefix(appPath, string(os.PathSeparator)) {
					appPath = string(os.PathSeparator) + appPath
				}
				return filepath.Dir(appPath)
			}
		}
	}
	base := strings.ToLower(filepath.Base(serverPath))
	if base == "bin" || base == "bin64" {
		return filepath.Dir(serverPath)
	}
	if info, err := os.Stat(serverPath); err == nil && !info.IsDir() {
		return filepath.Dir(serverPath)
	}
	return serverPath
}

func findSteamCMD(configuredPath string) string {
	configuredPath = filepath.Clean(strings.TrimSpace(configuredPath))
	if configuredPath != "" && configuredPath != "." {
		if executableFile(configuredPath) {
			return configuredPath
		}
		for _, name := range []string{"steamcmd.sh", "steamcmd", "steamcmd.exe"} {
			candidate := filepath.Join(configuredPath, name)
			if executableFile(candidate) {
				return candidate
			}
		}
	}
	for _, name := range []string{"steamcmd", "steamcmd.sh", "steamcmd.exe"} {
		if value, err := exec.LookPath(name); err == nil {
			return value
		}
	}
	return ""
}

func executableFile(value string) bool {
	info, err := os.Stat(value)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0111 != 0
}
