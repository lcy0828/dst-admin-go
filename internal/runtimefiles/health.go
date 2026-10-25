package runtimefiles

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

const maximumHealthBytes = int64(64 * 1024)

var healthVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// ReadRuntimeHealth selects the installed collector's health without letting
// retired versions overwrite it. worldRoot must be a trusted world directory.
// Legacy installations retain health.json; the transport name is unchanged.
func ReadRuntimeHealth(worldRoot string) ([]byte, os.FileInfo, bool, error) {
	managed := filepath.Join(worldRoot, "dst-admin")
	info, err := os.Lstat(managed)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, false, err
	}
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return nil, nil, false, errors.New("Runtime 安装目录不安全")
	}
	output := filepath.Join(worldRoot, "save", "mod_config_data", "dst-admin")
	if err == nil {
		data, _, exists, err := readTrustedRegular(filepath.Join(managed, "manifest.json"), maximumHealthBytes)
		if err != nil {
			return nil, nil, false, err
		}
		if exists {
			var manifest struct {
				Version string `json:"version"`
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				return nil, nil, false, err
			}
			if len(manifest.Version) > 64 || !healthVersion.MatchString(manifest.Version) {
				return nil, nil, false, errors.New("Runtime 安装版本无效")
			}
			data, info, exists, err := readTrustedRegular(filepath.Join(output, "health-"+manifest.Version+".json"), maximumHealthBytes)
			if err != nil || exists {
				return data, info, exists, err
			}
		}
	}
	// Only absence permits fallback. An unsafe or corrupt selected file must
	// remain visible to callers instead of being hidden by a legacy sample.
	return readTrustedRegular(filepath.Join(output, "health.json"), maximumHealthBytes)
}
