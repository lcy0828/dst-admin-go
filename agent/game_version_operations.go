package agent

import (
	"bufio"
	"bytes"
	"context"
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

	dstinstall "dont/internal/dstserver"
	"dont/shared"

	"github.com/shirou/gopsutil/v3/disk"
)

const (
	dstDedicatedServerAppID = "343050"
	maxGameUpdateLogBytes   = 2 * 1024 * 1024
)

var gameVersionPattern = regexp.MustCompile(`^[0-9]{1,64}$`)
var gameBuildIDPattern = regexp.MustCompile(`(?m)"buildid"\s+"([0-9]+)"`)

type gameVersionCommandRunner interface {
	Run(context.Context, string, []string, io.Writer) error
}

type execGameVersionCommand struct{}

func (execGameVersionCommand) Run(ctx context.Context, executable string, arguments []string, output io.Writer) error {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Stdout, command.Stderr = output, output
	return command.Run()
}

func validateGameVersionPayload(request shared.RuntimeOperationRequest) error {
	if request.GameVersion == nil || request.Console != nil || request.Logs != nil || request.Artifacts != nil ||
		request.Observation != nil || request.Migration != nil || request.Backup != nil || request.Mod != nil {
		return errors.New("游戏版本请求负载无效")
	}
	expected := strings.TrimSpace(request.GameVersion.ExpectedVersion)
	if request.Action == shared.RuntimeActionGameVersionObserve {
		if expected != "" || request.GameVersion.CleanCache {
			return errors.New("游戏版本观察请求包含更新参数")
		}
		return nil
	}
	if !gameVersionPattern.MatchString(expected) {
		return errors.New("目标游戏版本无效")
	}
	return nil
}

func (a *Agent) observeGameVersion(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	result := runtimeResult(request, shared.RuntimeOutcomeObserved, "游戏版本状态已读取")
	version, installed := readInstalledGameVersion(installation)
	steamcmd := findAgentSteamCMD(installation.SteamCMDPath)
	appID, updateMethod := gameVersionInstallationMetadata(installation)
	available, diskErr := availableGameBytes(gameInstallRoot(installation))
	value := shared.RuntimeGameVersionResult{
		Installed: installed, AppID: appID, UpdateMethod: updateMethod,
		CurrentVersion: version, AvailableBytes: available,
		SteamCMDAvailable: steamcmd != "", UpdateSupported: updateMethod == dstinstall.UpdateMethodSteamCMD && steamcmd != "",
		ObservedAt: a.now().UTC(),
	}
	result.GameVersion = &value
	if diskErr != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, diskErr.Error()
		return result, diskErr
	}
	if err := ctx.Err(); err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		return result, err
	}
	return result, nil
}

func (a *Agent) updateGameVersion(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	result := runtimeResult(request, shared.RuntimeOutcomeConfirmed, "DST 服务端版本已更新并校验")
	_, updateMethod := gameVersionInstallationMetadata(installation)
	if updateMethod != dstinstall.UpdateMethodSteamCMD {
		err := errors.New("该 DST 安装由 Steam 客户端管理，Agent 不能自动更新")
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		result.GameVersion = gameVersionResult(a.now, installation, "", "")
		return result, err
	}
	steamcmd := findAgentSteamCMD(installation.SteamCMDPath)
	if steamcmd == "" {
		err := errors.New("Agent 本机 SteamCMD 不可用")
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		result.GameVersion = gameVersionResult(a.now, installation, "", "")
		return result, err
	}
	root := gameInstallRoot(installation)
	if request.GameVersion.CleanCache {
		if err := cleanAgentSteamCache(steamcmd, root); err != nil {
			result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
			result.GameVersion = gameVersionResult(a.now, installation, steamcmd, "")
			return result, err
		}
	}
	output := &limitedGameVersionBuffer{limit: maxGameUpdateLogBytes}
	arguments := []string{"+force_install_dir", root, "+login", "anonymous", "+app_update", dstDedicatedServerAppID, "validate", "+quit"}
	runErr := a.gameVersionRunner.Run(ctx, steamcmd, arguments, output)
	current, installed := readInstalledGameVersion(installation)
	value := gameVersionResult(a.now, installation, steamcmd, output.String())
	value.CurrentVersion, value.Installed = current, installed
	result.GameVersion = value
	if runErr == nil && current != strings.TrimSpace(request.GameVersion.ExpectedVersion) {
		runErr = fmt.Errorf("更新后版本 %q 与目标版本 %q 不一致", current, request.GameVersion.ExpectedVersion)
	}
	if runErr != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, runErr.Error()
		return result, runErr
	}
	return result, nil
}

