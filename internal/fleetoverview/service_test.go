package fleetoverview

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"
)

type fleetTopologySource struct {
	snapshot topology.FleetSnapshot
	err      error
}

func (s fleetTopologySource) FleetTopology(context.Context) (topology.FleetSnapshot, error) {
	return s.snapshot, s.err
}

type fleetRooms struct {
	rooms  []rooms.Room
	worlds map[string][]rooms.World
}

func (s fleetRooms) List() ([]rooms.Room, error) { return append([]rooms.Room(nil), s.rooms...), nil }
func (s fleetRooms) Worlds(roomID string) ([]rooms.World, error) {
	return append([]rooms.World(nil), s.worlds[roomID]...), nil
}

type fleetRuntime struct {
	mu       sync.Mutex
	statuses map[string]shared.ShardRuntimeStatus
	failures map[string]error
	calls    []string
}

func (s *fleetRuntime) Status(_ context.Context, roomID, worldID string) (shared.ShardRuntimeStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, roomID+"/"+worldID)
	if err := s.failures[worldID]; err != nil {
		return shared.ShardRuntimeStatus{}, err
	}
	if status, exists := s.statuses[worldID]; exists {
		return status, nil
	}
	return shared.ShardRuntimeStatus{State: "stopped"}, nil
}

func (s *fleetRuntime) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func TestInventorySnapshotUsesProcessStateWithoutWorldProbes(t *testing.T) {
	local := topology.TargetSummary{ID: "local", Online: true, Configured: true, InventoryAvailable: true}
	remote := topology.TargetSummary{ID: "agent:node", Online: true, Configured: true, InventoryAvailable: true}
	room := rooms.Room{ID: "room", Name: "room", Managed: true}
	master := rooms.World{ID: "master", RoomID: room.ID, Role: rooms.WorldRoleMaster}
	caves := rooms.World{ID: "caves", RoomID: room.ID, Role: rooms.WorldRoleCaves}
	for _, scenario := range []struct {
		name   string
		target topology.TargetSummary
		status string
	}{
		{"online", remote, "running"},
		{"offline", topology.TargetSummary{ID: remote.ID, Configured: true, InventoryAvailable: true}, "unknown"},
		{"stale", topology.TargetSummary{ID: remote.ID, Online: true, Configured: true, InventoryAvailable: true, InventoryStale: true}, "unknown"},
		{"missing", topology.TargetSummary{ID: remote.ID, Online: true, Configured: true}, "unknown"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			runtime := &fleetRuntime{}
			service, err := New(fleetTopologySource{snapshot: topology.FleetSnapshot{
				Targets: []topology.TargetSummary{local, scenario.target},
				Rooms: []topology.Snapshot{{RoomID: room.ID, Placements: []topology.Placement{
					{WorldID: master.ID, AppliedTargetID: local.ID, State: topology.PlacementAligned, Running: true},
					{WorldID: caves.ID, AppliedTargetID: remote.ID, State: topology.PlacementAligned, Running: true},
				}}},
			}}, fleetRooms{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {master, caves}}}, runtime)
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.InventorySnapshot(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Rooms) != 1 || len(result.Rooms[0].Worlds) != 2 || !result.Rooms[0].MixedPlacement {
				t.Fatalf("unexpected room projection: %#v", result.Rooms)
			}
			if result.Rooms[0].Worlds[0].Status != "running" || result.Rooms[0].Worlds[1].Status != scenario.status {
				t.Fatalf("inventory statuses = %#v", result.Rooms[0].Worlds)
			}
			if runtime.callCount() != 0 {
				t.Fatalf("inventory-only overview made %d world probes", runtime.callCount())
			}
			full, err := service.Snapshot(context.Background(), "")
			if err != nil || runtime.callCount() == 0 || full.Rooms[0].Worlds[0].Status != "stopped" {
				t.Fatalf("full overview no longer probes runtime: result=%#v err=%v calls=%d", full, err, runtime.callCount())
			}
		})
	}
}

