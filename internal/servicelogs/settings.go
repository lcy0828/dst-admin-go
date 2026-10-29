package servicelogs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dont/shared"
	"github.com/google/uuid"
)

var ErrInvalid = errors.New("service log query or settings are invalid")
var ErrCursorExpired = errors.New("service log cursor has expired; refresh the query")
var ErrConflict = errors.New("service log settings changed; reload before saving")
var settingsMu sync.Mutex

func DefaultSettings() shared.ServiceLogSettings {
	return shared.ServiceLogSettings{Enabled: true, RetentionDays: 7, MaxMiB: 100, Revision: "default"}
}
func Root(config string) string { return filepath.Join(filepath.Dir(config), "service-logs") }
func ReadSettings(root string) shared.ServiceLogSettings {
	v := DefaultSettings()
	data, err := os.ReadFile(filepath.Join(root, "settings.json"))
	if err == nil {
		if json.Unmarshal(data, &v) != nil || validateSettings(v) != nil {
			return DefaultSettings()
		}
	}
	return v
}
func validateSettings(v shared.ServiceLogSettings) error {
	if v.RetentionDays < 1 || v.RetentionDays > 30 || v.MaxMiB < 10 || v.MaxMiB > 1024 {
		return ErrInvalid
	}
	if v.DebugUntil != nil && (v.DebugUntil.After(time.Now().Add(2 * time.Hour))) {
		return ErrInvalid
	}
	return nil
}
func SaveSettings(root string, v shared.ServiceLogSettings) (shared.ServiceLogSettings, error) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	if err := validateSettings(v); err != nil {
		return v, err
	}
	current := ReadSettings(root)
	if v.Revision != current.Revision {
		return current, ErrConflict
	}
	v.Revision = uuid.NewString()
	if err := os.MkdirAll(root, 0700); err != nil {
		return v, err
	}
	data, _ := json.Marshal(v)
	f, err := os.CreateTemp(root, ".settings-")
	if err != nil {
		return v, err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err != nil {
		return v, err
	}
	if closeErr != nil {
		return v, closeErr
	}
	err = os.Rename(f.Name(), filepath.Join(root, "settings.json"))
	return v, err
}
