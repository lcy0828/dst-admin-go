package runtimedriver

import (
	"context"
	"testing"

	"dont/shared"
)

func TestAgentModSchemaReadIsReadOnlyAndChecksIdentity(t *testing.T) {
	executor := &capturedModFetchExecutor{result: shared.RuntimeModResult{
		Complete: true, Schema: &shared.RuntimeModSchema{WorkshopID: "100", Parser: "go"},
	}}
	driver, err := NewAgent(executor)
	if err != nil {
		t.Fatal(err)
	}
	target := Target{TargetID: "agent:node", InstallationID: "native"}
	if _, err := driver.ReadModSchema(context.Background(), target, "100"); err != nil {
		t.Fatal(err)
	}
	if executor.request.Action != shared.RuntimeActionModSchemaRead || shared.RuntimeOperationRequiresLease(executor.request) || executor.targetID != target.TargetID || executor.request.InstallationID != target.InstallationID {
		t.Fatalf("request=%#v", executor.request)
	}
	executor.result.Schema.WorkshopID = "200"
	if _, err := driver.ReadModSchema(context.Background(), target, "100"); err == nil {
		t.Fatal("accepted another Mod's schema")
	}
}
