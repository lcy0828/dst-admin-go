package moddistribution

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dont/internal/steamvdf"
)

const maxObservedWorkshopManifestBytes = int64(8 * 1024 * 1024)

var (
	observedModNamePattern    = regexp.MustCompile(`(?m)^[\t ]*name[\t ]*=[\t ]*["']([^"'\r\n]{1,512})["']`)
	observedModVersionPattern = regexp.MustCompile(`(?m)^[\t ]*version[\t ]*=[\t ]*["']([^"'\r\n]{1,256})["']`)
)

type observedWorkshopItem struct {
	manifestID string
	updatedAt  *time.Time
	size       int64
}

func observeModIdentity(path string, size int64) (string, string, string) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", "mod_version_unreadable"
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, size+1))
	if err != nil || int64(len(data)) != size {
		return "", "", "mod_version_unreadable"
	}
	name := observedModString(data, observedModNamePattern, 512)
	match := observedModVersionPattern.FindSubmatch(data)
	if len(match) != 2 {
		return name, "", "mod_version_unavailable"
	}
	version := strings.TrimSpace(string(match[1]))
	if version == "" || !utf8.ValidString(version) || utf8.RuneCountInString(version) > 128 || strings.ContainsAny(version, "\x00\r\n") {
		return name, "", "mod_version_invalid"
	}
	return name, version, ""
}

func observedModString(data []byte, pattern *regexp.Regexp, maxRunes int) string {
	match := pattern.FindSubmatch(data)
	if len(match) != 2 {
		return ""
	}
	value := strings.TrimSpace(string(match[1]))
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes || strings.ContainsAny(value, "\x00\r\n") {
		return ""
	}
	return value
}

func observeWorkshopItems(installation TrustedInstallation) (map[string]observedWorkshopItem, string) {
	path := strings.TrimSpace(installation.WorkshopManifestPath)
	if path == "" && installation.WorkshopContentPath != "" {
		derived, err := deriveWorkshopManifestPath(installation.WorkshopContentPath)
		if err != nil {
			return map[string]observedWorkshopItem{}, "workshop_manifest_unavailable"
		}
		path = derived
	}
	if path == "" {
		return map[string]observedWorkshopItem{}, "workshop_manifest_unavailable"
	}
	if err := rejectSymlinkComponents(filepath.Dir(path)); err != nil {
		return map[string]observedWorkshopItem{}, "workshop_manifest_unsafe"
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return map[string]observedWorkshopItem{}, "workshop_manifest_missing"
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxObservedWorkshopManifestBytes {
		return map[string]observedWorkshopItem{}, "workshop_manifest_invalid"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]observedWorkshopItem{}, "workshop_manifest_unreadable"
	}
	root, err := steamvdf.Parse(data)
	if err != nil {
		return map[string]observedWorkshopItem{}, "workshop_manifest_invalid"
	}
	app, _ := root["AppWorkshop"].(map[string]interface{})
	installed, _ := app["WorkshopItemsInstalled"].(map[string]interface{})
	result := make(map[string]observedWorkshopItem, len(installed))
	for workshopID, raw := range installed {
		if !validWorkshopID(workshopID) {
			continue
		}
		fields, _ := raw.(map[string]interface{})
		manifestID, _ := fields["manifest"].(string)
		manifestID = strings.TrimSpace(manifestID)
		item := observedWorkshopItem{manifestID: manifestID}
		size, sizeErr := strconv.ParseInt(strings.TrimSpace(stringField(fields, "size")), 10, 64)
		if sizeErr == nil && size > 0 {
			item.size = size
		}
		stamp, _ := fields["timeupdated"].(string)
		seconds, parseErr := strconv.ParseInt(strings.TrimSpace(stamp), 10, 64)
		if parseErr == nil && seconds > 0 {
			updatedAt := time.Unix(seconds, 0).UTC()
			item.updatedAt = &updatedAt
		}
		result[workshopID] = item
	}
	return result, ""
}
