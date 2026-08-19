package runtimedriver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/shared"
)

type emptyMigrationExecutor struct{}

func (emptyMigrationExecutor) ExecuteShard(context.Context, string, shared.ShardOperationRequest, int) (agents.ShardExecutionResult, error) {
	return agents.ShardExecutionResult{}, nil
}

type failedMigrationExecutor struct {
	result agents.RuntimeExecutionResult
	err    error
}

type barrierArtifactExecutor struct {
	emptyMigrationExecutor
	data []byte
}

func (f barrierArtifactExecutor) ExecuteRuntime(context.Context, string, shared.RuntimeOperationRequest, int) (agents.RuntimeExecutionResult, error) {
	sum := sha256.Sum256(f.data)
	bundle := shared.RuntimeArtifactBundle{Kind: shared.ArtifactRuntimeBarrier, Artifacts: []shared.RuntimeArtifact{{
		Name: "snapshot-barrier.json", Size: int64(len(f.data)), SHA256: hex.EncodeToString(sum[:]), UpdatedAt: time.Now().UTC(), Data: f.data,
	}}}
	return agents.RuntimeExecutionResult{Result: shared.RuntimeOperationResult{Artifacts: &bundle}}, nil
}

func (f failedMigrationExecutor) ExecuteShard(context.Context, string, shared.ShardOperationRequest, int) (agents.ShardExecutionResult, error) {
	return agents.ShardExecutionResult{}, nil
}

func (f failedMigrationExecutor) ExecuteRuntime(context.Context, string, shared.RuntimeOperationRequest, int) (agents.RuntimeExecutionResult, error) {
	return f.result, f.err
}

func (emptyMigrationExecutor) ExecuteRuntime(context.Context, string, shared.RuntimeOperationRequest, int) (agents.RuntimeExecutionResult, error) {
	return agents.RuntimeExecutionResult{Result: shared.RuntimeOperationResult{}}, nil
}

func TestAgentDriverRejectsEmptySuccessfulMigrationResponse(t *testing.T) {
	driver, err := NewAgent(emptyMigrationExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Minute)
	_, err = driver.PrepareMigrationExport(context.Background(), Target{
		TargetID: "agent:node", InstallationID: "default", Cluster: "Cluster", Shard: "Master", TopologyRevision: "revision",
	}, Operation{ID: "operation", Key: "key", LeaseID: "lease", FencingToken: 1, LeaseExpiresAt: &expires}, "migration-response-0001")
	if err == nil || !strings.Contains(err.Error(), "未返回迁移") {
		t.Fatalf("empty migration response error=%v", err)
	}
}

func TestAgentDriverMarksOnlyPreDispatchMigrationBeginFailures(t *testing.T) {
	descriptor := MigrationDescriptor{MigrationID: "migration-import-0001", Size: 1, SHA256: strings.Repeat("a", 64)}
	target := Target{TargetID: "agent:node", InstallationID: "default", Cluster: "Cluster", Shard: "Master", TopologyRevision: "revision"}

	driver, err := NewAgent(failedMigrationExecutor{err: agents.ErrRuntimeInstallationNotRegistered})
	if err != nil {
		t.Fatal(err)
	}
	err = driver.BeginMigrationImport(context.Background(), target, Operation{ID: "operation"}, descriptor)
	if !errors.Is(err, ErrOperationNotDispatched) || !errors.Is(err, agents.ErrRuntimeInstallationNotRegistered) {
		t.Fatalf("pre-dispatch error=%v", err)
	}

	driver, err = NewAgent(failedMigrationExecutor{
		result: agents.RuntimeExecutionResult{RemoteID: "remote-operation"},
		err:    agents.ErrAgentOffline,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = driver.BeginMigrationImport(context.Background(), target, Operation{ID: "operation"}, descriptor)
	if errors.Is(err, ErrOperationNotDispatched) || !errors.Is(err, agents.ErrAgentOffline) {
		t.Fatalf("possibly dispatched error=%v", err)
	}
}

func TestSnapshotBarrierAcceptsRealKLEIPersistentJSON(t *testing.T) {
	data := []byte(`KLEI     1 {"schemaVersion":1,"producerVersion":"2.4.0","producerInstanceId":"barrier-instance","barrierId":"hot-test-0001","state":"prepared","sessionId":"SESSION","shardId":"2","snapshotBefore":7,"preparedAtUnix":1787118371}`)
	driver, err := NewAgent(barrierArtifactExecutor{data: data})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := driver.SnapshotBarrier(context.Background(), Target{TargetID: "agent:node", InstallationID: "container", Cluster: "Cluster", Shard: "Caves"}, "hot-test-0001")
	if err != nil || receipt.State != "prepared" || receipt.SnapshotBefore != 7 || receipt.ProducerInstanceID != "barrier-instance" {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
}