func TestSnapshotFiltersAppliedPlacementsAndKeepsMixedRoomContext(t *testing.T) {
	now := time.Now().UTC()
	local := topology.TargetSummary{
		ID: "local", Name: "本机", Kind: agents.RuntimeKindLocal, Online: true, Configured: true,
		InventoryAvailable: true,
	}
	remote := topology.TargetSummary{
		ID: "agent:debian12", Name: "Debian12", Kind: agents.RuntimeKindAgent, Online: true, Configured: true,
		InventoryAvailable: true,
	}
	room := rooms.Room{ID: "room-1", DirectoryName: "Cluster_1", Name: "测试房间", Managed: true, WorldCount: 2}
	master := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}
	caves := rooms.World{ID: "caves", RoomID: room.ID, DirectoryName: "Caves", Name: "Caves", Role: rooms.WorldRoleCaves}
	service, err := New(fleetTopologySource{snapshot: topology.FleetSnapshot{
		Targets: []topology.TargetSummary{local, remote}, ObservedAt: now,
		Rooms: []topology.Snapshot{{
			RoomID: room.ID, Revision: "revision-1", Targets: []topology.TargetSummary{local, remote},
			Placements: []topology.Placement{
				{WorldID: master.ID, WorldName: master.Name, AppliedTargetID: local.ID, DesiredTargetID: local.ID, State: topology.PlacementAligned, Running: true},
				{WorldID: caves.ID, WorldName: caves.Name, AppliedTargetID: remote.ID, DesiredTargetID: remote.ID, State: topology.PlacementAligned},
			},
		}},
	}}, fleetRooms{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {master, caves}}}, &fleetRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Snapshot(context.Background(), remote.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scope.TargetID != remote.ID || len(result.Targets) != 1 || len(result.Rooms) != 1 {
		t.Fatalf("unexpected scoped result: %#v", result)
	}
	if got := result.Rooms[0]; len(got.Worlds) != 1 || got.Worlds[0].ID != caves.ID || !got.MixedPlacement || len(got.TargetIDs) != 2 {
		t.Fatalf("unexpected room projection: %#v", got)
	}
	if result.Summary.Worlds != 1 || result.Summary.Stopped != 1 || result.Summary.Running != 0 {
		t.Fatalf("unexpected summary: %#v", result.Summary)
	}
	if result.Summary.Targets != 1 || result.Summary.OnlineTargets != 1 {
		t.Fatalf("summary target counts must follow the selected scope: %#v", result.Summary)
	}
}

