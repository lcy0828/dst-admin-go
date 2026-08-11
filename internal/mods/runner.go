package mods

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type DownloadRunner interface {
	Download(context.Context, []string, bool, io.Writer) error
}

type SteamCMDRunner struct {
	Executable   string
	DownloadRoot string
	AppID        string
}

func NewSteamCMDRunner(configuredPath, downloadRoot, appID string) *SteamCMDRunner {
	return &SteamCMDRunner{Executable: findSteamCMD(configuredPath), DownloadRoot: downloadRoot, AppID: appID}
}

func (r *SteamCMDRunner) Download(ctx context.Context, ids []string, validate bool, output io.Writer) error {
	if r.Executable == "" {
		return ErrSteamCMDUnavailable
	}
	ids = uniqueModIDs(ids)
	if len(ids) == 0 {
		return ErrInvalidModID
	}
	arguments := []string{"+force_install_dir", r.DownloadRoot, "+login", "anonymous"}
	for _, id := range ids {
		arguments = append(arguments, "+workshop_download_item", r.AppID, id)
		if validate {
			arguments = append(arguments, "validate")
		}
	}
	arguments = append(arguments, "+quit")
	command := exec.CommandContext(ctx, r.Executable, arguments...)
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		return fmt.Errorf("%w: %v", ErrSteamCMDDownload, err)
	}
	return nil
}

func findSteamCMD(configured string) string {
	configured = filepath.Clean(strings.TrimSpace(configured))
	if configured != "" && configured != "." {
		if executableFile(configured) {
			return configured
		}
		for _, name := range []string{"steamcmd.sh", "steamcmd", "steamcmd.exe"} {
			candidate := filepath.Join(configured, name)
			if executableFile(candidate) {
				return candidate
			}
		}
	}
	for _, name := range []string{"steamcmd", "steamcmd.sh", "steamcmd.exe"} {
		if value, err := exec.LookPath(name); err == nil {
			return value
		}
	}
	return ""
}

func executableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0111 != 0
}

var _ DownloadRunner = (*SteamCMDRunner)(nil)
