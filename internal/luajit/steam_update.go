package luajit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"dont/internal/dstserver"
	"dont/internal/runtimeperformance"
)

// SteamLauncherError separates a successful official installation from optional
// acceleration repair. GameAvailable is checked against the executable files,
// not inferred from Steam's build ID.
type SteamLauncherError struct {
	UpdateError, RestoreError error
	GameAvailable             bool
}

func (e *SteamLauncherError) Error() string {
	return errors.Join(e.UpdateError, e.RestoreError).Error()
}
func (e *SteamLauncherError) Unwrap() []error {
	if e.UpdateError == nil {
		return []error{e.RestoreError}
	}
	return []error{e.UpdateError, e.RestoreError}
}

func SteamUpdateResult(err error) (error, string) {
	var repair *SteamLauncherError
	if errors.As(err, &repair) && repair.GameAvailable {
		return repair.UpdateError, repair.RestoreError.Error()
	}
	return err, ""
}

// PreserveSteamLauncher belongs to explicit installation/update operations. The
// caller must hold the installation lock and have confirmed the game is stopped.
// A small intent file lets a retry finish maintenance after an interrupted
// SteamCMD run; ordinary world starts never repair installation files.
func PreserveSteamLauncher(o Options, update func() error) error {
	o = o.normalized()
	if !o.Supported() {
		return update()
	}
	if resolved, err := filepath.EvalSymlinks(o.ServerPath); err == nil {
		o.ServerPath = resolved
	}
	layout, ok := dstserver.Resolve(o.ServerPath, "64")
	if !ok {
		// A failed download can leave the entry point absent. The prior original
		// still identifies the layout; the intent below decides whether to repair.
		layout, ok = dstserver.Resolve(filepath.Join(o.ServerPath, "bin64", dstserver.BinaryX64+"_1"), "64")
		if ok {
			layout.Executable = strings.TrimSuffix(layout.Executable, "_1")
		}
	}
	if !ok || filepath.Base(layout.Executable) != dstserver.BinaryX64 {
		return update()
	}
	mod := filepath.Join(layout.ContentRoot, "mods", "DontStarveLuaJIT2")
	marker := filepath.Join(layout.ContentRoot, "data", "unsafedata", "ds_luajit_injector.path")
	stub := filepath.Join(layout.WorkingDirectory, "lib64", "libInjector.so")
	paths := []string{mod, marker, stub, layout.Executable, layout.Executable + "_1"}
	intent := filepath.Join(layout.InstallRoot, ".dst-admin-luajit-steam-update")
	if err := safeParent(layout.InstallRoot, intent); err != nil {
		return err
	}
	pending, err := os.ReadFile(intent)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(pending) > 0 && string(pending) != "v1\n" {
		return errors.New("LuaJIT 更新恢复标记无效，请检查安装目录")
	}
	if _, err := os.Lstat(filepath.Join(layout.InstallRoot, ".dst-admin-luajit-transaction")); err == nil {
		if err := recoverTransaction(layout.InstallRoot, paths); err != nil {
			return err
		}
	}
	wrapper, err := runtimeperformance.ManagedLinuxLauncher(layout)
	if err != nil {
		return err
	}
	current, err := readLauncher(layout.Executable)
	if err != nil && !(len(pending) > 0 && os.IsNotExist(err)) {
		return err
	}
	legacy := "#!/bin/bash\nexport LD_LIBRARY_PATH=./lib64\nexport LD_PRELOAD=./lib64/libInjector.so\n./" + filepath.Base(layout.Executable) + "_1 \"$@\"\n"
	known := string(current) == wrapper || string(current) == legacy
	// A prior manual Steam update may already have overwritten our launcher
	// without leaving an intent. Adopt only an otherwise intact plugin install;
	// neither a vanilla installation nor an unknown script authorizes wrapping.
	if !known && isGameELF(current) {
		report := o.Inspect()
		known = report.Provider == "dontstarve-luajit2" && len(report.Issues) == 1 && report.Issues[0] == "injector_wrapper_invalid"
	}
	if !known && len(pending) == 0 {
		return update()
	}
	if !known && bytes.HasPrefix(current, []byte("#!")) {
		return errors.New("LuaJIT 更新恢复遇到未知启动入口，请先检查服务端文件")
	}
	for _, path := range paths {
		if err := safeParent(layout.InstallRoot, path); err != nil {
			return err
		}
	}
	if err := writeFile(intent, []byte("v1\n"), 0600); err != nil {
		return err
	}
	// Let Steam validate/patch the official executable, rather than our shell
	// wrapper. Keep the original and intent intact until post-update repair.
	if string(current) == wrapper || string(current) == legacy {
		original, readErr := readLauncher(layout.Executable + "_1")
		if readErr == nil && isGameELF(original) {
			if err := exposeSteamExecutable(layout.Executable); err != nil {
				return err
			}
		}
	}
	runErr := update()
	// Even a failed SteamCMD run may already have replaced the launcher. Restore
	// its contract with the new ELF, never with the previous game's executable.
	restoreErr := restoreSteamLauncher(o, layout, paths, wrapper)
	if restoreErr == nil {
		restoreErr = os.Remove(intent)
	}
	if restoreErr != nil {
		restoreErr = fmt.Errorf("恢复 LuaJIT 启动入口失败，请重试游戏更新或校验: %w", restoreErr)
		entry, entryErr := readLauncher(layout.Executable)
		original, originalErr := readLauncher(layout.Executable + "_1")
		gameAvailable := entryErr == nil && (isGameELF(entry) || (string(entry) == wrapper || string(entry) == legacy) && originalErr == nil && isGameELF(original))
		return &SteamLauncherError{UpdateError: runErr, RestoreError: restoreErr, GameAvailable: gameAvailable}
	}
	return runErr
}

