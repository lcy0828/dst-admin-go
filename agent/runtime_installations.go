package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/go-ini/ini"
)

var (
	agentIdentityPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	runtimeInstallationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// RuntimeInstallation is configured on the Agent host. Controllers refer to
// it by ID and cannot override these trusted paths in an operation request.
type RuntimeInstallation struct {
	ID                  string
	Driver              string
	SavePath            string
	ServerPath          string
	SteamCMDPath        string
	UGCPath             string
	WorkshopContentPath string
	ModCachePath        string
	ModStatePath        string
	ServerMode          string
	ContainerEngine     string
	ConsoleSocket       string
	ConsoleSession      string
}

func runtimeInstallationReports(values []RuntimeInstallation) []map[string]string {
	result := make([]map[string]string, 0, len(values))
	for _, value := range values {
		result = append(result, map[string]string{
			"id":                    value.ID,
			"driver":                value.Driver,
			"save_path":             value.SavePath,
			"server_path":           value.ServerPath,
			"steamcmd_path":         value.SteamCMDPath,
			"ugc_path":              value.UGCPath,
			"workshop_content_path": value.WorkshopContentPath,
			"server_mode":           value.ServerMode,
		})
	}
	return result
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
			ID: installationID, Driver: section.Key("DRIVER").String(), SavePath: section.Key("SAVE_PATH").String(),
			ServerPath: section.Key("SERVER_PATH").String(), UGCPath: section.Key("UGC_PATH").String(),
			SteamCMDPath:        section.Key("STEAMCMD_PATH").String(),
			WorkshopContentPath: section.Key("WORKSHOP_CONTENT_PATH").String(), ModCachePath: section.Key("MOD_CACHE_PATH").String(),
			ModStatePath: section.Key("MOD_STATE_PATH").String(),
			ServerMode:   section.Key("SERVER_MODE").String(), ContainerEngine: section.Key("CONTAINER_ENGINE").String(),
			ConsoleSocket: section.Key("CONSOLE_SOCKET").String(), ConsoleSession: section.Key("CONSOLE_SESSION").String(),
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
		value.Driver = strings.ToLower(strings.TrimSpace(value.Driver))
		if value.Driver == "" {
			value.Driver = "native"
		}
		value.SavePath = filepath.Clean(strings.TrimSpace(value.SavePath))
		value.ServerPath = filepath.Clean(strings.TrimSpace(value.ServerPath))
		value.SteamCMDPath = strings.TrimSpace(value.SteamCMDPath)
		if value.SteamCMDPath != "" {
			value.SteamCMDPath = filepath.Clean(value.SteamCMDPath)
		}
		value.UGCPath = strings.TrimSpace(value.UGCPath)
		if value.UGCPath != "" {
			value.UGCPath = filepath.Clean(value.UGCPath)
		}
		value.WorkshopContentPath = strings.TrimSpace(value.WorkshopContentPath)
		if value.WorkshopContentPath == "" {
			value.WorkshopContentPath = value.UGCPath
			if value.WorkshopContentPath == "" {
				value.WorkshopContentPath = filepath.Join(value.ServerPath, "ugc_mods", "content", "322330")
			}
		}
		value.WorkshopContentPath = filepath.Clean(value.WorkshopContentPath)
		value.ModCachePath = strings.TrimSpace(value.ModCachePath)
		if value.ModCachePath == "" {
			value.ModCachePath = filepath.Join(value.ServerPath, ".dst-admin", "mod-cache")
		}
		value.ModCachePath = filepath.Clean(value.ModCachePath)
		value.ModStatePath = strings.TrimSpace(value.ModStatePath)
		if value.ModStatePath == "" {
			value.ModStatePath = filepath.Join(value.ServerPath, ".dst-admin", "mod-state")
		}
		value.ModStatePath = filepath.Clean(value.ModStatePath)
		value.ServerMode = strings.TrimSpace(value.ServerMode)
		if value.ServerMode == "" {
			value.ServerMode = "64"
		}
		value.ContainerEngine = strings.TrimSpace(value.ContainerEngine)
		value.ConsoleSocket = filepath.Clean(strings.TrimSpace(value.ConsoleSocket))
		value.ConsoleSession = strings.TrimSpace(value.ConsoleSession)
		if value.Driver == "container" {
			if value.ContainerEngine == "" {
				value.ContainerEngine = "docker"
			}
			if value.ConsoleSocket == "." {
				value.ConsoleSocket = "/run/dst-admin/tmux/tmux.sock"
			}
			if value.ConsoleSession == "" {
				value.ConsoleSession = "dst"
			}
		} else if value.ConsoleSocket == "." {
			value.ConsoleSocket = ""
		}
		if !runtimeInstallationID.MatchString(value.ID) || seen[value.ID] {
			return nil, fmt.Errorf("DST 安装 ID 无效或重复: %s", value.ID)
		}
		if value.Driver != "native" && value.Driver != "container" {
			return nil, fmt.Errorf("DST 安装 %s 的 DRIVER 必须为 native 或 container", value.ID)
		}
		if !trustedAbsolutePath(value.SavePath) || !trustedAbsolutePath(value.ServerPath) ||
			(value.SteamCMDPath != "" && !trustedAbsolutePath(value.SteamCMDPath)) ||
			(value.UGCPath != "" && !trustedAbsolutePath(value.UGCPath)) || !trustedAbsolutePath(value.WorkshopContentPath) ||
			!trustedAbsolutePath(value.ModCachePath) || !trustedAbsolutePath(value.ModStatePath) ||
			strings.ContainsAny(value.SavePath+value.ServerPath+value.SteamCMDPath+value.UGCPath+value.WorkshopContentPath+value.ModCachePath+value.ModStatePath+value.ConsoleSocket, "\x00\r\n") {
			return nil, fmt.Errorf("DST 安装 %s 包含无效路径", value.ID)
		}
		if pathsOverlap(value.ModCachePath, value.ModStatePath) {
			return nil, fmt.Errorf("DST 安装 %s 的 MOD_CACHE_PATH 与 MOD_STATE_PATH 不能重叠", value.ID)
		}
		if value.ServerMode != "32" && value.ServerMode != "64" {
			return nil, fmt.Errorf("DST 安装 %s 的 SERVER_MODE 必须为 32 或 64", value.ID)
		}
		if value.ConsoleSocket != "" && !trustedAbsolutePath(value.ConsoleSocket) {
			return nil, fmt.Errorf("DST 安装 %s 的 CONSOLE_SOCKET 必须是绝对路径", value.ID)
		}
		if value.Driver == "container" && (!trustedAbsolutePath(value.ConsoleSocket) ||
			(value.ContainerEngine != "docker" && value.ContainerEngine != "podman") ||
			!runtimeInstallationID.MatchString(value.ConsoleSession)) {
			return nil, fmt.Errorf("DST 安装 %s 的容器 Runtime 配置无效", value.ID)
		}
		seen[value.ID] = true
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func configureNativeConsoleSockets(values []RuntimeInstallation, stateFile string) ([]RuntimeInstallation, error) {
	stateFile = filepath.Clean(strings.TrimSpace(stateFile))
	if !trustedAbsolutePath(stateFile) {
		absolute, err := filepath.Abs(stateFile)
		if err != nil {
			return nil, err
		}
		stateFile = absolute
	}
	directory := filepath.Join(filepath.Dir(stateFile), "tmux")
	if runtime.GOOS == "darwin" {
		digest := sha256.Sum256([]byte(filepath.Dir(stateFile)))
		directory = filepath.Join(os.TempDir(), fmt.Sprintf("dst-admin-agent-%d-%s", os.Getuid(), hex.EncodeToString(digest[:6])))
	}
	needsDirectory := false
	for index := range values {
		if values[index].Driver != "native" || values[index].ConsoleSocket != "" {
			continue
		}
		digest := sha256.Sum256([]byte(values[index].ID))
		values[index].ConsoleSocket = filepath.Join(directory, "runtime-"+hex.EncodeToString(digest[:8])+".sock")
		needsDirectory = true
	}
	if needsDirectory {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("创建 Agent tmux socket 目录: %w", err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, fmt.Errorf("收紧 Agent tmux socket 目录权限: %w", err)
		}
	}
	return values, nil
}

func pathsOverlap(first, second string) bool {
	return pathWithinRoot(first, second) || pathWithinRoot(second, first)
}

func pathWithinRoot(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func trustedAbsolutePath(value string) bool {
	if runtime.GOOS == "windows" {
		return filepath.IsAbs(value) || regexp.MustCompile(`(?i)^(?:[a-z]:[\\/]|\\\\)`).MatchString(value)
	}
	return filepath.IsAbs(value)
}

func (a *Agent) runtimeInstallation(id string) (RuntimeInstallation, bool) {
	if a == nil || a.Config == nil {
		return RuntimeInstallation{}, false
	}
	for _, installation := range a.Config.RuntimeInstallations {
		if installation.ID == id {
			return installation, true
		}
	}
	return RuntimeInstallation{}, false
}
