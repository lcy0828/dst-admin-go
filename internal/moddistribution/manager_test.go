package moddistribution

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type testEnvironment struct {
	root    string
	cache   string
	state   string
	server  string
	saves   string
	config  Config
	manager *Manager
}

func newTestEnvironment(t *testing.T, reserve int64) testEnvironment {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	environment := testEnvironment{
		root: root, cache: filepath.Join(root, "cache"), state: filepath.Join(root, "state"),
		server: filepath.Join(root, "server"), saves: filepath.Join(root, "saves"),
	}
	for _, directory := range []string{environment.cache, environment.state, environment.server, environment.saves} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	environment.config = Config{
		CacheRoot: environment.cache, StateRoot: environment.state, NodeID: "node-main", ReserveBytes: reserve,
		Installations: []TrustedInstallation{{ID: "primary", NodeID: "node-main", ServerPath: environment.server, SavePath: environment.saves}},
	}
	manager, err := New(environment.config)
	if err != nil {
		t.Fatal(err)
	}
	environment.manager = manager
	t.Cleanup(func() {
		_ = makeTreeWritable(environment.cache)
		_ = makeTreeWritable(environment.server)
	})
	return environment
}

func (environment testEnvironment) importMod(t *testing.T, workshopID string, files map[string]string) Manifest {
	t.Helper()
	source, err := os.MkdirTemp(environment.root, "source-"+workshopID+"-")
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(source, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := environment.manager.Import(context.Background(), workshopID, source, Metadata{Title: "Test Mod"})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func release(operationID string, manifest Manifest, master, caves []byte) PlanInput {
	mod := ModVersion{WorkshopID: manifest.WorkshopID, TreeSHA256: manifest.TreeSHA256}
	return PlanInput{OperationID: operationID, NodeID: "node-main", Shards: []ShardRelease{
		{InstallationID: "primary", RoomID: "room-1", RoomDirectory: "Cluster_1", WorldID: "master", WorldDirectory: "Master", Mods: []ModVersion{mod}, ModOverrides: master},
		{InstallationID: "primary", RoomID: "room-1", RoomDirectory: "Cluster_1", WorldID: "caves", WorldDirectory: "Caves", Mods: []ModVersion{mod}, ModOverrides: caves},
	}}
}

func TestImmutableCacheManifestAndTamperDetection(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	manifest := environment.importMod(t, "1392778117", map[string]string{
		"modinfo.lua": "name='Legion'\nversion='7.6.5'\n", "scripts/main.lua": "return true\n",
	})
	if manifest.FileCount != 2 || manifest.Size == 0 || !validSHA256(manifest.TreeSHA256) {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
	contentRoot := filepath.Join(environment.manager.cacheVersionRoot(manifest.WorkshopID, manifest.TreeSHA256), "content")
	file := filepath.Join(contentRoot, "scripts", "main.lua")
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("cache file is not immutable: mode=%v err=%v", info.Mode(), err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := environment.manager.Verify(context.Background(), manifest.WorkshopID, manifest.TreeSHA256); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected integrity error, got %v", err)
	}
}

func TestWriteBundleIsDeterministic(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	manifest := environment.importMod(t, "102", map[string]string{
		"modinfo.lua": "name='bundle'\n", "scripts/main.lua": "return true\n",
	})
	var first bytes.Buffer
	if err := environment.manager.WriteBundle(context.Background(), manifest.WorkshopID, manifest.TreeSHA256, &first); err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := environment.manager.WriteBundle(context.Background(), manifest.WorkshopID, manifest.TreeSHA256, &second); err != nil {
		t.Fatal(err)
	}
	if first.Len() == 0 || !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("bundle output is empty or non-deterministic")
	}
}

func TestImportRejectsSymbolicLinksAndInsufficientDisk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic link permissions vary on Windows")
	}
	environment := newTestEnvironment(t, 0)
	source := filepath.Join(environment.root, "unsafe-source")
	outside := filepath.Join(environment.root, "outside.lua")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("return true"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "modmain.lua")); err != nil {
		t.Fatal(err)
	}
	if _, err := environment.manager.Import(context.Background(), "100", source, Metadata{}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("expected unsafe path error, got %v", err)
	}

	full := newTestEnvironment(t, math.MaxInt64)
	safe := filepath.Join(full.root, "safe-source")
	if err := os.MkdirAll(safe, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(safe, "modinfo.lua"), []byte("name='x'"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := full.manager.Import(context.Background(), "101", safe, Metadata{}); !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("expected disk error, got %v", err)
	}
}

