package mods

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"dont/internal/steamvdf"
)

const maxACFBytes = int64(8 * 1024 * 1024)

type WorkshopManifestItem struct {
	ManifestID string
	Size       int64
	UpdatedAt  time.Time
}

type workshopManifestItem = WorkshopManifestItem

func ReadWorkshopManifestItem(path, workshopID string) (WorkshopManifestItem, bool, error) {
	items, err := loadWorkshopManifest(path)
	if err != nil {
		return WorkshopManifestItem{}, false, err
	}
	item, exists := items[workshopID]
	return item, exists, nil
}

func loadWorkshopManifest(path string) (map[string]workshopManifestItem, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return map[string]workshopManifestItem{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxACFBytes {
		return nil, errors.New("Steam workshop manifest is unsafe or too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	root, err := steamvdf.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse Steam workshop manifest: %w", err)
	}
	app, _ := root["AppWorkshop"].(map[string]interface{})
	installed, _ := app["WorkshopItemsInstalled"].(map[string]interface{})
	result := make(map[string]workshopManifestItem, len(installed))
	for id, raw := range installed {
		if !validModID(id) {
			continue
		}
		fields, _ := raw.(map[string]interface{})
		item := workshopManifestItem{}
		item.ManifestID, _ = fields["manifest"].(string)
		size, _ := fields["size"].(string)
		item.Size, _ = strconv.ParseInt(size, 10, 64)
		stamp, _ := fields["timeupdated"].(string)
		seconds, parseErr := strconv.ParseInt(stamp, 10, 64)
		if parseErr == nil && seconds > 0 {
			item.UpdatedAt = time.Unix(seconds, 0).UTC()
		}
		result[id] = item
	}
	return result, nil
}
