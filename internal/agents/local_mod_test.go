package agents

import (
	"context"
	"dont/shared"
	"errors"
	"testing"
)

func TestLocalModRequestsRequireAgentCapabilityBeforeDispatch(t *testing.T) {
	service, _, _, transport := newAgentTestService(t)
	for _, mod := range []shared.RuntimeModRequest{{ModID: "local:fishdried_custom"}, {ModIDs: []string{"local:fishdried_custom"}}, {IncludeLocal: true}} {
		_, err := service.ExecuteRuntime(context.Background(), "agent:agent-primary", shared.RuntimeOperationRequest{Action: shared.RuntimeActionModSchemaRead, Mod: &mod}, 30)
		if !errors.Is(err, ErrUnsupportedAction) {
			t.Fatalf("old Agent accepted %+v: %v", mod, err)
		}
	}
	transport.mu.Lock()
	snapshot := transport.snapshots["agent-primary"]
	snapshot.Capabilities = append(snapshot.Capabilities, shared.LocalModCapability, "runtime.mods.schema.v1")
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()
	// Capability negotiation succeeds; the deliberately invalid installation
	// still fails normal scope validation rather than reaching a remote writer.
	_, err := service.ExecuteRuntime(context.Background(), "agent:agent-primary", shared.RuntimeOperationRequest{Action: shared.RuntimeActionModSchemaRead, InstallationID: "wrong", Mod: &shared.RuntimeModRequest{ModID: "local:fishdried_custom"}}, 30)
	if errors.Is(err, ErrUnsupportedAction) {
		t.Fatalf("new Agent capability was ignored: %v", err)
	}
}

func TestModReadsBindTheSelectedRegisteredInstallation(t *testing.T) {
	service, _, _, transport := newAgentTestService(t)
	transport.mu.Lock()
	snapshot := transport.snapshots["agent-primary"]
	snapshot.Capabilities = append(snapshot.Capabilities, shared.LocalModCapability, "runtime.mods.schema.v1")
	snapshot.Details["runtime_installations"] = []map[string]interface{}{
		{"id": "first", "driver": "native", "save_path": "/srv/first/saves", "server_path": "/srv/first/server", "server_mode": "64"},
		{"id": "second", "driver": "native", "save_path": "/srv/second/saves", "server_path": "/srv/second/server", "server_mode": "64"},
	}
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()
	request := shared.RuntimeOperationRequest{ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: "selected-mod-read", Action: shared.RuntimeActionModSchemaRead, InstallationID: "second", Cluster: "Mods", Shard: "Installation", Mod: &shared.RuntimeModRequest{ModID: "local:fishdried_custom"}}
	result, err := service.ExecuteRuntime(context.Background(), "agent:agent-primary", request, 30)
	if err != nil || result.Result.InstallationID != "second" {
		t.Fatalf("selected installation rejected or replaced: %+v %v", result, err)
	}
	request.InstallationID = "unknown"
	if _, err := service.ExecuteRuntime(context.Background(), "agent:agent-primary", request, 30); !errors.Is(err, ErrRuntimeInstallationNotRegistered) {
		t.Fatalf("unregistered scope accepted: %v", err)
	}
}
