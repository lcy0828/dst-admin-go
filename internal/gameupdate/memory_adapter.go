package gameupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MemoryRunner is a deterministic update adapter wired only by the router in test mode.
type MemoryRunner struct {
	serverPath string
	version    string
}

func NewMemoryRunner(serverPath, version string) *MemoryRunner {
	return &MemoryRunner{serverPath: filepath.Clean(serverPath), version: strings.TrimSpace(version)}
}

func (r *MemoryRunner) Run(ctx context.Context, _ string, arguments []string, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(arguments) != 8 || arguments[0] != "+force_install_dir" || filepath.Clean(arguments[1]) != r.serverPath || arguments[5] != "343050" {
		return fmt.Errorf("unexpected SteamCMD arguments")
	}
	if err := os.MkdirAll(r.serverPath, 0750); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.serverPath, "version.txt"), []byte(r.version+"\n"), 0640); err != nil {
		return err
	}
	_, err := fmt.Fprintf(output, "[DST-ADMIN-TEST] app 343050 updated to %s\n", r.version)
	return err
}

type MemoryLatestChecker struct{ version string }

func NewMemoryLatestChecker(version string) *MemoryLatestChecker {
	return &MemoryLatestChecker{version: strings.TrimSpace(version)}
}

func (c *MemoryLatestChecker) Check(ctx context.Context, _, local string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	return c.version, local == c.version, nil
}
