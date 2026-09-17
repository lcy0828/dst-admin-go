package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/operationprogress"
	"dont/shared"
)

func newModOperationAgent(t *testing.T) (*Agent, RuntimeInstallation) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, item os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if item.IsDir() {
				_ = os.Chmod(path, 0o700)
			} else {
				_ = os.Chmod(path, 0o600)
			}
			return nil
		})
	})
	serverPath := filepath.Join(root, "server")
	savePath := filepath.Join(root, "saves")
	for _, path := range []string{serverPath, savePath} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	agent, err := NewAgent(&Config{
		ServerURL: "ws://127.0.0.1:8081/agent", AgentID: "test-node", KeyFile: filepath.Join(root, "agent.conf"),
		OperationStateFile:   filepath.Join(root, "operation-state.json"),
		RuntimeInstallations: []RuntimeInstallation{{ID: "default", SavePath: savePath, ServerPath: serverPath, ServerMode: "64"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	installation, ok := agent.runtimeInstallation("default")
	if !ok {
		t.Fatal("normalized installation missing")
	}
	return agent, installation
}

func executeModRequest(t *testing.T, agent *Agent, sequence *int, action shared.RuntimeAction, mod shared.RuntimeModRequest) (shared.RuntimeModResult, error) {
	return executeModRequestContext(t, context.Background(), agent, sequence, action, mod)
}

func executeModRequestContext(t *testing.T, ctx context.Context, agent *Agent, sequence *int, action shared.RuntimeAction, mod shared.RuntimeModRequest) (shared.RuntimeModResult, error) {
	t.Helper()
	*sequence++
	request := shared.RuntimeOperationRequest{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion,
		OperationID:     fmt.Sprintf("mod-runtime-operation-%03d", *sequence),
		InstallationID:  "default", Action: action, Cluster: "Mods", Shard: "Installation", TopologyRevision: "revision-1",
		Mod: &mod,
	}
	if shared.RuntimeActionMutates(action) {
		expires := time.Now().UTC().Add(5 * time.Minute)
		request.OperationKey = fmt.Sprintf("mod-runtime-key-%03d", *sequence)
		request.LeaseID = "mod-runtime-lease"
		request.FencingToken = uint64(*sequence)
		request.LeaseExpiresAt = &expires
	}
	result, err := agent.executeRuntimeOperationContext(ctx, string(action), &request, 30)
	if result.Mod == nil {
		return shared.RuntimeModResult{}, err
	}
	return *result.Mod, err
}

type fixtureModFetchRunner struct {
	data  []byte
	calls int
	err   error
}

func (r *fixtureModFetchRunner) Download(_ context.Context, _ RuntimeInstallation, downloadRoot, workshopID string, _ bool) error {
	r.calls++
	if r.err != nil {
		return r.err
	}
	target := filepath.Join(downloadRoot, "steamapps", "workshop", "content", "322330", workshopID)
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(target, "modinfo.lua"), r.data, 0o600)
}

func uploadCacheBundle(t *testing.T, agent *Agent, sequence *int, uploadID, workshopID string, archive []byte, expectedTree string) shared.RuntimeModCacheManifest {
	t.Helper()
	digest := sha256.Sum256(archive)
	descriptor := shared.RuntimeModRequest{
		Kind: shared.RuntimeModUploadCacheBundle, UploadID: uploadID, WorkshopID: workshopID,
		ExpectedTreeSHA256: expectedTree, Size: int64(len(archive)), SHA256: hex.EncodeToString(digest[:]),
		Metadata: shared.RuntimeModMetadata{Title: "Test Mod", Version: "1.0.0", PublishedFileSize: int64(len(archive))},
	}
	if result, err := executeModRequest(t, agent, sequence, shared.RuntimeActionModUploadBegin, descriptor); err != nil || result.NextOffset != 0 {
		t.Fatalf("begin result=%#v err=%v", result, err)
	}
	for offset := int64(0); offset < descriptor.Size; {
		end := offset + 17
		if end > descriptor.Size {
			end = descriptor.Size
		}
		chunk := descriptor
		chunk.Offset, chunk.Data = offset, archive[offset:end]
		result, err := executeModRequest(t, agent, sequence, shared.RuntimeActionModUploadWrite, chunk)
		if err != nil || result.NextOffset != end {
			t.Fatalf("write offset=%d result=%#v err=%v", offset, result, err)
		}
		offset = result.NextOffset
	}
	result, err := executeModRequest(t, agent, sequence, shared.RuntimeActionModUploadCommit, descriptor)
	if err != nil || !result.Complete || result.CacheManifest == nil {
		t.Fatalf("commit result=%#v err=%v", result, err)
	}
	return *result.CacheManifest
}

func TestModCacheUploadInspectAndRestartResume(t *testing.T) {
	agent, installation := newModOperationAgent(t)
	data := []byte("name = 'runtime mod test'\n")
	archive := testModTar(t, []testTarEntry{{name: "modinfo.lua", data: data, kind: tar.TypeReg}})
	treeSHA := testSingleFileTreeSHA("modinfo.lua", data)
	sequence := 0

	digest := sha256.Sum256(archive)
	descriptor := shared.RuntimeModRequest{
		Kind: shared.RuntimeModUploadCacheBundle, UploadID: "cache-upload-resume-0001", WorkshopID: "1392778117",
		ExpectedTreeSHA256: treeSHA, Size: int64(len(archive)), SHA256: hex.EncodeToString(digest[:]),
	}
	if _, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModUploadBegin, descriptor); err != nil {
		t.Fatal(err)
	}
	first := descriptor
	first.Data = archive[:13]
	if result, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModUploadWrite, first); err != nil || result.NextOffset != 13 {
		t.Fatalf("first chunk=%#v err=%v", result, err)
	}

	restarted, err := NewAgent(&Config{
		ServerURL: "ws://127.0.0.1:8081/agent", AgentID: "test-node", KeyFile: agent.Config.KeyFile,
		OperationStateFile: agent.Config.OperationStateFile, RuntimeInstallations: []RuntimeInstallation{installation},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := executeModRequest(t, restarted, &sequence, shared.RuntimeActionModUploadBegin, descriptor); err != nil || result.NextOffset != 13 {
		t.Fatalf("resumed begin=%#v err=%v", result, err)
	}
	remainder := descriptor
	remainder.Offset, remainder.Data = 13, archive[13:]
	if result, err := executeModRequest(t, restarted, &sequence, shared.RuntimeActionModUploadWrite, remainder); err != nil || result.NextOffset != descriptor.Size {
		t.Fatalf("resumed write=%#v err=%v", result, err)
	}
	committed, err := executeModRequest(t, restarted, &sequence, shared.RuntimeActionModUploadCommit, descriptor)
	if err != nil || committed.CacheManifest == nil || committed.CacheManifest.TreeSHA256 != treeSHA {
		t.Fatalf("commit=%#v err=%v", committed, err)
	}
	inspected, err := executeModRequest(t, restarted, &sequence, shared.RuntimeActionModCacheInspect, shared.RuntimeModRequest{
		WorkshopID: "1392778117", ExpectedTreeSHA256: treeSHA,
	})
	if err != nil || inspected.CacheManifest == nil || inspected.CacheManifest.FileCount != 1 {
		t.Fatalf("inspect=%#v err=%v", inspected, err)
	}
}

func TestModFilesObserveReadsDiskWithoutInitializingPublicationState(t *testing.T) {
	agent, installation := newModOperationAgent(t)
	modRoot := filepath.Join(installation.ServerPath, "mods", "workshop-1392778117")
	if err := os.MkdirAll(modRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modRoot, "modinfo.lua"), []byte("name='ready'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sequence := 0
	result, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModFilesObserve, shared.RuntimeModRequest{
		WorkshopIDs: []string{"1392778117"},
	})
	if err != nil || result.Files == nil || result.Files.Mods["1392778117"].Status != shared.RuntimeModFileReady {
		t.Fatalf("files=%#v err=%v", result.Files, err)
	}
	if len(agent.modDistributions) != 0 {
		t.Fatalf("file observation initialized publication managers: %d", len(agent.modDistributions))
	}
	if _, err := os.Stat(installation.ModStatePath); !os.IsNotExist(err) {
		t.Fatalf("file observation created publication state: %v", err)
	}
}

func TestModFilesInventoryEnumeratesInstallationWithoutPublicationState(t *testing.T) {
	agent, installation := newModOperationAgent(t)
	modRoot := filepath.Join(installation.ServerPath, "mods", "workshop-1392778117")
	if err := os.MkdirAll(modRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modRoot, "modinfo.lua"), []byte("version='7.6.5'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sequence := 0
	result, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModFilesInventory, shared.RuntimeModRequest{})
	if err != nil || !result.Complete || result.Files == nil || result.Files.InstallationID != installation.ID {
		t.Fatalf("inventory=%#v err=%v", result.Files, err)
	}
	state, exists := result.Files.Mods["1392778117"]
	if !exists || state.Status != shared.RuntimeModFileReady || state.Version != "7.6.5" {
		t.Fatalf("inventory state=%#v", state)
	}
	if len(agent.modDistributions) != 0 {
		t.Fatalf("inventory initialized publication managers: %d", len(agent.modDistributions))
	}
	if _, err := os.Stat(installation.ModStatePath); !os.IsNotExist(err) {
		t.Fatalf("inventory created publication state: %v", err)
	}
}

func TestModFetchDownloadsIntoStagingAndImportsExactContent(t *testing.T) {
	agent, installation := newModOperationAgent(t)
	data := []byte("name = 'node fetched mod'\n")
	runner := &fixtureModFetchRunner{data: data}
	agent.modFetchRunner = runner
	sequence := 0
	treeSHA := testSingleFileTreeSHA("modinfo.lua", data)
	result, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModFetch, shared.RuntimeModRequest{
		WorkshopID: "1392778117", ExpectedTreeSHA256: treeSHA,
		FetchSources: []shared.RuntimeModFetchSource{shared.RuntimeModFetchSourceSteam},
		Metadata:     shared.RuntimeModMetadata{Title: "Node Fetch", PublishedFileSize: int64(len(data))},
	})
	if err != nil || !result.Complete || result.FetchSource != shared.RuntimeModFetchSourceSteam ||
		result.CacheManifest == nil || result.CacheManifest.TreeSHA256 != treeSHA || runner.calls != 1 {
		t.Fatalf("fetch result=%#v calls=%d err=%v", result, runner.calls, err)
	}
	if len(result.FetchAttempts) != 1 || result.FetchAttempts[0].Source != shared.RuntimeModFetchSourceSteam ||
		!result.FetchAttempts[0].Selected || result.FetchAttempts[0].Bytes != int64(len(data)) ||
		result.FetchAttempts[0].DurationMillis < 1 || result.FetchAttempts[0].BytesPerSecond < 1 {
		t.Fatalf("Steam fetch observations=%#v", result.FetchAttempts)
	}
	manager, err := agent.modManager(installation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(context.Background(), "1392778117", treeSHA); err != nil {
		t.Fatalf("fetched cache was not imported: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(installation.ModStatePath, "fetches"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("fetch staging was not cleaned: entries=%d err=%v", len(entries), err)
	}
}

func TestModFetchDownloadsLatestSteamContentWithoutExpectedTree(t *testing.T) {
	agent, installation := newModOperationAgent(t)
	data := []byte("name = 'latest node fetched mod'\n")
	runner := &fixtureModFetchRunner{data: data}
	agent.modFetchRunner = runner
	sequence := 0
	wantTreeSHA := testSingleFileTreeSHA("modinfo.lua", data)
	result, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModFetch, shared.RuntimeModRequest{
		WorkshopID:   "1392778117",
		FetchSources: []shared.RuntimeModFetchSource{shared.RuntimeModFetchSourceSteam},
		Metadata:     shared.RuntimeModMetadata{Title: "Latest Node Fetch", PublishedFileSize: int64(len(data))},
	})
	if err != nil || !result.Complete || result.CacheManifest == nil ||
		result.CacheManifest.TreeSHA256 != wantTreeSHA || runner.calls != 1 {
		t.Fatalf("latest fetch result=%#v calls=%d err=%v", result, runner.calls, err)
	}
	manager, err := agent.modManager(installation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(context.Background(), "1392778117", wantTreeSHA); err != nil {
		t.Fatalf("latest fetched cache was not imported: %v", err)
	}
}

func TestModFetchWithoutExpectedTreeRejectsNonSteamSources(t *testing.T) {
	agent, _ := newModOperationAgent(t)
	sequence := 0
	_, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModFetch, shared.RuntimeModRequest{
		WorkshopID:   "1392778117",
		FetchSources: []shared.RuntimeModFetchSource{shared.RuntimeModFetchSourceController},
	})
	if err == nil || !strings.Contains(err.Error(), "Mod 节点本地获取请求无效") {
		t.Fatalf("non-Steam source without expected tree was accepted: %v", err)
	}
}

func TestModFetchResumesControllerArtifactAndImportsExactTree(t *testing.T) {
	agent, installation := newModOperationAgent(t)
	data := []byte("name = 'controller artifact'\n")
	archive := testModTar(t, []testTarEntry{{name: "modinfo.lua", data: data, kind: tar.TypeReg}})
	treeSHA := testSingleFileTreeSHA("modinfo.lua", data)
	digest := sha256.Sum256(archive)
	bundleSHA := hex.EncodeToString(digest[:])
	const resumeOffset = 37
	gotRange := ""
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/mod-artifacts/1392778117/"+treeSHA || request.Header.Get("Authorization") != "Bearer range-token-0123456789abcdef01234567" {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		gotRange = request.Header.Get("Range")
		if gotRange != fmt.Sprintf("bytes=%d-", resumeOffset) {
			http.Error(response, "range required", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", resumeOffset, len(archive)-1, len(archive)))
		response.WriteHeader(http.StatusPartialContent)
		_, _ = response.Write(archive[resumeOffset:])
	}))
	defer server.Close()
	agent.Config.ServerURL = "ws" + strings.TrimPrefix(server.URL, "http") + "/agent"
	fetchRoot := filepath.Join(installation.ModStatePath, "fetches", "http")
	if err := os.MkdirAll(fetchRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(fetchRoot, ".artifact-"+bundleSHA+".part")
	if err := os.WriteFile(partial, archive[:resumeOffset], 0o600); err != nil {
		t.Fatal(err)
	}
	sequence := 0
	location := shared.RuntimeModFetchLocation{
		Source: shared.RuntimeModFetchSourceController, DownloadPath: "/mod-artifacts/1392778117/" + treeSHA,
		DownloadToken: "range-token-0123456789abcdef01234567", Size: int64(len(archive)), SHA256: bundleSHA,
	}
	var progress []operationprogress.Update
	progressContext := operationprogress.WithReporter(context.Background(), func(update operationprogress.Update) {
		progress = append(progress, update)
	})
	result, err := executeModRequestContext(t, progressContext, agent, &sequence, shared.RuntimeActionModFetch, shared.RuntimeModRequest{
		WorkshopID: "1392778117", ExpectedTreeSHA256: treeSHA,
		FetchSources:   []shared.RuntimeModFetchSource{shared.RuntimeModFetchSourceController},
		FetchLocations: []shared.RuntimeModFetchLocation{location},
		Metadata:       shared.RuntimeModMetadata{Title: "Controller Artifact", PublishedFileSize: int64(len(data))},
	})
	if err != nil || !result.Complete || result.FetchSource != shared.RuntimeModFetchSourceController ||
		result.CacheManifest == nil || result.CacheManifest.TreeSHA256 != treeSHA {
		t.Fatalf("controller fetch result=%#v range=%q err=%v", result, gotRange, err)
	}
	if len(result.FetchAttempts) != 1 || !result.FetchAttempts[0].Selected ||
		result.FetchAttempts[0].Bytes != int64(len(archive)-resumeOffset) || result.FetchAttempts[0].BytesPerSecond < 1 {
		t.Fatalf("controller fetch observations=%#v", result.FetchAttempts)
	}
	var sawTransfer, sawValidation, sawImport bool
	for _, update := range progress {
		if strings.Contains(update.Message, "Controller正在拉取") && update.CurrentBytes > resumeOffset &&
			update.TotalBytes == int64(len(archive)) && update.BytesPerSecond > 0 {
			sawTransfer = true
		}
		if strings.Contains(update.Message, "下载完成，正在校验") && update.BytesPerSecond == 0 {
			sawValidation = true
		}
		if strings.Contains(update.Message, "已导入") && update.BytesPerSecond == 0 {
			sawImport = true
		}
	}
	if !sawTransfer || !sawValidation || !sawImport {
		t.Fatalf("Controller progress missing transfer/validation/import phases: %#v", progress)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("completed Range file was not removed: %v", err)
	}
	manager, err := agent.modManager(installation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(context.Background(), "1392778117", treeSHA); err != nil {
		t.Fatalf("controller artifact was not imported: %v", err)
	}
}

func TestModFetchRejectsSteamVersionDriftWithoutPublishingIt(t *testing.T) {
	agent, installation := newModOperationAgent(t)
	runner := &fixtureModFetchRunner{data: []byte("name = 'newer steam version'\n")}
	agent.modFetchRunner = runner
	sequence := 0
	expected := strings.Repeat("a", 64)
	result, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModFetch, shared.RuntimeModRequest{
		WorkshopID: "1392778117", ExpectedTreeSHA256: expected,
		FetchSources: []shared.RuntimeModFetchSource{shared.RuntimeModFetchSourceSteam},
	})
	if err == nil || !strings.Contains(err.Error(), "期望版本不一致") || result.Complete || result.CacheManifest == nil {
		t.Fatalf("fetch drift result=%#v err=%v", result, err)
	}
	if len(result.FetchAttempts) != 1 || result.FetchAttempts[0].Status != shared.RuntimeModFetchStatusFailed ||
		result.FetchAttempts[0].ErrorCode != "STEAM_VERSION_MISMATCH" || result.FetchAttempts[0].Selected {
		t.Fatalf("Steam drift observations=%#v", result.FetchAttempts)
	}
	if _, statErr := os.Stat(filepath.Join(installation.ServerPath, "mods", "workshop-1392778117")); !os.IsNotExist(statErr) {
		t.Fatalf("version drift changed published Mod directory: %v", statErr)
	}
}

func TestModUploadSessionAcceptsDistinctIdempotencyKeysAcrossActions(t *testing.T) {
	agent, _ := newModOperationAgent(t)
	data := []byte("name = 'idempotency session test'\n")
	archive := testModTar(t, []testTarEntry{{name: "modinfo.lua", data: data, kind: tar.TypeReg}})
	treeSHA := testSingleFileTreeSHA("modinfo.lua", data)
	digest := sha256.Sum256(archive)
	descriptor := shared.RuntimeModRequest{
		Kind: shared.RuntimeModUploadCacheBundle, UploadID: "cache-upload-session-0001", WorkshopID: "1392778117",
		ExpectedTreeSHA256: treeSHA, Size: int64(len(archive)), SHA256: hex.EncodeToString(digest[:]),
	}
	expires := time.Now().UTC().Add(5 * time.Minute)
	execute := func(action shared.RuntimeAction, operationID, operationKey string, mod shared.RuntimeModRequest) shared.RuntimeOperationResult {
		t.Helper()
		request := shared.RuntimeOperationRequest{
			ProtocolVersion:  shared.RuntimeOperationProtocolVersion,
			OperationID:      operationID,
			OperationKey:     operationKey,
			InstallationID:   "default",
			Action:           action,
			Cluster:          "Mods",
			Shard:            "Installation",
			TopologyRevision: "revision-1",
			LeaseID:          "mod-upload-session-lease",
			FencingToken:     7,
			LeaseExpiresAt:   &expires,
			Mod:              &mod,
		}
		result, err := agent.executeRuntimeOperation(string(action), &request, 30)
		if err != nil {
			t.Fatalf("%s failed: %v", action, err)
		}
		return result
	}

	begin := execute(shared.RuntimeActionModUploadBegin, "mod-upload-session-begin", "mod-upload-session-key-begin", descriptor)
	if begin.Mod == nil || begin.Mod.NextOffset != 0 {
		t.Fatalf("unexpected begin result: %#v", begin)
	}
	writeRequest := descriptor
	writeRequest.Data = archive
	write := execute(shared.RuntimeActionModUploadWrite, "mod-upload-session-write", "mod-upload-session-key-write", writeRequest)
	if write.Mod == nil || write.Mod.NextOffset != descriptor.Size {
		t.Fatalf("unexpected write result: %#v", write)
	}
	commit := execute(shared.RuntimeActionModUploadCommit, "mod-upload-session-commit", "mod-upload-session-key-commit", descriptor)
	if commit.Mod == nil || !commit.Mod.Complete || commit.Mod.CacheManifest == nil || commit.Mod.CacheManifest.TreeSHA256 != treeSHA {
		t.Fatalf("unexpected commit result: %#v", commit)
	}
}

func TestModCacheUploadPreservesExecutableModeInTreeHash(t *testing.T) {
	agent, _ := newModOperationAgent(t)
	data := []byte("#!/bin/sh\nexit 0\n")
	archive := testModTar(t, []testTarEntry{{name: "tool.sh", data: data, kind: tar.TypeReg, mode: 0o755}})
	treeSHA := testSingleFileTreeSHAWithMode("tool.sh", data, 0o555)
	sequence := 0
	manifest := uploadCacheBundle(t, agent, &sequence, "cache-upload-executable-0001", "1392778117", archive, treeSHA)
	if manifest.TreeSHA256 != treeSHA || manifest.FileCount != 1 {
		t.Fatalf("executable manifest = %#v", manifest)
	}
}

func TestModTargetObservationReturnsUsableCapacity(t *testing.T) {
	agent, _ := newModOperationAgent(t)
	sequence := 0
	result, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModTargetObserve, shared.RuntimeModRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || result.RuntimeVersion != modRuntimeVersion || result.AvailableBytes < 0 {
		t.Fatalf("unexpected target observation: %#v", result)
	}
}

func TestModInstallationStateIsEmptyBeforeFirstPublication(t *testing.T) {
	agent, _ := newModOperationAgent(t)
	sequence := 0
	result, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModInstallationState, shared.RuntimeModRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || result.Installation != nil {
		t.Fatalf("unexpected unpublished installation state: %#v", result)
	}
}

func TestModReleasePlanLifecycleAndOverridesRead(t *testing.T) {
	agent, installation := newModOperationAgent(t)
	sequence := 0
	modData := []byte("name = 'release test'\n")
	archive := testModTar(t, []testTarEntry{{name: "modinfo.lua", data: modData, kind: tar.TypeReg}})
	treeSHA := testSingleFileTreeSHA("modinfo.lua", modData)
	uploadCacheBundle(t, agent, &sequence, "cache-upload-release-0001", "1392778117", archive, treeSHA)

	operationID := "release-operation-0001"
	overrides := []byte("return { [\"workshop-1392778117\"] = { enabled = true } }\n")
	wire := shared.RuntimeModPlanInput{
		OperationID: operationID, NodeID: "test-node",
		Shards: []shared.RuntimeModShardRelease{{
			InstallationID: "default", RoomID: "room-1", RoomDirectory: "Cluster_1",
			WorldID: "world-master", WorldDirectory: "Master", ModOverrides: overrides,
			Mods: []shared.RuntimeModVersion{{WorkshopID: "1392778117", TreeSHA256: treeSHA}},
		}},
	}
	planData, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	planDigest := sha256.Sum256(planData)
	descriptor := shared.RuntimeModRequest{
		Kind: shared.RuntimeModUploadReleasePlan, UploadID: "release-plan-upload-0001", OperationID: operationID,
		Size: int64(len(planData)), SHA256: hex.EncodeToString(planDigest[:]),
	}
	if _, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModReleasePlanBegin, descriptor); err != nil {
		t.Fatal(err)
	}
	write := descriptor
	write.Data = planData
	if result, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModReleasePlanWrite, write); err != nil || result.NextOffset != descriptor.Size {
		t.Fatalf("plan write=%#v err=%v", result, err)
	}
	planned, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModReleasePlanCommit, descriptor)
	if err != nil || planned.Release == nil || planned.Release.Phase != "planned" {
		t.Fatalf("planned=%#v err=%v", planned, err)
	}
	for _, step := range []struct {
		action shared.RuntimeAction
		phase  string
	}{
		{shared.RuntimeActionModReleasePrepare, "prepared"},
		{shared.RuntimeActionModReleasePublish, "published"},
		{shared.RuntimeActionModReleaseComplete, "committed"},
	} {
		result, err := executeModRequest(t, agent, &sequence, step.action, shared.RuntimeModRequest{OperationID: operationID})
		if err != nil || result.Release == nil || result.Release.Phase != step.phase {
			t.Fatalf("%s result=%#v err=%v", step.action, result, err)
		}
	}
	state, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModReleaseState, shared.RuntimeModRequest{OperationID: operationID})
	if err != nil || state.Release == nil || state.Release.State == nil || state.Release.State.LastOperationID != operationID {
		t.Fatalf("state=%#v err=%v", state, err)
	}
	installationState, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModInstallationState, shared.RuntimeModRequest{})
	if err != nil || installationState.Installation == nil || installationState.Installation.LastOperationID != operationID ||
		installationState.Installation.Mods["1392778117"] != treeSHA || len(installationState.Installation.Shards) != 1 {
		t.Fatalf("installation state=%#v err=%v", installationState.Installation, err)
	}
	read, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModOverridesRead, shared.RuntimeModRequest{
		RoomDirectory: "Cluster_1", WorldDirectory: "Master",
	})
	if err != nil || read.Overrides == nil || string(read.Overrides.Data) != string(overrides) || !read.Overrides.Complete {
		t.Fatalf("overrides=%#v err=%v", read.Overrides, err)
	}
	if data, err := os.ReadFile(filepath.Join(installation.ServerPath, "mods", "workshop-1392778117", "modinfo.lua")); err != nil || string(data) != string(modData) {
		t.Fatalf("published mod=%q err=%v", data, err)
	}
}

