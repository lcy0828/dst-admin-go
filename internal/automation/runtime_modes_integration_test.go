package automation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/jobs"
	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/shards"
	"dont/internal/topology"
	"dont/shared"

	"github.com/jinzhu/gorm"
)

type runtimeModeIntegrationCatalog struct{ automationTestCatalog }

func (runtimeModeIntegrationCatalog) Room(id string) (rooms.Room, error) {
	if id != "room" {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return rooms.Room{ID: id, DirectoryName: "survival", Managed: true}, nil
}

func (runtimeModeIntegrationCatalog) Worlds(id string) ([]rooms.World, error) {
	return []rooms.World{
		{ID: "master", RoomID: id, DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster},
		{ID: "caves", RoomID: id, DirectoryName: "Caves", Name: "Caves", Role: rooms.WorldRoleCaves},
	}, nil
}

type runtimeModeIntegrationTargets struct{ item agents.RuntimeTargetInventory }

func (s runtimeModeIntegrationTargets) RuntimeTargetInventories(context.Context) ([]agents.RuntimeTargetInventory, error) {
	return []agents.RuntimeTargetInventory{s.item}, nil
}

type runtimeModeIntegrationControl struct {
	*shards.MemoryControl
	mu    sync.RWMutex
	modes map[string]shared.RuntimePerformanceMode
}

func (c *runtimeModeIntegrationControl) StartWithRuntimeMode(ctx context.Context, room, world string, mode shared.RuntimePerformanceMode) error {
	if err := c.MemoryControl.Start(ctx, room, world); err != nil {
		return err
	}
	c.mu.Lock()
	c.modes[world] = mode
	c.mu.Unlock()
	return nil
}

func (c *runtimeModeIntegrationControl) Status(ctx context.Context, room, world string) (shards.RuntimeStatus, error) {
	status, err := c.MemoryControl.Status(ctx, room, world)
	c.mu.RLock()
	defer c.mu.RUnlock()
	if status.State == shards.RuntimeRunning {
		status.RuntimeMode = c.modes[world]
	}
	return status, err
}

type unexpectedRemoteRuntime struct{}

func (unexpectedRemoteRuntime) ExecuteShard(context.Context, string, shared.ShardOperationRequest, int) (agents.ShardExecutionResult, error) {
	return agents.ShardExecutionResult{}, errors.New("local lifecycle unexpectedly used remote transport")
}

func TestManualAndAutomationRestartUseTheSameResolvedLuaJITInstallation(t *testing.T) {
	for _, entry := range []string{"manual", "automation"} {
		for _, requested := range []shared.RuntimePerformanceMode{"", shared.RuntimePerformanceModeLuaJIT, shared.RuntimePerformanceModeArenaGC} {
			t.Run(entry+"/"+string(requested), func(t *testing.T) {
				db, err := gorm.Open("sqlite3", ":memory:")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				db.LogMode(false)
				db.DB().SetMaxOpenConns(1)
				roomStore := rooms.NewStore(db, "integration_")
				jobStore := jobs.NewStore(db, "integration_")
				topologyStore := topology.NewStore(db, "integration_")
				leases := operationlease.NewService(db, "integration_")
				for _, migrate := range []func() error{roomStore.Migrate, jobStore.Migrate, topologyStore.Migrate, leases.Migrate} {
					if err := migrate(); err != nil {
						t.Fatal(err)
					}
				}
				catalog := runtimeModeIntegrationCatalog{}
				performance := &shared.RuntimePerformanceReport{
					Status: shared.RuntimePerformanceReady, CanEnable: true, PackageVersion: "3.4.0",
					SupportedModes: []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame, shared.RuntimePerformanceModeLuaJIT, shared.RuntimePerformanceModeArenaGC},
				}
				now := time.Now().UTC()
				placement, err := topology.NewService(catalog, runtimeModeIntegrationTargets{item: agents.RuntimeTargetInventory{
					Target: agents.RuntimeTarget{ID: "local", Name: "本机", Kind: agents.RuntimeKindLocal, Configured: true, Online: true,
						DefaultInstallationID: "steam-main", Config: agents.RuntimeConfig{InstallationID: "steam-main"}, Performance: performance},
					Available: true, ObservedAt: &now, ReceivedAt: &now,
					Inventory: shared.RuntimeInventoryReport{CPU: shared.CPUInventory{LogicalProcessors: 20, PhysicalCores: 10},
						Memory: shared.MemoryInventory{TotalBytes: 16 << 30, AvailableBytes: 7 << 30}},
				}}, topologyStore)
				if err != nil {
					t.Fatal(err)
				}
				control := &runtimeModeIntegrationControl{MemoryControl: shards.NewMemoryControl(), modes: map[string]shared.RuntimePerformanceMode{}}
				ctx := context.Background()
				for _, world := range []string{"Master", "Caves"} {
					if err := control.StartWithRuntimeMode(ctx, "survival", world, shared.RuntimePerformanceModeLuaJIT); err != nil {
						t.Fatal(err)
					}
				}
				operations := shards.NewOperations(catalog, control)
				if err := operations.ConfigureDistributed(placement, unexpectedRemoteRuntime{}, leases); err != nil {
					t.Fatal(err)
				}
				if err := operations.ConfigureRuntimeModePersistence(roomStore, jobStore); err != nil {
					t.Fatal(err)
				}
				prefs, err := roomStore.RuntimeModePreferences("room")
				if err != nil || len(prefs) != 0 {
					t.Fatalf("expected empty initial preferences: %+v %v", prefs, err)
				}
				job, _, err := jobStore.Create("room.restart", "room", "", []jobs.TargetSpec{{ID: "master", Name: "Master"}, {ID: "caves", Name: "Caves"}})
				if err != nil {
					t.Fatal(err)
				}
				if entry == "automation" {
					executor := &DomainExecutor{roomActions: operations}
					parameters := map[string]interface{}{}
					if requested != "" {
						parameters["runtimeMode"] = string(requested)
					}
					if _, err := executor.Execute(ctx, Task{Action: ActionRoomRestart, RoomID: "room", Parameters: parameters}, job.ID); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := operations.RequireRuntimeSelection(ctx, "room", nil, requested, ""); err != nil {
						t.Fatal(err)
					}
					if err := operations.RequireCapacityConfirmation(ctx, shards.ActionRestart, "room", nil, false); err != nil {
						t.Fatal(err)
					}
					_, runner, err := operations.PlanWithOptions(shards.ActionRestart, "room", nil, shards.PlanOptions{RuntimeMode: requested})
					if err != nil {
						t.Fatal(err)
					}
					results := []jobs.TargetResult{}
					ctx = shards.WithOperationAudit(ctx, shards.OperationAuditMetadata{JobID: job.ID, Source: "api"})
					if err := runner(ctx, func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
						t.Fatal(err)
					}
					if len(results) != 2 {
						t.Fatalf("results=%+v", results)
					}
					for _, result := range results {
						if result.Status != jobs.StatusSucceeded || result.Error != nil || result.Warning != nil {
							t.Fatalf("result=%+v error=%+v warning=%+v", result, result.Error, result.Warning)
						}
					}
				}
				want := requested
				if want == "" {
					want = shared.RuntimePerformanceModeLuaJIT
				}
				plan, err := jobStore.RuntimeModePlan(job.ID, "room")
				if err != nil || len(plan) != 2 {
					t.Fatalf("plan=%+v error=%v", plan, err)
				}
				prefs, err = roomStore.RuntimeModePreferences("room")
				if err != nil || len(prefs) != 2 {
					t.Fatalf("preferences=%+v error=%v", prefs, err)
				}
				for _, selection := range plan {
					if selection.Mode != want || selection.InstallationID != "steam-main" || prefs[selection.WorldID].Mode != want {
						t.Fatalf("wrong resolved mode or installation: plan=%+v preferences=%+v", plan, prefs)
					}
				}
			})
		}
	}
}
