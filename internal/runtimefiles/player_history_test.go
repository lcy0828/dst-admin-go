package runtimefiles

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/shared"
)

func readHistoryFixture(t *testing.T, root, shard string) PlayerHistoryResult {
	t.Helper()
	bundle, err := ReadArtifacts(context.Background(), root, "all", shard, shared.ArtifactRuntimePlayerHistory)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateArtifactBundle(shared.ArtifactRuntimePlayerHistory, bundle); err != nil {
		t.Fatal(err)
	}
	var result PlayerHistoryResult
	if err := json.Unmarshal(bundle.Artifacts[0].Data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPlayerHistoryRetainsShortSessionsArchivesAndLocalWorldEvidence(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "all", "Master")
	archive := filepath.Join(world, "backup", "server_log")
	if err := os.MkdirAll(archive, 0700); err != nil {
		t.Fatal(err)
	}
	old := "[00:00:00]: Current time: Thu Sep 10 10:00:00 2026\n[00:00:01]: Client authenticated: (KU_ONE) Old Name\n[00:00:02]: User ID\tKU_ONE\tassigned ownership to entity\t17 - wendy\t\n"
	if err := os.WriteFile(filepath.Join(archive, "server_log_2026-09-10-11-00-00.txt"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	current := "[00:00:00]: Current time: Fri Sep 11 10:00:00 2026\n[00:47:32]: Client authenticated: (KU_ONE) New Name\n[00:47:37]: [Shard] (KU_ONE) disconnected from Master(1)\n[00:49:00]: [Shard] (KU_ONE) disconnected from Caves(2)\n"
	path := filepath.Join(world, "server_log.txt")
	if err := os.WriteFile(path, []byte(current), 0600); err != nil {
		t.Fatal(err)
	}
	result := readHistoryFixture(t, root, "Master")
	if !result.Complete || len(result.Players) != 1 {
		t.Fatalf("history=%#v", result)
	}
	p := result.Players[0]
	if p.Name != "New Name" || p.Prefab != "wendy" || p.LastSeenWorld != "Caves" || !p.LastSeenAt.Equal(time.Date(2026, 9, 11, 10, 49, 0, 0, time.Local)) || !p.FirstSeenAt.Equal(time.Date(2026, 9, 10, 10, 0, 1, 0, time.Local)) {
		t.Fatalf("history=%#v", p)
	}
	// Read a new short connection incrementally, including a split line.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("[00:50:00]: Client authenticated: (KU_TWO) Sec")
	f.Close()
	if got := readHistoryFixture(t, root, "Master"); len(got.Players) != 1 {
		t.Fatal("partial line was committed")
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString("ond\n[00:50:05]: [Shard] (KU_TWO) disconnected from Master(1)\n")
	f.Close()
	if got := readHistoryFixture(t, root, "Master"); len(got.Players) != 2 || got.Players[1].Name != "Second" {
		t.Fatalf("incremental history=%#v", got)
	}
}

func TestPlayerHistorySupportsMoreThanOnlinePlayerLimitAndRotation(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "all", "Master")
	os.MkdirAll(world, 0700)
	content := "[00:00:00]: Current time: Thu Sep 10 10:00:00 2026\n"
	for i := 0; i < 70; i++ {
		content += fmt.Sprintf("[00:00:01]: Client authenticated: (KU_%03d) Player %d\n", i, i)
	}
	path := filepath.Join(world, "server_log.txt")
	os.WriteFile(path, []byte(content), 0600)
	if got := readHistoryFixture(t, root, "Master"); len(got.Players) != 70 {
		t.Fatalf("history count=%d", len(got.Players))
	}
	os.Rename(path, path+".old")
	os.WriteFile(path, []byte("[00:00:00]: Current time: Fri Sep 11 10:00:00 2026\n[00:00:05]: Client authenticated: (KU_NEW) New\n"), 0600)
	if got := readHistoryFixture(t, root, "Master"); len(got.Players) != 71 {
		t.Fatalf("rotation erased history: %d", len(got.Players))
	}
}

func TestPlayerHistoryRejectsSymlinkedArchive(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "all", "Master")
	os.MkdirAll(filepath.Join(world, "backup"), 0700)
	os.Symlink(t.TempDir(), filepath.Join(world, "backup", "server_log"))
	if _, err := ReadArtifacts(context.Background(), root, "all", "Master", shared.ArtifactRuntimePlayerHistory); err == nil {
		t.Fatal("symlinked history accepted")
	}
}
