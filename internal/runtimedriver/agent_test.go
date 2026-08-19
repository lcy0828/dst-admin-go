package runtimedriver

import (
	"context"
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
