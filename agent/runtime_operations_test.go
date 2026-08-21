package agent

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/shards"
	"dont/shared"
)

func runtimeOperationRequest(action shared.RuntimeAction) shared.RuntimeOperationRequest {
	expires := time.Now().UTC().Add(5 * time.Minute)
	return shared.RuntimeOperationRequest{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: "runtime-operation-1", OperationKey: "runtime-key-1",
		InstallationID: "default", Action: action, Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1",
		LeaseID: "lease-1", FencingToken: 1, LeaseExpiresAt: &expires,
	}
}

func TestRuntimeConsoleSendIsIdempotent(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	agent, _ := newShardOperationAgent(t, runtimeControl)
	request := runtimeOperationRequest(shared.RuntimeActionConsoleSend)
	request.Console = &shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeManaged, Command: "c_announce(\"hello\")"}
	first, err := agent.executeRuntimeOperation(string(request.Action), &request, 10)
	if err != nil || first.Outcome != shared.RuntimeOutcomeSent || first.Idempotent {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := agent.executeRuntimeOperation(string(request.Action), &request, 10)
	if err != nil || !second.Idempotent || len(runtimeControl.calls) != 1 {
		t.Fatalf("second=%#v calls=%v err=%v", second, runtimeControl.calls, err)
	}
}

func TestRuntimeOperationRejectsStaleFencingToken(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	agent, _ := newShardOperationAgent(t, runtimeControl)

	current := runtimeOperationRequest(shared.RuntimeActionConsoleSend)
	current.OperationID, current.OperationKey = "runtime-operation-current", "runtime-key-current"
	current.LeaseID, current.FencingToken = "lease-current", 2
	current.Console = &shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeManaged, Command: "c_announce(\"current\")"}
	if _, err := agent.executeRuntimeOperation(string(current.Action), &current, 10); err != nil {
		t.Fatalf("current fencing operation failed: %v", err)
	}

	stale := runtimeOperationRequest(shared.RuntimeActionConsoleSend)
	stale.OperationID, stale.OperationKey = "runtime-operation-stale", "runtime-key-stale"
	stale.Console = &shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeManaged, Command: "c_announce(\"stale\")"}
	if _, err := agent.executeRuntimeOperation(string(stale.Action), &stale, 10); err == nil || !strings.Contains(err.Error(), "fencing token") {
		t.Fatalf("stale fencing error=%v", err)
	}
	if len(runtimeControl.calls) != 1 {
		t.Fatalf("stale operation reached runtime: calls=%v", runtimeControl.calls)
	}
}

func TestRuntimeOperationRejectsIdempotencyKeyReuseWithDifferentPayload(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	agent, _ := newShardOperationAgent(t, runtimeControl)
	request := runtimeOperationRequest(shared.RuntimeActionConsoleSend)
	request.Console = &shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeManaged, Command: "c_announce(\"first\")"}
	if _, err := agent.executeRuntimeOperation(string(request.Action), &request, 10); err != nil {
		t.Fatalf("first operation failed: %v", err)
	}

	conflict := request
	conflict.OperationID = "runtime-operation-conflict"
	conflict.Console = &shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeManaged, Command: "c_announce(\"different\")"}
	if _, err := agent.executeRuntimeOperation(string(conflict.Action), &conflict, 10); err == nil || !strings.Contains(err.Error(), "幂等键") {
		t.Fatalf("idempotency conflict error=%v", err)
	}
	if len(runtimeControl.calls) != 1 {
		t.Fatalf("conflicting operation reached runtime: calls=%v", runtimeControl.calls)
	}
}

