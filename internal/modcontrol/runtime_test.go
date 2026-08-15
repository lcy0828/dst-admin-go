package modcontrol

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/moddistribution"
	"dont/internal/modpublication"
	"dont/internal/runtimedriver"
	"dont/shared"
)

type runtimeFixture struct {
	root       string
	cache      string
	state      string
	server     string
	saves      string
	transfers  string
	workshop   string
	manager    *moddistribution.Manager
	manifest   moddistribution.Manifest
	artifact   modpublication.ContentArtifact
	modContent []byte
}

func newRuntimeFixture(t *testing.T, contentSize int) runtimeFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if entry.IsDir() {
				_ = os.Chmod(path, 0o700)
			} else {
				_ = os.Chmod(path, 0o600)
			}
			return nil
		})
	})
	fixture := runtimeFixture{
		root: root, cache: filepath.Join(root, "cache"), state: filepath.Join(root, "state"),
		server: filepath.Join(root, "server"), saves: filepath.Join(root, "saves"),
		transfers: filepath.Join(root, "transfers"), workshop: filepath.Join(root, "workshop", "1392778117"),
	}
	for _, directory := range []string{fixture.server, fixture.saves, fixture.workshop} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if contentSize < 1 {
		contentSize = 1
	}
	fixture.modContent = bytes.Repeat([]byte("m"), contentSize)
	if err := os.WriteFile(filepath.Join(fixture.workshop, "modinfo.lua"), fixture.modContent, 0o640); err != nil {
		t.Fatal(err)
	}
	fixture.manager, err = moddistribution.New(moddistribution.Config{
		CacheRoot: fixture.cache, StateRoot: fixture.state, NodeID: "local",
		Installations: []moddistribution.TrustedInstallation{{
			ID: "primary", NodeID: "local", ServerPath: fixture.server, SavePath: fixture.saves,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.manifest, err = fixture.manager.Import(context.Background(), "1392778117", fixture.workshop, moddistribution.Metadata{Title: "Runtime Test"})
	if err != nil {
		t.Fatal(err)
	}
	fixture.artifact = modpublication.ContentArtifact{
		WorkshopID: fixture.manifest.WorkshopID, TreeSHA256: fixture.manifest.TreeSHA256,
		ManifestSHA256: fixture.manifest.ManifestSHA256, Size: fixture.manifest.Size,
		FileCount: fixture.manifest.FileCount, SourceRef: "cache://1392778117/" + fixture.manifest.TreeSHA256,
	}
	return fixture
}

func publicationOperation(targetID, installationID string) modpublication.RuntimeOperation {
	expires := time.Now().UTC().Add(time.Minute)
	return modpublication.RuntimeOperation{
		PublicationID: "publication-runtime-0001", TopologyRevision: "topology-revision-0001",
		PlanHash: testSHA([]byte("plan")), Action: "prepare", IdempotencyKey: "publication-runtime-0001:target:prepare",
		Fences: []modpublication.Fence{
			{RoomID: "room-1", LeaseID: "lease-room-0001", OperationKey: "mod.publish:publication-runtime-0001", FencingToken: 1, ExpiresAt: expires},
			{RoomID: installationResource(targetID, installationID), LeaseID: "lease-install-0001", OperationKey: "mod.publish:publication-runtime-0001", FencingToken: 2, ExpiresAt: expires},
		},
	}
}

func remoteTarget(artifact modpublication.ContentArtifact, overrides []byte) modpublication.TargetPlan {
	return modpublication.TargetPlan{
		TargetID: "target-remote", NodeID: "agent-remote", InstallationID: "remote-primary",
		Mods: []modpublication.ContentArtifact{artifact},
		Worlds: []modpublication.WorldPlan{{
			RoomID: "room-1", RoomDirectory: "Cluster_1", WorldID: "master", WorldDirectory: "Master",
			Mods: []modpublication.ContentArtifact{artifact}, ModOverrides: overrides,
		}},
	}
}

func TestRemoteBundleUploadIsChunkedAndFullyVerified(t *testing.T) {
	fixture := newRuntimeFixture(t, shared.MaxChunkBytes*2+31)
	driver := &testModDriver{
		inspect: map[string]shared.RuntimeModCacheManifest{
			fixture.artifact.WorkshopID + "\x00manifest": {
				ManifestSHA256: fixture.artifact.ManifestSHA256, Size: fixture.artifact.Size, FileCount: fixture.artifact.FileCount,
			},
		},
	}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	target := remoteTarget(fixture.artifact, []byte("return {}\n"))
	operation := publicationOperation(target.TargetID, target.InstallationID)
	if err := runtime.EnsureCache(context.Background(), target, operation); err != nil {
		t.Fatal(err)
	}
	if driver.cacheDescriptor.WorkshopID != fixture.artifact.WorkshopID ||
		driver.cacheDescriptor.ExpectedTreeSHA256 != fixture.artifact.TreeSHA256 ||
		driver.cacheDescriptor.Size != int64(len(driver.cacheData)) || driver.cacheDescriptor.SHA256 != testSHA(driver.cacheData) {
		t.Fatalf("invalid cache descriptor: %#v data=%d", driver.cacheDescriptor, len(driver.cacheData))
	}
	if driver.cacheDescriptor.Metadata.PublishedFileSize != fixture.artifact.Size || len(driver.cacheChunkSizes) < 2 {
		t.Fatalf("bundle was not chunked with metadata: descriptor=%#v chunks=%#v", driver.cacheDescriptor, driver.cacheChunkSizes)
	}
	for _, size := range driver.cacheChunkSizes {
		if size <= 0 || size > shared.MaxChunkBytes {
			t.Fatalf("invalid cache chunk size %d", size)
		}
	}
	if driver.cacheTarget.TopologyRevision != operation.TopologyRevision || driver.cacheTarget.TopologyRevision == operation.PlanHash {
		t.Fatalf("runtime target used the wrong revision: %#v", driver.cacheTarget)
	}
	if driver.cacheOperation.LeaseID != "lease-install-0001" || driver.cacheOperation.FencingToken != 2 {
		t.Fatalf("installation fence was not propagated: %#v", driver.cacheOperation)
	}
	seenIDs := make(map[string]bool, len(driver.runtimeCalls))
	seenKeys := make(map[string]bool, len(driver.runtimeCalls))
	for _, call := range driver.runtimeCalls {
		if call.ID == "" || call.Key == "" || call.LeaseID != "lease-install-0001" || call.FencingToken != 2 {
			t.Fatalf("cache upload step left its installation session: %#v", call)
		}
		if seenIDs[call.ID] || seenKeys[call.Key] {
			t.Fatalf("cache upload steps reused an idempotency identity: %#v", driver.runtimeCalls)
		}
		seenIDs[call.ID], seenKeys[call.Key] = true, true
	}
}

func TestRemoteRuntimeRenewsExpiringInstallationFence(t *testing.T) {
	fixture := newRuntimeFixture(t, shared.MaxChunkBytes+17)
	driver := &testModDriver{inspect: map[string]shared.RuntimeModCacheManifest{
		fixture.artifact.WorkshopID + "\x00manifest": {
			ManifestSHA256: fixture.artifact.ManifestSHA256, Size: fixture.artifact.Size, FileCount: fixture.artifact.FileCount,
		},
	}}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	target := remoteTarget(fixture.artifact, []byte("return {}\n"))
	operation := publicationOperation(target.TargetID, target.InstallationID)
	expiring := time.Now().UTC().Add(10 * time.Second)
	for index := range operation.Fences {
		operation.Fences[index].ExpiresAt = expiring
	}
	renewedUntil := time.Now().UTC().Add(5 * time.Minute)
	renews := 0
	operation.RenewFences = func(_ context.Context, fences []modpublication.Fence) ([]modpublication.Fence, error) {
		renews++
		for index := range fences {
			fences[index].ExpiresAt = renewedUntil
		}
		return fences, nil
	}
	if err := runtime.EnsureCache(context.Background(), target, operation); err != nil {
		t.Fatal(err)
	}
	if renews != 1 {
		t.Fatalf("expected one proactive lease renewal, got %d", renews)
	}
	for _, call := range driver.runtimeCalls {
		if call.LeaseExpiresAt == nil || !call.LeaseExpiresAt.Equal(renewedUntil) {
			t.Fatalf("runtime step used stale lease expiry: %#v", call)
		}
	}
}

func TestRemoteCacheUsesTreeIdentityAndRejectsContentMismatch(t *testing.T) {
	fixture := newRuntimeFixture(t, 128)
	target := remoteTarget(fixture.artifact, []byte("return {}\n"))
	operation := publicationOperation(target.TargetID, target.InstallationID)
	t.Run("node-local manifest digest does not trigger upload", func(t *testing.T) {
		driver := &testModDriver{inspect: map[string]shared.RuntimeModCacheManifest{
			fixture.artifact.WorkshopID + "\x00" + fixture.artifact.TreeSHA256: {
				WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: fixture.artifact.TreeSHA256,
				ManifestSHA256: strings.Repeat("0", 64), Size: fixture.artifact.Size, FileCount: fixture.artifact.FileCount,
			},
			fixture.artifact.WorkshopID + "\x00manifest": {
				ManifestSHA256: fixture.artifact.ManifestSHA256, Size: fixture.artifact.Size, FileCount: fixture.artifact.FileCount,
			},
		}}
		runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, filepath.Join(fixture.root, "transfer-inspect"))
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.EnsureCache(context.Background(), target, operation); err != nil {
			t.Fatal(err)
		}
		if len(driver.cacheData) != 0 {
			t.Fatal("matching content was uploaded again because node-local manifest metadata differed")
		}
	})

	t.Run("commit accepts node-local manifest digest", func(t *testing.T) {
		committed := shared.RuntimeModCacheManifest{
			WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: fixture.artifact.TreeSHA256,
			ManifestSHA256: strings.Repeat("0", 64), Size: fixture.artifact.Size,
			FileCount: fixture.artifact.FileCount,
		}
		driver := &testModDriver{
			inspect: map[string]shared.RuntimeModCacheManifest{}, cacheCommit: &committed,
		}
		runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, filepath.Join(fixture.root, "transfer-manifest-metadata"))
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.EnsureCache(context.Background(), target, operation); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("commit mismatch rejected", func(t *testing.T) {
		bad := shared.RuntimeModCacheManifest{
			WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: fixture.artifact.TreeSHA256,
			ManifestSHA256: fixture.artifact.ManifestSHA256, Size: fixture.artifact.Size + 1,
			FileCount: fixture.artifact.FileCount,
		}
		driver := &testModDriver{
			inspect: map[string]shared.RuntimeModCacheManifest{}, cacheCommit: &bad,
		}
		runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, filepath.Join(fixture.root, "transfer-commit"))
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.EnsureCache(context.Background(), target, operation); err == nil {
			t.Fatal("mismatched committed manifest was accepted")
		}
	})
}

func TestRemoteReleasePlanUsesChunksIdentitiesAndLifecycle(t *testing.T) {
	fixture := newRuntimeFixture(t, 64)
	driver := &testModDriver{inspect: make(map[string]shared.RuntimeModCacheManifest)}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	overrides := bytes.Repeat([]byte("x"), shared.MaxChunkBytes*2+17)
	target := remoteTarget(fixture.artifact, overrides)
	operation := publicationOperation(target.TargetID, target.InstallationID)
	if err := runtime.Prepare(context.Background(), target, operation); err != nil {
		t.Fatal(err)
	}
	if driver.planDescriptor.OperationID != operation.PublicationID || driver.planDescriptor.Size != int64(len(driver.planData)) || driver.planDescriptor.SHA256 != testSHA(driver.planData) {
		t.Fatalf("invalid plan descriptor: %#v", driver.planDescriptor)
	}
	if len(driver.planChunkSizes) < 2 {
		t.Fatalf("release plan was not chunked: %#v", driver.planChunkSizes)
	}
	for _, size := range driver.planChunkSizes {
		if size <= 0 || size > shared.MaxChunkBytes {
			t.Fatalf("invalid plan chunk size %d", size)
		}
	}
	wire, err := decodePlan(driver.planData)
	if err != nil {
		t.Fatal(err)
	}
	if wire.OperationID != operation.PublicationID || wire.NodeID != target.NodeID || len(wire.Shards) != 1 ||
		wire.Shards[0].InstallationID != target.InstallationID || wire.Shards[0].RoomDirectory != "Cluster_1" ||
		wire.Shards[0].WorldDirectory != "Master" || !bytes.Equal(wire.Shards[0].ModOverrides, overrides) {
		t.Fatalf("release plan identities changed: %#v", wire)
	}
	if driver.planTarget.TopologyRevision != operation.TopologyRevision || driver.planOperation.FencingToken != 2 {
		t.Fatalf("plan target or fence mismatch: target=%#v operation=%#v", driver.planTarget, driver.planOperation)
	}
	if err := runtime.Publish(context.Background(), target, operation); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Rollback(context.Background(), target, operation); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Complete(context.Background(), target, operation); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(driver.operations, ","); got != "plan-commit,prepare,publish,rollback,complete" {
		t.Fatalf("unexpected remote lifecycle: %s", got)
	}
	seenIDs := make(map[string]bool, len(driver.runtimeCalls))
	seenKeys := make(map[string]bool, len(driver.runtimeCalls))
	for _, call := range driver.runtimeCalls {
		if call.ID == "" || call.Key == "" || call.Key == operation.Fences[1].OperationKey {
			t.Fatalf("runtime step did not receive an independent idempotency identity: %#v", call)
		}
		if seenIDs[call.ID] || seenKeys[call.Key] {
			t.Fatalf("runtime steps reused an idempotency identity: %#v", driver.runtimeCalls)
		}
		seenIDs[call.ID], seenKeys[call.Key] = true, true
	}
}

func TestLocalRuntimePublishesSameDesiredWorldState(t *testing.T) {
	fixture := newRuntimeFixture(t, 64)
	driver := &testModDriver{}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	overrides := []byte("return { [\"workshop-1392778117\"] = { enabled = true } }\n")
	target := modpublication.TargetPlan{
		TargetID: "local", NodeID: "local", InstallationID: "primary",
		Mods: []modpublication.ContentArtifact{fixture.artifact},
		Worlds: []modpublication.WorldPlan{{
			RoomID: "room-1", RoomDirectory: "Cluster_1", WorldID: "master", WorldDirectory: "Master",
			Mods: []modpublication.ContentArtifact{fixture.artifact}, ModOverrides: overrides,
		}},
	}
	operation := publicationOperation(target.TargetID, target.InstallationID)
	operation.PublicationID = "publication-local-0001"
	if err := runtime.EnsureCache(context.Background(), target, operation); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Prepare(context.Background(), target, operation); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Publish(context.Background(), target, operation); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Complete(context.Background(), target, operation); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(fixture.saves, "Cluster_1", "Master", "modoverrides.lua"))
	if err != nil || !bytes.Equal(actual, overrides) {
		t.Fatalf("local overrides=%q err=%v", actual, err)
	}
	actual, err = os.ReadFile(filepath.Join(fixture.server, "mods", "workshop-1392778117", "modinfo.lua"))
	if err != nil || !bytes.Equal(actual, fixture.modContent) {
		t.Fatalf("local mod content size=%d err=%v", len(actual), err)
	}
}

func TestRuntimeOperationRequiresSharedInstallationFence(t *testing.T) {
	target := modpublication.TargetPlan{TargetID: "target-1", InstallationID: "install-shared"}
	operation := publicationOperation(target.TargetID, target.InstallationID)
	wire, err := runtimeOperation(target, operation)
	if err != nil {
		t.Fatal(err)
	}
	if wire.LeaseID != "lease-install-0001" || wire.FencingToken != 2 ||
		!strings.HasPrefix(wire.ID, "modop-") || !strings.HasPrefix(wire.Key, "modkey-") ||
		wire.Key == operation.Fences[1].OperationKey {
		t.Fatalf("wrong installation fence: %#v", wire)
	}
	operation.Fences = operation.Fences[:1]
	if _, err := runtimeOperation(target, operation); err == nil {
		t.Fatal("room fence was incorrectly accepted as installation fence")
	}
}

func TestUploadBytesValidatesDescriptorResumeAndOffsets(t *testing.T) {
	data := bytes.Repeat([]byte("p"), shared.MaxChunkBytes+11)
	target := modpublication.TargetPlan{TargetID: "target-1", InstallationID: "install-1"}
	descriptor := transferDescriptor("plan", "publication-upload-0001", target, data)
	received := append([]byte(nil), data[:3]...)
	err := uploadBytes(context.Background(), data, descriptor,
		func(int64) (int64, error) { return 3, nil },
		func(offset int64, chunk []byte) (int64, error) {
			if offset != int64(len(received)) || len(chunk) > shared.MaxChunkBytes {
				return int64(len(received)), errors.New("invalid chunk")
			}
			received = append(received, chunk...)
			return int64(len(received)), nil
		},
	)
	if err != nil || !bytes.Equal(received, data) {
		t.Fatalf("resume failed: size=%d err=%v", len(received), err)
	}

	badDescriptor := descriptor
	badDescriptor.SHA256 = strings.Repeat("0", 64)
	beginCalled := false
	if err := uploadBytes(context.Background(), data, badDescriptor, func(int64) (int64, error) {
		beginCalled = true
		return 0, nil
	}, func(int64, []byte) (int64, error) { return 0, nil }); err == nil || beginCalled {
		t.Fatalf("bad descriptor reached remote: called=%v err=%v", beginCalled, err)
	}
	if err := uploadBytes(context.Background(), data, descriptor, func(int64) (int64, error) {
		return int64(len(data)) + 1, nil
	}, func(int64, []byte) (int64, error) { return 0, nil }); err == nil {
		t.Fatal("invalid resume offset was accepted")
	}
	if err := uploadBytes(context.Background(), data, descriptor, func(int64) (int64, error) {
		return 0, nil
	}, func(offset int64, chunk []byte) (int64, error) { return offset + int64(len(chunk)) - 1, nil }); err == nil {
		t.Fatal("inconsistent write offset was accepted")
	}
}

func TestContentSourceLocksRequestedTree(t *testing.T) {
	fixture := newRuntimeFixture(t, 64)
	source, err := NewContentSource(fixture.manager, filepath.Dir(fixture.workshop))
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := source.Resolve(context.Background(), modpublication.ModRequirement{WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: fixture.artifact.TreeSHA256})
	if err != nil || artifact.TreeSHA256 != fixture.artifact.TreeSHA256 || artifact.ManifestSHA256 != fixture.artifact.ManifestSHA256 {
		t.Fatalf("artifact=%#v err=%v", artifact, err)
	}
	_, err = source.Resolve(context.Background(), modpublication.ModRequirement{WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: strings.Repeat("f", 64)})
	if !errors.Is(err, modpublication.ErrPlanChanged) {
		t.Fatalf("expected tree lock failure, got %v", err)
	}
}

var _ runtimedriver.ModDriver = (*testModDriver)(nil)
