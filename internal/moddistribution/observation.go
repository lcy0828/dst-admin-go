package moddistribution

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"dont/internal/dsttime"
	"dont/internal/modmetadata"
	"dont/shared"
)

const maxObservedWorkshopIDs = 4096

// ObserveFiles reports current installation files without consulting release
// journals or installations.json. Exact version integrity remains the
// responsibility of the publication path.
func (m *Manager) ObserveFiles(ctx context.Context, installationID string, workshopIDs []string, worlds []ObserveWorld) (FilesObservation, error) {
	installation, exists := m.installations[installationID]
	if !exists {
		return FilesObservation{}, ErrInvalidInput
	}
	return ObserveInstallationFiles(ctx, installation, workshopIDs, worlds)
}

// InventoryFiles enumerates the Workshop content currently present on one
// installation. It reads only directory entries and small metadata files.
func (m *Manager) InventoryFiles(ctx context.Context, installationID string) (FilesObservation, error) {
	installation, exists := m.installations[installationID]
	if !exists {
		return FilesObservation{}, ErrInvalidInput
	}
	return InventoryInstallationFiles(ctx, installation)
}

// InventoryInstallationFiles is the side-effect-free inventory entry point
// used by local runtimes and Agents.
func InventoryInstallationFiles(ctx context.Context, installation TrustedInstallation) (FilesObservation, error) {
	root := filepath.Clean(installation.WorkshopContentPath)
	prefix := ""
	if root == "." || strings.TrimSpace(installation.WorkshopContentPath) == "" {
		root = filepath.Join(filepath.Clean(installation.ServerPath), "mods")
		prefix = "workshop-"
	}
	if !validIdentity(installation.ID) || !filepath.IsAbs(root) {
		return FilesObservation{}, ErrInvalidInput
	}
	if err := rejectSymlinkComponents(root); err != nil {
		return FilesObservation{}, err
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return FilesObservation{InstallationID: installation.ID, Mods: map[string]FileState{}, Worlds: map[string]WorldFileState{}, ObservedAt: time.Now().UTC()}, nil
	}
	if err != nil {
		return FilesObservation{}, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return FilesObservation{}, err
		}
		name := entry.Name()
		if prefix != "" {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			name = strings.TrimPrefix(name, prefix)
		}
		if entry.IsDir() && validWorkshopID(name) {
			ids = append(ids, name)
		}
	}
	if len(ids) > maxObservedWorkshopIDs {
		return FilesObservation{}, ErrInvalidInput
	}
	if len(ids) == 0 {
		return FilesObservation{InstallationID: installation.ID, Mods: map[string]FileState{}, Worlds: map[string]WorldFileState{}, ObservedAt: time.Now().UTC()}, nil
	}
	return observeInstallationFiles(ctx, installation, ids, nil)
}

// ObserveInstallationFiles is the side-effect-free entry point used by an
// Agent before any publication manager has been initialized.
func ObserveInstallationFiles(ctx context.Context, installation TrustedInstallation, workshopIDs []string, worlds []ObserveWorld) (FilesObservation, error) {
	for _, id := range workshopIDs {
		if !validWorkshopID(id) {
			return FilesObservation{}, ErrInvalidInput
		}
	}
	return ObserveInstallationMods(ctx, installation, workshopIDs, worlds)
}

// ObserveInstallationMods reads both sources without treating local IDs as
// Workshop download or publication identities.
func ObserveInstallationMods(ctx context.Context, installation TrustedInstallation, workshopIDs []string, worlds []ObserveWorld) (FilesObservation, error) {
	installation.ServerPath = filepath.Clean(installation.ServerPath)
	installation.SavePath = filepath.Clean(installation.SavePath)
	installation.WorkshopContentPath = filepath.Clean(installation.WorkshopContentPath)
	if !validIdentity(installation.ID) || !filepath.IsAbs(installation.ServerPath) || !filepath.IsAbs(installation.SavePath) ||
		installation.WorkshopContentPath != "." && !filepath.IsAbs(installation.WorkshopContentPath) ||
		len(workshopIDs) == 0 || len(workshopIDs) > maxObservedWorkshopIDs {
		return FilesObservation{}, ErrInvalidInput
	}
	for _, root := range []string{installation.ServerPath, installation.SavePath} {
		if err := rejectSymlinkComponents(root); err != nil {
			return FilesObservation{}, err
		}
	}
	if installation.WorkshopContentPath == "." {
		installation.WorkshopContentPath = ""
	} else if err := rejectSymlinkComponents(installation.WorkshopContentPath); err != nil {
		return FilesObservation{}, err
	}
	ids := append([]string(nil), workshopIDs...)
	sort.Strings(ids)
	ids = compactObservedStrings(ids)
	if len(ids) == 0 {
		return FilesObservation{}, ErrInvalidInput
	}
	return observeInstallationFiles(ctx, installation, ids, worlds)
}

