package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/gameupdate"
	"dont/internal/jobs"
	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/shared"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

type updateFlowRuntime struct {
	mu                    sync.Mutex
	versions              map[string]string
	states                map[string]shared.ShardRuntimeStatus
	writes, stops, starts int
}

func (r *updateFlowRuntime) ObserveInstallation(_ context.Context, target gameupdate.ReleaseInstallationPlan) (shared.RuntimeGameVersionResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return shared.RuntimeGameVersionResult{Installed: true, CurrentVersion: r.versions[target.TargetID], AvailableBytes: 45 << 30, SteamCMDAvailable: true, UpdateSupported: true}, nil
}
func (r *updateFlowRuntime) UpdateInstallation(_ context.Context, target gameupdate.ReleaseInstallationPlan, _ runtimedriver.Operation, expected string, _ bool) (shared.RuntimeGameVersionResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.versions[target.TargetID] = expected
	r.writes++
	return shared.RuntimeGameVersionResult{Installed: true, CurrentVersion: expected}, nil
}
func (r *updateFlowRuntime) Status(_ context.Context, shard gameupdate.ReleaseShardPlan) (shared.ShardRuntimeStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.states[shard.RoomID+"/"+shard.WorldID], nil
}
func (r *updateFlowRuntime) Stop(_ context.Context, shard gameupdate.ReleaseShardPlan, _ runtimedriver.Operation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states[shard.RoomID+"/"+shard.WorldID] = shared.ShardRuntimeStatus{State: "stopped"}
	r.stops++
	return nil
}
func (r *updateFlowRuntime) Start(_ context.Context, shard gameupdate.ReleaseShardPlan, operation runtimedriver.Operation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states[shard.RoomID+"/"+shard.WorldID] = shared.ShardRuntimeStatus{State: "running", SessionExists: true, RuntimeMode: operation.RuntimeMode}
	r.starts++
	return nil
}
func (*updateFlowRuntime) CaptureLogCursor(context.Context, gameupdate.ReleaseShardPlan) (string, int64, error) {
	return "test-log", 0, nil
}
func (*updateFlowRuntime) ReadLogs(context.Context, gameupdate.ReleaseShardPlan, string, int64) (shared.RuntimeLogChunk, error) {
	return shared.RuntimeLogChunk{}, nil
}

type updateFlowSnapshot struct {
	value gameupdate.ReleasePlacementSnapshot
}

func (s updateFlowSnapshot) Snapshot(context.Context) (gameupdate.ReleasePlacementSnapshot, error) {
	return s.value, nil
}

type updateFlowLatest struct{}

func (updateFlowLatest) Check(context.Context, string, string) (string, bool, error) {
	return "26335402", false, nil
}

type updateFlowLeases struct{}

func (updateFlowLeases) Acquire(_ context.Context, roomID, key string, ttl time.Duration) (operationlease.Lease, error) {
	return operationlease.Lease{RoomID: roomID, LeaseID: "test-" + roomID, OperationKey: key, FencingToken: 1, ExpiresAt: time.Now().Add(ttl)}, nil
}
func (updateFlowLeases) Renew(_ context.Context, lease operationlease.Lease, ttl time.Duration) (operationlease.Lease, error) {
	lease.ExpiresAt = time.Now().Add(ttl)
	return lease, nil
}
func (updateFlowLeases) Release(operationlease.Lease) error { return nil }

type updateFlowNoBackups struct{}

func (updateFlowNoBackups) CreateProtection(context.Context, string, string, string, *operationlease.Lease) (string, error) {
	return "", fmt.Errorf("backup must be skipped")
}
func (updateFlowNoBackups) VerifyProtection(context.Context, string, string, string) error {
	return fmt.Errorf("backup must be skipped")
}

