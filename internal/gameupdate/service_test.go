package gameupdate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/backups"
	dstinstall "dont/internal/dstserver"
	"dont/internal/jobs"
	"dont/internal/rooms"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type updateCatalog struct {
	rooms  []rooms.Room
	worlds map[string][]rooms.World
}

func (c updateCatalog) List() ([]rooms.Room, error) { return c.rooms, nil }
func (c updateCatalog) Worlds(roomID string) ([]rooms.World, error) {
	return c.worlds[roomID], nil
}

type updateControl struct {
	mu      sync.Mutex
	running map[string]bool
	events  *[]string
	fail    map[string]error
}

func (c *updateControl) IsRunning(_ context.Context, room, world string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running[room+"/"+world], nil
}

func (c *updateControl) Start(_ context.Context, room, world string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := "start:" + world
	*c.events = append(*c.events, key)
	if err := c.fail[key]; err != nil {
		return err
	}
	c.running[room+"/"+world] = true
	return nil
}

func (c *updateControl) Stop(_ context.Context, room, world string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := "stop:" + world
	*c.events = append(*c.events, key)
	if err := c.fail[key]; err != nil {
		return err
	}
	c.running[room+"/"+world] = false
	return nil
}

type updateBackups struct{ events *[]string }

func (b updateBackups) Create(_ context.Context, roomID, name string, kind backups.Kind, sourceJobID string) (backups.Backup, error) {
	*b.events = append(*b.events, "backup:"+roomID)
	if kind != backups.KindProtection || sourceJobID == "" || !strings.Contains(name, "保护备份") {
		return backups.Backup{}, errors.New("invalid protection backup request")
	}
	return backups.Backup{Name: name, Kind: kind, SourceJobID: sourceJobID}, nil
}

type updateRunner struct {
	events    *[]string
	arguments []string
	server    string
	err       error
}

func (r *updateRunner) Run(_ context.Context, _ string, arguments []string, output io.Writer) error {
	*r.events = append(*r.events, "runner")
	r.arguments = append([]string(nil), arguments...)
	_, _ = io.WriteString(output, "download complete\n")
	if r.err == nil {
		_ = os.WriteFile(filepath.Join(r.server, "version.txt"), []byte("200\n"), 0640)
	}
	return r.err
}

type staticLatest struct{}

func (staticLatest) Check(context.Context, string, string) (string, bool, error) {
	return "200", false, nil
}

type recordingLatest struct{ appID string }

func (c *recordingLatest) Check(_ context.Context, appID, local string) (string, bool, error) {
	c.appID = appID
	return local, true, nil
}

func newUpdateService(t *testing.T, runner CommandRunner) (*Service, *Store, updateCatalog, *updateControl, *[]string, string) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	room := rooms.Room{ID: "room", DirectoryName: "Cluster", Name: "Test", Managed: true}
	worlds := []rooms.World{
		{ID: "master", RoomID: room.ID, DirectoryName: "Master", Name: "Master", IsMaster: true},
		{ID: "caves", RoomID: room.ID, DirectoryName: "Caves", Name: "Caves"},
	}
	catalog := updateCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: worlds}}
	events := &[]string{}
	control := &updateControl{running: map[string]bool{"Cluster/Master": true, "Cluster/Caves": true}, events: events, fail: make(map[string]error)}
	server := filepath.Join(t.TempDir(), "server path;still-one-argument")
	if err := os.MkdirAll(server, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(server, "version.txt"), []byte("100\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if runner == nil {
		runner = &updateRunner{events: events, server: server}
	}
	service, err := NewService(Config{ServerPath: server}, catalog, control, updateBackups{events: events}, store, runner, staticLatest{})
	if err != nil {
		t.Fatal(err)
	}
	service.pollInterval = time.Millisecond
	service.startTimeout = time.Second
	service.stopTimeout = time.Second
	return service, store, catalog, control, events, server
}

