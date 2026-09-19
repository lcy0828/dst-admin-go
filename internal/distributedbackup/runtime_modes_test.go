package distributedbackup

import (
	"context"
	"sync"
	"testing"

	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/shared"
)

type backupModeControl struct {
	*backupTestControl
	modeMu sync.Mutex
	mode   shared.RuntimePerformanceMode
}

func (c *backupModeControl) Status(ctx context.Context, cluster, shard string) (shards.RuntimeStatus, error) {
	status, err := c.backupTestControl.Status(ctx, cluster, shard)
	c.modeMu.Lock()
	defer c.modeMu.Unlock()
	status.RuntimeMode = c.mode
	return status, err
}
func (c *backupModeControl) StartWithRuntimeMode(ctx context.Context, cluster, shard string, mode shared.RuntimePerformanceMode) error {
	c.modeMu.Lock()
	c.mode = mode
	c.modeMu.Unlock()
	return c.backupTestControl.Start(ctx, cluster, shard)
}
func TestBackupAndRestoreRetainRuntimeModeAcrossRecovery(t *testing.T) {
	for _, local := range []bool{true, false} {
		name := "split"
		if local {
			name = "local"
		}
		for _, mode := range []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame, shared.RuntimePerformanceModeLuaJIT, shared.RuntimePerformanceModeArenaGC} {
			t.Run(name+"/"+string(mode), func(t *testing.T) {
				f := newBackupPlacementFixture(t, local)
				router := f.coordinator.runtimes.(backupTestRouter)
				control := &backupModeControl{backupTestControl: f.master, mode: mode}
				driver, err := runtimedriver.NewNative(f.masterRoot, control)
				if err != nil {
					t.Fatal(err)
				}
				master := router.values["master"]
				master.driver = driver
				router.values["master"] = master
				set, err := f.coordinator.Create(context.Background(), "room", "mode preservation", "manual", "")
				if err != nil {
					t.Fatal(err)
				}
				status, _ := control.Status(context.Background(), "Cluster_1", "Master")
				if status.RuntimeMode != mode {
					t.Fatalf("backup switched mode to %q", status.RuntimeMode)
				}
				master.driver = restoreFaultDriver{Driver: driver, failStart: true}
				router.values["master"] = master
				restored, err := f.coordinator.Restore(context.Background(), set.ID, "测试房间", "")
				if err == nil {
					t.Fatal("expected startup failure")
				}
				op, err := f.store.Operation(restored.OperationID)
				if err != nil {
					t.Fatal(err)
				}
				if op.OriginalRuntimeModes["master"] != mode {
					t.Fatalf("mode not persisted: %#v", op)
				}
				control.modeMu.Lock()
				control.mode = ""
				control.modeMu.Unlock()
				master.driver = driver
				router.values["master"] = master
				if _, err = f.coordinator.RecoverOperation(context.Background(), restored.OperationID); err != nil {
					t.Fatal(err)
				}
				status, _ = control.Status(context.Background(), "Cluster_1", "Master")
				if status.RuntimeMode != mode {
					t.Fatalf("recovery switched mode to %q", status.RuntimeMode)
				}
			})
		}
	}
}

func TestColdBackupRejectsUnknownModeBeforeStoppingWorlds(t *testing.T) {
	f := newDistributedBackupFixture(t)
	router := f.coordinator.runtimes.(backupTestRouter)
	control := &backupModeControl{backupTestControl: f.master, mode: "unknown"}
	driver, err := runtimedriver.NewNative(f.masterRoot, control)
	if err != nil {
		t.Fatal(err)
	}
	master := router.values["master"]
	master.driver = driver
	router.values["master"] = master
	if _, err = f.coordinator.Create(context.Background(), "room", "unknown mode", "manual", ""); err == nil {
		t.Fatal("expected preflight failure")
	}
	if len(f.master.stops) > 0 || len(f.caves.stops) > 0 {
		t.Fatal("stopped worlds before mode was known")
	}
}
