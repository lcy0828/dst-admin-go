package gameupdate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

var buildIDPattern = regexp.MustCompile(`(?m)"buildid"\s+"([0-9]+)"`)
var publicBranchBuildIDPattern = regexp.MustCompile(`(?s)"branches"\s*\{.*?"public"\s*\{.*?"buildid"\s*"([0-9]+)"`)

type LatestChecker interface {
	Check(context.Context, string, string) (string, bool, error)
}

type steamBuildCacheEntry struct {
	version   string
	expiresAt time.Time
}

type SteamVersionChecker struct {
	client         *http.Client
	resolveAppInfo func(context.Context, string) (string, error)
	now            func() time.Time
	cacheTTL       time.Duration
	mu             sync.Mutex
	cache          map[string]steamBuildCacheEntry
}

func NewSteamVersionChecker(steamCMDPaths ...string) *SteamVersionChecker {
	configuredPath := ""
	if len(steamCMDPaths) > 0 {
		configuredPath = steamCMDPaths[0]
	}
	checker := &SteamVersionChecker{
		client: &http.Client{Timeout: 8 * time.Second}, now: time.Now, cacheTTL: 5 * time.Minute,
		cache: make(map[string]steamBuildCacheEntry),
	}
	checker.resolveAppInfo = func(ctx context.Context, appID string) (string, error) {
		return resolveSteamCMDPublicBuild(ctx, configuredPath, appID)
	}
	return checker
}

func (c *SteamVersionChecker) Check(ctx context.Context, appID, localVersion string) (string, bool, error) {
	required, upToDate, apiErr := c.checkSteamAPI(ctx, appID, localVersion)
	if apiErr == nil {
		if required != "" {
			return required, upToDate, nil
		}
		localVersion = strings.TrimSpace(localVersion)
		if upToDate && releaseVersionPattern.MatchString(localVersion) && localVersion != "0" {
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
	if c.now == nil {
		c.now = time.Now
	}
	if c.cacheTTL <= 0 {
		c.cacheTTL = 5 * time.Minute
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if cached, ok := c.cache[appID]; ok && now.Before(cached.expiresAt) {
		return cached.version, nil
	}
	version, err := c.resolveAppInfo(ctx, appID)
	if err != nil {
		return "", err
	}
	version = strings.TrimSpace(version)
	if !releaseVersionPattern.MatchString(version) {
		return "", errors.New("SteamCMD returned an invalid public build")
	}
	if c.cache == nil {
		c.cache = make(map[string]steamBuildCacheEntry)
	}
	c.cache[appID] = steamBuildCacheEntry{version: version, expiresAt: now.Add(c.cacheTTL)}
	return version, nil
}

func resolveSteamCMDPublicBuild(ctx context.Context, configuredPath, appID string) (string, error) {
	executable := findSteamCMD(configuredPath)
	if executable == "" {
		return "", errors.New("SteamCMD is unavailable for the latest build lookup")
	}
	commandContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(
		commandContext, executable,
		"+login", "anonymous", "+app_info_update", "1", "+app_info_print", appID, "+quit",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("SteamCMD app info lookup failed: %w", err)
	}
	return parseSteamCMDPublicBuild(output)
}

func parseSteamCMDPublicBuild(output []byte) (string, error) {
	match := publicBranchBuildIDPattern.FindSubmatch(output)
	if len(match) != 2 {
		return "", errors.New("SteamCMD app info omitted the public build")
	}
	return string(match[1]), nil
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
	candidates := []string{
		filepath.Join(root, "version.txt"),
		filepath.Join(serverPath, "version.txt"),
	}
	appIDs := uniqueAppIDs(requestedAppIDs)
	if len(appIDs) == 0 {
		if layout, ok := dstinstall.Resolve(serverPath, "64"); ok {
			appIDs = []string{layout.AppID}
		} else {
			appIDs = []string{dstinstall.AppIDDedicatedServer}
		}
	}
	candidates = append(candidates, steamManifestCandidates(root, appIDs...)...)
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
