package runtimefiles

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/shared"
)

func worldStateFilesFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	world := filepath.Join(root, "all", "Master")
	for _, directory := range []string{"save/session/SESSION", "save/mod_config_data/dst-admin", "dst-admin"} {
		if err := os.MkdirAll(filepath.Join(world, directory), 0750); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"customcommands.lua":      "-- managed loader\n",
		"dst-admin/bootstrap.lua": "-- managed bootstrap\n",
		"dst-admin/manifest.json": "{}",
		"server.ini":              "[SHARD]\nid = 1\n",
		"server_log.txt":          "[00:00:00]: Starting Up\n[00:00:00]: Current time: Sun Sep 6 12:00:00 2026\n",
		"save/mod_config_data/dst-admin/worldstate-a.json": "KLEI     1 {\"sequence\":1}",
		"save/mod_config_data/dst-admin/worldstate-b.json": "KLEI     1 {\"sequence\":2}",
	} {
		if err := os.WriteFile(filepath.Join(world, name), []byte(content), 0640); err != nil {
			t.Fatal(err)
		}
	}
	return root, world
}

func TestWorldStateReadReturnsBothFilesAndStartupIdentityWithoutHealth(t *testing.T) {
	root, world := worldStateFilesFixture(t)
	status := shared.ShardRuntimeStatus{State: "running", SessionExists: true}
	value, err := ReadWorldState(context.Background(), root, "all", "Master", status)
	if err != nil || value.ReadError != "" || value.Runtime != status || value.SessionID != "SESSION" || value.ShardID != "1" || value.StartedAt.IsZero() || len(value.Artifacts.Artifacts) != 2 {
		t.Fatalf("world files=%#v, error=%v", value, err)
	}
	if err := ValidateArtifactBundle(shared.ArtifactRuntimeWorldState, value.Artifacts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(world, "save/mod_config_data/dst-admin/health.json")); !os.IsNotExist(err) {
		t.Fatalf("read created a health file: %v", err)
	}
	path := filepath.Join(world, "save/mod_config_data/dst-admin/worldstate-a.json")
	if err := os.WriteFile(path, []byte(`{"sequence":3}`), 0640); err != nil {
		t.Fatal(err)
	}
	updated, err := ReadWorldState(context.Background(), root, "all", "Master", status)
	if err != nil || string(updated.Artifacts.Artifacts[0].Data) != `{"sequence":3}` {
		t.Fatalf("read reused earlier file contents: %#v, %v", updated, err)
	}
}

func TestWorldStateReadPreservesStoppedStatusAndFileErrors(t *testing.T) {
	root, world := worldStateFilesFixture(t)
	status := shared.ShardRuntimeStatus{State: "stopped"}
	if err := os.Remove(filepath.Join(world, "server.ini")); err != nil {
		t.Fatal(err)
	}
	value, err := ReadWorldState(context.Background(), root, "all", "Master", status)
	if err != nil || value.Runtime.State != "stopped" || !strings.Contains(value.ReadError, "server.ini") {
		t.Fatalf("file failure lost runtime status: %#v, %v", value, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadWorldState(ctx, root, "all", "Master", status); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestWorldStateReadBeforeFirstSampleHasEmptyBundleWithoutReadError(t *testing.T) {
	root, world := worldStateFilesFixture(t)
	for _, slot := range []string{"worldstate-a.json", "worldstate-b.json"} {
		if err := os.Remove(filepath.Join(world, "save/mod_config_data/dst-admin", slot)); err != nil {
			t.Fatal(err)
		}
	}
	value, err := ReadWorldState(context.Background(), root, "all", "Master", shared.ShardRuntimeStatus{State: "running"})
	if err != nil || value.ReadError != "" || value.Artifacts.Kind != shared.ArtifactRuntimeWorldState || len(value.Artifacts.Artifacts) != 0 {
		t.Fatalf("missing first sample treated as read failure: %#v, %v", value, err)
	}
}

func TestStoppedWorldWithoutSamplesDoesNotRequireGameGeneratedFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "testtest", "Master"), 0750); err != nil {
		t.Fatal(err)
	}
	value, err := ReadWorldState(context.Background(), root, "testtest", "Master", shared.ShardRuntimeStatus{State: "stopped"})
	if err != nil || value.ReadError != "" || value.Artifacts.Kind != shared.ArtifactRuntimeWorldState || len(value.Artifacts.Artifacts) != 0 || !value.StartedAt.IsZero() {
		t.Fatalf("unused world treated as failure: %+v %v", value, err)
	}
}

func TestRunningWorldWithoutCollectorReportsActionableError(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "room", "Master"), 0750); err != nil {
		t.Fatal(err)
	}
	value, err := ReadWorldState(context.Background(), root, "room", "Master", shared.ShardRuntimeStatus{State: "running"})
	if err != nil || !strings.Contains(value.ReadError, "采集脚本") {
		t.Fatalf("missing collector=%+v err=%v", value, err)
	}
}

func TestWorldStateReadRejectsSymlinkedIdentityAndLogs(t *testing.T) {
	for _, name := range []string{"server.ini", "server_log.txt"} {
		t.Run(name, func(t *testing.T) {
			root, world := worldStateFilesFixture(t)
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("outside"), 0640); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(world, name)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			value, err := ReadWorldState(context.Background(), root, "all", "Master", shared.ShardRuntimeStatus{State: "running"})
			if err != nil || value.ReadError == "" {
				t.Fatalf("accepted symlink: %#v, %v", value, err)
			}
		})
	}
}

func TestWorldStateStartTimeReadsOnlyBoundedLogPrefix(t *testing.T) {
	root, world := worldStateFilesFixture(t)
	path := filepath.Join(world, "server_log.txt")
	content := "[00:00:00]: Current time: Sun Sep 6 12:00:00 2026\n" + strings.Repeat("ordinary log line\n", 10000)
	if err := os.WriteFile(path, []byte(content), 0640); err != nil {
		t.Fatal(err)
	}
	value, err := ReadWorldState(context.Background(), root, "all", "Master", shared.ShardRuntimeStatus{State: "running"})
	if err != nil || value.ReadError != "" || !value.StartedAt.Equal(time.Date(2026, 9, 6, 12, 0, 0, 0, time.Local)) {
		t.Fatalf("startup identity=%#v, error=%v", value, err)
	}
}

func TestWorldStateReadKeepsOtherSlotWhenOneFileCannotBeRead(t *testing.T) {
	root, world := worldStateFilesFixture(t)
	first := filepath.Join(world, "save/mod_config_data/dst-admin/worldstate-a.json")
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(first, 0750); err != nil {
		t.Fatal(err)
	}
	value, err := ReadWorldState(context.Background(), root, "all", "Master", shared.ShardRuntimeStatus{State: "running"})
	if err != nil || value.ReadError != "" || len(value.Artifacts.Artifacts) != 1 || value.Artifacts.Artifacts[0].Name != "worldstate-b.json" {
		t.Fatalf("bad slot hid the other file: %#v, %v", value, err)
	}
	second := filepath.Join(world, "save/mod_config_data/dst-admin/worldstate-b.json")
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	value, err = ReadWorldState(context.Background(), root, "all", "Master", shared.ShardRuntimeStatus{State: "running"})
	if err != nil || value.ReadError == "" {
		t.Fatalf("all unreadable files were hidden: %#v, %v", value, err)
	}
}
