package luajit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"dont/internal/dstserver"
	"dont/internal/installationlock"
	"dont/internal/operationprogress"
	"dont/internal/runtimeperformance"
	"dont/shared"
	"github.com/shirou/gopsutil/v3/process"
)

var ErrRunning = errors.New("该 DST 安装仍有世界运行，请先停止这些世界再安装 LuaJIT")
var ErrUnsupported = errors.New("LuaJIT 安装目前支持 Linux x64 的 64 位 DST 专用服务器")
var ErrBusy = installationlock.ErrBusy

type Options struct {
	ServerPath, ServerMode, Platform, Architecture string
	Stopped                                        func(context.Context) error
	DependencyCheck                                func(context.Context, string, string) error
}

func (o Options) normalized() Options {
	if o.Platform == "" {
		o.Platform = runtime.GOOS
	}
	if o.Architecture == "" {
		o.Architecture = runtime.GOARCH
	}
	return o
}
func (o Options) Inspect() shared.RuntimePerformanceReport {
	return runtimeperformance.Inspect(runtimeperformance.Options{ServerPath: o.ServerPath, ServerMode: o.ServerMode, Platform: o.Platform, Architecture: o.Architecture})
}
func (o Options) Supported() bool {
	o = o.normalized()
	return o.Platform == "linux" && (o.Architecture == "amd64" || o.Architecture == "x86_64") && o.ServerMode == "64"
}