func TestRuntimeReadsLogsAndFixedArtifacts(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	agent, installation := newShardOperationAgent(t, runtimeControl)
	worldRoot := filepath.Join(installation.SavePath, "Cluster_1", "Master")
	if err := os.WriteFile(filepath.Join(worldRoot, "server_log.txt"), []byte("[00:00:00]: Current time: Wed Aug 19 19:56:40 2026\nfirst\nsecond needle\nthird\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worldRoot, "server_chat_log.txt"), []byte("[00:00:01]: [Say] (KU_ONE) Willow: hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(worldRoot, "save", "mod_config_data", "dst-admin")
	if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "health.json"), []byte(`{"ready":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	logs := runtimeOperationRequest(shared.RuntimeActionReadLogs)
	logs.OperationKey, logs.LeaseID, logs.FencingToken, logs.LeaseExpiresAt = "", "", 0, nil
	logs.Logs = &shared.RuntimeLogRequest{Cursor: -1, MaxBytes: 1024, MaxLines: 10, Query: "needle"}
	logResult, err := agent.executeRuntimeOperation(string(logs.Action), &logs, 10)
	if err != nil || logResult.Logs == nil || len(logResult.Logs.Lines) != 1 || !strings.Contains(logResult.Logs.Lines[0].Text, "needle") {
		t.Fatalf("logs=%#v err=%v", logResult.Logs, err)
	}
	logs.OperationID = "runtime-operation-chat"
	logs.Logs = &shared.RuntimeLogRequest{Source: shared.RuntimeLogSourceChat, Cursor: -1, MaxBytes: 1024, MaxLines: 10}
	chatResult, err := agent.executeRuntimeOperation(string(logs.Action), &logs, 10)
	if err != nil || chatResult.Logs == nil || chatResult.Logs.FileName != "server_chat_log.txt" || len(chatResult.Logs.Lines) != 1 {
		t.Fatalf("chat logs=%#v err=%v", chatResult.Logs, err)
	}
	wantStartedAt := time.Date(2026, time.August, 19, 19, 56, 40, 0, time.Local)
	if !chatResult.Logs.StartedAt.Equal(wantStartedAt) {
		t.Fatalf("chat startedAt=%s want=%s", chatResult.Logs.StartedAt, wantStartedAt)
	}

	artifacts := runtimeOperationRequest(shared.RuntimeActionReadArtifacts)
	artifacts.OperationID = "runtime-operation-2"
	artifacts.OperationKey, artifacts.LeaseID, artifacts.FencingToken, artifacts.LeaseExpiresAt = "", "", 0, nil
	artifacts.Artifacts = &shared.RuntimeArtifactRequest{Kind: shared.ArtifactRuntimeHealth}
	artifactResult, err := agent.executeRuntimeOperation(string(artifacts.Action), &artifacts, 10)
	if err != nil || artifactResult.Artifacts == nil || len(artifactResult.Artifacts.Artifacts) != 1 || string(artifactResult.Artifacts.Artifacts[0].Data) != `{"ready":true}` {
		t.Fatalf("artifacts=%#v err=%v", artifactResult.Artifacts, err)
	}
}

func TestRuntimeConsoleRequestRequiresLeaseAndRejectsNewlines(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	agent, _ := newShardOperationAgent(t, runtimeControl)
	request := runtimeOperationRequest(shared.RuntimeActionConsoleSend)
	request.Console = &shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeRaw, Command: "print(1)\nprint(2)"}
	if _, err := agent.executeRuntimeOperation(string(request.Action), &request, 10); err == nil || !strings.Contains(err.Error(), "内容无效") {
		t.Fatalf("newline error=%v", err)
	}
	request.Console.Command = "print(1)"
	request.LeaseID, request.LeaseExpiresAt = "", nil
	if _, err := agent.executeRuntimeOperation(string(request.Action), &request, 10); err == nil || !strings.Contains(err.Error(), "租约") {
		t.Fatalf("lease error=%v", err)
	}
}

func TestRuntimeConsoleHealth(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	agent, _ := newShardOperationAgent(t, runtimeControl)
	request := runtimeOperationRequest(shared.RuntimeActionConsoleHealth)
	request.OperationKey, request.LeaseID, request.FencingToken, request.LeaseExpiresAt = "", "", 0, nil
	result, err := agent.executeRuntimeOperation(string(request.Action), &request, 10)
	if err != nil || result.ConsoleHealth == nil || !result.ConsoleHealth.Available || result.ConsoleHealth.Runtime.State != "running" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestRuntimeCPURequestValidation(t *testing.T) {
	valid := runtimeOperationRequest(shared.RuntimeActionCPUApply)
	valid.CPU = &shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyExclusive, LogicalCPUIds: []int{0, 1}}
	if err := validateRuntimeOperationRequest(string(valid.Action), valid, 30, time.Now().UTC()); err != nil {
		t.Fatalf("valid CPU request rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*shared.RuntimeOperationRequest)
	}{
		{name: "missing payload", mutate: func(request *shared.RuntimeOperationRequest) { request.CPU = nil }},
		{name: "none with CPUs", mutate: func(request *shared.RuntimeOperationRequest) {
			request.CPU = &shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyNone, LogicalCPUIds: []int{0}}
		}},
		{name: "duplicate CPUs", mutate: func(request *shared.RuntimeOperationRequest) {
			request.CPU = &shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyShared, LogicalCPUIds: []int{1, 1}}
		}},
		{name: "unrelated payload", mutate: func(request *shared.RuntimeOperationRequest) {
			request.Console = &shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeManaged, Command: "c_save()"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := valid
			cpu := *valid.CPU
			cpu.LogicalCPUIds = append([]int(nil), valid.CPU.LogicalCPUIds...)
			request.CPU = &cpu
			test.mutate(&request)
			if err := validateRuntimeOperationRequest(string(request.Action), request, 30, time.Now().UTC()); err == nil {
				t.Fatal("invalid CPU request accepted")
			}
		})
	}

	observe := valid
	observe.Action = shared.RuntimeActionCPUObserve
	observe.OperationKey, observe.LeaseID, observe.FencingToken, observe.LeaseExpiresAt = "", "", 0, nil
	if err := validateRuntimeOperationRequest(string(observe.Action), observe, 30, time.Now().UTC()); err != nil {
		t.Fatalf("read-only CPU observation rejected: %v", err)
	}
}

func TestRuntimeMapRequestValidation(t *testing.T) {
	sessions := runtimeOperationRequest(shared.RuntimeActionMapSessions)
	sessions.OperationKey, sessions.LeaseID, sessions.FencingToken, sessions.LeaseExpiresAt = "", "", 0, nil
	sessions.Map = &shared.RuntimeMapRequest{}
	if err := validateRuntimeOperationRequest(string(sessions.Action), sessions, 30, time.Now().UTC()); err != nil {
		t.Fatalf("valid Session request rejected: %v", err)
	}

	render := runtimeOperationRequest(shared.RuntimeActionMapRender)
	render.Map = &shared.RuntimeMapRequest{
		TransferID: "map-transfer-0001", SessionID: "ABC123", FileName: "0000000010",
		Layers: []string{"terrain", "features", "worldState"},
	}
	if err := validateRuntimeOperationRequest(string(render.Action), render, 300, time.Now().UTC()); err != nil {
		t.Fatalf("valid render request rejected: %v", err)
	}

	read := runtimeOperationRequest(shared.RuntimeActionMapRead)
	read.OperationKey, read.LeaseID, read.FencingToken, read.LeaseExpiresAt = "", "", 0, nil
	read.Map = &shared.RuntimeMapRequest{TransferID: "map-transfer-0001", Offset: 1024}
	if err := validateRuntimeOperationRequest(string(read.Action), read, 30, time.Now().UTC()); err != nil {
		t.Fatalf("valid map read rejected: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*shared.RuntimeOperationRequest)
	}{
		{name: "path escape", mutate: func(request *shared.RuntimeOperationRequest) { request.Map.FileName = "../snapshot" }},
		{name: "duplicate layer", mutate: func(request *shared.RuntimeOperationRequest) { request.Map.Layers = []string{"terrain", "terrain"} }},
		{name: "unknown layer", mutate: func(request *shared.RuntimeOperationRequest) { request.Map.Layers = []string{"players"} }},
		{name: "unrelated payload", mutate: func(request *shared.RuntimeOperationRequest) { request.Logs = &shared.RuntimeLogRequest{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := render
			value := *render.Map
			value.Layers = append([]string(nil), render.Map.Layers...)
			request.Map = &value
			test.mutate(&request)
			if err := validateRuntimeOperationRequest(string(request.Action), request, 300, time.Now().UTC()); err == nil {
				t.Fatal("invalid map request accepted")
			}
		})
	}
}

func TestRuntimeConfigurationPublicationWritesManagedFiles(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}}
	agent, installation := newShardOperationAgent(t, runtimeControl)
	roomRoot := filepath.Join(installation.SavePath, "Cluster_1")
	clusterPath := filepath.Join(roomRoot, "cluster.ini")
	if err := os.WriteFile(clusterPath, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("cluster.ini")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	payload := archive.Bytes()
	digest := sha256.Sum256(payload)
	publicationID := "configuration-publication-0001"
	scope := "shared"
	steps := []struct {
		action shared.RuntimeAction
		value  shared.RuntimeConfigurationRequest
	}{
		{shared.RuntimeActionConfigurationBegin, shared.RuntimeConfigurationRequest{PublicationID: publicationID, Scope: scope, Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}},
		{shared.RuntimeActionConfigurationWrite, shared.RuntimeConfigurationRequest{PublicationID: publicationID, Scope: scope, Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:]), Data: payload}},
		{shared.RuntimeActionConfigurationPrepare, shared.RuntimeConfigurationRequest{PublicationID: publicationID, Scope: scope}},
		{shared.RuntimeActionConfigurationPublish, shared.RuntimeConfigurationRequest{PublicationID: publicationID, Scope: scope}},
		{shared.RuntimeActionConfigurationComplete, shared.RuntimeConfigurationRequest{PublicationID: publicationID, Scope: scope}},
	}
	for index, step := range steps {
		request := runtimeOperationRequest(step.action)
		request.OperationID = fmt.Sprintf("configuration-operation-%d", index)
		request.OperationKey = fmt.Sprintf("configuration-key-%d", index)
		request.Configuration = &step.value
		result, err := agent.executeRuntimeOperation(string(request.Action), &request, 30)
		if err != nil || result.Configuration == nil {
			t.Fatalf("step %s result=%#v error=%v", step.action, result, err)
		}
	}
	written, err := os.ReadFile(clusterPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != "new\n" {
		t.Fatalf("cluster.ini=%q", written)
	}
}

func TestRememberedRuntimeOperationsAreBounded(t *testing.T) {
	values := make(map[string]rememberedRuntimeOperation)
	for index := 0; index < maximumRememberedOperationsPerRoom+10; index++ {
		key := fmt.Sprintf("operation-%03d", index)
		values[key] = rememberedRuntimeOperation{
			Completed: true,
			Result:    shared.RuntimeOperationResult{ObservedAt: time.Unix(int64(index), 0).UTC()},
		}
	}
	trimRememberedRuntimeOperations(values)
	if len(values) != maximumRememberedOperationsPerRoom {
		t.Fatalf("remembered runtime operations=%d", len(values))
	}
	if _, exists := values["operation-000"]; exists {
		t.Fatal("oldest runtime operation was not trimmed")
	}
}

func TestRuntimeRestoreKeepsOffsetSafetyAfterIdempotencyHistoryIsTrimmed(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}}
	agent, _ := newShardOperationAgent(t, runtimeControl)
	const transferSize = maximumRememberedOperationsPerRoom + 1
	backupID := "restore-agent-offset-0001"
	digest := strings.Repeat("a", 64)
	sharedDigest := strings.Repeat("b", 64)

	begin := runtimeOperationRequest(shared.RuntimeActionRestoreBegin)
	begin.OperationID, begin.OperationKey = "restore-begin-operation", "restore-begin-key"
	begin.Cluster = "Cluster_2"
	begin.Backup = &shared.RuntimeBackupRequest{
		BackupID: backupID, Size: transferSize, ContentSize: 1, FileCount: 2,
		SHA256: digest, SharedSHA256: sharedDigest,
	}
	if _, err := agent.executeRuntimeOperation(string(begin.Action), &begin, 30); err != nil {
		t.Fatalf("begin restore failed: %v", err)
	}

	var first shared.RuntimeOperationRequest
	for offset := 0; offset < transferSize; offset++ {
		write := runtimeOperationRequest(shared.RuntimeActionRestoreWrite)
		write.OperationID = fmt.Sprintf("restore-write-operation-%03d", offset)
		write.OperationKey = fmt.Sprintf("restore-write-key-%03d", offset)
		write.Cluster = "Cluster_2"
		write.Backup = &shared.RuntimeBackupRequest{
			BackupID: backupID, Offset: int64(offset), Size: transferSize, SHA256: digest, Data: []byte{'x'},
		}
		if offset == 0 {
			first = write
		}
		result, err := agent.executeRuntimeOperation(string(write.Action), &write, 30)
		if err != nil || result.Backup == nil || result.Backup.NextOffset != int64(offset+1) {
			t.Fatalf("write %d result=%#v err=%v", offset, result.Backup, err)
		}
	}

	// The first write is older than the bounded idempotency history. The
	// transfer file's offset check remains the authoritative replay guard.
	if _, err := agent.executeRuntimeOperation(string(first.Action), &first, 30); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("evicted chunk replay error=%v", err)
	}
}

func TestAgentRuntimeMigrationActionsMoveShardEndToEnd(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}}
	agent, installation := newShardOperationAgent(t, runtimeControl)
	sourceFile := filepath.Join(installation.SavePath, "Cluster_1", "Master", "save", "session", "world-data")
	if err := os.MkdirAll(filepath.Dir(sourceFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceFile, []byte("agent-migration-data"), 0o600); err != nil {
		t.Fatal(err)
	}

	migrationID := "migration-agent-e2e-0001"
	sequence := 0
	mutate := func(action shared.RuntimeAction, cluster string, migration shared.RuntimeMigrationRequest) shared.RuntimeOperationResult {
		t.Helper()
		sequence++
		request := runtimeOperationRequest(action)
		request.OperationID = fmt.Sprintf("runtime-migration-%03d", sequence)
		request.OperationKey = fmt.Sprintf("runtime-migration-key-%03d", sequence)
		request.Cluster = cluster
		request.Migration = &migration
		result, err := agent.executeRuntimeOperation(string(action), &request, 30)
		if err != nil {
			t.Fatalf("%s failed: %v", action, err)
		}
		return result
	}
	observe := func(action shared.RuntimeAction, cluster string, migration shared.RuntimeMigrationRequest) shared.RuntimeOperationResult {
		t.Helper()
		sequence++
		request := runtimeOperationRequest(action)
		request.OperationID = fmt.Sprintf("runtime-migration-%03d", sequence)
		request.OperationKey, request.LeaseID, request.FencingToken, request.LeaseExpiresAt = "", "", 0, nil
		request.Cluster = cluster
		request.Migration = &migration
		result, err := agent.executeRuntimeOperation(string(action), &request, 30)
		if err != nil {
			t.Fatalf("%s failed: %v", action, err)
		}
		return result
	}

	prepared := mutate(shared.RuntimeActionMigrationExportPrepare, "Cluster_1", shared.RuntimeMigrationRequest{MigrationID: migrationID})
	if prepared.Migration == nil || prepared.Migration.Size == 0 || len(prepared.Migration.SHA256) != 64 {
		t.Fatalf("prepared migration=%#v", prepared.Migration)
	}
	descriptor := *prepared.Migration
	mutate(shared.RuntimeActionMigrationImportBegin, "Cluster_2", shared.RuntimeMigrationRequest{
		MigrationID: migrationID, Size: descriptor.Size, SHA256: descriptor.SHA256,
	})
	for offset := int64(0); offset < descriptor.Size; {
		chunk := observe(shared.RuntimeActionMigrationExportRead, "Cluster_1", shared.RuntimeMigrationRequest{MigrationID: migrationID, Offset: offset})
		if chunk.Migration == nil || len(chunk.Migration.Data) == 0 {
			t.Fatalf("migration chunk=%#v", chunk.Migration)
		}
		written := mutate(shared.RuntimeActionMigrationImportWrite, "Cluster_2", shared.RuntimeMigrationRequest{
			MigrationID: migrationID, Offset: offset, Size: descriptor.Size, SHA256: descriptor.SHA256, Data: chunk.Migration.Data,
		})
		if written.Migration == nil || written.Migration.NextOffset <= offset {
			t.Fatalf("migration write=%#v", written.Migration)
		}
		offset = written.Migration.NextOffset
	}
	mutate(shared.RuntimeActionMigrationImportCommit, "Cluster_2", shared.RuntimeMigrationRequest{MigrationID: migrationID})
	finalized := mutate(shared.RuntimeActionMigrationSourceFinalize, "Cluster_1", shared.RuntimeMigrationRequest{MigrationID: migrationID})
	if finalized.Migration == nil || finalized.Migration.RecoveryRef == "" {
		t.Fatalf("source finalize=%#v", finalized.Migration)
	}
	mutate(shared.RuntimeActionMigrationTargetComplete, "Cluster_2", shared.RuntimeMigrationRequest{MigrationID: migrationID})
	mutate(shared.RuntimeActionMigrationSourceComplete, "Cluster_1", shared.RuntimeMigrationRequest{MigrationID: migrationID})
	mutate(shared.RuntimeActionMigrationExportRelease, "Cluster_1", shared.RuntimeMigrationRequest{MigrationID: migrationID})

	targetData, err := os.ReadFile(filepath.Join(installation.SavePath, "Cluster_2", "Master", "save", "session", "world-data"))
	if err != nil || string(targetData) != "agent-migration-data" {
		t.Fatalf("target data=%q err=%v", targetData, err)
	}
	if _, err := os.Stat(filepath.Join(installation.SavePath, "Cluster_1", "Master")); !os.IsNotExist(err) {
		t.Fatalf("source shard remained active: %v", err)
	}
}
