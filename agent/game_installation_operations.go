package agent

import (
	"context"
	"errors"
	"strings"

	"dont/internal/gameinstall"
	"dont/shared"
)

func validateGameInstallationPayload(r shared.RuntimeOperationRequest) error {
	if r.GameInstallation == nil || r.LuaJIT != nil || r.Console != nil || r.Logs != nil || r.ChatLogs != nil || r.Artifacts != nil || r.Observation != nil || r.Migration != nil || r.Backup != nil || r.Mod != nil || r.GameVersion != nil || r.Network != nil || r.CPU != nil || r.Configuration != nil || r.Map != nil {
		return errors.New("游戏安装请求负载无效")
	}
	p := r.GameInstallation
	if len(p.Path) > 4096 || strings.ContainsAny(p.Path, "\x00\r\n") {
		return errors.New("游戏目录无效")
	}
	switch r.Action {
	case shared.RuntimeActionGameInstallationObserve:
		if p.Fingerprint != "" {
			return errors.New("检测请求不能携带接入凭据")
		}
	case shared.RuntimeActionGameInstallationInstall:
		if p.Path != "" || p.Fingerprint != "" {
			return errors.New("安装只允许使用节点已登记的位置")
		}
	case shared.RuntimeActionGameInstallationAdopt:
		if p.Path == "" || !luaJITDigest.MatchString(p.Fingerprint) {
			return errors.New("请先检测已有游戏目录")
		}
	default:
		return errors.New("游戏安装操作无效")
	}
	return nil
}
func (a *Agent) executeGameInstallation(ctx context.Context, i RuntimeInstallation, r shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	o := gameinstall.Options{ServerPath: i.ServerPath, SavePath: i.SavePath, SteamCMDPath: i.SteamCMDPath, ServerMode: i.ServerMode, Driver: i.Driver, Runner: a.gameVersionRunner}
	result := runtimeResult(r, shared.RuntimeOutcomeObserved, "游戏安装状态已读取")
	var report shared.GameInstallationReport
	var err error
	switch r.Action {
	case shared.RuntimeActionGameInstallationObserve:
		if r.GameInstallation.Path != "" {
			report, err = o.Probe(r.GameInstallation.Path)
		} else {
			report = o.Inspect()
		}
	case shared.RuntimeActionGameInstallationInstall:
		report, err = o.Install(ctx)
	case shared.RuntimeActionGameInstallationAdopt:
		report, err = o.Adopt(ctx, r.GameInstallation.Path, r.GameInstallation.Fingerprint)
	}
	result.GameInstallation = &report
	if err != nil {
		result.Outcome = shared.RuntimeOutcomeFailed
		result.Message = err.Error()
	} else if r.Action != shared.RuntimeActionGameInstallationObserve {
		result.Outcome = shared.RuntimeOutcomeConfirmed
		result.Message = "游戏服务端已就绪"
		a.sendActiveReport()
	}
	return result, err
}
