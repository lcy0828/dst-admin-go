package runtimedriver

import (
	"context"
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
