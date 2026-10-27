package gameupdate

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimeobservation"
	"dont/internal/topology"
	"dont/shared"
)

type regressionReleaseSource struct {
	item     agents.RuntimeTargetInventory
	now      *time.Time
	collects int
}

func (s *regressionReleaseSource) CachedRuntimeTargetInventories(context.Context) ([]agents.RuntimeTargetInventory, error) {
	return []agents.RuntimeTargetInventory{s.item}, nil
}
func (s *regressionReleaseSource) CollectRuntimeTargetInventory(context.Context, string, string) (agents.RuntimeTargetInventory, error) {
	s.collects++
	item := s.item
	now := *s.now
	item.ReceivedAt = &now
	item.ObservedAt = &now
	item.Inventory.ObservedAt = now
	return item, nil
}
func (*regressionReleaseSource) CheckpointRuntimeTargetInventory(string, shared.RuntimeInventoryReport) error {
	return nil
}

type regressionReleaseRooms struct{ snapshot ReleasePlacementSnapshot }

func (r regressionReleaseRooms) List() ([]rooms.Room, error) {
	seen := map[string]bool{}
	var out []rooms.Room
	for _, s := range r.snapshot.Shards {
		if !seen[s.Room.ID] {
			out = append(out, s.Room)
			seen[s.Room.ID] = true
		}
	}
	return out, nil
}
func (r regressionReleaseRooms) Worlds(id string) ([]rooms.World, error) {
	var out []rooms.World
	for _, s := range r.snapshot.Shards {
		if s.Room.ID == id {
			out = append(out, s.World)
		}
	}
	return out, nil
}

type regressionReleasePlacements struct{ snapshot ReleasePlacementSnapshot }

func (r regressionReleasePlacements) AppliedPlacement(roomID, worldID string) (topology.ExecutionPlacement, error) {
	for _, s := range r.snapshot.Shards {
		if s.Room.ID == roomID && s.World.ID == worldID {
			return topology.ExecutionPlacement{Revision: s.TopologyRevision, AppliedTargetID: s.Target.ID, AppliedInstallationID: s.Target.Config.InstallationID}, nil
		}
	}
	return topology.ExecutionPlacement{}, errors.New("missing world")
}

type regressionReleaseBackups struct {
	base  *coordinatorBackups
	after func()
}

func (b regressionReleaseBackups) CreateProtection(ctx context.Context, roomID, name, jobID string, lease *operationlease.Lease) (string, error) {
	id, err := b.base.CreateProtection(ctx, roomID, name, jobID, lease)
	if err == nil {
		b.after()
	}
	return id, err
}
func (b regressionReleaseBackups) VerifyProtection(ctx context.Context, id, room, job string) error {
	return b.base.VerifyProtection(ctx, id, room, job)
}

