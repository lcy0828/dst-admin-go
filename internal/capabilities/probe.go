package capabilities

import (
	"os"
	"os/exec"
	"runtime"
	"strings"

	dstinstall "dont/internal/dstserver"
)

type Tool struct {
	Available bool   `json:"available"`
	Path      string `json:"path,omitempty"`
}

type Path struct {
	Configured bool   `json:"configured"`
	Exists     bool   `json:"exists"`
	Writable   bool   `json:"writable"`
	Value      string `json:"value,omitempty"`
}

type Report struct {
	Platform string          `json:"platform"`
	Arch     string          `json:"arch"`
	Tools    map[string]Tool `json:"tools"`
	Paths    map[string]Path `json:"paths"`
	Features map[string]bool `json:"features"`
}

type Config struct {
	SavePath        string
	BackupPath      string
	ServerPath      string
	ServerMode      string
	SteamCMDPath    string
	LuaFallbackPath string
	MapRendererPath string
	MapPath         string
}

func Probe(config Config) Report {
	tmux := findTool("tmux", "")
	lua := findTool("lua", config.LuaFallbackPath)
	docker := findTool("docker", "")
	steamcmd := findTool("steamcmd", config.SteamCMDPath)
	mapRenderer := findTool("dst-map-renderer", config.MapRendererPath)
	_, serverAvailable := dstinstall.Resolve(config.ServerPath, config.ServerMode)
	paths := map[string]Path{
		"saves":   inspectPath(config.SavePath),
		"backups": inspectPath(config.BackupPath),
		"server":  inspectPath(config.ServerPath),
		"maps":    inspectPath(config.MapPath),
	}
	return Report{
		Platform: runtime.GOOS,
		Arch:     runtime.GOARCH,
		Tools: map[string]Tool{
			"tmux": tmux, "luaFallback": lua, "docker": docker, "steamcmd": steamcmd, "mapRenderer": mapRenderer,
		},
		Paths: paths,
		Features: map[string]bool{
			"embeddedLuaParser":   true,
			"externalLuaFallback": lua.Available,
			"localShardControl":   tmux.Available && serverAvailable,
			"backupRestore":       paths["saves"].Exists && paths["backups"].Configured,
			"dockerControl":       docker.Available,
			"agentControl":        true,
			"mapGeneration":       mapRenderer.Available && paths["maps"].Configured,
		},
	}
}

func findTool(name, configuredPath string) Tool {
	configuredPath = strings.TrimSpace(configuredPath)
	if configuredPath != "" {
		info, err := os.Stat(configuredPath)
		if err == nil {
			if info.IsDir() {
				candidate := configuredPath + string(os.PathSeparator) + name
				if executable(candidate) {
					return Tool{Available: true, Path: candidate}
				}
			} else if executable(configuredPath) {
				return Tool{Available: true, Path: configuredPath}
			}
		}
	}
	path, err := exec.LookPath(name)
	return Tool{Available: err == nil, Path: path}
}

func inspectPath(value string) Path {
	value = strings.TrimSpace(value)
	result := Path{Configured: value != "", Value: value}
	if value == "" {
		return result
	}
	info, err := os.Stat(value)
	if err != nil {
		if os.IsNotExist(err) {
			result.Writable = parentWritable(value)
		}
		return result
	}
	result.Exists = true
	if info.IsDir() {
		result.Writable = writableBits(info.Mode())
		return result
	}
	result.Writable = writableBits(info.Mode())
	return result
}

func parentWritable(value string) bool {
	for value != "" && value != string(os.PathSeparator) {
		value = parent(value)
		if info, err := os.Stat(value); err == nil && info.IsDir() {
			return writableBits(info.Mode())
		}
	}
	return false
}

func parent(value string) string {
	last := strings.LastIndex(strings.TrimRight(value, string(os.PathSeparator)), string(os.PathSeparator))
	if last <= 0 {
		return string(os.PathSeparator)
	}
	return value[:last]
}

func executable(value string) bool {
	info, err := os.Stat(value)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0111 != 0
}

func writableBits(mode os.FileMode) bool {
	return mode.Perm()&0222 != 0
}
