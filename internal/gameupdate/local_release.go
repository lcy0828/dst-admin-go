package gameupdate

import (
	"context"
	"dont/internal/installationlock"
	"errors"
	"strings"

	dstinstall "dont/internal/dstserver"
	"dont/shared"

	"github.com/shirou/gopsutil/v3/disk"
)

func (s *Service) ObserveReleaseInstallation(ctx context.Context) (shared.RuntimeGameVersionResult, error) {
	if err := ctx.Err(); err != nil {
		return shared.RuntimeGameVersionResult{}, err
	}
	current, installed := readLocalVersion(s.config.ServerPath, s.config.AppID)
	gameVersion, _ := readLocalGameVersion(s.config.ServerPath)
	executable := findSteamCMD(s.config.SteamCMDPath)
	available := uint64(0)
	usage, usageErr := disk.Usage(installRoot(s.config.ServerPath))
	if usageErr == nil {
		available = usage.Free
	}
	supported := !s.config.DisableUpdate && s.config.UpdateMethod == dstinstall.UpdateMethodSteamCMD && executable != ""
	result := shared.RuntimeGameVersionResult{
		Installed: installed, AppID: s.config.AppID, UpdateMethod: s.config.UpdateMethod,
		Branch:      dstinstall.SteamBranch(steamManifestCandidates(installRoot(s.config.ServerPath), s.config.AppID)...),
		GameVersion: gameVersion, SteamBuild: current, CurrentVersion: current, AvailableBytes: available,
		SteamCMDAvailable: executable != "", UpdateSupported: supported, ObservedAt: s.now().UTC(),
	}
	return result, usageErr
}

func (s *Service) UpdateReleaseInstallation(ctx context.Context, expectedVersion string, cleanCache bool) (shared.RuntimeGameVersionResult, error) {
	expectedVersion = strings.TrimSpace(expectedVersion)
	if !releaseVersionPattern.MatchString(expectedVersion) {
		return shared.RuntimeGameVersionResult{}, ErrReleaseInvalid
	}
	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return shared.RuntimeGameVersionResult{}, ErrUpdateInProgress
	}
	s.active = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active = false
		s.mu.Unlock()
	}()
	if s.config.DisableUpdate {
		return s.releaseVersionResult(""), ErrUpdateDisabled
	}
	if s.config.UpdateMethod != dstinstall.UpdateMethodSteamCMD {
		return s.releaseVersionResult(""), ErrSteamClientManaged
	}
	executable := findSteamCMD(s.config.SteamCMDPath)
	if executable == "" {
		return s.releaseVersionResult(""), ErrSteamCMDUnavailable
	}
	unlock, lockErr := installationlock.Acquire(installRoot(s.config.ServerPath))
	if lockErr != nil {
		return s.releaseVersionResult(""), lockErr
	}
	defer unlock()
	if cleanCache {
		if err := cleanSteamCache(executable, installRoot(s.config.ServerPath), dstinstall.AppIDDedicatedServer); err != nil {
			return s.releaseVersionResult(""), err
		}
	}
	output := &boundedBuffer{limit: 2 * 1024 * 1024}
	arguments := []string{"+force_install_dir", installRoot(s.config.ServerPath), "+login", "anonymous", "+app_update", dstinstall.AppIDDedicatedServer, "validate", "+quit"}
	runErr := s.runner.Run(ctx, executable, arguments, output)
	result := s.releaseVersionResult(output.String())
	if runErr == nil && result.CurrentVersion != expectedVersion {
		runErr = errors.New("SteamCMD 已结束，但安装版本与目标版本不一致")
	}
	return result, runErr
}

func (s *Service) releaseVersionResult(logText string) shared.RuntimeGameVersionResult {
	current, installed := readLocalVersion(s.config.ServerPath, s.config.AppID)
	gameVersion, _ := readLocalGameVersion(s.config.ServerPath)
	executable := findSteamCMD(s.config.SteamCMDPath)
	available := uint64(0)
	if usage, err := disk.Usage(installRoot(s.config.ServerPath)); err == nil {
		available = usage.Free
	}
	return shared.RuntimeGameVersionResult{
		Installed: installed, AppID: s.config.AppID, UpdateMethod: s.config.UpdateMethod,
		Branch:      dstinstall.SteamBranch(steamManifestCandidates(installRoot(s.config.ServerPath), s.config.AppID)...),
		GameVersion: gameVersion, SteamBuild: current, CurrentVersion: current, AvailableBytes: available,
		SteamCMDAvailable: executable != "", UpdateSupported: !s.config.DisableUpdate && s.config.UpdateMethod == dstinstall.UpdateMethodSteamCMD && executable != "",
		Log: logText, ObservedAt: s.now().UTC(),
	}
}
