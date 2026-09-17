package runtimefiles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"dont/internal/roomops"
	"dont/shared"
)

type ConfigurationConflictError struct {
	CurrentSHA256 string
}

func (e *ConfigurationConflictError) Error() string {
	return "modoverrides.lua changed since it was read; reload the configuration before saving"
}

// PublishModOverrides replaces only the selected world's file. The revision is
// checked on the owning machine, including immediately before the atomic rename.
func PublishModOverrides(ctx context.Context, saveRoot, cluster, shard, expectedSHA256 string, content []byte) (string, error) {
	decoded, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(decoded) != sha256.Size || len(content) > shared.MaximumRuntimeModOverridesBytes || len(content) == 0 {
		return "", errors.New("invalid Mod configuration write")
	}
	root, err := trustedShardPath(saveRoot, cluster, shard)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, "modoverrides.lua")
	ctx, release, err := roomops.Acquire(ctx, path)
	if err != nil {
		return "", err
	}
	defer release()
	current, info, exists, err := readTrustedRegular(path, shared.MaximumRuntimeModOverridesBytes)
	if err != nil {
		return "", err
	}
	currentSum := sha256.Sum256(current)
	currentRevision := hex.EncodeToString(currentSum[:])
	if !strings.EqualFold(expectedSHA256, currentRevision) {
		return currentRevision, &ConfigurationConflictError{CurrentSHA256: currentRevision}
	}
	if bytes.Equal(current, content) {
		return currentRevision, nil
	}
	mode := os.FileMode(0o640)
	if exists {
		mode = info.Mode().Perm()
	}
	temporary, err := os.CreateTemp(root, ".modoverrides-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	if err := temporary.Chmod(mode); err != nil {
		return "", err
	}
	if _, err := temporary.Write(content); err != nil {
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := trustedShardPath(saveRoot, cluster, shard); err != nil {
		return "", err
	}
	latest, _, _, err := readTrustedRegular(path, shared.MaximumRuntimeModOverridesBytes)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(latest, current) {
		sum := sha256.Sum256(latest)
		revision := hex.EncodeToString(sum[:])
		return revision, &ConfigurationConflictError{CurrentSHA256: revision}
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return "", err
	}
	if directory, err := os.Open(root); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}