// Exercise real planning, HTTP confirmation, durable coordination and job
// results together. Fixed preview hashes cannot detect this regression.
func TestGameUpdateSingleConfirmationThreeRoomsAndSixWorlds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"local", "agent", "split"} {
		t.Run(mode, func(t *testing.T) {
			runtime := &updateFlowRuntime{versions: map[string]string{}, states: map[string]shared.ShardRuntimeStatus{}}
			snapshot := updateFlowSnapshot{}
			for _, roomID := range []string{"room-one", "room-two", "room-three"} {
				for _, worldID := range []string{"Master", "Caves"} {
					targetID := "local"
					if mode == "agent" || mode == "split" && worldID == "Caves" {
						targetID = "agent:test-node"
					}
					runtime.versions[targetID] = "26230508"
					state := shared.ShardRuntimeStatus{State: "running", SessionExists: true}
					if roomID == "room-three" {
						state = shared.ShardRuntimeStatus{State: "stopped"}
					}
					runtime.states[roomID+"/"+worldID] = state
					snapshot.value.Shards = append(snapshot.value.Shards, gameupdate.ReleaseShardSnapshot{
						Room:               rooms.Room{ID: roomID, Name: roomID, DirectoryName: roomID, Managed: true},
						World:              rooms.World{ID: worldID, Name: worldID, DirectoryName: worldID, IsMaster: worldID == "Master"},
						Target:             agents.RuntimeTarget{ID: targetID, OS: "linux", Online: true, Config: agents.RuntimeConfig{InstallationID: "default"}, Capabilities: []string{gameupdate.RequiredUpdateCapability}},
						InventoryAvailable: true, InventoryHasShard: true, TopologyRevision: "revision-" + roomID,
					})
				}
			}
			planner, err := gameupdate.NewReleasePlanner(snapshot, runtime, updateFlowLatest{}, 2<<30)
			if err != nil {
				t.Fatal(err)
			}
			db, err := gorm.Open("sqlite3", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			db.SingularTable(true)
			db.LogMode(false)
			db.DB().SetMaxOpenConns(1)
			t.Cleanup(func() { _ = db.Close() })
			store := gameupdate.NewReleaseStore(db, "flow_")
			if err := store.Migrate(); err != nil {
				t.Fatal(err)
			}
			coordinator, err := gameupdate.NewReleaseCoordinator(planner, runtime, updateFlowLeases{}, updateFlowNoBackups{}, store)
			if err != nil {
				t.Fatal(err)
			}
			router, jobService := newGameReleaseHandlerApp(t, coordinator)
			targetID := "local"
			if mode == "agent" {
				targetID = "agent:test-node"
			}
			preview := performJSON(router, http.MethodPost, "/api/v2/game/releases/preview", map[string]interface{}{"targetIds": []string{targetID}}, nil, "")
			assertStatus(t, preview, http.StatusOK)
			if got := len(responseData(t, preview)["affectedRoomIds"].([]interface{})); got != 3 {
				t.Fatalf("affected rooms=%d", got)
			}
			response := performJSON(router, http.MethodPost, "/api/v2/game/releases", map[string]interface{}{
				"confirmed": true, "targetIds": []string{targetID}, "desiredVersion": "26335402",
				"policy": map[string]interface{}{"skipBackup": true, "stopDelaySeconds": 0, "loadConfirmation": "none"},
			}, nil, "")
			assertStatus(t, response, http.StatusAccepted)
			id := responseData(t, response)["id"].(string)
			waitForJobStatus(t, jobService, id, jobs.StatusSucceeded)
			value, err := coordinator.Get(id)
			if err != nil || value.Stage != gameupdate.ReleaseStageSucceeded || len(value.Shards) != 6 || len(value.ProtectionBackupIDs) != 0 {
				t.Fatalf("release=%+v err=%v", value, err)
			}
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			if runtime.stops != 4 || runtime.starts != 4 || runtime.writes != len(runtime.versions) {
				t.Fatalf("stops=%d starts=%d writes=%d", runtime.stops, runtime.starts, runtime.writes)
			}
			for key, state := range runtime.states {
				if key == "room-three/Master" || key == "room-three/Caves" {
					if state.State != "stopped" {
						t.Fatalf("originally stopped world started: %s", key)
					}
				} else if state.State != "running" {
					t.Fatalf("original running world not restored: %s", key)
				}
			}
		})
	}
}
