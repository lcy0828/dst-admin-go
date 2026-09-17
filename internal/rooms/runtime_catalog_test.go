package rooms

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"dont/shared"
)

func runtimeCatalogSource(targetID string, online, stale bool, roomDirectory string, shards ...shared.ShardInventoryReport) RuntimeCatalogSource {
	now := time.Now().UTC()
	return RuntimeCatalogSource{
		TargetID: targetID, Online: online, Available: true, Stale: stale, ObservedAt: now,
		Inventory: shared.RuntimeInventoryReport{
			ObservedAt: now,
			Rooms:      []shared.RoomInventoryReport{{Directory: roomDirectory, Name: "远程生存服", Shards: shards}},
		},
	}
}

func TestRuntimeCatalogAutomaticallyRegistersRemoteOnlyRoom(t *testing.T) {
	service, _ := newTestService(t)
	service.ConfigureLocalDiscovery(false)
	source := runtimeCatalogSource("agent:debian12", true, false, "Cluster_Remote",
		shared.ShardInventoryReport{Directory: "Master", Name: "Master", Role: "master", ServerPort: 10999},
		shared.ShardInventoryReport{Directory: "Caves", Name: "Caves", Role: "secondary", ServerPort: 11000},
	)
	if err := service.SyncRuntimeCatalog([]RuntimeCatalogSource{source}); err != nil {
		t.Fatal(err)
	}
	items, err := service.List()
	if err != nil || len(items) != 1 {
		t.Fatalf("rooms=%#v err=%v", items, err)
	}
	room := items[0]
	if !room.Managed || room.ControlState != "ready" || !room.ControlAvailable || room.DirectoryName != "Cluster_Remote" || !reflect.DeepEqual(room.TargetIDs, []string{"agent:debian12"}) {
		t.Fatalf("remote room=%#v", room)
	}
	worlds, err := service.Worlds(room.ID)
	if err != nil || len(worlds) != 2 || worlds[0].Role != WorldRoleMaster || worlds[1].Role != WorldRoleCaves {
		t.Fatalf("worlds=%#v err=%v", worlds, err)
	}
	if _, err := service.CreateWorld(room.ID, CreateWorldRequest{DirectoryName: "Extra", Type: "forest"}); err == nil {
		t.Fatal("remote-only room unexpectedly mutated the controller save path")
	}
}

func TestRuntimeCatalogPreservesArbitraryWorldTopology(t *testing.T) {
	service, _ := newTestService(t)
	service.ConfigureLocalDiscovery(false)
	source := runtimeCatalogSource("agent:mixed", true, false, "Cluster_Mixed",
		shared.ShardInventoryReport{Directory: "CavePrime", Name: "Cave Prime", ID: 7, Role: "master", Type: "cave", ServerPort: 12007},
		shared.ShardInventoryReport{Directory: "DeepTwo", Name: "Deep Two", ID: 9, Role: "secondary", Type: "cave", ServerPort: 12009},
		shared.ShardInventoryReport{Directory: "ForestThree", Name: "Forest Three", ID: 3, Role: "secondary", Type: "forest", ServerPort: 12003},
		shared.ShardInventoryReport{Directory: "ForestFive", Name: "Forest Five", ID: 5, Role: "secondary", Type: "forest", ServerPort: 12005},
	)
	if err := service.SyncRuntimeCatalog([]RuntimeCatalogSource{source}); err != nil {
		t.Fatal(err)
	}
	worlds, err := service.Worlds(EncodeID("Cluster_Mixed"))
	if err != nil || len(worlds) != 4 {
		t.Fatalf("worlds=%#v err=%v", worlds, err)
	}
	byDirectory := make(map[string]World, len(worlds))
	for _, world := range worlds {
		byDirectory[world.DirectoryName] = world
	}
	master := byDirectory["CavePrime"]
	if !master.IsMaster || master.Role != WorldRoleMaster || master.Type != WorldTypeCave || master.ShardID != 7 || master.ServerPort != 12007 {
		t.Fatalf("Cave Master=%#v", master)
	}
	if world := byDirectory["DeepTwo"]; world.IsMaster || world.Role != WorldRoleCaves || world.Type != WorldTypeCave || world.ShardID != 9 {
		t.Fatalf("secondary Cave=%#v", world)
	}
	for _, directory := range []string{"ForestThree", "ForestFive"} {
		world := byDirectory[directory]
		if world.IsMaster || world.Role != WorldRoleCustom || world.Type != WorldTypeForest || world.ShardID == 0 {
			t.Fatalf("secondary Forest %s=%#v", directory, world)
		}
	}
}

