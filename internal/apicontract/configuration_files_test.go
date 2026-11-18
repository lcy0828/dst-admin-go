package apicontract

import (
	"encoding/json"
	"testing"
	"time"

	"dont/internal/configuration"
	"dont/internal/mods"
)

func TestConfigurationFileToolsPreservePanelPermissionsAndReadOnlyPreviews(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for id, readOnly := range map[string]bool{
		"getWorldConfigurationFiles":     true,
		"previewWorldConfigurationFiles": true,
		"applyWorldConfigurationFiles":   false,
		"previewModConfigurationFile":    true,
		"applyModConfigurationFile":      false,
	} {
		t.Run(id, func(t *testing.T) {
			op, ok := c.Operation(id)
			if !ok || !op.Callable() {
				t.Fatal("configuration file operation is not discoverable")
			}
			policy := op.ExecutionPolicy()
			if policy.Profile != "panel-manage" || (policy.Access == "read") != readOnly {
				t.Fatalf("unexpected file editor permission: %+v", policy)
			}
			metadata := op.ToolMetadata()
			if (metadata.Access == "read") != readOnly || metadata.Destructive == readOnly || (readOnly && !metadata.Idempotent) {
				t.Fatalf("incorrect read/write hints: %+v", metadata)
			}
			if (policy.Confirmation == "none") != readOnly {
				t.Fatalf("unexpected confirmation policy: %+v", policy)
			}
			if _, err := op.AIInputSchema(); err != nil {
				t.Fatal(err)
			}
			if _, err := op.ResultSchema(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConfigurationFileSchemasMatchSerializedSourcesAndPreviews(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	server := configuration.WorldServerValues{ServerPort: 10999, IsMaster: true, ShardName: "Master", ShardID: 0}
	for _, sample := range []struct {
		operation string
		value     any
	}{
		{"getWorldConfigurationFiles", configuration.WorldFiles{Revision: "revision", WorldType: "forest", ServerIni: "[NETWORK]\nserver_port=10999\n", Leveldataoverride: "return { overrides = {} }\n", Server: server, Overrides: map[string]any{}, ModifiedAt: time.Now().UTC(), Sync: configuration.SyncState{Status: "synced", Source: "runtime-disk"}}},
		{"previewWorldConfigurationFiles", configuration.WorldFilesPreview{Revision: "revision", NextRevision: "next", Server: server, Overrides: map[string]any{}}},
		{"getModConfigurationFile", mods.ConfigurationFile{RoomID: "example", WorldID: "Master", FileName: "modoverrides.lua", Content: "return {}\n", Exists: true, Revision: "revision", WorldRevisions: map[string]string{"Master": "revision"}, TopologyRevision: "topology", ReadAt: time.Now().UTC()}},
		{"previewModConfigurationFile", mods.ConfigurationFilePreview{Revision: "revision", NextRevision: "next", WorldRevisions: map[string]string{"Master": "revision"}, TopologyRevision: "topology", NormalizedContent: "return {}\n"}},
		{"previewModConfigurationFile", mods.ConfigurationFilePreview{Revision: "revision", NextRevision: "next", NormalizedContent: "return {}\n", Changes: []mods.ConfigurationFileModChange{{ModID: "example_local", Source: "local", DirectoryName: "example_local", Operation: "add", Missing: true}}, MissingLocalModIDs: []string{"example_local"}, MissingWorkshopIDs: []string{}, Notices: []string{}}},
	} {
		t.Run(sample.operation, func(t *testing.T) {
			op, _ := c.Operation(sample.operation)
			response := op.Responses["200"]
			validator := &Operation{ID: "response", Method: "GET", catalog: c, RequestBody: Body{Required: true, Content: map[string]Media{"application/json": {Schema: response.Content["application/json"].Schema}}}}
			raw, err := json.Marshal(map[string]any{"body": map[string]any{"data": sample.value, "error": nil, "meta": map[string]any{"requestId": "example"}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := validator.Validate(raw); err != nil {
				t.Fatal(err)
			}
		})
	}
}
