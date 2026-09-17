package server

import (
	"encoding/json"
	"strings"
	"testing"

	"dont/shared"
)

func TestRuntimeConsoleProbePassesTransportLeaseValidation(t *testing.T) {
	request := shared.RuntimeOperationRequest{
		ProtocolVersion:  shared.RuntimeOperationProtocolVersion,
		OperationID:      "runtime-probe-transport-0001",
		Action:           shared.RuntimeActionConsoleSend,
		InstallationID:   "default",
		Cluster:          "Cluster_1",
		Shard:            "Master",
		TopologyRevision: "revision-1",
		Console: &shared.RuntimeConsoleRequest{
			Mode: shared.ConsoleModeProbe, CoalesceKey: "world-state", Command: "DSTAdmin.Refresh()",
		},
	}
	server := &Server{agents: make(map[string]*AgentConnection)}
	if _, err := server.SendRuntimeOperation("missing-agent", request, 30); err == nil || strings.Contains(err.Error(), "缺少租约") {
		t.Fatalf("probe request did not pass transport lease validation: %v", err)
	}

	request.Console.Mode = shared.ConsoleModeManaged
	request.Console.CoalesceKey = ""
	if _, err := server.SendRuntimeOperation("missing-agent", request, 30); err == nil || !strings.Contains(err.Error(), "缺少租约") {
		t.Fatalf("managed console request bypassed transport lease validation: %v", err)
	}
}

func TestRuntimeOperationAuditContentRedactsBinaryPayloads(t *testing.T) {
	request := shared.RuntimeOperationRequest{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion,
		OperationID:     "operation-audit-0001", InstallationID: "default",
		Action: shared.RuntimeActionModUploadWrite, Cluster: "Mods", Shard: "Installation", TopologyRevision: "revision-1",
		Mod: &shared.RuntimeModRequest{
			WorkshopID: "1392778117", Data: []byte("binary-content-must-not-be-retained"),
			FetchSources: []shared.RuntimeModFetchSource{shared.RuntimeModFetchSourceSteam},
			FetchLocations: []shared.RuntimeModFetchLocation{{
				Source: shared.RuntimeModFetchSourceController, DownloadPath: "/mod-artifacts/1392778117/tree", DownloadToken: "artifact-secret-token",
			}},
		},
		Migration: &shared.RuntimeMigrationRequest{
			Data: []byte("migration-secret"),
			FetchLocations: []shared.RuntimeMigrationFetchLocation{{
				DownloadPath: "/migration-peer/default/migration-audit-0001", DownloadToken: "migration-peer-secret-token",
			}},
		},
		Backup:        &shared.RuntimeBackupRequest{Data: []byte("backup-secret")},
		Configuration: &shared.RuntimeConfigurationRequest{Data: []byte("configuration-secret")},
		Console: &shared.RuntimeConsoleRequest{CommandDocument: &shared.RuntimeCommandDocument{
			RequestID: "document-audit-0001", Data: []byte("console-document-secret"),
		}},
	}
	content, err := runtimeOperationAuditContent(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"binary-content", "migration-secret", "migration-peer-secret-token", "backup-secret", "configuration-secret", "console-document-secret", "artifact-secret-token"} {
		if strings.Contains(content, secret) {
			t.Fatalf("audit retained binary payload %q: %s", secret, content)
		}
	}
	var decoded shared.RuntimeOperationRequest
	if err := json.Unmarshal([]byte(content), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Action != request.Action || decoded.Mod == nil || decoded.Mod.WorkshopID != "1392778117" ||
		len(decoded.Mod.FetchSources) != 1 || len(decoded.Mod.FetchLocations) != 1 || decoded.Mod.FetchLocations[0].DownloadToken != "" || len(decoded.Mod.Data) != 0 || decoded.Migration == nil || len(decoded.Migration.Data) != 0 ||
		len(decoded.Migration.FetchLocations) != 1 || decoded.Migration.FetchLocations[0].DownloadToken != "" ||
		decoded.Backup == nil || len(decoded.Backup.Data) != 0 || decoded.Configuration == nil || len(decoded.Configuration.Data) != 0 ||
		decoded.Console == nil || decoded.Console.CommandDocument == nil || len(decoded.Console.CommandDocument.Data) != 0 {
		t.Fatalf("audit identity changed: %#v", decoded)
	}
	if string(request.Mod.Data) != "binary-content-must-not-be-retained" {
		t.Fatal("audit redaction mutated the request sent to the Agent")
	}
	if request.Mod.FetchLocations[0].DownloadToken != "artifact-secret-token" {
		t.Fatal("audit redaction mutated the artifact token sent to the Agent")
	}
	if request.Migration.FetchLocations[0].DownloadToken != "migration-peer-secret-token" {
		t.Fatal("audit redaction mutated the migration token sent to the Agent")
	}
}

func TestRuntimeOperationTimeoutAllowsBoundedMigrationPeerFetch(t *testing.T) {
	if runtimeOperationTimeoutLimit(shared.RuntimeActionMigrationFetch) != 1800 ||
		runtimeOperationTimeoutLimit(shared.RuntimeActionMigrationImportWrite) != 300 {
		t.Fatal("unexpected Runtime operation timeout limits")
	}
}
