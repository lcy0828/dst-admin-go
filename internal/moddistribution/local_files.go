package moddistribution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LinkWorkshopMods exposes downloaded content through DST's standard local
// mods directory. Only explicit Mod actions call this; world start never does.
func LinkWorkshopMods(ctx context.Context, serverRoot, contentRoot string, workshopIDs []string) error {
	serverRoot, contentRoot = filepath.Clean(serverRoot), filepath.Clean(contentRoot)
	if !filepath.IsAbs(serverRoot) || !filepath.IsAbs(contentRoot) || len(workshopIDs) > maxObservedWorkshopIDs {
		return ErrInvalidInput
	}
	if len(workshopIDs) == 0 {
		return ctx.Err()
	}
	modsRoot := filepath.Join(serverRoot, "mods")
	for _, root := range []string{modsRoot, contentRoot} {
		if err := rejectSymlinkComponents(root); err != nil {
			return err
		}
	}
	for _, id := range workshopIDs {
		if !validWorkshopID(id) {
			return ErrInvalidInput
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		target := filepath.Join(modsRoot, "workshop-"+id)
		source := filepath.Join(contentRoot, id)
		if local, err := localModEntry(target, source); err != nil {
			return fmt.Errorf("Workshop %s local entry: %w", id, err)
		} else if local {
			continue
		}
		if state := observeInstallationMod(source); state.Status != FileReady {
			return fmt.Errorf("Workshop %s local content is unavailable: %s %s", id, state.Status, state.Reason)
		}
	}
	if err := os.MkdirAll(modsRoot, 0o750); err != nil {
		return err
	}
	for _, id := range workshopIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := filepath.Join(modsRoot, "workshop-"+id)
		source := filepath.Join(contentRoot, id)
		if exists, err := localModEntry(target, source); err != nil {
			return err
		} else if exists {
			continue
		}
		if err := os.Symlink(source, target); err != nil {
			if exists, inspectErr := localModEntry(target, source); !os.IsExist(err) || inspectErr != nil || !exists {
				return fmt.Errorf("Workshop %s local entry: %w", id, errors.Join(err, inspectErr))
			}
		}
	}
	return nil
}

// A real directory belongs to the operator. Never replace it with a link or
// accept an existing link that redirects outside the configured content root.
func localModEntry(path, source string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.IsDir() {
		if state := observeInstallationMod(path); state.Status != FileReady {
			return false, fmt.Errorf("existing local Mod directory is invalid: %s", state.Reason)
		}
		return true, nil
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return false, ErrUnsafePath
	}
	link, err := os.Readlink(path)
	if err != nil {
		return false, err
	}
	if !filepath.IsAbs(link) {
		link = filepath.Join(filepath.Dir(path), link)
	}
	if filepath.Clean(link) != filepath.Clean(source) {
		return false, ErrUnsafePath
	}
	if state := observeInstallationMod(source); state.Status != FileReady {
		return false, fmt.Errorf("linked Mod content is unavailable: %s %s", state.Status, state.Reason)
	}
	return true, nil
}

func (m *Manager) LinkLocalMods(ctx context.Context, installationID string, workshopIDs []string) error {
	installation, exists := m.installations[installationID]
	if !exists {
		return ErrInvalidInput
	}
	if strings.TrimSpace(installation.WorkshopContentPath) == "" {
		return nil
	}
	return LinkWorkshopMods(ctx, installation.ServerPath, installation.WorkshopContentPath, workshopIDs)
}
