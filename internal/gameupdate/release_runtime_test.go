package gameupdate

import (
	"context"
	"errors"
	"testing"

	"dont/internal/runtimedriver"
	"dont/shared"
)

type failingReleaseRouter struct{ err error }

func (r failingReleaseRouter) EndpointTarget(context.Context, string, string) (runtimedriver.RuntimeEndpoint, runtimedriver.Target, error) {
	return runtimedriver.RuntimeEndpoint{}, runtimedriver.Target{}, r.err
}

type countingLocalRelease struct{ observations int }

func (l *countingLocalRelease) ObserveReleaseInstallation(context.Context) (shared.RuntimeGameVersionResult, error) {
	l.observations++
	return shared.RuntimeGameVersionResult{Installed: true, CurrentVersion: "local"}, nil
}

func (*countingLocalRelease) UpdateReleaseInstallation(context.Context, string, bool) (shared.RuntimeGameVersionResult, error) {
	return shared.RuntimeGameVersionResult{}, nil
}

func TestDriverReleaseRuntimeNeverFallsBackRemoteInstallationToLocal(t *testing.T) {
	expected := errors.New("remote target unavailable")
	runtime, err := NewDriverReleaseRuntime(failingReleaseRouter{err: expected})
	if err != nil {
		t.Fatal(err)
	}
	plan := ReleaseInstallationPlan{
		TargetID: "agent:node-a", InstallationID: "primary",
		Shards: []ReleaseShardPlan{{
			RoomID: "room-a", WorldID: "Master", TargetID: "agent:node-a",
			InstallationID: "primary", TopologyRevision: "revision-a",
		}},
	}
	_, err = runtime.ObserveInstallation(context.Background(), plan)
	if !errors.Is(err, expected) {
		t.Fatalf("error=%v", err)
	}
}

func TestLocalGameVersionDriverAdaptsControllerService(t *testing.T) {
	local := &countingLocalRelease{}
	driver, err := NewLocalGameVersionDriver(local)
	if err != nil {
		t.Fatal(err)
	}
	result, err := driver.ObserveGameVersion(context.Background(), runtimedriver.Target{TargetID: "local", InstallationID: "default"})
	if err != nil || result.CurrentVersion != "local" || local.observations != 1 {
		t.Fatalf("result=%#v observations=%d err=%v", result, local.observations, err)
	}
}
