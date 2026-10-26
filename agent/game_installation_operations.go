package agent

import (
	"context"
	"errors"
	"strings"

	"dont/internal/gameinstall"
	"dont/internal/steamlogin"
	"dont/shared"
)

func validateGameInstallationPayload(r shared.RuntimeOperationRequest) error {
	if r.GameInstallation == nil || r.LuaJIT != nil || r.Console != nil || r.Logs != nil || r.ChatLogs != nil || r.Artifacts != nil || r.Observation != nil || r.Migration != nil || r.Backup != nil || r.Mod != nil || r.GameVersion != nil || r.Network != nil || r.CPU != nil || r.Configuration != nil || r.Map != nil {
		return errors.New("游戏安装请求负载无效")
	}
	p := r.GameInstallation
	if p.CheckUsage && (r.Action != shared.RuntimeActionGameInstallationObserve || p.Path != "" || p.Fingerprint != "") {
		return errors.New("使用状态检查只能读取已登记的安装位置")
	}
	if shared.IsSteamLoginAction(r.Action) {
		if p.Path != "" || p.Fingerprint != "" || p.SteamLogin == nil {
			return steamlogin.ErrInvalid
		}
		if (r.Action == shared.RuntimeActionSteamLoginObserve) != (p.SteamLogin.Action == "status") {
			return steamlogin.ErrInvalid
		}
		return steamlogin.Validate(*p.SteamLogin)
	}
	if p.SteamLogin != nil {
		return steamlogin.ErrInvalid
	}
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
	o := gameinstall.Options{ServerPath: i.ServerPath, SavePath: i.SavePath, SteamCMDPath: i.SteamCMDPath, ServerMode: i.ServerMode, Driver: i.Driver, Runner: a.gameVersionRunner, SteamLogin: a.Config.SteamLogin}
	result := runtimeResult(r, shared.RuntimeOutcomeObserved, "游戏安装状态已读取")
	if shared.IsSteamLoginAction(r.Action) {
		if a.Config.SteamLogin == nil {
			return result, errors.New("该节点未启用 Steam 登录管理")
		}
		status, err := a.Config.SteamLogin.Manage(findAgentSteamCMD(i.SteamCMDPath), *r.GameInstallation.SteamLogin)
		result.GameInstallation = &shared.GameInstallationReport{SteamLogin: &status}
		if err != nil {
			result.Outcome = shared.RuntimeOutcomeFailed
			result.Message = err.Error()
		} else if r.Action == shared.RuntimeActionSteamLoginManage {
			result.Outcome = shared.RuntimeOutcomeConfirmed
			result.Message = "Steam 登录设置已处理"
		}
		return result, err
	}
	var report shared.GameInstallationReport
	var err error
	switch r.Action {
	case shared.RuntimeActionGameInstallationObserve:
		if r.GameInstallation.Path != "" {
			report, err = o.Probe(r.GameInstallation.Path)
		} else if r.GameInstallation.CheckUsage {
			report = o.Check(ctx)
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
