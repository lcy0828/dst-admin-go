package shards

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MemoryControl is a deterministic process-control adapter for end-to-end tests.
// Router construction only permits it when DST_ADMIN_ENV=test.
type MemoryControl struct {
	mu      sync.RWMutex
	running map[string]bool
	logRoot string
}

func NewMemoryControlWithLogRoot(logRoot string) *MemoryControl {
	return &MemoryControl{running: make(map[string]bool), logRoot: filepath.Clean(logRoot)}
}

func NewMemoryControl() *MemoryControl {
	return &MemoryControl{running: make(map[string]bool)}
}

func (c *MemoryControl) IsRunning(ctx context.Context, roomName, worldName string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.running[memoryControlKey(roomName, worldName)], nil
}

func (c *MemoryControl) Status(ctx context.Context, roomName, worldName string) (RuntimeStatus, error) {
	running, err := c.IsRunning(ctx, roomName, worldName)
	if err != nil {
		return RuntimeStatus{State: RuntimeUnknown}, err
	}
	if running {
		return RuntimeStatus{State: RuntimeRunning, SessionExists: true}, nil
	}
	return RuntimeStatus{State: RuntimeStopped}, nil
}

func (c *MemoryControl) Start(ctx context.Context, roomName, worldName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.running[memoryControlKey(roomName, worldName)] = true
	c.mu.Unlock()
	c.appendLog(roomName, worldName, "[DST-ADMIN-TEST] shard started")
	c.ensureSession(roomName, worldName)
	return nil
}

func (c *MemoryControl) ensureSession(roomName, worldName string) {
	if c.logRoot == "" || c.logRoot == "." {
		return
	}
	directory := filepath.Join(c.logRoot, roomName, worldName, "save", "session", "E2ESESSION")
	if err := os.MkdirAll(filepath.Join(directory, "KU_E2E_"), 0750); err != nil {
		return
	}
	path := filepath.Join(directory, "0000000001")
	if _, err := os.Stat(path); err == nil {
		return
	}
	_ = os.WriteFile(path, []byte("return { map = { width = 60, height = 40 } }\n"), 0640)
}

func (c *MemoryControl) Stop(ctx context.Context, roomName, worldName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.running, memoryControlKey(roomName, worldName))
	c.mu.Unlock()
	c.appendLog(roomName, worldName, "[DST-ADMIN-TEST] shard stopped")
	return nil
}

func (c *MemoryControl) Cleanup(ctx context.Context, roomName, worldName string) error {
	return c.Stop(ctx, roomName, worldName)
}

func (c *MemoryControl) Send(ctx context.Context, roomName, worldName, command string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.appendLog(roomName, worldName, "[DST-ADMIN-TEST] "+command)
	return nil
}

func (c *MemoryControl) appendLog(roomName, worldName, message string) {
	if c.logRoot == "" || c.logRoot == "." {
		return
	}
	path := filepath.Join(c.logRoot, roomName, worldName, "server_log.txt")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(file, "[%s] %s\n", time.Now().UTC().Format(time.RFC3339), message)
	_ = file.Close()
}

func memoryControlKey(roomName, worldName string) string {
	return roomName + "\x00" + worldName
}
