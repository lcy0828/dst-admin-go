package gameupdate

import (
	"context"
	"dont/shared"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestMaintenanceSelectsConnectedInstallations(t *testing.T) {
	snapshot := ReleasePlacementSnapshot{Shards: []ReleaseShardSnapshot{
		releaseSnapshotShard("a", "Master", "local", "one", true),
		releaseSnapshotShard("a", "Caves", "agent:remote", "shared", false),
		releaseSnapshotShard("b", "Master", "agent:remote", "shared", true),
		releaseSnapshotShard("b", "Caves", "agent:remote", "other", false),
		releaseSnapshotShard("unrelated", "Master", "local", "two", true),
	}}
	rt := &fakeReleaseRuntime{observations: map[string]shared.RuntimeGameVersionResult{}, statuses: map[string]shared.ShardRuntimeStatus{}, errors: map[string]error{}}
	for _, shard := range snapshot.Shards {
		rt.observations[releaseSnapshotInstallationKey(shard)] = shared.RuntimeGameVersionResult{Installed: true, CurrentVersion: "701", AvailableBytes: 8 << 30, SteamCMDAvailable: true, UpdateSupported: true}
		rt.statuses[shard.Room.ID+"\x00"+shard.World.ID] = shared.ShardRuntimeStatus{State: "stopped"}
	}
	planner, err := NewReleasePlanner(fakeReleaseSnapshots{snapshot}, rt, fixedLatest{version: "701"}, 2<<30)
	if err != nil {
		t.Fatal(err)
	}
	input := ReleasePreviewRequest{Installations: []ReleaseInstallationSelection{{TargetID: "local", InstallationID: "one"}}, Policy: ReleasePolicyInput{Validate: true}}
	plan, err := planner.Preview(context.Background(), input)
	if err != nil || !plan.Ready || !plan.UpdateRequired || len(plan.Installations) != 3 || !reflect.DeepEqual(plan.AffectedRoomIDs, []string{"a", "b"}) {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if !reflect.DeepEqual(plan.SelectedInstallations, input.Installations) {
		t.Fatal("selection lost", plan.SelectedInstallations)
	}
	for _, c := range rt.calls {
		if strings.Contains(c, "two") {
			t.Fatal("unrelated installation observed", c)
		}
	}
	input.TargetIDs = []string{"local"}
	if _, err = planner.Preview(context.Background(), input); !errors.Is(err, ErrReleaseInvalid) {
		t.Fatal("mixed selectors accepted", err)
	}
	input.TargetIDs = nil
	input.Installations[0].InstallationID = "missing"
	if _, err = planner.Preview(context.Background(), input); !errors.Is(err, ErrReleaseInstallationUnused) {
		t.Fatal("unknown installation expanded scope", err)
	}
	input.Installations[0].InstallationID = "one"
	observation := rt.observations["local\x00one"]
	observation.UpdateSupported = false
	observation.SteamCMDAvailable = false
	rt.observations["local\x00one"] = observation
	plan, err = planner.Preview(context.Background(), input)
	if err != nil || plan.Ready {
		t.Fatalf("current build skipped validation requirements: %+v %v", plan, err)
	}
}

func TestMaintenanceRunsValidationAndRestoresOnlyOriginalWorlds(t *testing.T) {
	for _, restart := range []bool{true, false} {
		t.Run(map[bool]string{true: "restart", false: "stay-stopped"}[restart], func(t *testing.T) {
			f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
			f.runtime.versions["agent:node-b\x00primary"] = "701"
			plan, err := f.coordinator.planner.Preview(context.Background(), ReleasePreviewRequest{Installations: []ReleaseInstallationSelection{{TargetID: "agent:node-b", InstallationID: "primary"}}, Policy: ReleasePolicyInput{Validate: true, RestartRunning: &restart, LoadConfirmation: ReleaseLoadConfirmationNone}})
			if err != nil || !plan.Ready {
				t.Fatalf("preview: %+v %v", plan, err)
			}
			value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "maintenance", Plan: plan})
			if err != nil || value.Stage != ReleaseStageSucceeded {
				t.Fatalf("publish: %+v %v", value, err)
			}
			if f.runtime.updateCounts["agent:node-b\x00primary"] != 1 || f.runtime.updateCounts["agent:node-a\x00primary"] != 0 {
				t.Fatal("wrong validation scope", f.runtime.updateCounts)
			}
			events := f.runtime.mutationEvents()
			assertEventBefore(t, events, "stop:room-b\x00Master", "update:agent:node-b\x00primary")
			if restart {
				assertEventBefore(t, events, "update:agent:node-b\x00primary", "start:room-b\x00Master")
			}
			for _, event := range events {
				if strings.Contains(event, "Archive") || strings.Contains(event, "room-a") || (!restart && strings.HasPrefix(event, "start:")) {
					t.Fatal("unexpected world mutation", events)
				}
			}
		})
	}
}

func TestMaintenanceRetryPreservesSuccessfulValidation(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	plan, err := f.coordinator.planner.Preview(context.Background(), ReleasePreviewRequest{Policy: ReleasePolicyInput{Validate: true, LoadConfirmation: ReleaseLoadConfirmationNone}})
	if err != nil {
		t.Fatal(err)
	}
	f.runtime.updateErrors["agent:node-b\x00primary"] = errors.New("download interrupted")
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "retry-maintenance", Plan: plan})
	if err == nil {
		t.Fatal("failed update reported success")
	}
	for _, event := range f.runtime.mutationEvents() {
		if strings.HasPrefix(event, "start:") {
			t.Fatal("started after failure", event)
		}
	}
	delete(f.runtime.updateErrors, "agent:node-b\x00primary")
	value, err = f.coordinator.Retry(context.Background(), value.ID)
	if err != nil || value.Stage != ReleaseStageSucceeded {
		t.Fatalf("retry: %+v %v", value, err)
	}
	if f.runtime.updateCounts["agent:node-a\x00primary"] != 1 || f.runtime.updateCounts["agent:node-b\x00primary"] != 2 {
		t.Fatal("successful validation repeated or failed validation skipped", f.runtime.updateCounts)
	}
}