func TestModUploadRejectsWrongOffsetHashAndOversizedMessage(t *testing.T) {
	agent, _ := newModOperationAgent(t)
	sequence := 0
	archive := testModTar(t, []testTarEntry{{name: "modinfo.lua", data: []byte("x"), kind: tar.TypeReg}})
	descriptor := shared.RuntimeModRequest{
		Kind: shared.RuntimeModUploadCacheBundle, UploadID: "cache-upload-invalid-0001", WorkshopID: "1392778117",
		ExpectedTreeSHA256: strings.Repeat("a", 64), Size: int64(len(archive)), SHA256: strings.Repeat("b", 64),
	}
	if _, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModUploadBegin, descriptor); err != nil {
		t.Fatal(err)
	}
	wrong := descriptor
	wrong.Offset, wrong.Data = 1, archive[:1]
	if result, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModUploadWrite, wrong); err == nil || result.NextOffset != 0 {
		t.Fatalf("wrong offset result=%#v err=%v", result, err)
	}
	write := descriptor
	write.Data = archive
	if _, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModUploadWrite, write); err != nil {
		t.Fatal(err)
	}
	if _, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModUploadCommit, descriptor); err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("hash error=%v", err)
	}

	request := runtimeOperationRequest(shared.RuntimeActionModUploadBegin)
	request.Cluster, request.Shard = "Mods", "Installation"
	request.Mod = &shared.RuntimeModRequest{
		Kind: shared.RuntimeModUploadCacheBundle, UploadID: "cache-upload-100mb-0001", WorkshopID: "1392778117",
		ExpectedTreeSHA256: strings.Repeat("a", 64), Size: 100 << 20, SHA256: strings.Repeat("b", 64),
	}
	if err := validateRuntimeOperationRequest(string(request.Action), request, 30, time.Now().UTC()); err != nil {
		t.Fatalf("100 MB descriptor should be chunked, not rejected: %v", err)
	}
	request.Action = shared.RuntimeActionModUploadWrite
	request.Mod.Data = make([]byte, shared.MaxChunkBytes+1)
	if err := validateRuntimeOperationRequest(string(request.Action), request, 30, time.Now().UTC()); err == nil {
		t.Fatal("oversized Mod data message accepted")
	}
}

