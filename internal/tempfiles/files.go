// Package tempfiles owns request artifacts on persistent data volumes. Shared
// file locks protect active requests, including requests in another process;
// exclusive cleanup reclaims only abandoned files. No background worker runs.
package tempfiles

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Area int

const (
	Uploads Area = iota
	Exports
)

var errBusy = errors.New("temporary files are in use")

type File struct {
	*os.File
	lease *os.File
	once  sync.Once
	err   error
}

// Close removes the artifact before releasing its shared directory lock.
func (f *File) Close() error {
	f.once.Do(func() {
		closeErr := f.File.Close()
		removeErr := os.Remove(f.Name())
		if os.IsNotExist(removeErr) {
			removeErr = nil
		}
		f.err = errors.Join(closeErr, removeErr, f.lease.Close())
	})
	return f.err
}

func location(root string, area Area) (string, string, error) {
	if strings.TrimSpace(root) == "" {
		return "", "", errors.New("temporary file storage is not configured")
	}
	switch area {
	case Uploads:
		return filepath.Join(root, ".uploads"), "upload-", nil
	case Exports:
		return filepath.Join(root, ".exports"), "backup-", nil
	default:
		return "", "", errors.New("invalid temporary file area")
	}
}

func checkDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("temporary file directory is not a regular directory")
	}
	return nil
}

func openLease(directory string) (*os.File, error) {
	path := filepath.Join(directory, ".lock")
	if info, err := os.Lstat(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	} else if err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("temporary file lock is not a regular file")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
}

// Cleanup runs at service initialization and before creating an artifact. If
// any request is active, cleanup is deferred until a later demand or restart.
// Unknown files, directories and symlinks are never removed.
func Cleanup(root string, area Area) error {
	directory, prefix, err := location(root, area)
	if err != nil {
		return err
	}
	if err := checkDirectory(directory); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	lease, err := openLease(directory)
	if err != nil {
		return err
	}
	defer lease.Close()
	if err := tryLock(lease, true); errors.Is(err, errBusy) {
		return nil
	} else if err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) || !entry.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove abandoned temporary artifact: %w", err)
		}
	}
	return nil
}

func Create(ctx context.Context, root string, area Area) (*File, error) {
	directory, prefix, err := location(root, area)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	if err := Cleanup(root, area); err != nil {
		return nil, err
	}
	lease, err := openLease(directory)
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			lease.Close()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := tryLock(lease, false); err == nil {
			break
		} else if !errors.Is(err, errBusy) {
			return nil, err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	file, err := os.CreateTemp(directory, prefix+"*")
	if err != nil {
		return nil, err
	}
	owned = true
	return &File{File: file, lease: lease}, nil
}
