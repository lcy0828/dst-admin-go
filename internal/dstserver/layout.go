package dstserver

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	Binary                  = "dontstarve_dedicated_server_nullrenderer"
	BinaryX64               = "dontstarve_dedicated_server_nullrenderer_x64"
	LayoutUnix              = "unix"
	LayoutMac               = "macos-app"
	AppIDDedicatedServer    = "343050"
	AppIDGame               = "322330"
	UpdateMethodSteamCMD    = "steamcmd"
	UpdateMethodSteamClient = "steam-client"
)

type Layout struct {
	Executable       string
	WorkingDirectory string
	InstallRoot      string
	ContentRoot      string
	Kind             string
	AppID            string
	UpdateMethod     string
	UpdateSupported  bool
}

func Resolve(configuredPath, mode string) (Layout, bool) {
	configuredPath = filepath.Clean(strings.TrimSpace(configuredPath))
	if configuredPath == "" || configuredPath == "." {
		return Layout{}, false
	}
	if executable(configuredPath) {
		return layoutForExecutable(configuredPath), true
	}

	for _, candidate := range candidates(configuredPath, mode) {
		if executable(candidate) {
			return layoutForExecutable(candidate), true
		}
	}
	return Layout{}, false
}

func candidates(root, mode string) []string {
	names := []string{BinaryX64, Binary}
	binDirectories := []string{"bin64", "bin"}
	if strings.TrimSpace(mode) == "32" {
		names = []string{Binary, BinaryX64}
		binDirectories = []string{"bin", "bin64"}
	}

	values := make([]string, 0, 16)
	for _, name := range names {
		values = append(values, filepath.Join(root, name))
	}
	for _, directory := range binDirectories {
		for _, name := range names {
			values = append(values, filepath.Join(root, directory, name))
		}
	}

	appRoots := []string{
		root,
		filepath.Join(root, "dontstarve_steam.app"),
		filepath.Join(root, "dontstarve_dedicated_server_nullrenderer.app"),
	}
	for _, appRoot := range appRoots {
		for _, name := range names {
			values = append(values, filepath.Join(appRoot, "Contents", "MacOS", name))
		}
	}
	return uniquePaths(values)
}

func layoutForExecutable(executablePath string) Layout {
	executablePath = filepath.Clean(executablePath)
	layout := Layout{
		Executable:       executablePath,
		WorkingDirectory: filepath.Dir(executablePath),
		InstallRoot:      filepath.Dir(executablePath),
		ContentRoot:      filepath.Dir(executablePath),
		Kind:             LayoutUnix,
		AppID:            AppIDDedicatedServer,
		UpdateMethod:     UpdateMethodSteamCMD,
		UpdateSupported:  true,
	}
	if appRoot := enclosingApp(executablePath); appRoot != "" {
		layout.InstallRoot = filepath.Dir(appRoot)
		layout.ContentRoot = filepath.Join(appRoot, "Contents")
		layout.Kind = LayoutMac
		if strings.EqualFold(filepath.Base(appRoot), "dontstarve_steam.app") {
			layout.AppID = AppIDGame
			layout.UpdateMethod = UpdateMethodSteamClient
			layout.UpdateSupported = false
		}
		return layout
	}
	base := strings.ToLower(filepath.Base(layout.WorkingDirectory))
	if base == "bin" || base == "bin64" {
		layout.InstallRoot = filepath.Dir(layout.WorkingDirectory)
		layout.ContentRoot = layout.InstallRoot
	}
	return layout
}

func enclosingApp(value string) string {
	for current := filepath.Clean(value); ; current = filepath.Dir(current) {
		if strings.HasSuffix(strings.ToLower(filepath.Base(current)), ".app") {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
	}
}

func executable(value string) bool {
	info, err := os.Stat(value)
	if err != nil || info.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0111 != 0
}

func uniquePaths(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = filepath.Clean(value)
		if seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func SteamClientLibraryDirectory(layout Layout) string {
	if layout.Kind != LayoutMac {
		return ""
	}
	candidates := []string{}
	if configured := strings.TrimSpace(os.Getenv("DST_ADMIN_STEAM_CLIENT_LIBRARY_PATH")); configured != "" {
		candidates = append(candidates, configured)
	}
	steamRoot := filepath.Dir(filepath.Dir(filepath.Dir(layout.InstallRoot)))
	candidates = append(candidates, filepath.Join(steamRoot, "Steam.AppBundle", "Steam", "Contents", "MacOS"))
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "Library", "Application Support", "Steam", "Steam.AppBundle", "Steam", "Contents", "MacOS"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(filepath.Join(candidate, "steamclient.dylib")); err == nil && !info.IsDir() {
			return filepath.Clean(candidate)
		}
	}
	return ""
}