func TestReleaseRegressionInventoryExpiresDuringProtection(t *testing.T) {
	for _, targetID := range []string{"local", "agent:node"} {
		for _, mode := range []string{"within-window", "expires", "explicit-refresh"} {
			t.Run(targetID+"/"+mode, func(t *testing.T) {
				f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
				snapshot := f.coordinator.planner.snapshots.(fakeReleaseSnapshots).value
				for i := range snapshot.Shards {
					snapshot.Shards[i].Target.ID = targetID
					snapshot.Shards[i].Target.Config.InstallationID = "primary"
				}
				f.runtime.versions[releaseInstallationKey(targetID, "primary")] = "700"
				now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
				received := now
				item := agents.RuntimeTargetInventory{Target: snapshot.Shards[0].Target, Available: true, ReceivedAt: &received, ObservedAt: &received}
				for _, roomID := range []string{"room-a", "room-b"} {
					report := shared.RoomInventoryReport{Directory: "Cluster_" + roomID}
					for _, shard := range snapshot.Shards {
						if shard.Room.ID == roomID {
							report.Shards = append(report.Shards, shared.ShardInventoryReport{Directory: shard.World.DirectoryName})
						}
					}
					item.Inventory.Rooms = append(item.Inventory.Rooms, report)
				}
				source := &regressionReleaseSource{item: item, now: &now}
				observations, err := runtimeobservation.New(source, runtimeobservation.Options{Now: func() time.Time { return now }})
				if err != nil {
					t.Fatal(err)
				}
				defer observations.Close()
				if _, err = observations.Refresh(context.Background(), runtimeobservation.Scope{}); err != nil {
					t.Fatal(err)
				}
				productionSnapshot, err := NewTopologyReleaseSnapshot(regressionReleaseRooms{snapshot}, regressionReleasePlacements{snapshot}, observations)
				if err != nil {
					t.Fatal(err)
				}
				f.coordinator.planner.snapshots = productionSnapshot
				f.coordinator.now = func() time.Time { return now }
				plan, err := f.coordinator.Preview(context.Background(), ReleasePreviewRequest{Policy: ReleasePolicyInput{LoadConfirmation: ReleaseLoadConfirmationNone}})
				if err != nil || !plan.Ready {
					t.Fatalf("preview ready=%v err=%v blockers=%v", plan.Ready, err, plan.Blockers)
				}
				base := f.coordinator.backups.(*coordinatorBackups)
				f.coordinator.backups = regressionReleaseBackups{base: base, after: func() {
					delay := 46 * time.Second
					if mode == "within-window" {
						delay = 30 * time.Second
					}
					now = now.Add(delay)
					if mode == "explicit-refresh" {
						if _, err := observations.Refresh(context.Background(), runtimeobservation.Scope{TargetID: targetID}); err != nil {
							t.Fatal(err)
						}
					}
				}}
				value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "research-expiry", Plan: plan})
				fresh, previewErr := f.coordinator.previewStoredPlan(context.Background(), plan)
				if err != nil || value.Stage != ReleaseStageSucceeded {
					t.Fatalf("release failed stage=%s code=%s err=%v fresh=%v preview=%v", value.Stage, value.ErrorCode, err, fresh.Blockers, previewErr)
				}
				if f.runtime.updateCounts[releaseInstallationKey(targetID, "primary")] != 1 {
					t.Fatal("shared installation was not updated exactly once")
				}
				if source.collects > 6 {
					t.Fatalf("unbounded inventory collection: %d", source.collects)
				}

			})
		}
	}
}

func TestReleaseRegressionImplicitGameModeChangesAfterBackup(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	base := f.coordinator.backups.(*coordinatorBackups)
	f.coordinator.backups = regressionReleaseBackups{base: base, after: func() {
		f.runtime.mu.Lock()
		defer f.runtime.mu.Unlock()
		for key, state := range f.runtime.states {
			if state.State == "running" {
				state.RuntimeMode = shared.RuntimePerformanceModeGame
				f.runtime.states[key] = state
			}
		}
	}}
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "research-mode", Plan: f.plan})
	if err != nil || value.Stage != ReleaseStageSucceeded {
		t.Fatalf("equivalent default mode failed: stage=%s code=%s err=%v", value.Stage, value.ErrorCode, err)
	}
}

type regressionReleaseDiskRuntime struct {
	*coordinatorRuntime
	blocked bool
}

func (r *regressionReleaseDiskRuntime) ObserveInstallation(ctx context.Context, target ReleaseInstallationPlan) (shared.RuntimeGameVersionResult, error) {
	result, err := r.coordinatorRuntime.ObserveInstallation(ctx, target)
	if r.blocked {
		result.AvailableBytes = 0
	}
	return result, err
}
func TestReleaseRegressionBlockedPreflightCanKeepSameHash(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	runtime := &regressionReleaseDiskRuntime{coordinatorRuntime: f.runtime}
	f.coordinator.planner.runtime = runtime
	base := f.coordinator.backups.(*coordinatorBackups)
	f.coordinator.backups = regressionReleaseBackups{base: base, after: func() { runtime.blocked = true }}
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "research-ready", Plan: f.plan})
	if !errors.Is(err, ErrReleasePreviewBlocked) || value.ErrorCode != "DISK_INSUFFICIENT" {
		t.Fatalf("blocked preview was ignored: stage=%s code=%s err=%v", value.Stage, value.ErrorCode, err)
	}
	if f.runtime.updateCounts["agent:node-a\x00primary"] != 0 {
		t.Fatal("unsafe write after dynamic disk blocker")
	}
	for key, status := range f.runtime.states {
		if key != "room-b\x00Archive" && status.State != "running" {
			t.Fatalf("pre-write failure did not restore %s", key)
		}
	}
}
