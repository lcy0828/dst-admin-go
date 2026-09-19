package distributedbackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/shared"
)

type restoreFaultDriver struct {
	runtimedriver.Driver
	failPublish, failRollback, failStart bool
}

func (d restoreFaultDriver) PublishRestore(ctx context.Context, target runtimedriver.Target, op runtimedriver.Operation, id string, publishShared bool) (string, error) {
	if d.failPublish {
		return "", errors.New("injected publish failure")
	}
	return d.Driver.PublishRestore(ctx, target, op, id, publishShared)
}
func (d restoreFaultDriver) RollbackRestore(ctx context.Context, target runtimedriver.Target, op runtimedriver.Operation, id string) error {
	if d.failRollback {
		return errors.New("injected rollback failure")
	}
	return d.Driver.RollbackRestore(ctx, target, op, id)
}
func (d restoreFaultDriver) ExecuteShard(ctx context.Context, target runtimedriver.Target, op runtimedriver.Operation, action shared.ShardAction, timeout time.Duration) (shared.ShardOperationResult, error) {
	if d.failStart && action == shared.ShardActionStart {
		return shared.ShardOperationResult{}, errors.New("injected restart failure")
	}
	return d.Driver.ExecuteShard(ctx, target, op, action, timeout)
}
func TestRestoreMustNotRestartMixedSaveAfterRollbackFailure(t *testing.T) {
	for _, local := range []bool{true, false} {
		name := "split-target"
		if local {
			name = "all-local"
		}
		t.Run(name, func(t *testing.T) { assertRestoreRollbackSafety(t, local) })
	}
}
func assertRestoreRollbackSafety(t *testing.T, local bool) {
	t.Helper()
	f := newBackupPlacementFixture(t, local)
	set, err := f.coordinator.Create(context.Background(), "room", "fault regression", "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	writeShardFixture(t, f.masterRoot, "Master", "master-v2")
	writeShardFixture(t, f.cavesRoot, "Caves", "caves-v2")
	router := f.coordinator.runtimes.(backupTestRouter)
	master := router.values["master"]
	master.driver = restoreFaultDriver{Driver: master.driver, failPublish: true}
	router.values["master"] = master
	caves := router.values["caves"]
	caves.driver = restoreFaultDriver{Driver: caves.driver, failRollback: true}
	router.values["caves"] = caves
	result, err := f.coordinator.Restore(context.Background(), set.ID, "测试房间", "")
	if err == nil {
		t.Fatal("fault not triggered")
	}
	op, loadErr := f.store.Operation(result.OperationID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	ms, _ := f.master.Status(context.Background(), "Cluster_1", "Master")
	cs, _ := f.caves.Status(context.Background(), "Cluster_1", "Caves")
	mc, _ := os.ReadFile(filepath.Join(f.masterRoot, "Cluster_1", "Master", "save", "session", "SESSION", "0000000001"))
	cc, _ := os.ReadFile(filepath.Join(f.cavesRoot, "Cluster_1", "Caves", "save", "session", "SESSION", "0000000001"))
	if ms.State != shards.RuntimeStopped || cs.State != shards.RuntimeStopped {
		t.Fatalf("restore restarted mixed generations: Master=%s(%s), Caves=%s(%s)", ms.State, mc, cs.State, cc)
	}
	if op.Status != OperationRecoveryRequired {
		t.Fatalf("expected recoverable rollback failure: %#v", op)
	}
	// A recovery retry while rollback still fails must also keep the room stopped.
	if _, retryErr := f.coordinator.RecoverOperation(context.Background(), result.OperationID); retryErr == nil {
		t.Fatal("expected rollback retry failure")
	}
	ms, _ = f.master.Status(context.Background(), "Cluster_1", "Master")
	cs, _ = f.caves.Status(context.Background(), "Cluster_1", "Caves")
	if ms.State != shards.RuntimeStopped || cs.State != shards.RuntimeStopped {
		t.Fatalf("unsafe restart: Master=%s(%s), Caves=%s(%s), operation=%s, error=%v", ms.State, mc, cs.State, cc, op.Status, err)
	}
	// Once the node can roll back again, both original generations resume.
	caves.driver = caves.driver.(restoreFaultDriver).Driver
	router.values["caves"] = caves
	recovered, retryErr := f.coordinator.RecoverOperation(context.Background(), result.OperationID)
	if retryErr != nil {
		t.Fatal(retryErr)
	}
	if recovered.Status != OperationRolledBack {
		t.Fatalf("recovery status=%s", recovered.Status)
	}
	cc, err = os.ReadFile(filepath.Join(f.cavesRoot, "Cluster_1", "Caves", "save", "session", "SESSION", "0000000001"))
	if err != nil || string(cc) != "caves-v2" {
		t.Fatalf("rollback save=%s err=%v", cc, err)
	}
}
func TestRestoreRestartFailureMustRemainRecoverable(t *testing.T) {
	f := newDistributedBackupFixture(t)
	set, err := f.coordinator.Create(context.Background(), "room", "fault regression", "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	router := f.coordinator.runtimes.(backupTestRouter)
	master := router.values["master"]
	master.driver = restoreFaultDriver{Driver: master.driver, failStart: true}
	router.values["master"] = master
	result, err := f.coordinator.Restore(context.Background(), set.ID, "测试房间", "")
	if err == nil {
		t.Fatal("fault not triggered")
	}
	op, loadErr := f.store.Operation(result.OperationID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if op.Status != OperationRecoveryRequired {
		t.Fatalf("restart failed but operation is %s (phase=%s error=%q): %v", op.Status, op.Phase, op.Failure, err)
	}
	master.driver = master.driver.(restoreFaultDriver).Driver
	router.values["master"] = master
	writeShardFixture(t, f.cavesRoot, "Caves", "caves-after-restart")
	recovered, retryErr := f.coordinator.RecoverOperation(context.Background(), result.OperationID)
	if retryErr != nil {
		t.Fatal(retryErr)
	}
	if recovered.Status != OperationSucceeded {
		t.Fatalf("recovery status=%s", recovered.Status)
	}
	data, readErr := os.ReadFile(filepath.Join(f.cavesRoot, "Cluster_1", "Caves", "save", "session", "SESSION", "0000000001"))
	if readErr != nil || string(data) != "caves-after-restart" {
		t.Fatalf("recovery overwrote resumed world: %s, %v", data, readErr)
	}
}

func TestColdBackupRestartFailureRetainsVerifiedSetForRecovery(t *testing.T) {
	f := newDistributedBackupFixture(t)
	router := f.coordinator.runtimes.(backupTestRouter)
	master := router.values["master"]
	original := master.driver
	master.driver = restoreFaultDriver{Driver: original, failStart: true}
	router.values["master"] = master
	set, err := f.coordinator.Create(context.Background(), "room", "backup", "manual", "")
	if err == nil {
		t.Fatal("expected startup failure")
	}
	if set.Status != StatusVerified {
		t.Fatalf("backup status=%s", set.Status)
	}
	ops, err := f.store.ListOperations("room")
	if err != nil || len(ops) != 1 {
		t.Fatalf("operations=%v err=%v", ops, err)
	}
	if ops[0].Status != OperationRecoveryRequired {
		t.Fatalf("operation=%#v", ops[0])
	}
	master.driver = original
	router.values["master"] = master
	recovered, err := f.coordinator.RecoverOperation(context.Background(), ops[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != OperationSucceeded {
		t.Fatalf("operation=%#v", recovered)
	}
	set, err = f.store.GetSet(set.ID)
	if err != nil || set.Status != StatusVerified {
		t.Fatalf("completed backup invalidated: %#v %v", set, err)
	}
}
