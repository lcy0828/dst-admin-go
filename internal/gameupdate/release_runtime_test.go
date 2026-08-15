package gameupdate

import (
	"context"
	"errors"
	"testing"

	"dont/internal/runtimedriver"
	"dont/shared"
)

type failingReleaseRouter struct{ err error }

func (r failingReleaseRouter) DriverTarget(context.Context, string, string) (runtimedriver.Driver, runtimedriver.Target, error) {
	return nil, runtimedriver.Target{}, r.err
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
	local := &countingLocalRelease{}
	runtime, err := NewDriverReleaseRuntime(failingReleaseRouter{err: expected}, local)
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
	if local.observations != 0 {
		t.Fatalf("remote resolution used local fallback %d times", local.observations)
	}
}
