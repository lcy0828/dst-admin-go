package moddistribution

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"dont/internal/steamvdf"
)

func deriveWorkshopManifestPath(contentPath string) (string, error) {
	contentPath = filepath.Clean(contentPath)
	appID := filepath.Base(contentPath)
	contentRoot := filepath.Dir(contentPath)
	if !validWorkshopID(appID) || filepath.Base(contentRoot) != "content" {
		return "", ErrInvalidInput
	}
	workshopRoot := filepath.Dir(contentRoot)
	if _, err := trustedExistingDirectory(workshopRoot); err != nil {
		return "", err
	}
	return filepath.Join(workshopRoot, "appworkshop_"+appID+".acf"), nil
}

func composeWorkshopManifest(current []byte, installation TrustedInstallation, mods []ModVersion) ([]byte, error) {
	if installation.WorkshopManifestPath == "" {
		return nil, nil
	}
	appID := filepath.Base(installation.WorkshopContentPath)
	root := map[string]interface{}{}
	if len(current) > 0 {
		parsed, err := steamvdf.Parse(current)
		if err != nil {
			return nil, fmt.Errorf("parse Steam Workshop manifest: %w", err)
		}
		root = parsed
	}
	app := childObject(root, "AppWorkshop")
	if configured, _ := app["appid"].(string); configured != "" && configured != appID {
		return nil, ErrConflict
	}
	app["appid"] = appID
	setDefaultString(app, "NeedsDownload", "0")
	setDefaultString(app, "NeedsUpdate", "0")
	setDefaultString(app, "TimeLastAppRan", "0")
	setDefaultString(app, "TimeLastUpdated", "0")
	setDefaultString(app, "LastBuildID", "0")
	installed := childObject(app, "WorkshopItemsInstalled")
	details := childObject(app, "WorkshopItemDetails")
	for _, mod := range mods {
		existingFields, alreadyInstalled := installed[mod.WorkshopID].(map[string]interface{})
		if strings.TrimSpace(mod.Metadata.SteamManifestID) == "" {
			if alreadyInstalled {
				continue
			}
			return nil, fmt.Errorf("%w for Workshop %s", ErrWorkshopRegistration, mod.WorkshopID)
		}
		updatedAt := mod.Metadata.SteamUpdatedAt.Unix()
		if updatedAt < 0 {
			updatedAt = 0
		}
		size := mod.Metadata.PublishedFileSize
		if size < 1 {
			size, _ = strconv.ParseInt(stringField(existingFields, "size"), 10, 64)
		}
		if size < 1 {
			return nil, fmt.Errorf("%w for Workshop %s size", ErrWorkshopRegistration, mod.WorkshopID)
		}
		manifestID := mod.Metadata.SteamManifestID
		updated := strconv.FormatInt(updatedAt, 10)
		installed[mod.WorkshopID] = map[string]interface{}{
			"manifest":    manifestID,
			"size":        strconv.FormatInt(size, 10),
			"timeupdated": updated,
		}
		details[mod.WorkshopID] = map[string]interface{}{
			"latest_manifest":    manifestID,
			"latest_timeupdated": updated,
			"manifest":           manifestID,
			"timetouched":        updated,
			"timeupdated":        updated,
		}
	}
	var sizeOnDisk int64
	for _, raw := range installed {
		fields, _ := raw.(map[string]interface{})
		size, _ := strconv.ParseInt(stringField(fields, "size"), 10, 64)
		if size > 0 {
			sizeOnDisk += size
		}
	}
	app["SizeOnDisk"] = strconv.FormatInt(sizeOnDisk, 10)
	encoded, err := steamvdf.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("render Steam Workshop manifest: %w", err)
	}
	return encoded, nil
}

func validateWorkshopManifestInventory(content []byte, expected map[string]string) error {
	if len(expected) == 0 {
		return nil
	}
	root, err := steamvdf.Parse(content)
	if err != nil {
		return fmt.Errorf("%w: parse Steam Workshop manifest: %v", ErrConflict, err)
	}
	app, ok := root["AppWorkshop"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("%w: Steam Workshop manifest has no AppWorkshop inventory", ErrConflict)
	}
	installed, ok := app["WorkshopItemsInstalled"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("%w: Steam Workshop manifest has no installed inventory", ErrConflict)
	}
	missing := make([]string, 0)
	invalid := make([]string, 0)
	for workshopID := range expected {
		fields, registered := installed[workshopID].(map[string]interface{})
		if !registered {
			missing = append(missing, workshopID)
			continue
		}
		size, sizeErr := strconv.ParseInt(stringField(fields, "size"), 10, 64)
		if strings.TrimSpace(stringField(fields, "manifest")) == "" || sizeErr != nil || size < 1 {
			invalid = append(invalid, workshopID)
		}
	}
	sort.Strings(missing)
	sort.Strings(invalid)
	if len(missing) > 0 {
		return fmt.Errorf("%w: Steam Workshop manifest is missing installed items: %s", ErrConflict, strings.Join(missing, ", "))
	}
	if len(invalid) > 0 {
		return fmt.Errorf("%w: Steam Workshop manifest has invalid installed items: %s", ErrConflict, strings.Join(invalid, ", "))
	}
	return nil
}

func childObject(parent map[string]interface{}, key string) map[string]interface{} {
	if current, ok := parent[key].(map[string]interface{}); ok {
		return current
	}
	created := map[string]interface{}{}
	parent[key] = created
	return created
}

func setDefaultString(parent map[string]interface{}, key, value string) {
	if _, exists := parent[key]; !exists {
		parent[key] = value
	}
}

func stringField(parent map[string]interface{}, key string) string {
	value, _ := parent[key].(string)
	return value
}
