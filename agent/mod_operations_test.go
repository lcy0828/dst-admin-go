package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	result, err := agent.executeRuntimeOperation(string(action), &request, 30)
	if result.Mod == nil {
		return shared.RuntimeModResult{}, err
	}
	return *result.Mod, err
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

type testTarEntry struct {
	name string
	data []byte
	kind byte
	link string
}

func testModTar(t *testing.T, entries []testTarEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(entry.data)), Typeflag: entry.kind, Linkname: entry.link}
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
	fileDigest := sha256.Sum256(data)
	hash := sha256.New()
	fmt.Fprintf(hash, "file\x00%s\x00%o\x00%d\x00%s\n", path, 0o444, len(data), hex.EncodeToString(fileDigest[:]))
	return hex.EncodeToString(hash.Sum(nil))
}
