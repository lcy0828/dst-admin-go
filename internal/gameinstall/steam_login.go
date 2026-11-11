package gameinstall

import (
	"context"
	"errors"
	"time"

	"dont/internal/agents"
	"dont/internal/steamlogin"
	"dont/shared"
	"github.com/google/uuid"
)

func (s *Service) SteamLogin(ctx context.Context, targetID, installationID string, input shared.SteamLoginRequest) (shared.SteamLoginStatus, error) {
	if err := steamlogin.Validate(input); err != nil {
		return shared.SteamLoginStatus{}, err
	}
	t, i, err := s.resolve(targetID, installationID)
	if err != nil {
		return shared.SteamLoginStatus{}, err
	}
	if !t.Online {
		return shared.SteamLoginStatus{}, agents.ErrAgentOffline
	}
	if t.ID == "local" {
		if s.steamLogin == nil {
			return shared.SteamLoginStatus{}, agents.ErrUnsupportedAction
		}
		return s.steamLogin.Manage(options(t, i).steamCMD(), input)
	}
	capable, capableV2 := false, false
	for _, capability := range t.Capabilities {
		if capability == shared.SteamLoginCapability {
			capable = true
		}
		if capability == shared.SteamLoginV2Capability {
			capableV2 = true
		}
	}
	if !capable {
		return shared.SteamLoginStatus{}, errors.New("请升级该节点的 Agent 以支持 Steam 账号登录")
	}
	if input.RequiresV2Capability() && !capableV2 {
		return shared.SteamLoginStatus{}, errors.New("请升级该节点的 Agent 以支持仅用户名凭据复用或一次性验证码登录")
	}
	action := shared.RuntimeActionSteamLoginObserve
	if input.Action != "status" {
		action = shared.RuntimeActionSteamLoginManage
	}
	r := request(i, action, shared.GameInstallationRequest{SteamLogin: &input})
	if input.Action != "status" {
		key := uuid.NewString()
		lease, err := s.leases.Acquire(ctx, "steam-login:"+t.ID+":"+i.ID, key, time.Minute)
		if err != nil {
			return shared.SteamLoginStatus{}, err
		}
		defer s.leases.Release(lease)
		r.OperationKey, r.LeaseID, r.FencingToken, r.LeaseExpiresAt = key, lease.LeaseID, lease.FencingToken, &lease.ExpiresAt
	}
	result, err := s.targets.ExecuteRuntime(ctx, t.ID, r, 30)
	if err != nil {
		return shared.SteamLoginStatus{}, err
	}
	if result.Result.GameInstallation == nil || result.Result.GameInstallation.SteamLogin == nil {
		return shared.SteamLoginStatus{}, errors.New("该节点未返回 Steam 登录状态")
	}
	if result.Result.Outcome == shared.RuntimeOutcomeFailed {
		return shared.SteamLoginStatus{}, errors.New(result.Result.Message)
	}
	status := *result.Result.GameInstallation.SteamLogin
	if input.Action == "configure" && input.UseCachedCredentials && status.PasswordSet {
		return shared.SteamLoginStatus{}, errors.New("该节点未清除已保存的密码，请升级该节点的 Agent 后重试")
	}
	return status, nil
}
