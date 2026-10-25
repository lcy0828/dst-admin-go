package saveimport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/distributedbackup"
	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/runtimefiles"
	"dont/shared"
)

func analyzedSave(t *testing.T, app applyTestApp, entries []archiveTestEntry) Session {
	t.Helper()
	value, err := app.service.Upload(context.Background(), "Import", "save.zip", bytes.NewReader(createZIP(t, entries)))
	if err != nil {
		t.Fatal(err)
	}
	value, err = app.service.Analyze(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func importSaveEntries() []archiveTestEntry {
	return []archiveTestEntry{
		{name: "cluster.ini", content: clusterINI("Imported")},
		{name: "cluster_token.txt", content: "source-token-1234567890"},
		{name: "Master/server.ini", content: serverINI(true, 1, 10999)},
		{name: "Master/save/session/ACTIVE/0000000001", content: "world-snapshot"},
	}
}

func TestImportRejectsIncompleteSaveButAllowsTemplatesAndLegacyIndex(t *testing.T) {
	for _, tc := range []struct {
		name, index, content string
		template, blocked    bool
	}{
		{name: "missing index", blocked: true},
		{name: "corrupt index", index: "shardindex", content: "broken", blocked: true},
		{name: "wrong session", index: "shardindex", content: `return {session_id="OTHER"}`, blocked: true},
		{name: "empty index with snapshots", index: "shardindex", content: `return {}`, blocked: true},
		{name: "selected save", index: "shardindex", content: `return {session_id="ACTIVE"}`},
		{name: "legacy save", index: "saveindex", content: `return {slots={{session_id="ACTIVE"}}}`},
		{name: "configuration template", template: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newApplyTestApp(t)
			entries := importSaveEntries()
			if tc.template {
				entries = entries[:3]
			}
			if tc.index != "" {
				entries = append(entries, archiveTestEntry{name: "Master/save/" + tc.index, content: tc.content})
			}
			value := analyzedSave(t, app, entries)
			candidate := value.Manifest.Candidates[0]
			if got := candidate.Compatibility == "blocked"; got != tc.blocked {
				t.Fatalf("candidate=%+v", candidate)
			}
			result, err := app.service.Apply(context.Background(), value.ID, "test", ApplyRequest{CandidateID: candidate.ID, Mode: ApplyModeNew, DirectoryName: "Imported", TokenPolicy: TokenSource, NetworkPolicy: NetworkAuto, ModPolicy: ModsPreserve})
			if tc.blocked {
				if !errors.Is(err, ErrInvalidArchive) {
					t.Fatalf("incomplete save accepted: %+v %v", result, err)
				}
				if _, err := os.Stat(filepath.Join(app.saveRoot, "Imported")); !os.IsNotExist(err) {
					t.Fatalf("invalid save published: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			manager, _ := dstruntime.NewManager(app.saveRoot, app.rooms)
			status, err := manager.StatusRoom(result.RoomID)
			if err != nil || len(status) != 1 || status[0].State != dstruntime.InstallStateInstalled {
				t.Fatalf("collector=%+v %v", status, err)
			}
		})
	}
}

func TestApplyRechecksSaveIndexAndPreservesOriginalOnFailure(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedRoom(t, app, "Target", "Target", "target-token-1234567890", 12001, "original")
	entries := append(importSaveEntries(), archiveTestEntry{name: "Master/save/shardindex", content: `return {session_id="ACTIVE"}`})
	value := analyzedSave(t, app, entries)
	path := filepath.Join(app.importRoot, value.ID, "content", filepath.FromSlash(value.Manifest.Candidates[0].Root), "Master/save/shardindex")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err := app.service.Apply(context.Background(), value.ID, "test", ApplyRequest{CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeReplace, TargetRoomID: target.ID, Confirmation: target.Name, TokenPolicy: TokenPreserve, NetworkPolicy: NetworkPreserve, ModPolicy: ModsPreserve})
	if !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("staging not revalidated: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(app.saveRoot, "Target/Master/save/session/old/0000000001"))
	if err != nil || string(data) != "original" {
		t.Fatalf("original save changed: %q %v", data, err)
	}
}

func TestReplacementPreservesCollectorChoiceAndUserLua(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "opted-out"}[enabled], func(t *testing.T) {
			app := newApplyTestApp(t)
			target := createManagedRoom(t, app, "Target", "Target", "target-token-1234567890", 12001, "original")
			path := filepath.Join(app.saveRoot, "Target/Master/customcommands.lua")
			user := `print("user hook")` + "\n"
			if err := os.WriteFile(path, []byte(user), 0600); err != nil {
				t.Fatal(err)
			}
			manager, _ := dstruntime.NewManager(app.saveRoot, app.rooms)
			if enabled {
				if _, err := manager.InstallWorld(context.Background(), target.ID, rooms.EncodeID("Master")); err != nil {
					t.Fatal(err)
				}
			}
			entries := append(importSaveEntries(), archiveTestEntry{name: "Master/save/shardindex", content: `return {session_id="ACTIVE"}`})
			value := analyzedSave(t, app, entries)
			_, err := app.service.Apply(context.Background(), value.ID, "test", ApplyRequest{CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeReplace, TargetRoomID: target.ID, Confirmation: target.Name, TokenPolicy: TokenPreserve, NetworkPolicy: NetworkPreserve, ModPolicy: ModsPreserve})
			if err != nil {
				t.Fatal(err)
			}
			status, err := manager.StatusRoom(target.ID)
			if err != nil || len(status) != 1 || (status[0].State == dstruntime.InstallStateInstalled) != enabled {
				t.Fatalf("collector=%+v %v", status, err)
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(data, []byte(user[:len(user)-1])) {
				t.Fatalf("user Lua lost: %q %v", data, err)
			}
		})
	}
}

func TestImportedMultipleSessionsUseGameIndexForTelemetry(t *testing.T) {
	app := newApplyTestApp(t)
	entries := append(importSaveEntries(),
		archiveTestEntry{name: "Master/save/shardindex", content: `return {session_id="ACTIVE"}`},
		archiveTestEntry{name: "Master/save/session/UNUSED/0000000042", content: "unused snapshot"},
	)
	value := analyzedSave(t, app, entries)
	result, err := app.service.Apply(context.Background(), value.ID, "test", ApplyRequest{CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeNew, DirectoryName: "Imported", TokenPolicy: TokenSource, NetworkPolicy: NetworkAuto, ModPolicy: ModsPreserve})
	if err != nil {
		t.Fatal(err)
	}
	world := filepath.Join(app.saveRoot, "Imported/Master")
	now := time.Now().Truncate(time.Second)
	if err := os.Chtimes(filepath.Join(world, "save/session/UNUSED"), now.Add(time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world, "server_log.txt"), []byte("[00:00:00]: Current time: "+now.Add(-time.Minute).In(time.Local).Format(time.ANSIC)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := dstruntime.WorldStateSnapshot{SchemaVersion: dstruntime.ProtocolVersion, ProducerVersion: dstruntime.RuntimeVersion, ProducerInstanceID: "current", SessionID: "ACTIVE", ShardID: "1", Sequence: 1, CapturedAtUnix: now.Unix(), Complete: true, Season: "autumn"}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world, "save/mod_config_data/dst-admin/worldstate-a.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	files, err := runtimefiles.ReadWorldState(context.Background(), app.saveRoot, "Imported", "Master", shared.ShardRuntimeStatus{State: "running"})
	if err != nil || files.ReadError != "" || files.SessionID != "ACTIVE" {
		t.Fatalf("wrong runtime identity: %+v %v", files, err)
	}
	manager, err := dstruntime.NewManager(app.saveRoot, app.rooms)
	if err != nil {
		t.Fatal(err)
	}
	got, err := manager.ReadWorldState(context.Background(), result.RoomID, rooms.EncodeID("Master"))
	if err != nil || got.SessionID != "ACTIVE" {
		t.Fatalf("valid sample rejected: %+v %v", got, err)
	}
}

type importTargetReader struct {
	roots   map[string]string
	cluster string
	err     error
}

func (r importTargetReader) ReadConfiguration(ctx context.Context, _, worldID, scope string) (runtimedriver.ConfigurationSnapshot, error) {
	if r.err != nil {
		return runtimedriver.ConfigurationSnapshot{}, r.err
	}
	world, err := rooms.DecodeID(worldID)
	if err != nil {
		return runtimedriver.ConfigurationSnapshot{}, err
	}
	result, err := runtimefiles.ReadConfiguration(ctx, r.roots[world], r.cluster, world, scope)
	return runtimedriver.ConfigurationSnapshot{Target: runtimedriver.Target{TargetID: r.roots[world], InstallationID: "default", Cluster: r.cluster, Shard: world}, Result: result}, err
}
func (r importTargetReader) RevealClusterToken(ctx context.Context, _, worldID string) (shared.RuntimeClusterTokenReveal, error) {
	if r.err != nil {
		return shared.RuntimeClusterTokenReveal{}, r.err
	}
	world, err := rooms.DecodeID(worldID)
	if err != nil {
		return shared.RuntimeClusterTokenReveal{}, err
	}
	return runtimefiles.ReadClusterToken(ctx, r.roots[world], r.cluster, world)
}

func TestRemoteOnlyReplacementReadsAppliedNodesAndNeverControllerShadow(t *testing.T) {
	app := newApplyTestApp(t)
	app.rooms.ConfigureLocalDiscovery(false)
	now := time.Now().UTC()
	if err := app.rooms.SyncRuntimeCatalog([]rooms.RuntimeCatalogSource{{TargetID: "agent:remote", Online: true, Available: true, ObservedAt: now, Inventory: shared.RuntimeInventoryReport{ObservedAt: now, Rooms: []shared.RoomInventoryReport{{Directory: "RemoteRoom", Name: "Remote room", Shards: []shared.ShardInventoryReport{{Directory: "Master", Name: "Master", Role: "master", ID: 1, ServerPort: 12001}, {Directory: "Caves", Name: "Caves", Role: "caves", ID: 2, ServerPort: 12002}}}}}}}); err != nil {
		t.Fatal(err)
	}
	room, err := app.rooms.Room(rooms.EncodeID("RemoteRoom"))
	if err != nil || !room.Managed {
		t.Fatalf("remote room=%+v %v", room, err)
	}
	roots := map[string]string{"Master": t.TempDir(), "Caves": t.TempDir()}
	for world, root := range roots {
		cluster := clusterINI("Remote") + "\n[SHARD]\ncluster_key=target-key\nbind_ip=0.0.0.0\nmaster_port=10888\n"
		if world == "Caves" {
			cluster += "master_ip=192.0.2.24\n"
		}
		for name, data := range map[string]string{"cluster.ini": cluster, "cluster_token.txt": "target-token-1234567890", world + "/server.ini": serverINI(world == "Master", map[string]int{"Master": 1, "Caves": 2}[world], map[string]int{"Master": 12001, "Caves": 12002}[world]), world + "/leveldataoverride.lua": "return {}"} {
			path := filepath.Join(root, "RemoteRoom", name)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	coordinator := &coordinatedRestorerStub{restoreResult: distributedbackup.RestoreResult{OperationID: "restore"}, operations: []distributedbackup.Operation{{ID: "restore", SetID: "imported-backup-set", Kind: "restore", Status: distributedbackup.OperationSucceeded}}}
	service := coordinatedApplyService(t, app, coordinator, deniedSaveImportMutationGuard{})
	service.ConfigureRuntimeReader(importTargetReader{roots: roots, cluster: "RemoteRoom"})
	value := uploadTwoWorldImport(t, app, "Forest", 1, true, "Underground", 2, false)
	request := ApplyRequest{CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeReplace, TargetRoomID: room.ID, Confirmation: room.Name, TokenPolicy: TokenPreserve, NetworkPolicy: NetworkPreserve, ModPolicy: ModsPreserve}
	if _, err := service.Apply(context.Background(), value.ID, "test", request); err != nil {
		t.Fatal(err)
	}
	if coordinator.token != "target-token-1234567890" || coordinator.ports["Forest"] != 12001 || coordinator.ports["Underground"] != 12002 {
		t.Fatalf("wrong target configuration: %+v", coordinator)
	}
	if bytes.Contains(coordinator.clusters["Forest"], []byte("master_ip")) || !bytes.Contains(coordinator.clusters["Underground"], []byte("192.0.2.24")) {
		t.Fatalf("lost per-node network settings: %q", coordinator.clusters)
	}
	// A stale Controller directory cannot hide a disconnected runtime.
	path := filepath.Join(app.saveRoot, "RemoteRoom")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "cluster.ini"), []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	service.ConfigureRuntimeReader(importTargetReader{err: errors.New("agent disconnected")})
	value = uploadTwoWorldImport(t, app, "Forest", 1, true, "Underground", 2, false)
	request.CandidateID = value.Manifest.Candidates[0].ID
	if _, err := service.Apply(context.Background(), value.ID, "test", request); err == nil || len(coordinator.imported) != 1 {
		t.Fatalf("fell back to shadow directory: %v", err)
	}
}
