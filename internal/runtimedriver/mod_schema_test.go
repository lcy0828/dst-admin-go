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

func TestAgentLocalModSchemaKeepsCanonicalIdentity(t *testing.T) {
	executor := &capturedModFetchExecutor{result: shared.RuntimeModResult{Complete: true, Schema: &shared.RuntimeModSchema{ModID: "local:123", Name: "Local 123", Parser: "go"}}}
	driver, _ := NewAgent(executor)
	target := Target{TargetID: "agent:node", InstallationID: "native"}
	if _, err := driver.ReadModSchema(context.Background(), target, "local:123"); err != nil {
		t.Fatal(err)
	}
	if executor.request.Mod.ModID != "local:123" || executor.request.Mod.WorkshopID != "" {
		t.Fatalf("local identity sent as Workshop: %+v", executor.request.Mod)
	}
	executor.result.Schema.ModID = "123"
	if _, err := driver.ReadModSchema(context.Background(), target, "local:123"); err == nil {
		t.Fatal("accepted Workshop schema for local numeric directory")
	}
}
