package moddistribution

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"dont/shared"
)

// Installation returns only a previously registered, validated installation.
func (m *Manager) Installation(id string) (TrustedInstallation, bool) {
	value, ok := m.installations[id]
	return value, ok
}

func (m *Manager) ObserveMods(ctx context.Context, installationID string, ids []string, worlds []ObserveWorld) (FilesObservation, error) {
	installation, ok := m.installations[installationID]
	if !ok {
		return FilesObservation{}, ErrInvalidInput
	}
	return ObserveInstallationMods(ctx, installation, ids, worlds)
}

func (m *Manager) InventoryMods(ctx context.Context, installationID string) (FilesObservation, error) {
	installation, ok := m.installations[installationID]
	if !ok {
		return FilesObservation{}, ErrInvalidInput
	}
	return InventoryInstallationMods(ctx, installation)
}

func InventoryInstallationMods(ctx context.Context, installation TrustedInstallation) (FilesObservation, error) {
	result, err := InventoryInstallationFiles(ctx, installation)
	if err != nil {
		return result, err
	}
	metadataCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	root := filepath.Join(installation.ServerPath, "mods")
	if err := rejectSymlinkComponents(root); err != nil {
		return result, err
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		ref, valid := shared.ModReferenceFromKey(entry.Name())
		if !valid || ref.Source != "local" || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		// A helper directory without modinfo is not an installed Mod. Missing
		// configured entries are still returned by ObserveInstallationMods.
		if _, err := os.Lstat(filepath.Join(root, entry.Name(), "modinfo.lua")); os.IsNotExist(err) {
			continue
		}
		result.Mods[ref.ID] = observeInstallationModContext(metadataCtx, filepath.Join(root, entry.Name()), ref.ID)
		if len(result.Mods) > maxObservedWorkshopIDs {
			return FilesObservation{}, ErrInvalidInput
		}
	}
	result.ObservedAt = time.Now().UTC()
	return result, nil
}