func TestRuntimeCatalogOldAgentDoesNotReportMasterAsForest(t *testing.T) {
	service, _ := newTestService(t)
	service.ConfigureLocalDiscovery(false)
	source := runtimeCatalogSource("agent:old", true, false, "Cluster_Old",
		shared.ShardInventoryReport{Directory: "Primary", Name: "Primary", ID: 4, Role: "master"},
	)
	if err := service.SyncRuntimeCatalog([]RuntimeCatalogSource{source}); err != nil {
		t.Fatal(err)
	}
	worlds, err := service.Worlds(EncodeID("Cluster_Old"))
	if err != nil || len(worlds) != 1 || worlds[0].Type != WorldTypeUnknown || !worlds[0].IsMaster || worlds[0].ShardID != 4 {
		t.Fatalf("old Agent world=%#v err=%v", worlds, err)
	}
}

func TestRuntimeCatalogMergesSplitRoomAcrossTargets(t *testing.T) {
	service, _ := newTestService(t)
	service.ConfigureLocalDiscovery(false)
	master := runtimeCatalogSource("agent:master", true, false, "Cluster_Split",
		shared.ShardInventoryReport{Directory: "Master", Name: "Master", Role: "master"},
	)
	caves := runtimeCatalogSource("agent:caves", true, false, "Cluster_Split",
		shared.ShardInventoryReport{Directory: "Caves", Name: "Caves", Role: "secondary"},
	)
	if err := service.SyncRuntimeCatalog([]RuntimeCatalogSource{master, caves}); err != nil {
		t.Fatal(err)
	}
	roomID := EncodeID("Cluster_Split")
	room, err := service.Room(roomID)
	if err != nil {
		t.Fatal(err)
	}
	if room.WorldCount != 2 || !reflect.DeepEqual(room.TargetIDs, []string{"agent:caves", "agent:master"}) {
		t.Fatalf("room=%#v", room)
	}
	worlds, err := service.Worlds(roomID)
	if err != nil || len(worlds) != 2 {
		t.Fatalf("worlds=%#v err=%v", worlds, err)
	}
	if !reflect.DeepEqual(worlds[0].TargetIDs, []string{"agent:master"}) || !reflect.DeepEqual(worlds[1].TargetIDs, []string{"agent:caves"}) {
		t.Fatalf("split placements lost: %#v", worlds)
	}
}

