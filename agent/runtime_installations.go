package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/go-ini/ini"
)

var runtimeInstallationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// RuntimeInstallation is configured on the Agent host. Controllers refer to
// it by ID and cannot override these trusted paths in an operation request.
type RuntimeInstallation struct {
	ID         string
	SavePath   string
	ServerPath string
	UGCPath    string
	ServerMode string
}

func loadRuntimeInstallations(configPath string) ([]RuntimeInstallation, error) {
	if !isINIConfigPath(configPath) {
		return nil, nil
	}
	if _, err := os.Stat(configPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	config, err := ini.Load(configPath)
	if err != nil {
		return nil, err
	}
	values := make([]RuntimeInstallation, 0)
	for _, sectionName := range config.SectionStrings() {
		installationID, ok := installationIDFromSection(sectionName)
		if !ok {
			continue
		}
		section := config.Section(sectionName)
		if explicitID := strings.TrimSpace(section.Key("INSTALLATION_ID").String()); explicitID != "" {
			installationID = explicitID
		}
		values = append(values, RuntimeInstallation{
			ID: installationID, SavePath: section.Key("SAVE_PATH").String(),
			ServerPath: section.Key("SERVER_PATH").String(), UGCPath: section.Key("UGC_PATH").String(),
			ServerMode: section.Key("SERVER_MODE").String(),
		})
	}
	return normalizeRuntimeInstallations(values)
}

func installationIDFromSection(section string) (string, bool) {
	section = strings.TrimSpace(section)
	if strings.EqualFold(section, "runtime") {
		return "default", true
	}
	lower := strings.ToLower(section)
	for _, prefix := range []string{"runtime.", "runtime:", "runtime "} {
		if strings.HasPrefix(lower, prefix) {
			value := strings.Trim(strings.TrimSpace(section[len(prefix):]), `"'`)
			return value, value != ""
		}
	}
	return "", false
}

func normalizeRuntimeInstallations(values []RuntimeInstallation) ([]RuntimeInstallation, error) {
	seen := make(map[string]bool, len(values))
	result := make([]RuntimeInstallation, 0, len(values))
	for _, value := range values {
		value.ID = strings.TrimSpace(value.ID)
		value.SavePath = filepath.Clean(strings.TrimSpace(value.SavePath))
		value.ServerPath = filepath.Clean(strings.TrimSpace(value.ServerPath))
		value.UGCPath = strings.TrimSpace(value.UGCPath)
		if value.UGCPath != "" {
			value.UGCPath = filepath.Clean(value.UGCPath)
		}
		value.ServerMode = strings.TrimSpace(value.ServerMode)
		if value.ServerMode == "" {
			value.ServerMode = "64"
		}
		if !runtimeInstallationID.MatchString(value.ID) || seen[value.ID] {
			return nil, fmt.Errorf("DST 安装 ID 无效或重复: %s", value.ID)
		}
		if !trustedAbsolutePath(value.SavePath) || !trustedAbsolutePath(value.ServerPath) ||
			(value.UGCPath != "" && !trustedAbsolutePath(value.UGCPath)) ||
			strings.ContainsAny(value.SavePath+value.ServerPath+value.UGCPath, "\x00\r\n") {
			return nil, fmt.Errorf("DST 安装 %s 包含无效路径", value.ID)
		}
		if value.ServerMode != "32" && value.ServerMode != "64" {
			return nil, fmt.Errorf("DST 安装 %s 的 SERVER_MODE 必须为 32 或 64", value.ID)
		}
		seen[value.ID] = true
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func trustedAbsolutePath(value string) bool {
	if runtime.GOOS == "windows" {
		return filepath.IsAbs(value) || regexp.MustCompile(`(?i)^(?:[a-z]:[\\/]|\\\\)`).MatchString(value)
	}
	return filepath.IsAbs(value)
}

func (a *Agent) runtimeInstallation(id string) (RuntimeInstallation, bool) {
	for _, installation := range a.Config.RuntimeInstallations {
		if installation.ID == id {
			return installation, true
		}
	}
	return RuntimeInstallation{}, false
}
