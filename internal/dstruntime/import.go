package dstruntime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dont/internal/rooms"
)

// PrepareImportedWorld operates exclusively on a caller-owned staging world.
// Replacements inherit the target's collector opt-in; new imports enable it.
// It must run before publication, never as part of ordinary game startup.
func PrepareImportedWorld(ctx context.Context, incoming, current string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	customPath := filepath.Join(incoming, customCommandsName)
	custom, mode, exists, err := readRegular(customPath, maxCustomBytes)
	if err != nil {
		return err
	}
	enabled := true
	if current != "" {
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect current runtime world: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeRuntimePath
		}
		previous, _, _, err := readRegular(filepath.Join(current, customCommandsName), maxCustomBytes)
		if err != nil {
			return err
		}
		user, found, err := removeManagedBlock(previous)
		if err != nil {
			return err
		}
		enabled = found
		if !exists && len(bytes.TrimSpace(user)) > 0 {
			custom, mode = user, 0640
			if err := atomicWrite(customPath, custom, mode); err != nil {
				return err
			}
		}
	}
	manager := &Manager{root: filepath.Dir(incoming), now: time.Now, locks: make(map[string]*sync.Mutex)}
	world := rooms.World{DirectoryName: filepath.Base(incoming)}
	if enabled {
		_, err := manager.install(rooms.Room{Managed: true}, world)
		return err
	}
	// An archive must not re-enable a collector the operator has uninstalled.
	if _, err := manager.worldPath(rooms.Room{}, world); err != nil {
		return err
	}
	if _, err := checkExecutionDirectory(incoming, false); err != nil {
		return err
	}
	user, _, err := removeManagedBlock(custom)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(user)) == 0 {
		if err := os.Remove(customPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := atomicWrite(customPath, user, mode); err != nil {
		return err
	}
	for _, name := range append(managedPaths(), executionPaths()...) {
		if err := os.Remove(filepath.Join(incoming, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	removeEmptyManagedDirectory(incoming)
	return nil
}