func exposeSteamExecutable(path string) error {
	source, err := os.Open(path + "_1")
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.CreateTemp(filepath.Dir(path), ".luajit-steam-*")
	if err != nil {
		return err
	}
	defer os.Remove(target.Name())
	if err = target.Chmod(0755); err == nil {
		_, err = io.Copy(target, source)
	}
	if err == nil {
		err = target.Sync()
	}
	closeErr := target.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(target.Name(), path)
}

func restoreSteamLauncher(o Options, layout dstserver.Layout, paths []string, wrapper string) (err error) {
	current, err := readLauncher(layout.Executable)
	if err != nil {
		return err
	}
	if string(current) != wrapper {
		legacy := "#!/bin/bash\nexport LD_LIBRARY_PATH=./lib64\nexport LD_PRELOAD=./lib64/libInjector.so\n./" + filepath.Base(layout.Executable) + "_1 \"$@\"\n"
		if string(current) != legacy && !isGameELF(current) {
			return errors.New("SteamCMD 未生成有效的 Linux x64 服务端程序")
		}
		// Use the installer's journal layout so interrupted maintenance can also
		// be recovered by an explicit LuaJIT reinstall.
		tx, beginErr := beginTransaction(layout.InstallRoot, paths)
		if beginErr != nil {
			return beginErr
		}
		defer func() {
			if err != nil {
				err = errors.Join(err, tx.rollback())
			} else {
				err = tx.complete()
			}
		}()
		if isGameELF(current) {
			if err = os.Remove(layout.Executable + "_1"); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err = copyPath(layout.Executable, layout.Executable+"_1"); err != nil {
				return err
			}
		}
		if err = writeFile(layout.Executable, []byte(wrapper), 0755); err != nil {
			return err
		}
	}
	if report := o.Inspect(); !report.CanEnable {
		return fmt.Errorf("LuaJIT 安装检测未通过: %s", strings.Join(report.Issues, ", "))
	}
	return nil
}

func readLauncher(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 16<<10))
}

func isGameELF(value []byte) bool {
	return len(value) >= 20 && bytes.Equal(value[:6], []byte{0x7f, 'E', 'L', 'F', 2, 1}) && value[18] == 62 && value[19] == 0
}
