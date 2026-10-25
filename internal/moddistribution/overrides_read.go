package moddistribution

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ReadOverrides preserves absence as nil. An existing empty or unsafe file is
// an error, never permission to reconstruct an operator's configuration.
func ReadOverrides(ctx context.Context, saveRoot, cluster, shard string) ([]byte, error) {
	if !filepath.IsAbs(saveRoot) || !safeComponent(cluster) || !safeComponent(shard) {
		return nil, ErrInvalidInput
	}
	path := filepath.Join(saveRoot, cluster, shard, "modoverrides.lua")
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 8<<20 {
		return nil, errors.New("modoverrides.lua is empty, unsafe or exceeds 8 MiB")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.Join(errors.New("modoverrides.lua changed while opening"), err)
	}
	data, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	if err != nil || int64(len(data)) != info.Size() || len(data) > 8<<20 || !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return nil, errors.Join(errors.New("modoverrides.lua is incomplete or invalid UTF-8"), err)
	}
	return data, ctx.Err()
}
