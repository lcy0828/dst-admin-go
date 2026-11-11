package gameinstall

import (
	"context"
	"errors"
	"testing"

	"dont/internal/agents"
	"dont/internal/operationlease"
	"dont/shared"
	"github.com/jinzhu/gorm"
)

type steamLoginRemoteTargets struct {
	values  []agents.RuntimeTarget
	target  string
	request shared.RuntimeOperationRequest
	failure error
	calls   int
}

func (f *steamLoginRemoteTargets) RuntimeTargets() ([]agents.RuntimeTarget, error) {
	return f.values, nil
}
func (f *steamLoginRemoteTargets) ExecuteRuntime(_ context.Context, target string, request shared.RuntimeOperationRequest, _ int) (agents.RuntimeExecutionResult, error) {
	f.calls++
	f.target, f.request = target, request
	return agents.RuntimeExecutionResult{Result: shared.RuntimeOperationResult{Outcome: shared.RuntimeOutcomeConfirmed, GameInstallation: &shared.GameInstallationReport{SteamLogin: &shared.SteamLoginStatus{Mode: "account", Username: "remote_user", PasswordSet: true, State: "configured"}}}}, f.failure
}

func TestRemoteSteamLoginKeepsExactNodeAndInstallationAndFencesChanges(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	leases := operationlease.NewService(db, "steam_login_test_")
	if err = leases.Migrate(); err != nil {
		t.Fatal(err)
	}
	f := &steamLoginRemoteTargets{values: []agents.RuntimeTarget{{ID: "agent:node", Online: true, Capabilities: []string{shared.SteamLoginCapability}, Installations: []agents.RuntimeInstallation{{ID: "alternate"}, {ID: "default"}}}}}
	s := NewService(f, nil, leases)
	input := shared.SteamLoginRequest{Action: "configure", Mode: "account", Username: "remote_user", Password: "test-only-password"}
	status, err := s.SteamLogin(context.Background(), "agent:node", "alternate", input)
	if err != nil || status.Username != input.Username {
		t.Fatal(status, err)
	}
	r := f.request
	if f.target != "agent:node" || r.InstallationID != "alternate" || r.Action != shared.RuntimeActionSteamLoginManage || r.LeaseID == "" || r.FencingToken == 0 || r.LeaseExpiresAt == nil || r.GameInstallation.SteamLogin.Password != input.Password {
		t.Fatal("login mutation lost node identity, credentials or fencing", r.Action, r.InstallationID)
	}
	_, err = s.SteamLogin(context.Background(), "agent:node", "alternate", shared.SteamLoginRequest{Action: "status"})
	if err != nil || f.request.Action != shared.RuntimeActionSteamLoginObserve || f.request.LeaseID != "" || f.request.GameInstallation.SteamLogin.Password != "" {
		t.Fatal("observe was not read-only", err)
	}
	f.failure = errors.New("disconnected")
	if _, err = s.SteamLogin(context.Background(), "agent:node", "alternate", input); !errors.Is(err, f.failure) {
		t.Fatal("remote error hidden", err)
	}
	if _, err = s.SteamLogin(context.Background(), "agent:node", "missing", input); !errors.Is(err, agents.ErrRuntimeInstallationNotRegistered) {
		t.Fatal("missing installation fell back", err)
	}
	calls := f.calls
	f.values[0].Online = false
	if _, err = s.SteamLogin(context.Background(), "agent:node", "alternate", input); !errors.Is(err, agents.ErrAgentOffline) || f.calls != calls {
		t.Fatal("offline target used another runtime", err)
	}
	f.values[0].Online = true
	f.values[0].Capabilities = []string{shared.SteamLoginCapability}
	f.failure = nil
	calls = f.calls
	for _, v2Input := range []shared.SteamLoginRequest{
		{Action: "configure", Mode: "account", Username: "remote_user", UseCachedCredentials: true},
		{Action: "login", Code: "ABCDE"},
	} {
		if _, err = s.SteamLogin(context.Background(), "agent:node", "alternate", v2Input); err == nil || f.calls != calls {
			t.Fatalf("v1-only Agent accepted v2 Steam login payload %+v: err=%v calls=%d", v2Input, err, f.calls)
		}
	}
	f.values[0].Capabilities = nil
	if _, err = s.SteamLogin(context.Background(), "agent:node", "alternate", input); err == nil || f.calls != calls {
		t.Fatal("old Agent accepted account settings", err)
	}
}
