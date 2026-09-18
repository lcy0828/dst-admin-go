package gameupdate

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"dont/internal/maintenance"
	"dont/shared"
)

type automaticOnlineFunc func(context.Context, string) (int, error)

func (f automaticOnlineFunc) OnlinePlayers(ctx context.Context, roomID string) (int, error) {
	return f(ctx, roomID)
}

func automaticFixture(t *testing.T, split bool) releaseCoordinatorFixture {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationLogs)
	cavesTarget, cavesInstallation := "local", "default"
	if split {
		cavesTarget, cavesInstallation = "agent:caves", "primary"
	}
	// The second room shares the selected installation and has another shard
	// elsewhere. An unrelated installation on the same node stays untouched.
	snapshot := ReleasePlacementSnapshot{TopologyRevision: strings.Repeat("a", 64), Shards: []ReleaseShardSnapshot{
		releaseSnapshotShard("room-a", "Master", "local", "default", true),
		releaseSnapshotShard("room-a", "Caves", cavesTarget, cavesInstallation, false),
		releaseSnapshotShard("room-b", "Master", "local", "default", true),
		releaseSnapshotShard("room-b", "Archive", "agent:archive", "primary", false),
		releaseSnapshotShard("unrelated", "Master", "local", "other", true),
	}}
	f.coordinator.planner.snapshots = fakeReleaseSnapshots{snapshot}
	f.runtime.versions = make(map[string]string)
	for _, shard := range snapshot.Shards {
		f.runtime.versions[releaseSnapshotInstallationKey(shard)] = "700"
	}
	f.runtime.states["unrelated\x00Master"] = shared.ShardRuntimeStatus{State: "running", SessionExists: true}
	f.coordinator.ConfigureOnlineCounter(automaticOnlineFunc(func(context.Context, string) (int, error) { return 0, nil }))
	return f
}

func TestAutomaticGameUpdateLocalAndSplitSharedRooms(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "split"}[split], func(t *testing.T) {
			f := automaticFixture(t, split)
			updated, err := f.coordinator.UpdateRoomWhenEmpty(context.Background(), "room-a", "job-auto")
			if err != nil || !updated {
				t.Fatalf("updated=%v err=%v", updated, err)
			}
			values, total, err := f.coordinator.List(10, 0)
			if err != nil || total != 1 || !reflect.DeepEqual(values[0].Plan.AffectedRoomIDs, []string{"room-a", "room-b"}) {
				t.Fatalf("releases=%+v total=%d err=%v", values, total, err)
			}
			for key, version := range f.runtime.versions {
				if key == releaseInstallationKey("local", "other") {
					if version != "700" {
						t.Fatal("updated unrelated installation")
					}
				} else if version != "701" {
					t.Fatalf("mixed build on %s: %s", key, version)
				}
			}
			events := f.runtime.mutationEvents()
			assertEventBefore(t, events, "stop:room-a\x00Caves", "stop:room-a\x00Master")
			assertEventBefore(t, events, "update:agent:archive\x00primary", "start:room-a\x00Master")
			for _, event := range events {
				if strings.Contains(event, "unrelated") || event == "start:room-b\x00Archive" {
					t.Fatalf("touched unrelated/stopped world: %s", event)
				}
			}
			if !values[0].Plan.Policy.RequireEmpty || len(values[0].ProtectionBackupIDs) != 2 {
				t.Fatalf("missing protection or empty policy: %+v", values[0])
			}
		})
	}
}

func TestAutomaticGameUpdateSkipsCurrentAndOccupiedOrUnknownRooms(t *testing.T) {
	for _, condition := range []string{"current", "occupied", "offline", "unconfigured"} {
		t.Run(condition, func(t *testing.T) {
			f := automaticFixture(t, true)
			switch condition {
			case "current":
				for key := range f.runtime.versions {
					f.runtime.versions[key] = "701"
				}
			case "occupied", "offline":
				f.coordinator.ConfigureOnlineCounter(automaticOnlineFunc(func(_ context.Context, roomID string) (int, error) {
					if roomID == "room-b" {
						if condition == "offline" {
							return 0, errors.New("Agent offline")
						}
						return 1, nil
					}
					return 0, nil
				}))
			case "unconfigured":
				f.coordinator.ConfigureOnlineCounter(nil)
			}
			updated, err := f.coordinator.UpdateRoomWhenEmpty(context.Background(), "room-a", "job-skip")
			if updated || (condition == "current" && err != nil) || (condition != "current" && err == nil) {
				t.Fatalf("updated=%v err=%v", updated, err)
			}
			if len(f.runtime.mutationEvents()) != 0 || len(f.coordinator.backups.(*coordinatorBackups).ids) != 0 {
				t.Fatal("skipped maintenance mutated runtime or created backups")
			}
		})
	}
}

func TestAutomaticGameUpdateRechecksAfterProtectionBackup(t *testing.T) {
	f := automaticFixture(t, false)
	f.coordinator.ConfigureOnlineCounter(automaticOnlineFunc(func(context.Context, string) (int, error) {
		if len(f.coordinator.backups.(*coordinatorBackups).ids) > 0 {
			return 1, nil
		}
		return 0, nil
	}))
	updated, err := f.coordinator.UpdateRoomWhenEmpty(context.Background(), "room-a", "job-joining")
	if updated || !errors.Is(err, maintenance.ErrPlayersOnline) || len(f.runtime.mutationEvents()) != 0 {
		t.Fatalf("joining player was ignored: updated=%v err=%v events=%v", updated, err, f.runtime.mutationEvents())
	}
}

func TestAutomaticGameUpdateRequiresRecoveryBeforeAnotherAttempt(t *testing.T) {
	f := automaticFixture(t, true)
	f.runtime.updateErrors[releaseInstallationKey("agent:caves", "primary")] = errors.New("download interrupted")
	if _, err := f.coordinator.UpdateRoomWhenEmpty(context.Background(), "room-a", "job-failed"); !errors.Is(err, ErrReleaseRecoveryNeeded) {
		t.Fatalf("expected recoverable failure: %v", err)
	}
	before := f.runtime.mutationEvents()
	if _, err := f.coordinator.UpdateRoomWhenEmpty(context.Background(), "room-a", "job-next"); !errors.Is(err, ErrReleaseRecoveryNeeded) {
		t.Fatalf("new automatic release bypassed recovery: %v", err)
	}
	if !reflect.DeepEqual(before, f.runtime.mutationEvents()) {
		t.Fatal("new attempt changed runtime")
	}
	values, total, err := f.coordinator.List(10, 0)
	if err != nil || total != 1 {
		t.Fatalf("lost recovery evidence: total=%d err=%v", total, err)
	}
	f.runtime.updateErrors = make(map[string]error)
	f.coordinator.ConfigureOnlineCounter(automaticOnlineFunc(func(context.Context, string) (int, error) { return 1, nil }))
	if _, err := f.coordinator.Retry(context.Background(), values[0].ID); !errors.Is(err, maintenance.ErrPlayersOnline) {
		t.Fatalf("recovery lost empty guard: %v", err)
	}
	f.coordinator.ConfigureOnlineCounter(automaticOnlineFunc(func(context.Context, string) (int, error) { return 0, nil }))
	if value, err := f.coordinator.Retry(context.Background(), values[0].ID); err != nil || value.Stage != ReleaseStageSucceeded {
		t.Fatalf("recovery failed: stage=%s err=%v", value.Stage, err)
	}
	if f.runtime.states["room-a\x00Master"].State != "running" || f.runtime.states["room-b\x00Archive"].State != "stopped" {
		t.Fatal("recovery lost original running set")
	}
}
