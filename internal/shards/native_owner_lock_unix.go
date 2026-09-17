//go:build darwin || linux

package shards

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type nativeRuntimeOwnerRecord struct {
	PID        int       `json:"pid"`
	Label      string    `json:"label,omitempty"`
	Executable string    `json:"executable,omitempty"`
	Hostname   string    `json:"hostname,omitempty"`
	AcquiredAt time.Time `json:"acquiredAt"`
}

type nativeRuntimeOwner struct {
	mu   sync.Mutex
	file *os.File
}

func acquireNativeRuntimeOwner(saveRoot, label string) (*nativeRuntimeOwner, error) {
	directory := nativeRuntimeStateDirectory(saveRoot)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create native Runtime state directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("protect native Runtime state directory: %w", err)
	}
	path := filepath.Join(directory, "owner.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open native Runtime owner lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("protect native Runtime owner lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		detail := readNativeRuntimeOwner(file)
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			if detail == "" {
				detail = "另一个本机 Controller 或 Agent 正在持有该存档"
			}
			return nil, fmt.Errorf("RUNTIME_OWNER_CONFLICT: %s；同一 SAVE_PATH 只能由一个 Runtime 管理", detail)
		}
		return nil, fmt.Errorf("lock native Runtime owner: %w", err)
	}
	record := nativeRuntimeOwnerRecord{PID: os.Getpid(), Label: strings.TrimSpace(label), AcquiredAt: time.Now().UTC()}
	record.Hostname, _ = os.Hostname()
	record.Executable, _ = os.Executable()
	data, err := json.Marshal(record)
	if err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, err
	}
	if err := file.Truncate(0); err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err == nil {
		_, err = file.Write(append(data, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("record native Runtime owner: %w", err)
	}
	return &nativeRuntimeOwner{file: file}, nil
}

func readNativeRuntimeOwner(file *os.File) string {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return ""
	}
	var record nativeRuntimeOwnerRecord
	if json.Unmarshal(data, &record) != nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if value := strings.TrimSpace(record.Label); value != "" {
		parts = append(parts, value)
	}
	if record.PID > 0 {
		parts = append(parts, fmt.Sprintf("PID %d", record.PID))
	}
	if value := strings.TrimSpace(record.Hostname); value != "" {
		parts = append(parts, "主机 "+value)
	}
	return strings.Join(parts, " · ")
}

func (owner *nativeRuntimeOwner) close() error {
	if owner == nil {
		return nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.file == nil {
		return nil
	}
	file := owner.file
	owner.file = nil
	return errors.Join(unix.Flock(int(file.Fd()), unix.LOCK_UN), file.Close())
}
