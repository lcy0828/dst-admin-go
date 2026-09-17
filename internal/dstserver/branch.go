package dstserver

import (
	"os"
	"strings"

	"dont/internal/steamvdf"
)

// SteamBranch reads the installed channel, not the latest branch in appinfo.
func SteamBranch(manifests ...string) string {
	for _, path := range manifests {
		info, err := os.Stat(path)
		if err != nil || info.Size() > 1024*1024 {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		root, err := steamvdf.Parse(data)
		if err != nil {
			continue
		}
		app, ok := root["AppState"].(map[string]interface{})
		if !ok {
			continue
		}
		config, _ := app["UserConfig"].(map[string]interface{})
		for key, value := range config {
			if strings.EqualFold(key, "betakey") {
				branch, _ := value.(string)
				if branch = strings.TrimSpace(branch); branch != "" {
					return branch
				}
			}
		}
		return "public"
	}
	return ""
}