func TestPlanRejectsMutableInstallationVersionConflict(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	first := environment.importMod(t, "200", map[string]string{"modinfo.lua": "version='1'"})
	second := environment.importMod(t, "200", map[string]string{"modinfo.lua": "version='2'"})
	input := release("release-conflict-001", first, nil, nil)
	input.Shards[1].Mods[0].TreeSHA256 = second.TreeSHA256
	if _, err := environment.manager.BuildPlan(context.Background(), input); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected installation version conflict, got %v", err)
	}
}

func TestPublishKeepsRoomWorldOverridesIndependent(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	manifest := environment.importMod(t, "300", map[string]string{"modinfo.lua": "version='1'", "modmain.lua": "return true"})
	master := []byte("return {[\"workshop-300\"]={enabled=true,configuration_options={difficulty=\"hard\"}}}\n")
	caves := []byte("return {[\"workshop-300\"]={enabled=false,configuration_options={difficulty=\"easy\"}}}\n")
	plan, err := environment.manager.BuildPlan(context.Background(), release("release-apply-0001", manifest, master, caves))
	if err != nil {
		t.Fatal(err)
	}
	if err := environment.manager.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(environment.saves, "Cluster_1", "Master", "modoverrides.lua"), master)
	assertFileContent(t, filepath.Join(environment.saves, "Cluster_1", "Caves", "modoverrides.lua"), caves)
	assertFileContent(t, filepath.Join(environment.server, "mods", "workshop-300", "modmain.lua"), []byte("return true"))
	setup, err := os.ReadFile(filepath.Join(environment.server, "mods", "dedicated_server_mods_setup.lua"))
	if err != nil || !strings.Contains(string(setup), `ServerModSetup("300")`) {
		t.Fatalf("managed dedicated server setup was not published: %q err=%v", setup, err)
	}
	state, err := environment.manager.State("primary")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Shards) != 2 || state.Shards["room-1/master"].ConfigSHA256 == state.Shards["room-1/caves"].ConfigSHA256 {
		t.Fatalf("world configurations were not isolated: %#v", state.Shards)
	}
}