// AcquireInstallation excludes package mutation and process creation across
// embedded Runtime and standalone Agent processes sharing the same game files.
func AcquireInstallation(serverPath, mode string) (func(), error) {
	layout, ok := dstserver.Resolve(serverPath, mode)
	if !ok {
		return nil, errors.New("尚未安装 DST 专用服务器")
	}
	root, err := filepath.EvalSymlinks(layout.InstallRoot)
	if err != nil {
		return nil, err
	}
	return installationlock.Acquire(root)
}
func (o Options) CheckStopped(ctx context.Context) error {
	if o.Stopped != nil {
		if err := o.Stopped(ctx); err != nil {
			return err
		}
	}
	layout, ok := dstserver.Resolve(o.ServerPath, o.ServerMode)
	if !ok {
		return errors.New("尚未安装 DST 专用服务器")
	}
	processes, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return fmt.Errorf("无法确认游戏进程状态: %w", err)
	}
	directory, _ := filepath.EvalSymlinks(layout.WorkingDirectory)
	for _, p := range processes {
		if p.Pid == int32(os.Getpid()) {
			continue
		}
		executable, e := p.ExeWithContext(ctx)
		if e != nil || !strings.HasPrefix(filepath.Base(executable), "dontstarve_") {
			// Binfmt runtimes (including amd64 containers on ARM hosts) expose
			// their interpreter as /proc/PID/exe. The process name and argv still
			// identify the actual game; never mistake it for a stopped server.
			name, _ := p.NameWithContext(ctx)
			if strings.HasPrefix(name, "dontstarve_") || strings.Contains(filepath.Base(executable), "rosetta") || strings.HasPrefix(filepath.Base(executable), "qemu-") {
				args, argErr := p.CmdlineSliceWithContext(ctx)
				if argErr != nil {
					return fmt.Errorf("无法确认游戏进程 %d 的安装路径: %w", p.Pid, argErr)
				}
				for _, arg := range args {
					if strings.HasPrefix(filepath.Base(arg), "dontstarve_") {
						executable = arg
						if !filepath.IsAbs(executable) {
							cwd, cwdErr := p.CwdWithContext(ctx)
							if cwdErr != nil {
								return cwdErr
							}
							executable = filepath.Join(cwd, executable)
						}
						e = nil
						break
					}
				}
			}
		}
		if e == nil && strings.HasPrefix(filepath.Base(executable), "dontstarve_") {
			parent, _ := filepath.EvalSymlinks(filepath.Dir(executable))
			if parent == directory {
				return ErrRunning
			}
		}
	}
	return ctx.Err()
}
func Install(ctx context.Context, o Options, release shared.LuaJITRelease, archive string) (report shared.RuntimePerformanceReport, err error) {
	o = o.normalized()
	if !o.Supported() || release.OS != "linux" || release.Arch != "amd64" {
		return report, ErrUnsupported
	}
	if !packageID.MatchString(release.SHA256) || release.Size <= 0 || release.Size > MaxPackageBytes {
		return report, ErrInvalidPackage
	}
	layout, ok := dstserver.Resolve(o.ServerPath, o.ServerMode)
	if !ok {
		return report, errors.New("尚未安装 DST 专用服务器")
	}
	unlock, err := AcquireInstallation(o.ServerPath, o.ServerMode)
	if err != nil {
		return report, err
	}
	defer unlock()
	if err = o.CheckStopped(ctx); err != nil {
		return report, err
	}
	binary := layout.Executable
	original := binary + "_1"
	mod := filepath.Join(layout.ContentRoot, "mods", "DontStarveLuaJIT2")
	marker := filepath.Join(layout.ContentRoot, "data", "unsafedata", "ds_luajit_injector.path")
	stub := filepath.Join(layout.WorkingDirectory, "lib64", "libInjector.so")
	paths := []string{mod, marker, stub, binary, original}
	for _, p := range paths {
		if err = safeParent(layout.InstallRoot, p); err != nil {
			return report, err
		}
	}
	if err = recoverTransaction(layout.InstallRoot, paths); err != nil {
		return report, err
	}
	emit(ctx, "verify", 25, "正在校验 LuaJIT 安装包")
	f, err := os.Open(archive)
	if err != nil {
		return report, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(h, io.LimitReader(f, MaxPackageBytes+1))
	f.Close()
	if copyErr != nil {
		return report, copyErr
	}
	if n != release.Size || hex.EncodeToString(h.Sum(nil)) != release.SHA256 {
		return report, errors.New("LuaJIT 安装包 SHA-256 校验失败")
	}
	stage, err := os.MkdirTemp(layout.InstallRoot, ".luajit-stage-*")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(stage)
	packageRoot := filepath.Join(stage, "package")
	if err = os.Mkdir(packageRoot, 0750); err != nil {
		return report, err
	}
	version, err := unpack(ctx, archive, packageRoot)
	if err != nil {
		return report, err
	}
	if version != release.Version {
		return report, errors.New("LuaJIT 安装包版本与所选版本不一致")
	}
	checkDependencies := o.DependencyCheck
	if checkDependencies == nil {
		checkDependencies = CheckDependencies
	}
	if err = checkDependencies(ctx, packageRoot, layout.WorkingDirectory); err != nil {
		return report, err
	}
	if err = o.CheckStopped(ctx); err != nil {
		return report, err
	}
	emit(ctx, "install", 60, "正在安装 LuaJIT 和启动入口")
	current, err := os.ReadFile(binary)
	if err != nil {
		return report, err
	}
	ownWrapper := strings.HasPrefix(string(current), "#!/bin/sh\n# DST Admin LuaJIT launcher v1\n")
	// Adopt the exact upstream launcher, preserving its original executable.
	legacyWrapper := "#!/bin/bash\nexport LD_LIBRARY_PATH=./lib64\nexport LD_PRELOAD=./lib64/libInjector.so\n./" + filepath.Base(original) + " \"$@\"\n"
	ownWrapper = ownWrapper || string(current) == legacyWrapper
	if !ownWrapper && !(len(current) >= 4 && string(current[:4]) == "\x7fELF") {
		return report, errors.New("现有启动入口由其他工具管理，请先恢复原版 DST 启动文件")
	}
	if ownWrapper {
		data, e := os.ReadFile(original)
		if e != nil || len(data) < 4 || string(data[:4]) != "\x7fELF" {
			return report, errors.New("LuaJIT 原始游戏程序缺失，请先修复 DST 安装")
		}
	}
	// Backup every path before replacing it. The journal is retained on a failed
	// rollback, so an incomplete installation never silently loses its old files.
	tx, err := beginTransaction(layout.InstallRoot, paths)
	if err != nil {
		return report, err
	}
	defer func() {
		if err != nil {
			if restoreErr := tx.rollback(); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("回滚失败，备份保留在 %s: %w", tx.root, restoreErr))
			}
			return
		}
		err = tx.complete()
	}()
	if !ownWrapper {
		if err = writeFile(original, current, 0755); err != nil {
			return report, err
		}
	}
	if err = os.MkdirAll(filepath.Dir(mod), 0755); err != nil {
		return report, err
	}
	if err = os.RemoveAll(mod); err != nil {
		return report, err
	}
	if err = os.Rename(packageRoot, mod); err != nil {
		return report, err
	}
	stubBytes, err := os.ReadFile(filepath.Join(mod, "bin64", "linux", "lib64", "libInjector.so"))
	if err != nil {
		return report, err
	}
	if err = writeFile(stub, stubBytes, 0755); err != nil {
		return report, err
	}
	if err = writeFile(marker, []byte(filepath.Join(mod, "libInjector.so")+"\n"), 0644); err != nil {
		return report, err
	}
	modRelative, err := filepath.Rel(layout.WorkingDirectory, mod)
	if err != nil {
		return report, err
	}
	rootRelative, err := filepath.Rel(layout.WorkingDirectory, layout.InstallRoot)
	if err != nil {
		return report, err
	}
	wrapper := "#!/bin/sh\n# DST Admin LuaJIT launcher v1\nset -eu\nbin_dir=$(CDPATH= cd -- \"$(dirname -- \"$0\")\" && pwd)\n" +
		"mod_root=\"$bin_dir\"/" + shellLiteral(modRelative) + "\ninstall_root=\"$bin_dir\"/" + shellLiteral(rootRelative) + "\n" +
		"exec 9>\"$install_root/.dst-admin-luajit.lock\"\nflock -s -n 9 || exit 75\n[ ! -d \"$install_root/.dst-admin-luajit-transaction\" ] || exit 75\n" +
		"export LD_LIBRARY_PATH=\"$bin_dir/lib64${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}\"\nexport DS_LUAJIT_INJECTOR=\"$mod_root/libInjector.so\"\nexport DS_LUAJIT_INJECTOR_DIR=\"$mod_root\"\nexport DS_LUAJIT_PLUGIN_DIR=\"$mod_root/plugins\"\nexport LD_PRELOAD=\"$bin_dir/lib64/libInjector.so\"\ncd \"$bin_dir\"\nexec \"$bin_dir/" + filepath.Base(original) + "\" \"$@\"\n"

	if err = writeFile(binary, []byte(wrapper), 0755); err != nil {
		return report, err
	}
	if err = ctx.Err(); err != nil {
		return report, err
	}
	emit(ctx, "inspect", 90, "正在确认 LuaJIT 安装状态")
	report = o.Inspect()
	if !report.CanEnable || report.PackageVersion != release.Version {
		return report, fmt.Errorf("安装检测未通过: %s", strings.Join(report.Issues, ", "))
	}
	if err = writeFile(filepath.Join(mod, "dst_admin_release.json"), []byte(fmt.Sprintf("{\"id\":%q,\"version\":%q}\n", release.ID, release.Version)), 0644); err != nil {
		return report, err
	}
	emit(ctx, "done", 100, "LuaJIT 安装完成，可在启动世界时选择")
	return report, nil
}
func emit(ctx context.Context, stage string, percent int, message string) {
	operationprogress.Report(ctx, operationprogress.Update{Stage: "luajit." + stage, Percent: percent, Message: message})
}
func writeFile(name string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".luajit-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), name)
}

func shellLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
