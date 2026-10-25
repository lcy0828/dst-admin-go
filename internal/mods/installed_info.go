package mods

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// ParseInstalledModInfo uses a manually installed directory when present,
// otherwise the installation's Workshop content, matching file observation.
func ParseInstalledModInfo(ctx context.Context, parser ModInfoParser, workshopRoot, serverRoot, modID string) (ParserResult, error) {
	ref, valid := ParseReference(modID)
	if !valid {
		return ParserResult{}, ErrInvalidModID
	}
	if err := ctx.Err(); err != nil {
		return ParserResult{}, err
	}
	modID = ref.ID
	root, directory := workshopRoot, filepath.Join(workshopRoot, modID)
	if ref.Source == "local" {
		if serverRoot == "" {
			return ParserResult{}, ErrModInfoUnavailable
		}
		root, directory = serverRoot, filepath.Join(serverRoot, "mods", ref.DirectoryName)
		parent, parentErr := os.Lstat(filepath.Join(serverRoot, "mods"))
		if parentErr != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
			return ParserResult{}, fmt.Errorf("Mod %s: unsafe local mods root: %v", modID, parentErr)
		}
		info, err := os.Lstat(directory)
		if err != nil {
			return ParserResult{}, fmt.Errorf("Mod %s: %w: %v", modID, ErrModInfoUnavailable, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ParserResult{}, fmt.Errorf("Mod %s: unsafe local directory", modID)
		}
		if _, err := safeRegularFile(filepath.Join(directory, "modinfo.lua")); err != nil {
			return ParserResult{}, fmt.Errorf("Mod %s: unsafe local modinfo.lua: %w", modID, err)
		}
	}
	if serverRoot != "" {
		manual := filepath.Join(serverRoot, "mods", ref.DirectoryName)
		if info, err := os.Lstat(manual); err == nil && info.IsDir() {
			root, directory = serverRoot, manual
		} else if err != nil && !os.IsNotExist(err) {
			return ParserResult{}, fmt.Errorf("inspect Mod %s directory: %w", modID, err)
		}
	}
	if root == "" {
		return ParserResult{}, ErrModInfoUnavailable
	}
	path, err := safeContainedFile(root, filepath.Join(directory, "modinfo.lua"))
	if os.IsNotExist(err) {
		return ParserResult{}, fmt.Errorf("Mod %s: %w", modID, ErrModInfoUnavailable)
	}
	if err != nil {
		return ParserResult{}, fmt.Errorf("inspect Mod %s modinfo.lua: %w", modID, err)
	}
	parsed, err := parser.Parse(ctx, modID, path)
	if err != nil {
		return ParserResult{}, fmt.Errorf("parse Mod %s modinfo.lua: %w", modID, err)
	}
	return parsed, nil
}
