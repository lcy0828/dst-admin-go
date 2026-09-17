package modcontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/moddistribution"
	"dont/internal/modpublication"
	"dont/internal/operationprogress"
	"dont/internal/runtimedriver"
	"dont/internal/topology"
	"dont/shared"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
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

type modMutationObserver struct{ targets []string }

func (o *modMutationObserver) RuntimeTargetChanged(targetID string) {
	o.targets = append(o.targets, targetID)
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

type testControllerArtifactSource struct {
	location shared.RuntimeModFetchLocation
	err      error
	issued   int
	revoked  []string
}

func (s *testControllerArtifactSource) Issue(_ context.Context, _, _, _ string, _ time.Duration) (shared.RuntimeModFetchLocation, string, error) {
	s.issued++
	if s.err != nil {
		return shared.RuntimeModFetchLocation{}, "", s.err
	}
	return s.location, s.location.DownloadToken, nil
}

func (s *testControllerArtifactSource) Revoke(token string) {
	s.revoked = append(s.revoked, token)
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

func TestRemoteCacheUsesNodeFetchBeforeControllerUpload(t *testing.T) {
	fixture := newRuntimeFixture(t, 64)
	manifest := shared.RuntimeModCacheManifest{
		WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: fixture.artifact.TreeSHA256,
		ManifestSHA256: fixture.artifact.ManifestSHA256, Size: fixture.artifact.Size, FileCount: fixture.artifact.FileCount,
	}
	driver := &testModDriver{inspect: map[string]shared.RuntimeModCacheManifest{}, fetchManifest: &manifest}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	target := remoteTarget(fixture.artifact, []byte("return {}\n"))
	target.Capabilities = []string{modpublication.RequiredCapability, modpublication.FetchCapability}
	operation := publicationOperation(target.TargetID, target.InstallationID)
	if err := runtime.EnsureCache(context.Background(), target, operation); err != nil {
		t.Fatal(err)
	}
	if driver.fetchCalls != 1 || driver.cacheDescriptor.WorkshopID != "" {
		t.Fatalf("fetch calls=%d upload=%#v", driver.fetchCalls, driver.cacheDescriptor)
	}
	if driver.fetchTarget.TopologyRevision != operation.TopologyRevision || driver.fetchOperation.FencingToken != 2 {
		t.Fatalf("fetch target or fence mismatch: target=%#v operation=%#v", driver.fetchTarget, driver.fetchOperation)
	}
}

func TestFetchLatestArtifactUsesTargetSteamAndReturnsActualManifest(t *testing.T) {
	fixture := newRuntimeFixture(t, 64)
	manifest := shared.RuntimeModCacheManifest{
		WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: fixture.artifact.TreeSHA256,
		ManifestSHA256: fixture.artifact.ManifestSHA256, Size: fixture.artifact.Size, FileCount: fixture.artifact.FileCount,
		Metadata: shared.RuntimeModMetadata{SteamManifestID: "123456789", SteamUpdatedAt: time.Now().UTC()},
	}
	driver := &testModDriver{fetchManifest: &manifest}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	target := remoteTarget(fixture.artifact, []byte("return {}\n"))
	artifact, err := runtime.FetchLatestArtifact(context.Background(), target, publicationOperation(target.TargetID, target.InstallationID), fixture.artifact.WorkshopID, shared.RuntimeModMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if driver.fetchCalls != 1 || driver.fetchWorkshopID != fixture.artifact.WorkshopID || driver.fetchTreeSHA != "" || len(driver.fetchLocations[0]) != 0 {
		t.Fatalf("latest fetch did not use target Steam directly: calls=%d workshop=%q tree=%q locations=%#v", driver.fetchCalls, driver.fetchWorkshopID, driver.fetchTreeSHA, driver.fetchLocations)
	}
	if artifact.TreeSHA256 != fixture.artifact.TreeSHA256 || artifact.ManifestSHA256 != fixture.artifact.ManifestSHA256 ||
		!strings.HasPrefix(artifact.SourceRef, "runtime://"+target.TargetID+"/"+target.InstallationID+"/") {
		t.Fatalf("artifact=%#v", artifact)
	}
}

func TestFetchLatestArtifactRejectsInvalidManifestDigests(t *testing.T) {
	fixture := newRuntimeFixture(t, 64)
	manifest := shared.RuntimeModCacheManifest{
		WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: "not-a-sha",
		ManifestSHA256: fixture.artifact.ManifestSHA256, Size: fixture.artifact.Size, FileCount: fixture.artifact.FileCount,
	}
	driver := &testModDriver{fetchManifest: &manifest}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	target := remoteTarget(fixture.artifact, []byte("return {}\n"))
	if _, err := runtime.FetchLatestArtifact(context.Background(), target, publicationOperation(target.TargetID, target.InstallationID), fixture.artifact.WorkshopID, shared.RuntimeModMetadata{}); err == nil || !strings.Contains(err.Error(), "无效的 Mod 缓存 manifest") {
		t.Fatalf("invalid manifest digest was accepted: %v", err)
	}
}

func TestRemoteCacheCoalescesConcurrentExactFetches(t *testing.T) {
	fixture := newRuntimeFixture(t, 64)
	manifest := shared.RuntimeModCacheManifest{
		WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: fixture.artifact.TreeSHA256,
		ManifestSHA256: fixture.artifact.ManifestSHA256, Size: fixture.artifact.Size, FileCount: fixture.artifact.FileCount,
	}
	started, release := make(chan struct{}), make(chan struct{})
	var startedOnce sync.Once
	driver := &testModDriver{
		inspect: map[string]shared.RuntimeModCacheManifest{}, fetchManifest: &manifest,
		fetchHook: func() {
			startedOnce.Do(func() { close(started) })
			<-release
		},
	}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	target := remoteTarget(fixture.artifact, []byte("return {}\n"))
	target.Capabilities = []string{modpublication.RequiredCapability, modpublication.FetchCapability}
	operation := publicationOperation(target.TargetID, target.InstallationID)
	results := make(chan error, 2)
	go func() { results <- runtime.EnsureCache(context.Background(), target, operation) }()
	<-started
	go func() { results <- runtime.EnsureCache(context.Background(), target, operation) }()
	key := strings.Join([]string{target.TargetID, target.InstallationID, fixture.artifact.WorkshopID, strings.ToLower(fixture.artifact.TreeSHA256)}, "\x00")
	for {
		runtime.convergenceMu.Lock()
		waiters := 0
		if active := runtime.convergenceRuns[key]; active != nil {
			waiters = active.waiters
		}
		runtime.convergenceMu.Unlock()
		if waiters == 1 {
			break
		}
		goruntime.Gosched()
	}
	close(release)
	for index := 0; index < 2; index++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	driver.mu.Lock()
	fetchCalls := driver.fetchCalls
	driver.mu.Unlock()
	if fetchCalls != 1 {
		t.Fatalf("concurrent exact content fetched %d times", fetchCalls)
	}
}

func TestRemoteCacheFallsBackToExactControllerArtifactAfterFetchFailure(t *testing.T) {
	fixture := newRuntimeFixture(t, 64)
	committed := shared.RuntimeModCacheManifest{
		WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: fixture.artifact.TreeSHA256,
		ManifestSHA256: fixture.artifact.ManifestSHA256, Size: fixture.artifact.Size, FileCount: fixture.artifact.FileCount,
	}
	driver := &testModDriver{
		inspect: map[string]shared.RuntimeModCacheManifest{}, fetchErr: errors.New("Steam current version differs"), cacheCommit: &committed,
	}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	target := remoteTarget(fixture.artifact, []byte("return {}\n"))
	target.Capabilities = []string{modpublication.RequiredCapability, modpublication.FetchCapability}
	if err := runtime.EnsureCache(context.Background(), target, publicationOperation(target.TargetID, target.InstallationID)); err != nil {
		t.Fatal(err)
	}
	if driver.fetchCalls != 1 || driver.cacheDescriptor.WorkshopID != fixture.artifact.WorkshopID {
		t.Fatalf("fetch calls=%d upload=%#v", driver.fetchCalls, driver.cacheDescriptor)
	}
}

func TestRemoteCacheUsesRangeArtifactBeforeChunkUpload(t *testing.T) {
	fixture := newRuntimeFixture(t, 64)
	manifest := shared.RuntimeModCacheManifest{
		WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: fixture.artifact.TreeSHA256,
		ManifestSHA256: fixture.artifact.ManifestSHA256, Size: fixture.artifact.Size, FileCount: fixture.artifact.FileCount,
	}
	driver := &testModDriver{
		inspect: map[string]shared.RuntimeModCacheManifest{}, fetchErr: errors.New("Steam current version differs"),
		controllerFetchManifest: &manifest,
		fetchProgressHook: func(ctx context.Context, locations []shared.RuntimeModFetchLocation) {
			if len(locations) > 0 {
				operationprogress.Report(ctx, operationprogress.Update{
					Stage: operationprogress.StageModCache, Percent: 40,
					Message: "Controller正在拉取模组制品", CurrentBytes: 400, TotalBytes: 1000, BytesPerSecond: 8 << 20,
				})
			}
		},
	}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	location := shared.RuntimeModFetchLocation{
		Source: shared.RuntimeModFetchSourceController, DownloadPath: "/mod-artifacts/1392778117/tree",
		DownloadToken: "short-lived-token", Size: 2048, SHA256: strings.Repeat("a", 64),
	}
	source := &testControllerArtifactSource{location: location}
	if err := runtime.ConfigureArtifactSource(source); err != nil {
		t.Fatal(err)
	}
	target := remoteTarget(fixture.artifact, []byte("return {}\n"))
	target.Capabilities = []string{modpublication.RequiredCapability, modpublication.FetchCapability}
	var progress []operationprogress.Update
	progressContext := operationprogress.WithReporter(context.Background(), func(update operationprogress.Update) {
		progress = append(progress, update)
	})
	if err := runtime.EnsureCache(progressContext, target, publicationOperation(target.TargetID, target.InstallationID)); err != nil {
		t.Fatal(err)
	}
	if driver.fetchCalls != 2 || len(driver.fetchLocations) != 2 || len(driver.fetchLocations[0]) != 0 ||
		len(driver.fetchLocations[1]) != 1 || driver.fetchLocations[1][0].DownloadToken != location.DownloadToken {
		t.Fatalf("unexpected source selection: calls=%d locations=%#v", driver.fetchCalls, driver.fetchLocations)
	}
	if driver.cacheDescriptor.WorkshopID != "" || source.issued != 1 || len(source.revoked) != 1 || source.revoked[0] != location.DownloadToken {
		t.Fatalf("Range fallback did not replace chunk upload: upload=%#v issued=%d revoked=%v", driver.cacheDescriptor, source.issued, source.revoked)
	}
	var sawSteam, sawFailure, sawController, sawFallbackTransfer bool
	for _, update := range progress {
		sawSteam = sawSteam || strings.Contains(update.Message, "运行节点通过 SteamCMD")
		sawFailure = sawFailure || strings.Contains(update.Message, "节点 SteamCMD 下载失败")
		sawController = sawController || strings.Contains(update.Message, "改由 Controller 拉取")
		sawFallbackTransfer = sawFallbackTransfer || update.BytesPerSecond == 8<<20 &&
			strings.Contains(update.Message, "节点 SteamCMD 下载失败") && strings.Contains(update.Message, "Controller")
	}
	if !sawSteam || !sawFailure || !sawController || !sawFallbackTransfer {
		t.Fatalf("fallback progress did not preserve source messages and speed: %#v", progress)
	}
}

func TestRemoteCachePersistsEveryFetchPathAndSelectsControllerArtifact(t *testing.T) {
	fixture := newRuntimeFixture(t, 64)
	manifest := shared.RuntimeModCacheManifest{
		WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: fixture.artifact.TreeSHA256,
		ManifestSHA256: fixture.artifact.ManifestSHA256, Size: fixture.artifact.Size, FileCount: fixture.artifact.FileCount,
	}
	driver := &testModDriver{
		inspect: map[string]shared.RuntimeModCacheManifest{}, fetchErr: errors.New("Steam unavailable"),
		controllerFetchManifest: &manifest,
	}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := modpublication.NewReplicaStore(db, "fetch_observations_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	target := remoteTarget(fixture.artifact, []byte("return {}\n"))
	target.Capabilities = []string{modpublication.RequiredCapability, modpublication.FetchCapability}
	operation := publicationOperation(target.TargetID, target.InstallationID)
	now := time.Now().UTC()
	artifactID := shaHex([]byte(target.TargetID + "\x00" + target.InstallationID + "\x00" + fixture.artifact.WorkshopID + "\x00" + strings.ToLower(fixture.artifact.TreeSHA256)))
	if err := db.Exec(`INSERT INTO fetch_observations_mod_artifact_replica
		(id,target_id,installation_id,workshop_id,tree_sha256,manifest_sha256,size,file_count,desired_revision,cached,published,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, artifactID, target.TargetID, target.InstallationID, fixture.artifact.WorkshopID,
		fixture.artifact.TreeSHA256, fixture.artifact.ManifestSHA256, fixture.artifact.Size, fixture.artifact.FileCount,
		operation.PlanHash, false, false, now, now).Error; err != nil {
		t.Fatal(err)
	}
	world := target.Worlds[0]
	configSHA := shaHex(world.ModOverrides)
	worldID := shaHex([]byte(strings.Join([]string{world.RoomID, world.WorldID, target.TargetID, target.InstallationID, fixture.artifact.WorkshopID, strings.ToLower(fixture.artifact.TreeSHA256), strings.ToLower(configSHA)}, "\x00")))
	if err := db.Exec(`INSERT INTO fetch_observations_mod_world_replica
		(id,room_id,world_id,target_id,installation_id,workshop_id,tree_sha256,config_sha256,desired_revision,configured,loaded,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, worldID, world.RoomID, world.WorldID, target.TargetID, target.InstallationID,
		fixture.artifact.WorkshopID, fixture.artifact.TreeSHA256, configSHA, operation.PlanHash, false, false, now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := runtime.ConfigureReplicaStore(store); err != nil {
		t.Fatal(err)
	}
	location := shared.RuntimeModFetchLocation{
		Source: shared.RuntimeModFetchSourceController, DownloadPath: "/mod-artifacts/1392778117/tree",
		DownloadToken: "short-lived-token", Size: 2048, SHA256: strings.Repeat("a", 64),
	}
	if err := runtime.ConfigureArtifactSource(&testControllerArtifactSource{location: location}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.EnsureCache(context.Background(), target, operation); err != nil {
		t.Fatal(err)
	}
	state, err := store.Room("room-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 1 || len(state.Items[0].Targets) != 1 {
		t.Fatalf("replica state=%#v", state)
	}
	observed := state.Items[0].Targets[0]
	if observed.FetchSource != "controller" || len(observed.FetchAttempts) != 5 {
		t.Fatalf("fetch source=%q attempts=%#v", observed.FetchSource, observed.FetchAttempts)
	}
	bySource := make(map[string]modpublication.ReplicaFetchAttempt, len(observed.FetchAttempts))
	for _, attempt := range observed.FetchAttempts {
		bySource[attempt.Source] = attempt
	}
	if bySource["steam"].Status != "failed" || bySource["peer"].Status != "unavailable" ||
		!bySource["controller"].Selected || bySource["controller"].Status != "succeeded" ||
		bySource["legacy_upload"].Status != "skipped" {
		t.Fatalf("fetch path observations=%#v", bySource)
	}
}

func TestRemoteCacheSeedsInstallationOnlyReplicaBeforeRecordingCache(t *testing.T) {
	fixture := newRuntimeFixture(t, 64)
	manifest := shared.RuntimeModCacheManifest{
		WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: fixture.artifact.TreeSHA256,
		ManifestSHA256: fixture.artifact.ManifestSHA256, Size: fixture.artifact.Size, FileCount: fixture.artifact.FileCount,
	}
	driver := &testModDriver{inspect: map[string]shared.RuntimeModCacheManifest{
		fixture.artifact.WorkshopID + "\x00" + fixture.artifact.TreeSHA256: manifest,
	}}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := modpublication.NewReplicaStore(db, "installation_update_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.ConfigureReplicaStore(store); err != nil {
		t.Fatal(err)
	}
	target := modpublication.TargetPlan{
		TargetID: "agent:remote", NodeID: "remote", InstallationID: "native",
		Mods: []modpublication.ContentArtifact{fixture.artifact},
	}
	operation := publicationOperation(target.TargetID, target.InstallationID)
	if err := runtime.EnsureCache(context.Background(), target, operation); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.CachedCandidates(fixture.artifact.WorkshopID, fixture.artifact.TreeSHA256, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].TargetID != target.TargetID || candidates[0].InstallationID != target.InstallationID {
		t.Fatalf("installation replica candidates=%#v", candidates)
	}
}

func TestRuntimeSelectsExactOnlinePeerCandidate(t *testing.T) {
	fixture := newRuntimeFixture(t, 64)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := modpublication.NewReplicaStore(db, "peer_select_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	insert := `INSERT INTO peer_select_mod_artifact_replica
		(id,target_id,installation_id,workshop_id,tree_sha256,manifest_sha256,size,file_count,desired_revision,cached,published,fetch_source,observed_at,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	if err := db.Exec(insert, strings.Repeat("1", 64), "agent:source", "native", fixture.artifact.WorkshopID,
		fixture.artifact.TreeSHA256, fixture.artifact.ManifestSHA256, fixture.artifact.Size, fixture.artifact.FileCount,
		strings.Repeat("2", 64), true, false, "steam", now, now, now).Error; err != nil {
		t.Fatal(err)
	}
	location := shared.RuntimeModFetchLocation{
		Source: shared.RuntimeModFetchSourcePeer, DownloadURL: "http://192.0.2.10:18081/mod-peer/native/" + fixture.artifact.WorkshopID + "/" + fixture.artifact.TreeSHA256,
		DownloadPath:  "/mod-peer/native/" + fixture.artifact.WorkshopID + "/" + fixture.artifact.TreeSHA256,
		DownloadToken: strings.Repeat("t", 32), Size: 1024, SHA256: strings.Repeat("3", 64),
	}
	driver := &testModDriver{peerGrantLocation: location}
	source := &SnapshotSource{}
	runtime, err := NewRuntime(source, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ConfigureReplicaStore(store); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), snapshotContextKey{}, &snapshotState{
		loaded: true,
		value: runtimeSnapshot{executions: map[string]topology.ExecutionPlacement{
			"agent:source\x00native": {
				Revision: "source-revision",
				Target:   agents.RuntimeTarget{ID: "agent:source", Online: true, Capabilities: []string{"runtime.mods.peer.v1"}},
			},
		}},
	})
	target := remoteTarget(fixture.artifact, []byte("return {}\n"))
	target.TargetID, target.InstallationID = "agent:destination", "native"
	locations, err := runtime.modPeerLocations(ctx, target, fixture.artifact)
	if err != nil || len(locations) != 1 || locations[0].DownloadURL != location.DownloadURL {
		t.Fatalf("locations=%#v err=%v", locations, err)
	}
	if len(driver.peerGrantTargets) != 1 || driver.peerGrantTargets[0].TargetID != "agent:source" ||
		driver.peerGrantTargets[0].InstallationID != "native" || driver.peerGrantSubjects[0] != "agent:destination" {
		t.Fatalf("grants=%#v subjects=%#v", driver.peerGrantTargets, driver.peerGrantSubjects)
	}
}

func TestRuntimeConvergedDetectsLocalInstallationDrift(t *testing.T) {
	fixture := newRuntimeFixture(t, 32)
	master := []byte("return {[\"workshop-1392778117\"]={enabled=true}}\n")
	caves := []byte("return {[\"workshop-1392778117\"]={enabled=false}}\n")
	version := moddistribution.ModVersion{WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: fixture.artifact.TreeSHA256}
	distributionPlan, err := fixture.manager.BuildPlan(context.Background(), moddistribution.PlanInput{
		OperationID: "runtime-converged-0001", NodeID: "local",
		Shards: []moddistribution.ShardRelease{
			{InstallationID: "primary", RoomID: "room-1", RoomDirectory: "Cluster_1", WorldID: "master", WorldDirectory: "Master", Mods: []moddistribution.ModVersion{version}, ModOverrides: master},
			{InstallationID: "primary", RoomID: "room-1", RoomDirectory: "Cluster_1", WorldID: "caves", WorldDirectory: "Caves", Mods: []moddistribution.ModVersion{version}, ModOverrides: caves},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.Apply(context.Background(), distributionPlan); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, &testModDriver{}, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	target := modpublication.TargetPlan{
		TargetID: "local", NodeID: "local", InstallationID: "primary",
		Mods: []modpublication.ContentArtifact{fixture.artifact},
		Worlds: []modpublication.WorldPlan{
			{RoomID: "room-1", RoomDirectory: "Cluster_1", WorldID: "master", WorldDirectory: "Master", Mods: []modpublication.ContentArtifact{fixture.artifact}, ModOverrides: master},
			{RoomID: "room-1", RoomDirectory: "Cluster_1", WorldID: "caves", WorldDirectory: "Caves", Mods: []modpublication.ContentArtifact{fixture.artifact}, ModOverrides: caves},
		},
	}
	plan := modpublication.Plan{TopologyRevision: "topology-1", Targets: []modpublication.TargetPlan{target}}
	converged, err := runtime.Converged(context.Background(), plan)
	if err != nil || !converged {
		t.Fatalf("converged=%v err=%v", converged, err)
	}
	if err := os.WriteFile(filepath.Join(fixture.saves, "Cluster_1", "Master", "modoverrides.lua"), []byte("return {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	converged, err = runtime.Converged(context.Background(), plan)
	if err != nil || converged {
		t.Fatalf("drift converged=%v err=%v", converged, err)
	}
}

func TestMissingRemoteInstallationStateCompatibility(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		missing bool
	}{
		{name: "wrapped not exist", err: fmt.Errorf("read state: %w", os.ErrNotExist), missing: true},
		{name: "legacy linux agent", err: errors.New("open /var/lib/dst-admin-agent/mod-state/installations.json: no such file or directory"), missing: true},
		{name: "legacy windows agent", err: errors.New(`open C:\\dst-admin\\mod-state\\installations.json: The system cannot find the file specified.`), missing: true},
		{name: "permission denied", err: errors.New("open /var/lib/dst-admin-agent/mod-state/installations.json: permission denied")},
		{name: "unrelated missing file", err: errors.New("open modoverrides.lua: no such file or directory")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if actual := missingRemoteInstallationState(test.err); actual != test.missing {
				t.Fatalf("missing=%v want=%v err=%v", actual, test.missing, test.err)
			}
		})
	}
}

func TestConflictingRemoteInstallationStateIsRecoverableDrift(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		conflict bool
	}{
		{name: "sentinel", err: moddistribution.ErrConflict, conflict: true},
		{name: "wrapped sentinel", err: fmt.Errorf("observe: %w", moddistribution.ErrConflict), conflict: true},
		{name: "serialized Agent error", err: errors.New("mod distribution state conflicts with the requested release"), conflict: true},
		{name: "unrelated conflict", err: errors.New("room operation lease conflict")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if actual := conflictingRemoteInstallationState(test.err); actual != test.conflict {
				t.Fatalf("conflict=%v want=%v err=%v", actual, test.conflict, test.err)
			}
		})
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
	mutations := &modMutationObserver{}
	if err := runtime.ConfigureMutationObserver(mutations); err != nil {
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
	if len(mutations.targets) != 3 || mutations.targets[0] != target.TargetID || mutations.targets[1] != target.TargetID || mutations.targets[2] != target.TargetID {
		t.Fatalf("mutation targets=%v", mutations.targets)
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
