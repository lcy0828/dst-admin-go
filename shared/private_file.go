package shared

import (
	"fmt"
	"os"
	"path/filepath"
)

const PrivateFileMode os.FileMode = 0o600

// WritePrivateFile atomically replaces a file whose contents may contain
// credentials. Both the temporary file and final path are restricted to the
// current user.
func WritePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create private file directory: %w", err)
		}
	}

	temporary, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create private temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(PrivateFileMode); err != nil {
		return fmt.Errorf("restrict private temporary file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write private temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync private temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close private temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace private file: %w", err)
	}
	committed = true
	if err := os.Chmod(path, PrivateFileMode); err != nil {
		return fmt.Errorf("restrict private file: %w", err)
	}
	return nil
}

func EnsurePrivateFile(path string) error {
	if err := os.Chmod(path, PrivateFileMode); err != nil {
		return fmt.Errorf("restrict private file: %w", err)
	}
	return nil
}