func gameVersionResult(now func() time.Time, installation RuntimeInstallation, steamcmd, logText string) *shared.RuntimeGameVersionResult {
	version, installed := readInstalledGameVersion(installation)
	appID, updateMethod := gameVersionInstallationMetadata(installation)
	available, _ := availableGameBytes(gameInstallRoot(installation))
	return &shared.RuntimeGameVersionResult{
		Installed: installed, AppID: appID, UpdateMethod: updateMethod,
		CurrentVersion: version, AvailableBytes: available,
		SteamCMDAvailable: steamcmd != "", UpdateSupported: updateMethod == dstinstall.UpdateMethodSteamCMD && steamcmd != "", Log: logText,
		ObservedAt: now().UTC(),
	}
}

func gameVersionInstallationMetadata(installation RuntimeInstallation) (string, string) {
	if layout, ok := dstinstall.Resolve(installation.ServerPath, installation.ServerMode); ok {
		return layout.AppID, layout.UpdateMethod
	}
	return dstDedicatedServerAppID, dstinstall.UpdateMethodSteamCMD
}

func gameInstallRoot(installation RuntimeInstallation) string {
	if layout, ok := dstinstall.Resolve(installation.ServerPath, installation.ServerMode); ok {
		return layout.InstallRoot
	}
	return filepath.Clean(installation.ServerPath)
}

func readInstalledGameVersion(installation RuntimeInstallation) (string, bool) {
	root := gameInstallRoot(installation)
	appID, _ := gameVersionInstallationMetadata(installation)
	candidates := gameManifestCandidates(root, appID)
	candidates = append(candidates, filepath.Join(root, "version.txt"), filepath.Join(installation.ServerPath, "version.txt"))
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		data, err := os.ReadFile(candidate)
		if err != nil || len(data) > 1024*1024 {
			continue
		}
		if filepath.Base(candidate) == "version.txt" {
			scanner := bufio.NewScanner(bytes.NewReader(data))
			if scanner.Scan() {
				value := strings.TrimSpace(scanner.Text())
				if gameVersionPattern.MatchString(value) {
					return value, true
				}
			}
			continue
		}
		if match := gameBuildIDPattern.FindSubmatch(data); len(match) == 2 {
			return string(match[1]), true
		}
	}
	return "", false
}

func gameManifestCandidates(root, appID string) []string {
	result := make([]string, 0, 12)
	for current, depth := filepath.Clean(root), 0; depth < 6; depth++ {
		name := "appmanifest_" + appID + ".acf"
		result = append(result, filepath.Join(current, name), filepath.Join(current, "steamapps", name))
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return result
}

func findAgentSteamCMD(configured string) string {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		configured = filepath.Clean(configured)
		if executableGameFile(configured) {
			return configured
		}
		for _, name := range []string{"steamcmd", "steamcmd.sh", "steamcmd.exe"} {
			candidate := filepath.Join(configured, name)
			if executableGameFile(candidate) {
				return candidate
			}
		}
	}
	for _, name := range []string{"steamcmd", "steamcmd.sh", "steamcmd.exe"} {
		if value, err := exec.LookPath(name); err == nil {
			if absolute, absErr := filepath.Abs(value); absErr == nil && executableGameFile(absolute) {
				return absolute
			}
		}
	}
	return ""
}

func executableGameFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0
}

func availableGameBytes(root string) (uint64, error) {
	usage, err := disk.Usage(root)
	if err != nil {
		return 0, fmt.Errorf("读取 DST 安装磁盘空间: %w", err)
	}
	return usage.Free, nil
}

func cleanAgentSteamCache(executable, installRoot string) error {
	roots := []string{filepath.Join(filepath.Dir(executable), "steamapps"), filepath.Join(installRoot, "steamapps")}
	seen := make(map[string]bool, len(roots))
	for _, root := range roots {
		root = filepath.Clean(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		for _, relative := range []string{filepath.Join("downloading", dstDedicatedServerAppID), filepath.Join("temp", dstDedicatedServerAppID), "appmanifest_" + dstDedicatedServerAppID + ".acf"} {
			target := filepath.Join(root, relative)
			if !pathWithinRoot(target, root) {
				return errors.New("Steam 缓存路径越界")
			}
			info, err := os.Lstat(target)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return errors.New("Steam 缓存目标不能是符号链接")
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		}
	}
	return nil
}

type limitedGameVersionBuffer struct {
	buffer bytes.Buffer
	limit  int
	cut    bool
}

func (b *limitedGameVersionBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value, b.cut = value[:remaining], true
		}
		_, _ = b.buffer.Write(value)
	} else {
		b.cut = true
	}
	return original, nil
}

func (b *limitedGameVersionBuffer) String() string {
	if b.cut {
		return b.buffer.String() + "\n[DST Admin] SteamCMD 输出超过 2 MiB，已截断\n"
	}
	return b.buffer.String()
}