func observeInstallationFiles(ctx context.Context, installation TrustedInstallation, ids []string, worlds []ObserveWorld) (FilesObservation, error) {
	metadataCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	result := FilesObservation{
		InstallationID: installation.ID,
		Mods:           make(map[string]FileState, len(ids)),
		Worlds:         make(map[string]WorldFileState, len(worlds)),
		ObservedAt:     time.Now().UTC(),
	}
	workshopItems, workshopMetadataReason := observeWorkshopItems(installation)
	for _, workshopID := range ids {
		if err := ctx.Err(); err != nil {
			return FilesObservation{}, err
		}
		ref, valid := shared.ParseModReference(workshopID)
		if !valid || ref.ID != workshopID {
			return FilesObservation{}, ErrInvalidInput
		}
		if ref.Source == "local" {
			result.Mods[ref.ID] = observeInstallationModContext(metadataCtx, filepath.Join(installation.ServerPath, "mods", ref.DirectoryName), ref.ID)
			continue
		}
		state := observeInstallationModContext(metadataCtx, installationModTarget(installation, workshopID), workshopID)
		manual := false
		if installation.WorkshopContentPath != "" {
			entry := filepath.Join(installation.ServerPath, "mods", "workshop-"+workshopID)
			if info, err := os.Lstat(entry); err == nil && info.IsDir() {
				state = observeInstallationModContext(metadataCtx, entry, workshopID)
				manual = true
			} else if state.Status == FileReady {
				linked, linkErr := localModEntry(entry, installationModTarget(installation, workshopID))
				if linkErr != nil {
					state.Reason = "local_mod_entry_invalid"
				} else if !linked {
					state.Reason = "local_mod_entry_missing"
				}
			}
		}
		if item, exists := workshopItems[workshopID]; exists && !manual {
			state.SteamManifestID = item.manifestID
			state.SteamUpdatedAt = item.updatedAt
			state.InstalledSize = item.size
		} else if state.Status == FileReady && workshopMetadataReason == "" && !manual {
			state.MetadataReason = "workshop_manifest_item_missing"
		}
		if state.MetadataReason == "" && workshopMetadataReason != "" && !manual {
			state.MetadataReason = workshopMetadataReason
		}
		result.Mods[workshopID] = state
	}
	for _, world := range worlds {
		if err := ctx.Err(); err != nil {
			return FilesObservation{}, err
		}
		if !validIdentity(world.RoomID) || !validIdentity(world.WorldID) || !safeComponent(world.RoomDirectory) || !safeComponent(world.WorldDirectory) {
			return FilesObservation{}, ErrInvalidInput
		}
		key := world.RoomID + "/" + world.WorldID
		if _, duplicate := result.Worlds[key]; duplicate {
			return FilesObservation{}, ErrInvalidInput
		}
		loaded, observed, started := observeLoadedModsGeneration(filepath.Join(installation.SavePath, world.RoomDirectory, world.WorldDirectory, "server_log.txt"), ids)
		result.Worlds[key] = WorldFileState{RoomID: world.RoomID, WorldID: world.WorldID, LoadedModIDs: loaded, LogObserved: observed, LogStartedAt: started}
	}
	return result, nil
}

func compactObservedStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func observeInstallationMod(root string) FileState {
	ref, _ := shared.ModReferenceFromKey(filepath.Base(root))
	return observeInstallationModContext(context.Background(), root, ref.ID)
}
func observeInstallationModContext(ctx context.Context, root, id string) FileState {
	if err := rejectSymlinkComponents(filepath.Dir(root)); err != nil {
		return FileState{Status: FileInvalid, Reason: "unsafe_path"}
	}
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return FileState{Status: FileMissing}
	}
	if err != nil {
		return FileState{Status: FileInvalid, Reason: "unreadable"}
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return FileState{Status: FileInvalid, Reason: "invalid_directory"}
	}
	modInfoPath := filepath.Join(root, "modinfo.lua")
	info, err = os.Lstat(modInfoPath)
	if os.IsNotExist(err) {
		return FileState{Status: FileInvalid, Reason: "missing_modinfo"}
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxOverridesBytes {
		return FileState{Status: FileInvalid, Reason: "invalid_modinfo"}
	}
	value := modmetadata.Read(ctx, modInfoPath, id, modmetadata.Language(ctx))
	return FileState{DSTCompatible: value.DSTCompatible, APIVersion: value.APIVersion, Status: FileReady, Name: value.Name, Author: value.Author, Description: value.Description, Version: value.Version, MetadataReason: value.Reason}
}

func observeLoadedMods(path string, workshopIDs []string) ([]string, bool) {
	ids, observed, _ := observeLoadedModsGeneration(path, workshopIDs)
	return ids, observed
}

func observeLoadedModsGeneration(path string, workshopIDs []string) ([]string, bool, time.Time) {
	if err := rejectSymlinkComponents(filepath.Dir(path)); err != nil {
		return []string{}, false, time.Time{}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 {
		return []string{}, false, time.Time{}
	}
	file, err := os.Open(path)
	if err != nil {
		return []string{}, false, time.Time{}
	}
	defer file.Close()
	prefix, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil {
		return []string{}, false, time.Time{}
	}
	started, _ := dsttime.FindStartTime(string(prefix))
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return []string{}, false, time.Time{}
	}
	const tailBytes int64 = 2 << 20
	if info.Size() > tailBytes {
		if _, err := file.Seek(info.Size()-tailBytes, io.SeekStart); err != nil {
			return []string{}, false, time.Time{}
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, tailBytes))
	if err != nil {
		return []string{}, false, time.Time{}
	}
	data = bytes.ToLower(data)
	lines := bytes.Split(data, []byte{'\n'})
	loaded := make([]string, 0, len(workshopIDs))
	for _, workshopID := range workshopIDs {
		ref, valid := shared.ParseModReference(workshopID)
		if !valid {
			continue
		}
		needle := strings.ToLower(ref.DirectoryName)
		for _, line := range lines {
			if !bytes.Contains(line, []byte("loading mod:")) && !bytes.Contains(line, []byte("loading modmain.lua")) && !bytes.Contains(line, []byte("registering shard mod namespace")) && !bytes.Contains(line, []byte("registering prefabs")) {
				continue
			}
			fields := strings.Fields(string(line))
			for i := range fields {
				fields[i] = strings.Trim(fields[i], "\"':")
			}
			if slices.Contains(fields, needle) {
				loaded = append(loaded, workshopID)
				break
			}
		}
	}
	return loaded, true, started
}