func TestModArchiveRejectsTraversalLinksAndSpecialFiles(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tests := []testTarEntry{
		{name: "../escape", data: []byte("x"), kind: tar.TypeReg},
		{name: "link", kind: tar.TypeSymlink, link: "target"},
		{name: "pipe", kind: tar.TypeFifo},
	}
	for index, entry := range tests {
		archive := testModTar(t, []testTarEntry{entry})
		archivePath := filepath.Join(root, fmt.Sprintf("archive-%d.tar", index))
		target := filepath.Join(root, fmt.Sprintf("target-%d", index))
		if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := extractModArchive(context.Background(), archivePath, target); err == nil {
			t.Fatalf("unsafe tar entry accepted: %#v", entry)
		}
	}
}

func TestRuntimeWorkshopDownloadRootUsesConfiguredWorkshopTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workshop")
	content := filepath.Join(root, "steamapps", "workshop", "content", "322330")
	actual, err := runtimeWorkshopDownloadRoot(content, "322330")
	if err != nil {
		t.Fatal(err)
	}
	if actual != root {
		t.Fatalf("download root = %q, want %q", actual, root)
	}
	if _, err := runtimeWorkshopDownloadRoot(filepath.Join(root, "content", "322330"), "322330"); err == nil {
		t.Fatal("invalid Workshop tree was accepted")
	}
}

type testTarEntry struct {
	name string
	data []byte
	kind byte
	link string
	mode int64
}

func testModTar(t *testing.T, entries []testTarEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for _, entry := range entries {
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{Name: entry.name, Mode: mode, Size: int64(len(entry.data)), Typeflag: entry.kind, Linkname: entry.link}
		if entry.kind != tar.TypeReg && entry.kind != tar.TypeRegA {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(entry.data) > 0 {
			if _, err := writer.Write(entry.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func testSingleFileTreeSHA(path string, data []byte) string {
	return testSingleFileTreeSHAWithMode(path, data, 0o444)
}

func testSingleFileTreeSHAWithMode(path string, data []byte, mode uint32) string {
	fileDigest := sha256.Sum256(data)
	hash := sha256.New()
	fmt.Fprintf(hash, "file\x00%s\x00%o\x00%d\x00%s\n", path, mode, len(data), hex.EncodeToString(fileDigest[:]))
	return hex.EncodeToString(hash.Sum(nil))
}
