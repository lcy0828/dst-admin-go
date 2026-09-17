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
	if !validModID(modID) {
		return ParserResult{}, ErrInvalidModID
	}
	if err := ctx.Err(); err != nil {
		return ParserResult{}, err
	}
	root, directory := workshopRoot, filepath.Join(workshopRoot, modID)
	if serverRoot != "" {
		manual := filepath.Join(serverRoot, "mods", "workshop-"+modID)
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
