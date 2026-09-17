package gameinstall

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"dont/internal/dstserver"
	"dont/internal/installationlock"
	"dont/internal/operationprogress"
	"dont/shared"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/process"
)

var ErrRunning = errors.New("请先停止使用该服务端目录的全部世界")
var ErrOccupied = errors.New("当前安装目录已有内容，请选择已登记的其他安装；不会覆盖现有文件")
var versionPattern = regexp.MustCompile(`^[0-9]{1,64}$`)

type Runner interface {
	Run(context.Context, string, []string, io.Writer) error
}
type Options struct {
	ServerPath, SavePath, SteamCMDPath, ServerMode, Driver, Platform, Architecture string
	Runner                                                                         Runner
}

func (o Options) platform() string {
	if o.Platform != "" {
		return o.Platform
	}
	return runtime.GOOS
}
func (o Options) root() string {
	if layout, ok := dstserver.Resolve(o.ServerPath, o.ServerMode); ok {
		return layout.InstallRoot
	}
	p := filepath.Clean(o.ServerPath)
	if filepath.Base(p) == dstserver.Binary || filepath.Base(p) == dstserver.BinaryX64 {
		p = filepath.Dir(p)
	}
	if filepath.Base(p) == "bin64" || filepath.Base(p) == "bin" {
		p = filepath.Dir(p)
	}
	return p
}
func canonical(path string) string {
	if value, err := filepath.EvalSymlinks(path); err == nil {
		return value
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(canonical(parent), filepath.Base(path))
}
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
func (o Options) validate() error {
	if !filepath.IsAbs(o.ServerPath) || !filepath.IsAbs(o.SavePath) || o.root() == string(os.PathSeparator) || strings.ContainsAny(o.ServerPath+o.SavePath, "\x00\r\n") {
		return errors.New("请先配置有效的游戏安装目录和存档目录")
	}
	root, save := canonical(o.root()), canonical(o.SavePath)
	if within(root, save) || within(save, root) {
		return errors.New("游戏安装目录与存档目录不能重叠")
	}
	if o.Driver != "" && o.Driver != "native" {
		return errors.New("当前安装驱动暂不支持此操作")
	}
	return nil
}
func (o Options) steamCMD() string {
	if o.SteamCMDPath == "" {
		return ""
	}
	p, err := exec.LookPath(o.SteamCMDPath)
	if err != nil {
		return ""
	}
	return p
}
func readVersion(root string) string {
	f, err := os.Open(filepath.Join(root, "version.txt"))
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 128))
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(b))
	if !versionPattern.MatchString(v) {
		return ""
	}
	return v
}
func emptyDestination(root string) error {
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrOccupied
	}
	f, err := os.Open(root)
	if err != nil {
		return err
	}
	defer f.Close()
	entries, err := f.Readdirnames(1)
	if len(entries) != 0 {
		return ErrOccupied
	}
	if err != nil && err != io.EOF {
		return err
	}
	return nil
}
func (o Options) Inspect() shared.GameInstallationReport {
	r := shared.GameInstallationReport{ServerPath: o.root(), SavePath: o.SavePath, SteamCMDAvailable: o.steamCMD() != ""}
	if err := o.validate(); err != nil {
		r.Reason = err.Error()
		return r
	}
	layout, ok := dstserver.Resolve(o.ServerPath, o.ServerMode)
	if ok {
		r.ResolvedPath = canonical(layout.InstallRoot)
		r.GameVersion = readVersion(layout.ContentRoot)
		r.Installed = r.GameVersion != "" && validateGameBinary(layout.Executable, true) == nil
	}
	arch := o.Architecture
	if arch == "" {
		arch = runtime.GOARCH
	}
	r.CanInstall = o.platform() == "linux" && (arch == "amd64" || arch == "x86_64") && r.SteamCMDAvailable && (!ok || layout.UpdateSupported)
	r.CanAdopt = o.platform() != "windows" && !ok && emptyDestination(o.root()) == nil
	if ok && !r.Installed {
		r.Reason = "服务端文件不完整或损坏，请执行更新 / 校验"
	}
	if !r.CanInstall {
		switch {
		case ok && !layout.UpdateSupported:
			r.Reason = "此安装由 Steam 客户端维护"
		case !r.SteamCMDAvailable:
			r.Reason = "请先在运行机器配置 SteamCMD"
		default:
			r.Reason = "在线安装目前支持 Linux x64"
		}
	}
	return r
}
func (o Options) Probe(path string) (shared.GameInstallationReport, error) {
	if err := o.validate(); err != nil {
		return shared.GameInstallationReport{}, err
	}
	if !filepath.IsAbs(path) || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n") {
		return shared.GameInstallationReport{}, errors.New("请输入所选运行机器上的绝对目录")
	}
	layout, ok := dstserver.Resolve(path, o.ServerMode)
	if !ok || readVersion(layout.ContentRoot) == "" {
		return shared.GameInstallationReport{}, errors.New("该目录未检测到完整的 DST 服务端程序和 version.txt")
	}
	if err := validateGameBinary(layout.Executable, true); err != nil {
		return shared.GameInstallationReport{}, err
	}
	root, err := filepath.EvalSymlinks(layout.InstallRoot)
	if err != nil {
		return shared.GameInstallationReport{}, err
	}
	save, destination := canonical(o.SavePath), canonical(o.root())
	if within(root, save) || within(save, root) || within(root, destination) || within(destination, root) {
		return shared.GameInstallationReport{}, errors.New("所选游戏目录不能与存档或当前安装位置重叠")
	}
	f, err := os.Open(layout.Executable)
	if err != nil {
		return shared.GameInstallationReport{}, err
	}
	defer f.Close()
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", root, readVersion(layout.ContentRoot))
	n, err := io.Copy(h, io.LimitReader(f, 128<<20+1))
	if err != nil || n > 128<<20 {
		return shared.GameInstallationReport{}, errors.New("游戏程序无法校验")
	}
	r := o.Inspect()
	r.Installed = true
	r.ResolvedPath = root
	r.GameVersion = readVersion(layout.ContentRoot)
	r.Fingerprint = hex.EncodeToString(h.Sum(nil))
	return r, nil
}

