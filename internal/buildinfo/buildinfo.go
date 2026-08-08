package buildinfo

import (
	"runtime/debug"
	"strings"
)

var (
	Version   = "dev"
	Commit    = ""
	BuildTime = ""
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
	Dirty     bool   `json:"dirty"`
}

func Current() Info {
	result := Info{Version: clean(Version, "dev"), Commit: clean(Commit, "unknown"), BuildTime: clean(BuildTime, "unknown")}
	if details, ok := debug.ReadBuildInfo(); ok {
		if result.Version == "dev" && details.Main.Version != "" && details.Main.Version != "(devel)" {
			result.Version = details.Main.Version
		}
		for _, setting := range details.Settings {
			switch setting.Key {
			case "vcs.revision":
				if result.Commit == "unknown" {
					result.Commit = clean(setting.Value, "unknown")
				}
			case "vcs.time":
				if result.BuildTime == "unknown" {
					result.BuildTime = clean(setting.Value, "unknown")
				}
			case "vcs.modified":
				result.Dirty = setting.Value == "true"
			}
		}
	}
	return result
}

func clean(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}
