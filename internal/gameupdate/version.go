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
	"time"
)

var buildIDPattern = regexp.MustCompile(`(?m)"buildid"\s+"([0-9]+)"`)

type LatestChecker interface {
	Check(context.Context, string) (string, bool, error)
}

type SteamVersionChecker struct{ client *http.Client }

func NewSteamVersionChecker() *SteamVersionChecker {
	return &SteamVersionChecker{client: &http.Client{Timeout: 8 * time.Second}}
}

func (c *SteamVersionChecker) Check(ctx context.Context, localVersion string) (string, bool, error) {
	endpoint := "https://api.steampowered.com/ISteamApps/UpToDateCheck/v1/"
	query := url.Values{"appid": {"343050"}, "version": {localVersion}}
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

func readLocalVersion(serverPath string) (string, bool) {
	root := installRoot(serverPath)
	candidates := []string{
		filepath.Join(root, "version.txt"),
		filepath.Join(serverPath, "version.txt"),
		filepath.Join(root, "steamapps", "appmanifest_343050.acf"),
		filepath.Join(filepath.Dir(root), "steamapps", "appmanifest_343050.acf"),
	}
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

func installRoot(serverPath string) string {
	serverPath = filepath.Clean(strings.TrimSpace(serverPath))
	if serverPath == "." || serverPath == "" {
		return serverPath
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
