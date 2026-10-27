package gameinstall

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/shirou/gopsutil/v3/process"
)

// EnsureNoSteamUpdate rejects SteamCMD left behind by a crashed service. File
// locks alone cannot prove the child process has exited after its parent dies.
// It runs only immediately before an explicit maintenance write.
func EnsureNoSteamUpdate(ctx context.Context, root string) error {
	processes, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return err
	}
	for _, p := range processes {
		name, _ := p.NameWithContext(ctx)
		if !strings.Contains(strings.ToLower(name), "steamcmd") {
			continue
		}
		args, err := p.CmdlineSliceWithContext(ctx)
		if err != nil {
			if errors.Is(err, process.ErrorProcessNotRunning) {
				continue
			}
			return fmt.Errorf("无法确认 SteamCMD 进程 %d 是否已停止: %w", p.Pid, err)
		}
		cwd, _ := p.CwdWithContext(ctx)
		if steamUpdateConflicts(args, root, cwd) {
			return fmt.Errorf("SteamCMD 进程 %d 仍在维护此安装，请等待其退出后继续校验", p.Pid)
		}
	}
	return ctx.Err()
}

func steamUpdateConflicts(args []string, root, cwd string) bool {
	update, directory := false, ""
	for index, argument := range args {
		switch strings.ToLower(argument) {
		case "+app_update":
			update = true
		case "+force_install_dir":
			if index+1 < len(args) {
				directory = args[index+1]
			}
		}
	}
	if !update {
		return false
	}
	// An updating process without a known installation is unsafe to overlap.
	if directory == "" {
		return true
	}
	if !filepath.IsAbs(directory) {
		if cwd == "" {
			return true
		}
		directory = filepath.Join(cwd, directory)
	}
	first, second := canonical(filepath.Clean(root)), canonical(filepath.Clean(directory))
	return first == second || strings.EqualFold(filepath.VolumeName(first), filepath.VolumeName(second)) && filepath.VolumeName(first) != "" && strings.EqualFold(first, second)
}