func validateGameBinary(path string, allowLauncher bool) error {
	name := filepath.Base(path)
	if allowLauncher && name != dstserver.Binary && name != dstserver.BinaryX64 {
		return errors.New("所选文件不是 DST 服务端启动程序")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return err
	}
	if len(b) >= 20 && string(b[:4]) == "\x7fELF" {
		machine := binary.LittleEndian.Uint16(b[18:20])
		if b[5] == 1 && (machine == 62 || machine == 3) {
			return nil
		}
	}
	if runtime.GOOS == "darwin" && len(b) >= 4 {
		magic := binary.BigEndian.Uint32(b[:4])
		if magic == 0xfeedfacf || magic == 0xcffaedfe || magic == 0xcafebabe {
			return nil
		}
	}
	if allowLauncher && (strings.HasPrefix(string(b), "#!/bin/sh\n# DST Admin LuaJIT launcher v1\n") || strings.HasPrefix(string(b), "#!/bin/bash\nexport LD_LIBRARY_PATH=./lib64\nexport LD_PRELOAD=./lib64/libInjector.so\n")) {
		return validateGameBinary(path+"_1", false)
	}
	return errors.New("服务端程序格式不受支持或已损坏")
}
func stopped(ctx context.Context, root string) error {
	root = canonical(root)
	ps, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return err
	}
	for _, p := range ps {
		executable, e := p.ExeWithContext(ctx)
		if e == nil && strings.HasPrefix(filepath.Base(executable), "dontstarve_") && within(root, canonical(executable)) {
			return ErrRunning
		}
	}
	return ctx.Err()
}
func (o Options) guard() (func(), error) {
	key := sha256.Sum256([]byte(filepath.Clean(o.root())))
	return installationlock.Acquire(filepath.Join(filepath.Dir(o.root()), ".dst-admin-installations", hex.EncodeToString(key[:8])))
}
func (o Options) Adopt(ctx context.Context, path, fingerprint string) (shared.GameInstallationReport, error) {
	if err := o.validate(); err != nil {
		return o.Inspect(), err
	}
	unlock, err := o.guard()
	if err != nil {
		return o.Inspect(), err
	}
	defer unlock()
	candidate, err := o.Probe(path)
	if err != nil {
		return o.Inspect(), err
	}
	if fingerprint == "" || candidate.Fingerprint != fingerprint {
		return o.Inspect(), errors.New("所选服务端已变化，请重新检测目录")
	}
	if !o.Inspect().CanAdopt {
		return o.Inspect(), ErrOccupied
	}
	if err = stopped(ctx, candidate.ResolvedPath); err != nil {
		return o.Inspect(), err
	}
	unlockSource, err := installationlock.Acquire(candidate.ResolvedPath)
	if err != nil {
		return o.Inspect(), err
	}
	defer unlockSource()
	// Recheck under the source lock: another installer may have modified the
	// source or started a world between inspection and lock acquisition.
	if err = stopped(ctx, candidate.ResolvedPath); err != nil {
		return o.Inspect(), err
	}
	verified, err := o.Probe(path)
	if err != nil {
		return o.Inspect(), err
	}
	if verified.Fingerprint != fingerprint {
		return o.Inspect(), errors.New("所选服务端已变化，请重新检测目录")
	}
	if err = ctx.Err(); err != nil {
		return o.Inspect(), err
	}
	// Keep the registered path stable for every Runtime consumer. Only an empty
	// destination may become an alias; source files and saves are never moved.
	root := o.root()
	if err = os.MkdirAll(filepath.Dir(root), 0755); err != nil {
		return o.Inspect(), err
	}
	if err = emptyDestination(root); err != nil {
		return o.Inspect(), err
	}
	existed := false
	if _, err = os.Lstat(root); err == nil {
		if err = os.Remove(root); err != nil {
			return o.Inspect(), err
		}
		existed = true
	}
	if err = os.Symlink(candidate.ResolvedPath, root); err != nil {
		if existed {
			_ = os.Mkdir(root, 0755)
		}
		return o.Inspect(), err
	}
	return o.Inspect(), nil
}
func (o Options) Install(ctx context.Context) (shared.GameInstallationReport, error) {
	before := o.Inspect()
	if err := o.validate(); err != nil {
		return before, err
	}
	if !before.CanInstall {
		return before, errors.New(before.Reason)
	}
	unlock, err := o.guard()
	if err != nil {
		return before, err
	}
	defer unlock()
	root := o.root()
	if err = stopped(ctx, root); err != nil {
		return before, err
	}
	unlockGame, err := installationlock.Acquire(root)
	if err != nil {
		return before, err
	}
	defer unlockGame()
	if err = stopped(ctx, root); err != nil {
		return before, err
	}
	usage, err := disk.Usage(root)
	if err != nil {
		return before, err
	}
	if usage.Free < 6<<30 {
		return before, errors.New("安装磁盘至少需要 6 GiB 可用空间")
	}
	operationprogress.Report(ctx, operationprogress.Update{Stage: "game.install", Percent: 0, Message: "正在连接 Steam 并准备安装"})
	args := []string{"+force_install_dir", root, "+login", "anonymous", "+app_update", dstserver.AppIDDedicatedServer, "validate", "+quit"}
	out := &installOutput{ctx: ctx, now: time.Now}
	if o.Runner != nil {
		err = o.Runner.Run(ctx, o.steamCMD(), args, out)
	} else {
		command := exec.CommandContext(ctx, o.steamCMD(), args...)
		command.Stdout = out
		command.Stderr = out
		err = command.Run()
	}
	logText := out.finish()
	if err != nil {
		return o.Inspect(), fmt.Errorf("SteamCMD 安装失败: %w\n%s", err, logText)
	}
	after := o.Inspect()
	if !after.Installed || after.GameVersion == "" {
		return after, errors.New("SteamCMD 已退出，但未检测到完整服务端，请重试并检查下载结果")
	}
	operationprogress.Report(ctx, operationprogress.Update{Stage: "game.ready", Percent: 100, Message: "游戏服务端已安装并校验"})
	return after, nil
}

type limitedOutput struct{ text string }

func (b *limitedOutput) Write(p []byte) (int, error) {
	b.text += string(p)
	if len(b.text) > 8192 {
		b.text = b.text[len(b.text)-8192:]
	}
	return len(p), nil
}