func TestSnapshotRejectsUnknownTarget(t *testing.T) {
	service, err := New(fleetTopologySource{snapshot: topology.FleetSnapshot{
		Targets: []topology.TargetSummary{{ID: "local"}}, ObservedAt: time.Now().UTC(),
	}}, fleetRooms{}, &fleetRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Snapshot(context.Background(), "agent:missing")
	if !errors.Is(err, ErrRuntimeTargetNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestSnapshotKeepsOfflineTargetWorldVisibleAsUnknown(t *testing.T) {
	remote := topology.TargetSummary{ID: "agent:offline", Name: "离线节点", Configured: true, Online: false}
	room := rooms.Room{ID: "room", Name: "房间", Managed: true}
	world := rooms.World{ID: "world", RoomID: room.ID, Name: "Master"}
	runtime := &fleetRuntime{}
	service, err := New(fleetTopologySource{snapshot: topology.FleetSnapshot{
		Targets: []topology.TargetSummary{remote}, ObservedAt: time.Now().UTC(),
		Rooms: []topology.Snapshot{{RoomID: room.ID, Placements: []topology.Placement{{
			WorldID: world.ID, AppliedTargetID: remote.ID, DesiredTargetID: remote.ID, State: topology.PlacementTargetOffline,
		}}}},
	}}, fleetRooms{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Snapshot(context.Background(), remote.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rooms) != 1 || result.Rooms[0].Worlds[0].Status != "unknown" || result.Rooms[0].Worlds[0].ControlAvailable {
		t.Fatalf("offline world was not preserved: %#v", result)
	}
	if runtime.callCount() != 0 {
		t.Fatalf("offline target must not trigger runtime status calls: %#v", runtime.calls)
	}
}

func TestSnapshotShowsInventoryCollectionFailureOnWorld(t *testing.T) {
	target := topology.TargetSummary{
		ID: "agent:node", Name: "node", Online: true, Configured: true,
		InventoryAvailable: true, InventoryStale: true, ObservationError: "I/O Operation Failed",
	}
	room := rooms.Room{ID: "room", Name: "room", Managed: true}
	world := rooms.World{ID: "master", RoomID: room.ID, Name: "Master"}
	service, err := New(fleetTopologySource{snapshot: topology.FleetSnapshot{
		Targets: []topology.TargetSummary{target}, ObservedAt: time.Now().UTC(),
		Rooms: []topology.Snapshot{{RoomID: room.ID, Placements: []topology.Placement{{
			WorldID: world.ID, AppliedTargetID: target.ID, DesiredTargetID: target.ID,
			State: topology.PlacementInventoryStale,
		}}}},
	}}, fleetRooms{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}}, &fleetRuntime{})
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.Snapshot(context.Background(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Rooms[0].Worlds[0]; got.Status != "unknown" || got.StatusMessage != "I/O Operation Failed" {
		t.Fatalf("world did not expose the collection failure: %#v", got)
	}
}

func TestSnapshotDeduplicatesIdenticalTopologyProblems(t *testing.T) {
	target := topology.TargetSummary{ID: "local", Online: true, Configured: true, InventoryAvailable: true}
	room := rooms.Room{ID: "room", Managed: true}
	world := rooms.World{ID: "world", RoomID: room.ID}
	problem := topology.Issue{Code: "PLACEMENT_WARNING", TargetID: target.ID, WorldID: world.ID, Severity: topology.SeverityWarning, Message: "warning"}
	service, err := New(fleetTopologySource{snapshot: topology.FleetSnapshot{
		Targets: []topology.TargetSummary{target}, ObservedAt: time.Now().UTC(),
		Rooms: []topology.Snapshot{{
			RoomID: room.ID,
			Placements: []topology.Placement{{
				WorldID: world.ID, AppliedTargetID: target.ID, DesiredTargetID: target.ID, State: topology.PlacementAligned,
			}},
			Issues: []topology.Issue{problem, problem},
		}},
	}}, fleetRooms{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}}, &fleetRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Issues) != 1 {
		t.Fatalf("issues = %#v", result.Issues)
	}
}

func TestSnapshotIncludesDiscoveredRoomWithoutTopologyInTargetScope(t *testing.T) {
	remote := topology.TargetSummary{
		ID: "agent:debian12", Name: "Debian12", Kind: agents.RuntimeKindAgent,
		Online: true, Configured: true, InventoryAvailable: true,
	}
	room := rooms.Room{
		ID: "remote-room", DirectoryName: "Cluster_Remote", Name: "远程房间", Managed: false,
		TargetIDs: []string{remote.ID}, AvailableTargetIDs: []string{remote.ID},
	}
	world := rooms.World{
		ID: "remote-master", RoomID: room.ID, DirectoryName: "Master", Name: "Master",
		Role: rooms.WorldRoleMaster, TargetIDs: []string{remote.ID}, AvailableTargetIDs: []string{remote.ID},
	}
	service, err := New(fleetTopologySource{snapshot: topology.FleetSnapshot{
		Targets: []topology.TargetSummary{remote}, ObservedAt: time.Now().UTC(),
	}}, fleetRooms{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}}, &fleetRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Snapshot(context.Background(), remote.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rooms) != 1 || !result.Rooms[0].Managed || result.Rooms[0].ControlState != "unavailable" || len(result.Rooms[0].Worlds) != 1 {
		t.Fatalf("room without topology missing: %#v", result)
	}
	got := result.Rooms[0].Worlds[0]
	if got.Status != "unknown" || got.ControlAvailable || got.Placement.AppliedTargetID != remote.ID || got.Target == nil {
		t.Fatalf("unexpected unavailable world: %#v", got)
	}
	if result.Summary.Attention != 1 || len(result.Issues) != 1 || result.Issues[0].Code != "ROOM_TOPOLOGY_UNAVAILABLE" {
		t.Fatalf("unexpected unavailable summary: %#v %#v", result.Summary, result.Issues)
	}
}

func TestSnapshotKeepsRunningSaveFailureControllableAndCountsAttention(t *testing.T) {
	target := topology.TargetSummary{
		ID: "local", Name: "本机", Kind: agents.RuntimeKindLocal, Online: true, Configured: true,
		InventoryAvailable: true,
	}
	room := rooms.Room{ID: "room", Name: "房间", Managed: true}
	world := rooms.World{ID: "master", RoomID: room.ID, Name: "Master"}
	runtime := &fleetRuntime{statuses: map[string]shared.ShardRuntimeStatus{
		world.ID: {
			State: "running", Code: "SAVE_WRITE_FAILED", Message: "session directory is not writable",
			SessionExists: true,
		},
	}}
	service, err := New(fleetTopologySource{snapshot: topology.FleetSnapshot{
		Targets: []topology.TargetSummary{target}, ObservedAt: time.Now().UTC(),
		Rooms: []topology.Snapshot{{RoomID: room.ID, Placements: []topology.Placement{{
			WorldID: world.ID, AppliedTargetID: target.ID, DesiredTargetID: target.ID,
			State: topology.PlacementAligned, Running: true,
		}}}},
	}}, fleetRooms{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	got := result.Rooms[0].Worlds[0]
	if got.Status != "running" || got.StatusCode != "SAVE_WRITE_FAILED" || !got.ControlAvailable || got.StatusMessage == "" {
		t.Fatalf("save failure was not preserved as a controllable running world: %#v", got)
	}
	if result.Summary.Running != 1 || result.Summary.Stopped != 0 || result.Summary.Attention != 1 {
		t.Fatalf("unexpected save failure summary: %#v", result.Summary)
	}
}

func TestSnapshotContainsRuntimeStatusFailureToOneWorld(t *testing.T) {
	target := topology.TargetSummary{ID: "local", Online: true, Configured: true, InventoryAvailable: true}
	room := rooms.Room{ID: "room", Managed: true}
	world := rooms.World{ID: "master", RoomID: room.ID}
	service, err := New(fleetTopologySource{snapshot: topology.FleetSnapshot{
		Targets: []topology.TargetSummary{target}, ObservedAt: time.Now().UTC(),
		Rooms: []topology.Snapshot{{RoomID: room.ID, Placements: []topology.Placement{{
			WorldID: world.ID, AppliedTargetID: target.ID, DesiredTargetID: target.ID, State: topology.PlacementAligned,
		}}}},
	}}, fleetRooms{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}}, &fleetRuntime{
		failures: map[string]error{world.ID: errors.New("runtime timed out")},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	got := result.Rooms[0].Worlds[0]
	if got.Status != "unknown" || got.ControlAvailable || got.StatusMessage != "runtime timed out" {
		t.Fatalf("runtime failure leaked or retained stale inventory state: %#v", got)
	}
	if result.Summary.Attention != 1 {
		t.Fatalf("unexpected runtime failure summary: %#v", result.Summary)
	}
}
