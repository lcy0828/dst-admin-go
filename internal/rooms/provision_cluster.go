package rooms

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-ini/ini"
)

var provisionOperationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$`)

func (s *Service) StageProvisionCluster(roomID, operationID string, data []byte) (bool, error) {
	room, err := s.catalog.Room(roomID)
	if err != nil {
		return false, err
	}
	if !room.Managed || !provisionOperationID.MatchString(operationID) || len(data) == 0 || int64(len(data)) > maximumProvisionFileBytes {
		return false, ErrInvalidRoom
	}
	if _, err := ini.Load(data); err != nil {
		return false, fmt.Errorf("parse provisioned cluster.ini: %w", err)
	}
	roomRoot := filepath.Join(s.catalog.root, room.DirectoryName)
	target := filepath.Join(roomRoot, "cluster.ini")
	current, err := os.ReadFile(target)
	if err != nil {
		return false, err
	}
	if bytes.Equal(current, data) {
		return false, nil
	}
	stage := filepath.Join(roomRoot, ".dst-admin-provisions", operationID)
	if err := ensureContained(roomRoot, stage); err != nil {
		return false, err
	}
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return false, err
	}
	if err := writeExclusiveRegular(filepath.Join(stage, "original-cluster.ini"), current, 0o600); err != nil && !errors.Is(err, os.ErrExist) {
		return false, err
	}
	if err := atomicProvisionWrite(filepath.Join(stage, "next-cluster.ini"), data, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) PublishProvisionCluster(roomID, operationID string) error {
	roomRoot, stage, err := s.provisionClusterPaths(roomID, operationID)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(stage, "next-cluster.ini"))
	if err != nil {
		return err
	}
	if err := atomicProvisionWrite(filepath.Join(roomRoot, "cluster.ini"), data, 0o640); err != nil {
		return err
	}
	return atomicProvisionWrite(filepath.Join(stage, "published"), []byte("published\n"), 0o600)
}

func (s *Service) RollbackProvisionCluster(roomID, operationID string) error {
	roomRoot, stage, err := s.provisionClusterPaths(roomID, operationID)
	if err != nil {
		return err
	}
	original, readErr := os.ReadFile(filepath.Join(stage, "original-cluster.ini"))
	if os.IsNotExist(readErr) {
		return nil
	}
	if readErr != nil {
		return readErr
	}
	if _, publishedErr := os.Stat(filepath.Join(stage, "published")); publishedErr == nil {
		if err := atomicProvisionWrite(filepath.Join(roomRoot, "cluster.ini"), original, 0o640); err != nil {
			return err
		}
	} else if !os.IsNotExist(publishedErr) {
		return publishedErr
	}
	return os.RemoveAll(stage)
}

func (s *Service) CompleteProvisionCluster(roomID, operationID string) error {
	_, stage, err := s.provisionClusterPaths(roomID, operationID)
	if err != nil {
		return err
	}
	return os.RemoveAll(stage)
}

func (s *Service) provisionClusterPaths(roomID, operationID string) (string, string, error) {
	room, err := s.catalog.Room(roomID)
	if err != nil {
		return "", "", err
	}
	if !provisionOperationID.MatchString(strings.TrimSpace(operationID)) {
		return "", "", ErrInvalidID
	}
	roomRoot := filepath.Join(s.catalog.root, room.DirectoryName)
	stage := filepath.Join(roomRoot, ".dst-admin-provisions", operationID)
	if err := ensureContained(roomRoot, stage); err != nil {
		return "", "", err
	}
	return roomRoot, stage, nil
}

func writeExclusiveRegular(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func atomicProvisionWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".provision-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