func TestRollbackRestoresModAndWorldConfigurations(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	manifest := environment.importMod(t, "400", map[string]string{"modinfo.lua": "version='new'", "new.lua": "new"})
	oldMod := filepath.Join(environment.server, "mods", "workshop-400")
	if err := os.MkdirAll(oldMod, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldMod, "old.lua"), []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	oldMaster := []byte("return {old=true}\n")
	oldCaves := []byte("return {old_caves=true}\n")
	writeWorldConfig(t, environment.saves, "Master", oldMaster)
	writeWorldConfig(t, environment.saves, "Caves", oldCaves)
	plan, err := environment.manager.BuildPlan(context.Background(), release("release-rollback-01", manifest, []byte("return {master=true}\n"), []byte("return {caves=true}\n")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := environment.manager.Prepare(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if _, err := environment.manager.Publish(context.Background(), plan.OperationID); err != nil {
		t.Fatal(err)
	}
	if err := environment.manager.Rollback(context.Background(), plan.OperationID); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(oldMod, "old.lua"), []byte("old"))
	if _, err := os.Stat(filepath.Join(oldMod, "new.lua")); !os.IsNotExist(err) {
		t.Fatalf("new mod survived rollback: %v", err)
	}
	assertFileContent(t, filepath.Join(environment.saves, "Cluster_1", "Master", "modoverrides.lua"), oldMaster)
	assertFileContent(t, filepath.Join(environment.saves, "Cluster_1", "Caves", "modoverrides.lua"), oldCaves)
}

func TestNewManagerRecoversUncommittedPublishByRollback(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	manifest := environment.importMod(t, "500", map[string]string{"modinfo.lua": "version='new'"})
	writeWorldConfig(t, environment.saves, "Master", []byte("return {old=true}\n"))
	writeWorldConfig(t, environment.saves, "Caves", []byte("return {old=true}\n"))
	plan, err := environment.manager.BuildPlan(context.Background(), release("release-recover-01", manifest, []byte("return {new=true}\n"), []byte("return {new=true}\n")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := environment.manager.Prepare(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if _, err := environment.manager.Publish(context.Background(), plan.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := New(environment.config); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(environment.saves, "Cluster_1", "Master", "modoverrides.lua"), []byte("return {old=true}\n"))
	if _, err := os.Stat(filepath.Join(environment.server, "mods", "workshop-500")); !os.IsNotExist(err) {
		t.Fatalf("uncommitted mod was not removed: %v", err)
	}
}

func TestRecoveryCompletesAtomicallyCommittedState(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	manifest := environment.importMod(t, "600", map[string]string{"modinfo.lua": "version='new'"})
	plan, err := environment.manager.BuildPlan(context.Background(), release("release-commit-0001", manifest, []byte("return {new=true}\n"), []byte("return {new=true}\n")))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := environment.manager.Prepare(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	journal, err = environment.manager.Publish(context.Background(), plan.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	journal.Phase = PhaseCompleting
	if err := environment.manager.writeInstallationStates(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if err := environment.manager.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	recovered, err := New(environment.config)
	if err != nil {
		t.Fatal(err)
	}
	state, err := recovered.State("primary")
	if err != nil || state.LastOperationID != plan.OperationID {
		t.Fatalf("committed state was not retained: state=%#v err=%v", state, err)
	}
	assertFileContent(t, filepath.Join(environment.server, "mods", "workshop-600", "modinfo.lua"), []byte("version='new'"))
	if _, err := os.Stat(environment.manager.journalPath(plan.OperationID)); !os.IsNotExist(err) {
		t.Fatalf("journal was not cleaned: %v", err)
	}
}

func TestPrepareRejectsSymlinkAtPublishTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic link permissions vary on Windows")
	}
	environment := newTestEnvironment(t, 0)
	manifest := environment.importMod(t, "700", map[string]string{"modinfo.lua": "version='1'"})
	outside := filepath.Join(environment.root, "outside-mod")
	if err := os.MkdirAll(filepath.Join(environment.server, "mods"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(environment.server, "mods", "workshop-700")); err != nil {
		t.Fatal(err)
	}
	plan, err := environment.manager.BuildPlan(context.Background(), release("release-symlink-01", manifest, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := environment.manager.Prepare(context.Background(), plan); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("expected unsafe path error, got %v", err)
	}
}

func TestConfiguredRootRejectsOriginalSymlinkComponent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic link permissions vary on Windows")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realServer := filepath.Join(root, "real-server")
	serverLink := filepath.Join(root, "server-link")
	for _, directory := range []string{realServer, filepath.Join(root, "saves"), filepath.Join(root, "cache"), filepath.Join(root, "state")} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(realServer, serverLink); err != nil {
		t.Fatal(err)
	}
	_, err = New(Config{
		CacheRoot: filepath.Join(root, "cache"), StateRoot: filepath.Join(root, "state"), NodeID: "node-main",
		Installations: []TrustedInstallation{{ID: "primary", NodeID: "node-main", ServerPath: serverLink, SavePath: filepath.Join(root, "saves")}},
	})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("expected configured symlink rejection, got %v", err)
	}
}

func TestEmptyModSetClearsManagedSetupAndKeepsManualSetup(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	setupPath := filepath.Join(environment.server, "mods", "dedicated_server_mods_setup.lua")
	if err := os.MkdirAll(filepath.Dir(setupPath), 0o750); err != nil {
		t.Fatal(err)
	}
	manual := "ServerModCollectionSetup(\"manual-collection\")\n\n-- BEGIN DST-ADMIN MANAGED MODS\nServerModSetup(\"999\")\n-- END DST-ADMIN MANAGED MODS\n"
	if err := os.WriteFile(setupPath, []byte(manual), 0o640); err != nil {
		t.Fatal(err)
	}
	input := PlanInput{OperationID: "release-empty-0001", NodeID: "node-main", Shards: []ShardRelease{{
		InstallationID: "primary", RoomID: "room-1", RoomDirectory: "Cluster_1",
		WorldID: "master", WorldDirectory: "Master",
	}}}
	plan, err := environment.manager.BuildPlan(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Installations[0].Mods) != 0 || string(plan.Installations[0].Shards[0].ModOverrides) != "return {\n}\n" {
		t.Fatalf("empty desired state was not preserved: %#v", plan.Installations[0])
	}
	if err := environment.manager.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	setup, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(setup), "manual-collection") || strings.Contains(string(setup), "ServerModSetup(\"999\")") {
		t.Fatalf("managed setup did not preserve manual content or clear managed mods: %q", setup)
	}
	assertFileContent(t, filepath.Join(environment.saves, "Cluster_1", "Master", "modoverrides.lua"), []byte("return {\n}\n"))
}

func TestManagedSetupPreservesCRLFOutsideManagedBlock(t *testing.T) {
	current := []byte("ServerModCollectionSetup(\"manual\")\r\n\r\n-- BEGIN DST-ADMIN MANAGED MODS\r\nServerModSetup(\"old\")\r\n-- END DST-ADMIN MANAGED MODS\r\n\r\n-- operator note\r\n")
	result, err := composeManagedSetup(current, []ModVersion{{WorkshopID: "1392778117"}})
	if err != nil {
		t.Fatal(err)
	}
	expected := "ServerModCollectionSetup(\"manual\")\r\n\r\n-- BEGIN DST-ADMIN MANAGED MODS\r\nServerModSetup(\"1392778117\")\r\n-- END DST-ADMIN MANAGED MODS\r\n\r\n-- operator note\r\n"
	if string(result) != expected {
		t.Fatalf("CRLF or manual setup content changed:\n got %q\nwant %q", result, expected)
	}
}

func TestJournalRejectsTrailingJSONAndTampering(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	manifest := environment.importMod(t, "800", map[string]string{"modinfo.lua": "version='1'"})
	plan, err := environment.manager.BuildPlan(context.Background(), release("release-journal-001", manifest, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := environment.manager.Prepare(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	journalPath := environment.manager.journalPath(plan.OperationID)
	file, err := os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := environment.manager.readJournal(plan.OperationID); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected trailing JSON rejection, got %v", err)
	}
}

func TestOnlyOneDistributionOperationCanBePrepared(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	manifest := environment.importMod(t, "900", map[string]string{"modinfo.lua": "version='1'"})
	first, err := environment.manager.BuildPlan(context.Background(), release("release-active-0001", manifest, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	second, err := environment.manager.BuildPlan(context.Background(), release("release-active-0002", manifest, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := environment.manager.Prepare(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := environment.manager.Prepare(context.Background(), second); !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("expected operation gate, got %v", err)
	}
	if err := environment.manager.Rollback(context.Background(), first.OperationID); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentPrepareAllowsExactlyOneOperation(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	manifest := environment.importMod(t, "903", map[string]string{"modinfo.lua": "version='1'"})
	inputs := []string{"release-race-0001", "release-race-0002"}
	plans := make([]Plan, len(inputs))
	for index, operationID := range inputs {
		plan, err := environment.manager.BuildPlan(context.Background(), release(operationID, manifest, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		plans[index] = plan
	}
	type result struct {
		operationID string
		err         error
	}
	results := make(chan result, len(plans))
	var group sync.WaitGroup
	for _, plan := range plans {
		plan := plan
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := environment.manager.Prepare(context.Background(), plan)
			results <- result{operationID: plan.OperationID, err: err}
		}()
	}
	group.Wait()
	close(results)
	succeeded, blocked, winner := 0, 0, ""
	for item := range results {
		if item.err == nil {
			succeeded++
			winner = item.operationID
		} else if errors.Is(item.err, ErrOperationInProgress) {
			blocked++
		} else {
			t.Fatalf("unexpected concurrent prepare error: %v", item.err)
		}
	}
	if succeeded != 1 || blocked != 1 {
		t.Fatalf("unexpected concurrent result: succeeded=%d blocked=%d", succeeded, blocked)
	}
	if err := environment.manager.Rollback(context.Background(), winner); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRollsBackCrashBetweenAtomicMutations(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	manifest := environment.importMod(t, "901", map[string]string{"modinfo.lua": "version='new'"})
	oldMod := filepath.Join(environment.server, "mods", "workshop-901")
	if err := os.MkdirAll(oldMod, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldMod, "modinfo.lua"), []byte("version='old'"), 0o640); err != nil {
		t.Fatal(err)
	}
	plan, err := environment.manager.BuildPlan(context.Background(), release("release-midcrash-01", manifest, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := environment.manager.Prepare(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	journal.Phase = PhasePublishing
	if err := environment.manager.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	if err := environment.manager.publishMutation(journal, journal.Mutations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := New(environment.config); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(oldMod, "modinfo.lua"), []byte("version='old'"))
}

func TestCompleteDetectsPublishedTargetTampering(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	manifest := environment.importMod(t, "902", map[string]string{"modinfo.lua": "version='1'"})
	plan, err := environment.manager.BuildPlan(context.Background(), release("release-tamper-001", manifest, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := environment.manager.Prepare(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if _, err := environment.manager.Publish(context.Background(), plan.OperationID); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(environment.server, "mods", "workshop-902", "modinfo.lua")
	if err := os.Chmod(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := environment.manager.Complete(context.Background(), plan.OperationID); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected publish integrity failure, got %v", err)
	}
	if err := environment.manager.Rollback(context.Background(), plan.OperationID); err != nil {
		t.Fatal(err)
	}
}

func TestPublishRefusesExternalChangeAfterPrepare(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	manifest := environment.importMod(t, "904", map[string]string{"modinfo.lua": "version='1'"})
	writeWorldConfig(t, environment.saves, "Master", []byte("return {before=true}\n"))
	writeWorldConfig(t, environment.saves, "Caves", []byte("return {before=true}\n"))
	plan, err := environment.manager.BuildPlan(context.Background(), release("release-external-01", manifest, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := environment.manager.Prepare(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	master := filepath.Join(environment.saves, "Cluster_1", "Master", "modoverrides.lua")
	if err := os.WriteFile(master, []byte("return {operator_change=true}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := environment.manager.Publish(context.Background(), plan.OperationID); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected external-change conflict, got %v", err)
	}
	if err := environment.manager.Rollback(context.Background(), plan.OperationID); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, master, []byte("return {operator_change=true}\n"))
	_ = journal
}

func writeWorldConfig(t *testing.T, saveRoot, world string, content []byte) {
	t.Helper()
	path := filepath.Join(saveRoot, "Cluster_1", world, "modoverrides.lua")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o640); err != nil {
		t.Fatal(err)
	}
}

func assertFileContent(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(expected) {
		t.Fatalf("unexpected %s content: got %q want %q", path, actual, expected)
	}
}
