package configpublication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrRevisionConflict = errors.New("configuration files changed before publication")

// ValidateApply limits the inline operation to the existing room/world editors.
// Large transfers and cross-target publication retain the staged protocol.
func ValidateApply(descriptor Descriptor, data []byte, expected map[string]string) error {
	if err := validateDescriptor(descriptor); err != nil {
		return err
	}
	if len(data) == 0 || len(data) > MaxChunkBytes || descriptor.Size != int64(len(data)) {
		return ErrInvalidRequest
	}
	names := []string{"cluster.ini"}
	if descriptor.Scope == ScopeWorld {
		names = []string{"server.ini", "leveldataoverride.lua"}
	} else if descriptor.Scope != ScopeShared {
		return ErrInvalidRequest
	}
	if len(expected) != len(names) {
		return ErrInvalidRequest
	}
	for _, name := range names {
		digest, err := hex.DecodeString(expected[name])
		if err != nil || len(digest) != sha256.Size {
			return ErrInvalidRequest
		}
	}
	return nil
}

// Apply completes a small single-target save within the Runtime call. It reuses
// the same atomic writes and rollback as distributed publication.
func (m *Manager) Apply(ctx context.Context, descriptor Descriptor, data []byte, expected map[string]string) ([]string, error) {
	if err := ValidateApply(descriptor, data, expected); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	offset, err := m.Begin(descriptor)
	if err != nil {
		return nil, err
	}
	rollback := func(err error) ([]string, error) {
		return nil, errors.Join(err, m.Rollback(descriptor.PublicationID))
	}
	if offset != 0 {
		return rollback(ErrConflict)
	}
	if _, err := m.Write(descriptor.PublicationID, 0, data); err != nil {
		return rollback(err)
	}
	if err := m.Prepare(ctx, descriptor.PublicationID); err != nil {
		return rollback(err)
	}
	if err := ctx.Err(); err != nil {
		return rollback(err)
	}
	if err := m.publish(descriptor.PublicationID, expected); err != nil {
		return rollback(err)
	}
	root, err := m.targetRoot(descriptor)
	if err != nil {
		return rollback(err)
	}
	for name := range expected {
		_, actual, readErr := describeFile(filepath.Join(root, name))
		_, staged, stageErr := describeFile(filepath.Join(m.stagePath(descriptor.PublicationID), name))
		if readErr != nil || stageErr != nil || actual != staged {
			return rollback(errors.Join(readErr, stageErr, ErrIntegrity))
		}
	}
	if err := m.Complete(descriptor.PublicationID); err != nil {
		return []string{fmt.Sprintf("configuration saved; cleanup failed: %v", err)}, nil
	}
	return nil, nil
}

func verifyExpectedFile(path, expected string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: %s", ErrRevisionConflict, filepath.Base(path))
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return ErrConflict
	}
	_, actual, err := describeFile(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("%w: %s", ErrRevisionConflict, filepath.Base(path))
	}
	return nil
}
