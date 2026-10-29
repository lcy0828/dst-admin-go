package routers

import (
	"context"
	"errors"
	"testing"

	"dont/internal/deploymentprofile"
	"dont/internal/rooms"
	"dont/internal/shards"
	"dont/internal/systemsettings"
)

func TestOnlyGatewayTogglePreservesLocalWorlds(t *testing.T) {
	for _, test := range []struct {
		name    string
		local   bool
		member  bool
		fields  []string
		allowed bool
	}{
		{"gateway only", true, false, []string{"fleet.controllerEnabled"}, true},
		{"pending directory change", true, false, []string{"fleet.controllerEnabled", "paths.save"}, false},
		{"disable local executor", true, false, []string{"fleet.controllerEnabled", "fleet.localExecutorEnabled"}, false},
		{"transfer control", true, false, []string{"fleet.controllerEnabled", "fleet.memberEnabled"}, false},
		{"member ownership", true, true, []string{"fleet.controllerEnabled"}, false},
		{"controller only", false, false, []string{"fleet.controllerEnabled"}, false},
		{"empty change", true, false, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := deploymentprofile.Profile{LocalExecutorEnabled: test.local, MemberEnabled: test.member}
			if preservesLocalWorlds(profile, test.fields) != test.allowed {
				t.Fatalf("unexpected reload impact for %v", test.fields)
			}
		})
	}
}

type reloadTestCatalog struct{}

func (reloadTestCatalog) List() ([]rooms.Room, error) {
	return []rooms.Room{{ID: "room", Name: "测试房间", DirectoryName: "Cluster"}}, nil
}
func (reloadTestCatalog) Worlds(string) ([]rooms.World, error) {
	return []rooms.World{{Name: "地面", DirectoryName: "Master"}}, nil
}

type reloadTestStatus struct {
	status shards.RuntimeStatus
	err    error
}

func (s reloadTestStatus) Status(context.Context, string, string) (shards.RuntimeStatus, error) {
	return s.status, s.err
}

func TestRuntimeEnvironmentChangesRequireStoppedWorlds(t *testing.T) {
	for _, test := range []struct {
		name   string
		status shards.RuntimeStatus
		err    error
		allow  bool
	}{
		{"stopped", shards.RuntimeStatus{State: shards.RuntimeStopped}, nil, true},
		{"failed without session", shards.RuntimeStatus{State: shards.RuntimeFailed}, nil, true},
		{"running", shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}, nil, false},
		{"failed with live session", shards.RuntimeStatus{State: shards.RuntimeFailed, SessionExists: true}, nil, false},
		{"unknown", shards.RuntimeStatus{State: shards.RuntimeUnknown}, nil, false},
		{"inspection failed", shards.RuntimeStatus{}, errors.New("inspection failed"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := requireStoppedLocalWorlds(context.Background(), reloadTestCatalog{}, reloadTestStatus{test.status, test.err})
			if test.allow && err != nil || !test.allow && !errors.Is(err, systemsettings.ErrLocalWorldsRunning) {
				t.Fatalf("unexpected reload guard: %v", err)
			}
		})
	}
}