func TestExecuteProtectsThenStopsAndRestartsInShardOrder(t *testing.T) {
	service, store, catalog, control, events, server := newUpdateService(t, nil)
	runner := &updateRunner{events: events, server: server}
	service.runner = runner
	running := []plannedWorld{
		{roomID: "room", roomName: "Cluster", worldID: "master", worldName: "Master", isMaster: true},
		{roomID: "room", roomName: "Cluster", worldID: "caves", worldName: "Caves"},
	}
	var results []jobs.TargetResult
	err := service.execute(context.Background(), "job-1", UpdateRequest{RestartRunning: true}, "/tmp/steamcmd", catalog.rooms, running, func(value jobs.TargetResult) {
		results = append(results, value)
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"backup:room", "stop:Caves", "stop:Master", "runner", "start:Master", "start:Caves"}
	if !reflect.DeepEqual(*events, expected) {
		t.Fatalf("events = %#v, expected %#v", *events, expected)
	}
	if len(results) != 6 {
		t.Fatalf("target results = %#v", results)
	}
	run, err := store.Get("job-1")
	if err != nil || run.Status != "succeeded" || run.BeforeVersion != "100" || run.AfterVersion != "200" || !strings.Contains(run.Log, "download complete") {
		t.Fatalf("run = %#v, %v", run, err)
	}
	if len(runner.arguments) != 8 || runner.arguments[1] != server || runner.arguments[5] != "343050" {
		t.Fatalf("SteamCMD arguments = %#v", runner.arguments)
	}
	for _, world := range []string{"Cluster/Master", "Cluster/Caves"} {
		if !control.running[world] {
			t.Fatalf("%s was not restarted", world)
		}
	}
}

func TestUpdateFailureIsPersistedAndFailsRunnerAfterRecovery(t *testing.T) {
	service, store, catalog, _, events, server := newUpdateService(t, nil)
	runner := &updateRunner{events: events, server: server, err: errors.New("steam unavailable")}
	service.runner = runner
	running := []plannedWorld{{roomID: "room", roomName: "Cluster", worldID: "master", worldName: "Master", isMaster: true}}
	err := service.execute(context.Background(), "job-fail", UpdateRequest{RestartRunning: true}, "/tmp/steamcmd", catalog.rooms, running, func(jobs.TargetResult) {})
	if err == nil || !strings.Contains(err.Error(), "steam unavailable") {
		t.Fatalf("execute error = %v", err)
	}
	run, getErr := store.Get("job-fail")
	if getErr != nil || run.Status != "failed" || !strings.Contains(run.ErrorMessage, "steam unavailable") {
		t.Fatalf("run = %#v, %v", run, getErr)
	}
	if got := (*events)[len(*events)-1]; got != "start:Master" {
		t.Fatalf("last event = %q", got)
	}
}

func TestStopFailureRecoversAlreadyStoppedWorlds(t *testing.T) {
	service, _, catalog, control, events, _ := newUpdateService(t, nil)
	control.fail["stop:Master"] = errors.New("stop failed")
	running := []plannedWorld{
		{roomID: "room", roomName: "Cluster", worldID: "master", worldName: "Master", isMaster: true},
		{roomID: "room", roomName: "Cluster", worldID: "caves", worldName: "Caves"},
	}
	err := service.execute(context.Background(), "job-stop", UpdateRequest{}, "/tmp/steamcmd", catalog.rooms, running, func(jobs.TargetResult) {})
	if err == nil || !control.running["Cluster/Caves"] {
		t.Fatalf("error = %v, caves running = %t", err, control.running["Cluster/Caves"])
	}
	expected := []string{"backup:room", "stop:Caves", "stop:Master", "start:Caves"}
	if !reflect.DeepEqual(*events, expected) {
		t.Fatalf("events = %#v", *events)
	}
}

func TestPrepareRejectsConcurrentUpdatesAndRequiresExactConfirmation(t *testing.T) {
	service, _, _, control, _, _ := newUpdateService(t, nil)
	control.running = make(map[string]bool)
	executable := filepath.Join(t.TempDir(), "steamcmd")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0750); err != nil {
		t.Fatal(err)
	}
	service.config.SteamCMDPath = executable
	if _, _, _, err := service.Prepare(context.Background(), UpdateRequest{Confirmation: "wrong"}); !errors.Is(err, ErrConfirmationNeeded) {
		t.Fatalf("confirmation error = %v", err)
	}
	_, _, release, err := service.Prepare(context.Background(), UpdateRequest{Confirmation: "更新游戏"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := service.Prepare(context.Background(), UpdateRequest{Confirmation: "更新游戏"}); !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("concurrent error = %v", err)
	}
	release()
	_, _, releaseAgain, err := service.Prepare(context.Background(), UpdateRequest{Confirmation: "更新游戏"})
	if err != nil {
		t.Fatal(err)
	}
	releaseAgain()
}

func TestSteamClientInstallReportsExternalUpdateAndRejectsPrepare(t *testing.T) {
	base, store, catalog, control, events, _ := newUpdateService(t, nil)
	steamApps := filepath.Join(t.TempDir(), "steamapps")
	gameRoot := filepath.Join(steamApps, "common", "Don't Starve Together")
	executablePath := filepath.Join(gameRoot, "dontstarve_steam.app", "Contents", "MacOS", dstinstall.Binary)
	if err := os.MkdirAll(filepath.Dir(executablePath), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executablePath, []byte("test"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(steamApps, "appmanifest_322330.acf"), []byte(`"AppState" { "buildid" "654321" }`), 0640); err != nil {
		t.Fatal(err)
	}
	steamCMD := filepath.Join(t.TempDir(), "steamcmd")
	if err := os.WriteFile(steamCMD, []byte("#!/bin/sh\nexit 0\n"), 0750); err != nil {
		t.Fatal(err)
	}
	latest := &recordingLatest{}
	service, err := NewService(
		Config{ServerPath: gameRoot, SteamCMDPath: steamCMD},
		catalog, control, updateBackups{events: events}, store, base.runner, latest,
	)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Version(context.Background())
	if !report.Installed || report.LocalVersion != "654321" || report.AppID != dstinstall.AppIDGame {
		t.Fatalf("version report = %#v", report)
	}
	if report.UpdateMethod != dstinstall.UpdateMethodSteamClient || report.UpdateSupported || !report.SteamCMDAvailable {
		t.Fatalf("update capability = %#v", report)
	}
	if latest.appID != dstinstall.AppIDGame {
		t.Fatalf("latest checker app ID = %q", latest.appID)
	}
	if _, _, _, err := service.Prepare(context.Background(), UpdateRequest{Confirmation: "更新游戏"}); !errors.Is(err, ErrSteamClientManaged) {
		t.Fatalf("prepare error = %v", err)
	}
}

func TestCleanSteamCacheOnlyRemovesApp343050(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "steamcmd")
	server := filepath.Join(t.TempDir(), "server")
	for _, base := range []string{filepath.Join(root, "steamapps"), filepath.Join(server, "steamapps")} {
		for _, target := range []string{filepath.Join(base, "downloading", "343050"), filepath.Join(base, "downloading", "999999")} {
			if err := os.MkdirAll(target, 0750); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(base, "appmanifest_343050.acf"), []byte("manifest"), 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanSteamCache(executable, server, dstinstall.AppIDDedicatedServer); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{filepath.Join(root, "steamapps"), filepath.Join(server, "steamapps")} {
		if _, err := os.Stat(filepath.Join(base, "downloading", "343050")); !os.IsNotExist(err) {
			t.Fatalf("343050 cache still exists: %v", err)
		}
		if _, err := os.Stat(filepath.Join(base, "downloading", "999999")); err != nil {
			t.Fatalf("unrelated cache removed: %v", err)
		}
	}
}
