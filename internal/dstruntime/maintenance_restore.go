package dstruntime

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
)

// Maintenance waits for asynchronous game callbacks after publishing files.
// Recover only our publication; filesystem edits during that wait belong to
// the operator. Build and validate the entire plan before changing any file.
func (m *Manager) restoreMaintenanceBackup(worldPath, backupID string, publication runtimePublication) error {
	manifest, previous, err := readStateBackup(worldPath, backupID)
	if err != nil {
		return err
	}
	if manifest.SchemaVersion == 1 {
		previous = append(previous, restoredExecutionFiles(previous)...)
	}
	if _, err := checkExecutionDirectory(worldPath, false); err != nil {
		return err
	}
	current, err := captureFiles(worldPath, runtimeStatePaths())
	if err != nil {
		return err
	}
	if len(publication) != len(current) || len(previous) != len(current) {
		return fmt.Errorf("%w: incomplete maintenance publication", ErrManagedFileChanged)
	}
	observed := make(map[string]capturedFile, len(current))
	for _, file := range current {
		observed[file.path] = file
	}
	for i := range previous {
		restored := &previous[i]
		file := observed[restored.path]
		expected, exists := publication[restored.path]
		if !exists || !file.exists {
			return fmt.Errorf("%w: %s changed during maintenance", ErrManagedFileChanged, restored.path)
		}
		if sha256.Sum256(file.data) != expected {
			if file.path != customCommandsName {
				return fmt.Errorf("%w: %s changed during maintenance", ErrManagedFileChanged, file.path)
			}
			merged, err := mergeMaintenanceLoader(file.data, restored.data)
			if err != nil {
				return err
			}
			restored.data = merged
			restored.exists = restored.exists || len(bytes.TrimSpace(merged)) > 0
		}
		// Installation publishes assets as 0640 and preserves the original
		// customcommands mode. Keep explicit permission changes made afterward.
		publishedMode := os.FileMode(0640)
		if file.path == customCommandsName && restored.mode != 0 {
			publishedMode = restored.mode.Perm()
		}
		if file.mode.Perm() != publishedMode || restored.mode == 0 {
			restored.mode = file.mode
		}
	}
	if err := validateCapturedLua(previous); err != nil {
		return fmt.Errorf("%w: recovered Lua validation: %v", ErrManagedFileChanged, err)
	}
	if _, err := checkExecutionDirectory(worldPath, true); err != nil {
		return err
	}
	if err := restoreFiles(worldPath, previous); err != nil {
		return errors.Join(err, restoreFiles(worldPath, current))
	}
	removeEmptyManagedDirectory(worldPath)
	return nil
}

func mergeMaintenanceLoader(current, previous []byte) ([]byte, error) {
	location, found, err := locateManagedBlock(current)
	if err != nil || !found || !bytes.Equal(current[location.start:location.end], []byte(managedLoader+"\n")) {
		return nil, fmt.Errorf("%w: managed loader changed during maintenance", ErrManagedFileChanged)
	}
	oldLocation, oldFound, err := locateManagedBlock(previous)
	if err != nil {
		return nil, err
	}
	result := append([]byte(nil), current[:location.start]...)
	if oldFound {
		result = append(result, previous[oldLocation.start:oldLocation.end]...)
		if len(result) > 0 && result[len(result)-1] != '\n' && location.end < len(current) {
			result = append(result, '\n')
		}
	}
	return append(result, current[location.end:]...), nil
}
