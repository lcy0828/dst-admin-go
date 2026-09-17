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
	"dont/internal/runtimefiles"
	"dont/shared"
)

type emptyMigrationExecutor struct{}

func TestAgentModWriteRequiresConfirmedDigestAndPreservesConflict(t *testing.T) {
	content := []byte("return {}\n")
	digest := sha256.Sum256(content)
	for _, value := range []struct {
		name         string
		result       *shared.RuntimeConfigurationResult
		err          error
		wantError    bool
		wantConflict bool
	}{
		{name: "confirmed", result: &shared.RuntimeConfigurationResult{Complete: true, SHA256: hex.EncodeToString(digest[:])}},
		{name: "missing", wantError: true},
		{name: "wrong digest", result: &shared.RuntimeConfigurationResult{Complete: true, SHA256: strings.Repeat("a", 64)}, wantError: true},
		{name: "conflict", result: &shared.RuntimeConfigurationResult{RevisionConflict: true, SHA256: strings.Repeat("b", 64)}, err: errors.New("remote failure"), wantError: true, wantConflict: true},
	} {
		t.Run(value.name, func(t *testing.T) {
			driver, err := NewAgent(failedMigrationExecutor{result: agents.RuntimeExecutionResult{
				Result: shared.RuntimeOperationResult{Configuration: value.result},
			}, err: value.err})
			if err != nil {
				t.Fatal(err)
			}
			err = driver.WriteModOverrides(context.Background(), Target{TargetID: "agent:node", InstallationID: "native", Cluster: "Cluster", Shard: "Master"}, Operation{}, strings.Repeat("a", 64), content)
			var conflict *runtimefiles.ConfigurationConflictError
			if (err != nil) != value.wantError || errors.As(err, &conflict) != value.wantConflict {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

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

type capturedModFetchExecutor struct {
	emptyMigrationExecutor
	targetID string
	request  shared.RuntimeOperationRequest
	timeout  int
	result   shared.RuntimeModResult
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

func (f *capturedModFetchExecutor) ExecuteRuntime(_ context.Context, targetID string, request shared.RuntimeOperationRequest, timeout int) (agents.RuntimeExecutionResult, error) {
	f.targetID, f.request, f.timeout = targetID, request, timeout
	return agents.RuntimeExecutionResult{Result: shared.RuntimeOperationResult{Mod: &f.result}}, nil
}

func (emptyMigrationExecutor) ExecuteRuntime(context.Context, string, shared.RuntimeOperationRequest, int) (agents.RuntimeExecutionResult, error) {
	return agents.RuntimeExecutionResult{Result: shared.RuntimeOperationResult{}}, nil
}

type migrationPeerExecutor struct {
	requests []shared.RuntimeOperationRequest
	timeouts []int
	fail     bool
}

func (*migrationPeerExecutor) ExecuteShard(context.Context, string, shared.ShardOperationRequest, int) (agents.ShardExecutionResult, error) {
	return agents.ShardExecutionResult{}, nil
}

func (e *migrationPeerExecutor) ExecuteRuntime(_ context.Context, _ string, request shared.RuntimeOperationRequest, timeout int) (agents.RuntimeExecutionResult, error) {
	e.requests = append(e.requests, request)
	e.timeouts = append(e.timeouts, timeout)
	result := shared.RuntimeOperationResult{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: request.OperationID, OperationKey: request.OperationKey,
		InstallationID: request.InstallationID, Action: request.Action, Cluster: request.Cluster, Shard: request.Shard,
		Migration: &shared.RuntimeMigrationResult{
			MigrationID: request.Migration.MigrationID, Size: request.Migration.Size, SHA256: request.Migration.SHA256,
		},
	}
	if request.Action == shared.RuntimeActionMigrationPeerGrant {
		result.Migration.Complete = true
		result.Migration.FetchLocation = &shared.RuntimeMigrationFetchLocation{
			DownloadURL:   "http://peer.example.test/migration-peer/default/" + request.Migration.MigrationID,
			DownloadPath:  "/migration-peer/default/" + request.Migration.MigrationID,
			DownloadToken: strings.Repeat("t", 40), Size: request.Migration.Size, SHA256: request.Migration.SHA256,
			ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
		}
		return agents.RuntimeExecutionResult{RemoteID: "peer-grant", Result: result}, nil
	}
	result.Migration.NextOffset = request.Migration.Size / 2
	if e.fail {
		return agents.RuntimeExecutionResult{RemoteID: "peer-fetch", Result: result}, errors.New("confirmed peer fetch failure")
	}
	result.Migration.NextOffset, result.Migration.Complete = request.Migration.Size, true
	return agents.RuntimeExecutionResult{RemoteID: "peer-fetch", Result: result}, nil
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

func TestAgentDriverUsesTypedMigrationPeerContracts(t *testing.T) {
	executor := &migrationPeerExecutor{}
	driver, err := NewAgent(executor)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := MigrationDescriptor{MigrationID: "migration-peer-driver-0001", Size: 4096, SHA256: strings.Repeat("a", 64)}
	target := Target{TargetID: "agent:source", InstallationID: "default", Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1"}
	location, err := driver.GrantMigrationExport(context.Background(), target, descriptor, "agent:target")
	if err != nil || location.Size != descriptor.Size {
		t.Fatalf("location=%#v err=%v", location, err)
	}
	expires := time.Now().UTC().Add(5 * time.Minute)
	operation := Operation{ID: "peer-fetch-operation", Key: "peer-fetch-key", LeaseID: "lease-1", FencingToken: 1, LeaseExpiresAt: &expires}
	bytes, err := driver.FetchMigrationImport(context.Background(), target, operation, descriptor, []shared.RuntimeMigrationFetchLocation{location})
	if err != nil || bytes != descriptor.Size || len(executor.requests) != 2 || executor.timeouts[1] != 1800 {
		t.Fatalf("bytes=%d requests=%#v timeouts=%v err=%v", bytes, executor.requests, executor.timeouts, err)
	}
	if executor.requests[0].Action != shared.RuntimeActionMigrationPeerGrant || executor.requests[0].Migration.PeerSubject != "agent:target" ||
		executor.requests[1].Action != shared.RuntimeActionMigrationFetch || len(executor.requests[1].Migration.FetchLocations) != 1 {
		t.Fatalf("unexpected Peer requests: %#v", executor.requests)
	}
}

func TestAgentDriverMarksConfirmedMigrationPeerFailureAsFallbackSafe(t *testing.T) {
	executor := &migrationPeerExecutor{fail: true}
	driver, err := NewAgent(executor)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := MigrationDescriptor{MigrationID: "migration-peer-driver-fail-0001", Size: 4096, SHA256: strings.Repeat("a", 64)}
	expires := time.Now().UTC().Add(5 * time.Minute)
	_, err = driver.FetchMigrationImport(context.Background(), Target{
		TargetID: "agent:target", InstallationID: "default", Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1",
	}, Operation{ID: "peer-fail-operation", Key: "peer-fail-key", LeaseID: "lease-1", FencingToken: 1, LeaseExpiresAt: &expires}, descriptor, []shared.RuntimeMigrationFetchLocation{{
		DownloadURL:   "http://peer.example.test/migration-peer/default/" + descriptor.MigrationID,
		DownloadPath:  "/migration-peer/default/" + descriptor.MigrationID,
		DownloadToken: strings.Repeat("t", 40), Size: descriptor.Size, SHA256: descriptor.SHA256, ExpiresAt: expires,
	}})
	if !errors.Is(err, ErrMigrationPeerFallback) {
		t.Fatalf("confirmed fetch error=%v", err)
	}
}

func TestSnapshotBarrierAcceptsRealKLEIPersistentJSON(t *testing.T) {
	data := []byte(`KLEI     1 {"schemaVersion":1,"producerVersion":"2.4.6","producerInstanceId":"barrier-instance","barrierId":"hot-test-0001","state":"prepared","sessionId":"SESSION","shardId":"2","snapshotBefore":7,"preparedAtUnix":1787118371}`)
	driver, err := NewAgent(barrierArtifactExecutor{data: data})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := driver.SnapshotBarrier(context.Background(), Target{TargetID: "agent:node", InstallationID: "container", Cluster: "Cluster", Shard: "Caves"}, "hot-test-0001")
	if err != nil || receipt.State != "prepared" || receipt.SnapshotBefore != 7 || receipt.ProducerInstanceID != "barrier-instance" {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
}

func TestAgentFetchModCachePreservesInstallationOperationAndSourceContract(t *testing.T) {
	const workshopID = "1392778117"
	treeSHA := strings.Repeat("a", 64)
	manifest := shared.RuntimeModCacheManifest{
		WorkshopID: workshopID, TreeSHA256: treeSHA, ManifestSHA256: strings.Repeat("b", 64),
		Size: 1048576, FileCount: 12,
	}
	executor := &capturedModFetchExecutor{result: shared.RuntimeModResult{
		Complete: true, FetchSource: shared.RuntimeModFetchSourceSteam, CacheManifest: &manifest,
	}}
	driver, err := NewAgent(executor)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(5 * time.Minute)
	target := Target{
		TargetID: "agent:node-one", InstallationID: "native", RoomID: "room-one", WorldID: "master",
		Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "topology-revision-one",
	}
	operation := Operation{
		ID: "mod-fetch-operation", Key: "mod-fetch-step", LeaseID: "room-lease",
		FencingToken: 17, LeaseExpiresAt: &expires,
	}
	metadata := shared.RuntimeModMetadata{Title: "Global Positions", Version: "1.7.6", PublishedFileSize: manifest.Size}

	got, err := driver.FetchModCache(context.Background(), target, operation, workshopID, treeSHA, metadata, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Manifest.TreeSHA256 != treeSHA || got.Manifest.WorkshopID != workshopID {
		t.Fatalf("unexpected cache manifest: %#v", got)
	}
	if len(got.Attempts) != 1 || got.Attempts[0].Source != shared.RuntimeModFetchSourceSteam || !got.Attempts[0].Selected {
		t.Fatalf("fetch observations=%#v", got.Attempts)
	}
	request := executor.request
	if executor.targetID != target.TargetID || executor.timeout != 300 || request.ProtocolVersion != shared.RuntimeOperationProtocolVersion {
		t.Fatalf("unexpected fetch dispatch: target=%q timeout=%d request=%#v", executor.targetID, executor.timeout, request)
	}
	if request.Action != shared.RuntimeActionModFetch || request.InstallationID != target.InstallationID ||
		request.TopologyRevision != target.TopologyRevision || request.Cluster != "Mods" || request.Shard != "Installation" {
		t.Fatalf("fetch did not use the installation-scoped Mod contract: %#v", request)
	}
	if request.OperationID != operation.ID || request.OperationKey != operation.Key || request.LeaseID != operation.LeaseID ||
		request.FencingToken != operation.FencingToken || request.LeaseExpiresAt == nil || !request.LeaseExpiresAt.Equal(expires) {
		t.Fatalf("fetch lost idempotency or fencing identity: %#v", request)
	}
	if request.Mod == nil || request.Mod.WorkshopID != workshopID || request.Mod.ExpectedTreeSHA256 != treeSHA ||
		request.Mod.Metadata != metadata || len(request.Mod.FetchSources) != 1 || request.Mod.FetchSources[0] != shared.RuntimeModFetchSourceSteam {
		t.Fatalf("fetch payload changed: %#v", request.Mod)
	}
}

func TestAgentGrantModArtifactUsesOptionalPeerContract(t *testing.T) {
	treeSHA := strings.Repeat("a", 64)
	location := shared.RuntimeModFetchLocation{
		Source: shared.RuntimeModFetchSourcePeer, DownloadURL: "http://192.0.2.10:18081/mod-peer/native/1392778117/" + treeSHA,
		DownloadPath: "/mod-peer/native/1392778117/" + treeSHA, DownloadToken: strings.Repeat("t", 32),
		Size: 1024, SHA256: strings.Repeat("b", 64),
	}
	executor := &capturedModFetchExecutor{result: shared.RuntimeModResult{Complete: true, FetchLocation: &location}}
	driver, err := NewAgent(executor)
	if err != nil {
		t.Fatal(err)
	}
	target := Target{TargetID: "agent:source", InstallationID: "native", TopologyRevision: "revision"}
	got, err := driver.GrantModArtifact(context.Background(), target, "agent:destination", "1392778117", treeSHA)
	if err != nil || got.DownloadURL != location.DownloadURL {
		t.Fatalf("location=%#v err=%v", got, err)
	}
	if executor.targetID != target.TargetID || executor.request.Action != shared.RuntimeActionModPeerGrant ||
		executor.request.Mod == nil || executor.request.Mod.PeerSubject != "agent:destination" ||
		executor.request.InstallationID != "native" {
		t.Fatalf("peer grant request=%#v target=%q", executor.request, executor.targetID)
	}
}