func TestRuntimeCatalogRetainsManagedOfflineRoomAndDropsForgottenDiscovery(t *testing.T) {
	service, _ := newTestService(t)
	service.ConfigureLocalDiscovery(false)
	source := runtimeCatalogSource("agent:offline", true, false, "Cluster_Offline",
		shared.ShardInventoryReport{Directory: "Master", Name: "Master", Role: "master"},
	)
	if err := service.SyncRuntimeCatalog([]RuntimeCatalogSource{source}); err != nil {
		t.Fatal(err)
	}
	roomID := EncodeID("Cluster_Offline")
	offline := source
	offline.Online, offline.Stale = false, true
	if err := service.SyncRuntimeCatalog([]RuntimeCatalogSource{offline}); err != nil {
		t.Fatal(err)
	}
	room, err := service.Room(roomID)
	if err != nil || len(room.TargetIDs) != 1 || len(room.AvailableTargetIDs) != 0 {
		t.Fatalf("offline room=%#v err=%v", room, err)
	}
	if err := service.SyncRuntimeCatalog(nil); err != nil {
		t.Fatal(err)
	}
	room, err = service.Room(roomID)
	if err != nil || !room.Managed || room.ControlState != "offline" || room.ControlAvailable || !reflect.DeepEqual(room.TargetIDs, []string{"agent:offline"}) {
		t.Fatalf("retained room=%#v err=%v", room, err)
	}
	if err := service.Unadopt(roomID); err != nil {
		t.Fatal(err)
	}
	if err := service.SyncRuntimeCatalog(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Room(roomID); err != ErrRoomNotFound {
		t.Fatalf("forgotten discovery error=%v", err)
	}
}

func TestRuntimeCatalogRemovesManagedRoomAfterKnownTargetConfirmsAbsence(t *testing.T) {
	service, _ := newTestService(t)
	service.ConfigureLocalDiscovery(false)
	source := runtimeCatalogSource("agent:online", true, false, "Cluster_Removed",
		shared.ShardInventoryReport{Directory: "Master", Name: "Master", Role: "master"},
	)
	if err := service.SyncRuntimeCatalog([]RuntimeCatalogSource{source}); err != nil {
		t.Fatal(err)
	}
	roomID := EncodeID("Cluster_Removed")
	removed := []string{}
	service.AddManagedRoomLifecycle(nil, func(value string) { removed = append(removed, value) })
	source.Inventory.Rooms = nil
	source.ObservedAt = source.ObservedAt.Add(time.Second)
	source.Inventory.ObservedAt = source.ObservedAt

	if err := service.SyncRuntimeCatalog([]RuntimeCatalogSource{source}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Room(roomID); err != ErrRoomNotFound {
		t.Fatalf("removed room error=%v", err)
	}
	if managed, err := service.store.IsManaged(roomID); err != nil || managed {
		t.Fatalf("managed=%v err=%v", managed, err)
	}
	if !reflect.DeepEqual(removed, []string{roomID}) {
		t.Fatalf("unmanaged notifications=%#v", removed)
	}
}

func TestRuntimeCatalogRetainsSplitRoomUntilEveryKnownTargetConfirmsAbsence(t *testing.T) {
	service, _ := newTestService(t)
	service.ConfigureLocalDiscovery(false)
	master := runtimeCatalogSource("agent:master", true, false, "Cluster_Split_Removed",
		shared.ShardInventoryReport{Directory: "Master", Name: "Master", Role: "master"},
	)
	caves := runtimeCatalogSource("agent:caves", true, false, "Cluster_Split_Removed",
		shared.ShardInventoryReport{Directory: "Caves", Name: "Caves", Role: "secondary"},
	)
	if err := service.SyncRuntimeCatalog([]RuntimeCatalogSource{master, caves}); err != nil {
		t.Fatal(err)
	}
	roomID := EncodeID("Cluster_Split_Removed")
	master.Inventory.Rooms = nil
	caves.Online, caves.Stale = false, true
	if err := service.SyncRuntimeCatalog([]RuntimeCatalogSource{master, caves}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Room(roomID); err != nil {
		t.Fatalf("room removed while one known target was unconfirmed: %v", err)
	}
	caves.Online, caves.Stale = true, false
	caves.Inventory.Rooms = nil
	if err := service.SyncRuntimeCatalog([]RuntimeCatalogSource{master, caves}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Room(roomID); err != ErrRoomNotFound {
		t.Fatalf("room remained after every target confirmed absence: %v", err)
	}
}

func TestRuntimeCatalogRetainsSplitWorldUntilItsTargetConfirmsAbsence(t *testing.T) {
	service, _ := newTestService(t)
	service.ConfigureLocalDiscovery(false)
	master := runtimeCatalogSource("agent:master", true, false, "Cluster_Split_World",
		shared.ShardInventoryReport{Directory: "Master", Name: "Master", Role: "master"},
	)
	caves := runtimeCatalogSource("agent:caves", true, false, "Cluster_Split_World",
		shared.ShardInventoryReport{Directory: "Caves", Name: "Caves", Role: "secondary"},
	)
	if err := service.SyncRuntimeCatalog([]RuntimeCatalogSource{master, caves}); err != nil {
		t.Fatal(err)
	}
	roomID := EncodeID("Cluster_Split_World")

	caves.Online, caves.Available, caves.Stale = false, false, true
	caves.Inventory.Rooms = nil
	if err := service.SyncRuntimeCatalog([]RuntimeCatalogSource{master, caves}); err != nil {
		t.Fatal(err)
	}
	worlds, err := service.Worlds(roomID)
	if err != nil || len(worlds) != 2 {
		t.Fatalf("worlds removed before the Caves target confirmed absence: %#v error=%v", worlds, err)
	}
	if len(worlds[1].AvailableTargetIDs) != 0 || !reflect.DeepEqual(worlds[1].TargetIDs, []string{"agent:caves"}) {
		t.Fatalf("unavailable Caves placement=%#v", worlds[1])
	}

	caves.Online, caves.Available, caves.Stale = true, true, false
	if err := service.SyncRuntimeCatalog([]RuntimeCatalogSource{master, caves}); err != nil {
		t.Fatal(err)
	}
	worlds, err = service.Worlds(roomID)
	if err != nil || len(worlds) != 1 || worlds[0].DirectoryName != "Master" {
		t.Fatalf("Caves remained after its target confirmed absence: %#v error=%v", worlds, err)
	}
}

func TestControllerOnlyCreatePersistsManagedRoomWithoutLocalRuntimeSource(t *testing.T) {
	service, _ := newTestService(t)
	service.ConfigureLocalDiscovery(false)

	created, err := service.Create(CreateRequest{
		DirectoryName: "controller_template",
		Name:          "控制端配置模板",
		GameMode:      "survival",
		MaxPlayers:    6,
		IncludeCaves:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created.Managed || len(created.TargetIDs) != 0 || len(created.AvailableTargetIDs) != 0 {
		t.Fatalf("created room=%#v", created)
	}

	if err := service.SyncRuntimeCatalog(nil); err != nil {
		t.Fatal(err)
	}
	rooms, err := service.List()
	if err != nil || len(rooms) != 1 || rooms[0].ID != created.ID || !rooms[0].Managed {
		t.Fatalf("rooms=%#v err=%v", rooms, err)
	}
	if len(rooms[0].TargetIDs) != 0 || len(rooms[0].AvailableTargetIDs) != 0 {
		t.Fatalf("controller template unexpectedly became a runtime source: %#v", rooms[0])
	}
	worlds, err := service.Worlds(created.ID)
	if err != nil || len(worlds) != 2 {
		t.Fatalf("worlds=%#v err=%v", worlds, err)
	}
	for _, world := range worlds {
		if len(world.TargetIDs) != 0 || len(world.AvailableTargetIDs) != 0 {
			t.Fatalf("world unexpectedly became a runtime source: %#v", world)
		}
	}
}

func TestControllerOnlyCatalogTracksLocalTemplateMutationsImmediately(t *testing.T) {
	service, _ := newTestService(t)
	service.ConfigureLocalDiscovery(false)

	room, err := service.Create(CreateRequest{
		DirectoryName: "catalog_mutations",
		Name:          "目录一致性测试",
		GameMode:      "survival",
		MaxPlayers:    6,
	})
	if err != nil {
		t.Fatal(err)
	}
	world, err := service.CreateWorld(room.ID, CreateWorldRequest{DirectoryName: "Caves", Type: "cave"})
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogWorldCount(t, service, room.ID, 2)

	deletedWorld, err := service.DeleteWorld(room.ID, world.ID, DeleteWorldRequest{Confirmation: room.Name})
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogWorldCount(t, service, room.ID, 1)
	if _, err := service.RestoreWorld(room.ID, filepath.Base(deletedWorld.RecoveryName)); err != nil {
		t.Fatal(err)
	}
	assertCatalogWorldCount(t, service, room.ID, 2)

	deletedRoom, err := service.DeleteRoom(room.ID, DeleteRoomRequest{Confirmation: room.Name})
	if err != nil {
		t.Fatal(err)
	}
	if rooms, err := service.List(); err != nil || len(rooms) != 0 {
		t.Fatalf("rooms after delete=%#v err=%v", rooms, err)
	}
	if _, err := service.RestoreRoom(filepath.Base(deletedRoom.RecoveryName)); err != nil {
		t.Fatal(err)
	}
	rooms, err := service.List()
	if err != nil || len(rooms) != 1 || rooms[0].ID != room.ID || !rooms[0].Managed {
		t.Fatalf("rooms after restore=%#v err=%v", rooms, err)
	}
	assertCatalogWorldCount(t, service, room.ID, 2)
}

func assertCatalogWorldCount(t *testing.T, service *Service, roomID string, want int) {
	t.Helper()
	worlds, err := service.Worlds(roomID)
	if err != nil || len(worlds) != want {
		t.Fatalf("worlds=%#v err=%v want=%d", worlds, err, want)
	}
}
