package backups

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/internal/runtimeguard"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type testCatalog struct {
	room   rooms.Room
	worlds []rooms.World
}

func (c testCatalog) Room(string) (rooms.Room, error)      { return c.room, nil }
func (c testCatalog) Worlds(string) ([]rooms.World, error) { return c.worlds, nil }

type testRuntime struct {
	running map[string]bool
	sent    []string
}

type deniedBackupMutationGuard struct{}

func (deniedBackupMutationGuard) RequireRoom(string) error {
	return runtimeguard.ErrRemoteMutationUnavailable
}

func (deniedBackupMutationGuard) RequireWorld(string, string) error {
	return runtimeguard.ErrRemoteMutationUnavailable
}

func (r *testRuntime) IsRunning(_ context.Context, _, world string) (bool, error) {
	return r.running[world], nil
}

func (r *testRuntime) Send(_ context.Context, _, _, command string) error {
	r.sent = append(r.sent, command)
	return nil
}

func newBackupService(t *testing.T) (*Service, *testRuntime, string, string) {
	t.Helper()
	saveRoot := filepath.Join(t.TempDir(), "saves")
	backupRoot := filepath.Join(t.TempDir(), "backups")
	roomPath := filepath.Join(saveRoot, "room")
	if err := os.MkdirAll(filepath.Join(roomPath, "Master"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roomPath, "cluster.ini"), []byte("[NETWORK]\ncluster_name = Test\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roomPath, "Master", "server.ini"), []byte("[SHARD]\nis_master = true\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roomPath, "Master", "session-data"), []byte("before"), 0640); err != nil {
		t.Fatal(err)
	}
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
	room := rooms.Room{ID: rooms.EncodeID("room"), DirectoryName: "room", Name: "Test Room", Managed: true}
	world := rooms.World{ID: rooms.EncodeID("Master"), RoomID: room.ID, DirectoryName: "Master", Name: "Master", IsMaster: true}
	runtime := &testRuntime{running: make(map[string]bool)}
	service, err := NewService(saveRoot, backupRoot, testCatalog{room: room, worlds: []rooms.World{world}}, runtime, store)
	if err != nil {
		t.Fatal(err)
	}
	return service, runtime, roomPath, backupRoot
}

func TestStoppedRoomCreatesVerifiedLegacyArchive(t *testing.T) {
	service, _, _, _ := newBackupService(t)
	value, err := service.Create(context.Background(), rooms.EncodeID("room"), "第一次备份", KindManual, "job-id")
	if err != nil {
		t.Fatal(err)
	}
	if value.VerifiedAt == nil || value.SHA256 == "" || value.FileCount < 3 || value.SourceJobID != "job-id" {
		t.Fatalf("backup = %#v", value)
	}
	file, _, _, err := service.Open(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
}

func TestLiveLegacyBackupRequiresCoordinatorWithoutSideEffects(t *testing.T) {
	for _, kind := range []Kind{KindManual, KindSnapshot, KindProtection} {
		service, runtime, _, root := newBackupService(t)
		runtime.running["Master"] = true
		if _, err := service.Create(context.Background(), rooms.EncodeID("room"), "live", kind, "job-id"); !errors.Is(err, ErrConsistentBackupRequired) {
			t.Fatalf("kind=%s err=%v", kind, err)
		}
		if len(runtime.sent) != 0 {
			t.Fatal("legacy backup sent an unacknowledged save")
		}
		files, err := filepath.Glob(filepath.Join(root, "room", "*.zip"))
		if err != nil || len(files) != 0 {
			t.Fatalf("live archive published: %v %v", files, err)
		}
	}
}

func TestRestoreCreatesProtectionBackupAndAtomicallyReplacesRoom(t *testing.T) {
	service, _, roomPath, _ := newBackupService(t)
	value, err := service.Create(context.Background(), rooms.EncodeID("room"), "可恢复", KindManual, "")
	if err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(roomPath, "Master", "session-data")
	if err := os.WriteFile(dataPath, []byte("after"), 0640); err != nil {
		t.Fatal(err)
	}
	protection, err := service.Restore(context.Background(), rooms.EncodeID("room"), value.ID, "Test Room", "restore-job")
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "before" || protection.Kind != KindProtection || protection.SourceJobID != "restore-job" {
		t.Fatalf("restored content = %q, protection = %#v", content, protection)
	}
	items, err := service.List(rooms.EncodeID("room"))
	if err != nil || len(items) != 2 {
		t.Fatalf("backups = %#v, %v", items, err)
	}
}

func TestRestoreRequiresStoppedWorldAndExactRoomName(t *testing.T) {
	service, runtime, _, _ := newBackupService(t)
	value, err := service.Create(context.Background(), rooms.EncodeID("room"), "备份", KindManual, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Restore(context.Background(), rooms.EncodeID("room"), value.ID, "wrong", ""); !errors.Is(err, ErrConfirmationNeeded) {
		t.Fatalf("confirmation error = %v", err)
	}
	runtime.running["Master"] = true
	if _, err := service.Restore(context.Background(), rooms.EncodeID("room"), value.ID, "Test Room", ""); !errors.Is(err, ErrWorldRunning) {
		t.Fatalf("running error = %v", err)
	}
	items, _ := service.List(rooms.EncodeID("room"))
	if len(items) != 1 {
		t.Fatalf("restore created protection backup before safety checks: %#v", items)
	}
}

func TestUploadRejectsZipSlipAndDoesNotPublish(t *testing.T) {
	service, _, _, backupRoot := newBackupService(t)
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("../cluster.ini")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("bad"))
	_ = writer.Close()
	if _, err := service.Import(context.Background(), rooms.EncodeID("room"), "bad", "bad.zip", &archive); !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("upload error = %v", err)
	}
	items, _ := service.List(rooms.EncodeID("room"))
	if len(items) != 0 {
		t.Fatalf("invalid upload was persisted: %#v", items)
	}
	files, err := filepath.Glob(filepath.Join(backupRoot, "room", "*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("invalid upload files = %#v, %v", files, err)
	}
}

func TestPolicyCanBeDisabledAndSnapshotsArePruned(t *testing.T) {
	service, _, _, _ := newBackupService(t)
	roomID := rooms.EncodeID("room")
	policy, err := service.SavePolicy(roomID, PolicyRequest{Enabled: true, IntervalMinute: 15, MaxSnapshots: 1})
	if err != nil || policy.NextRunAt == nil {
		t.Fatalf("enabled policy = %#v, %v", policy, err)
	}
	for index := 0; index < 2; index++ {
		if _, err := service.Create(context.Background(), roomID, "snapshot", KindSnapshot, ""); err != nil {
			t.Fatal(err)
		}
	}
	_, removed, err := service.PruneSnapshots(context.Background(), roomID, 1)
	if err != nil || removed != 1 {
		t.Fatalf("prune = %d, %v", removed, err)
	}
	policy, err = service.SavePolicy(roomID, PolicyRequest{Enabled: false, IntervalMinute: 15, MaxSnapshots: 1})
	if err != nil || policy.Enabled || policy.NextRunAt != nil {
		t.Fatalf("disabled policy = %#v, %v", policy, err)
	}
}

func TestSchedulerSubmitsPersistentSnapshotJobAndAdvancesPolicy(t *testing.T) {
	service, _, _, _ := newBackupService(t)
	roomID := rooms.EncodeID("room")
	if _, err := service.SavePolicy(roomID, PolicyRequest{Enabled: true, IntervalMinute: 15, MaxSnapshots: 2}); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	jobStore := jobs.NewStore(service.store.db, "scheduler_")
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(service, jobService)
	if err := scheduler.RunDue(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		policy, policyErr := service.Policy(roomID)
		if policyErr != nil {
			t.Fatal(policyErr)
		}
		if policy.LastJobID != "" {
			job, getErr := jobService.Get(policy.LastJobID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if job.Status == jobs.StatusQueued || job.Status == jobs.StatusRunning {
				if time.Now().After(deadline) {
					t.Fatalf("snapshot job did not finish: %#v", job)
				}
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if job.Status != jobs.StatusSucceeded || policy.NextRunAt == nil {
				t.Fatalf("job = %#v, policy = %#v", job, policy)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("snapshot job did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	items, err := service.List(roomID)
	if err != nil || len(items) != 1 || items[0].Kind != KindSnapshot {
		t.Fatalf("snapshot backups = %#v, %v", items, err)
	}
}

func TestListAdoptsLegacyArchivesAndMarksInvalidFiles(t *testing.T) {
	service, _, _, backupRoot := newBackupService(t)
	roomID := rooms.EncodeID("room")
	created, err := service.Create(context.Background(), roomID, "source", KindManual, "")
	if err != nil {
		t.Fatal(err)
	}
	file, _, _, err := service.Open(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	validBytes, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(backupRoot, "room")
	if err := os.WriteFile(filepath.Join(directory, "legacy.zip"), validBytes, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "broken.zip"), []byte("not a zip"), 0640); err != nil {
		t.Fatal(err)
	}
	items, err := service.List(roomID)
	if err != nil || len(items) != 2 {
		t.Fatalf("legacy backups = %#v, %v", items, err)
	}
	statuses := map[string]string{}
	for _, item := range items {
		statuses[item.FileName] = item.Status
		if item.Kind != KindImported {
			t.Fatalf("legacy kind = %s", item.Kind)
		}
	}
	if statuses["legacy.zip"] != "verified" || statuses["broken.zip"] != "invalid" {
		t.Fatalf("legacy statuses = %#v", statuses)
	}
}

func TestRemoteRoomGuardBlocksBackupMutationsWithoutSideEffects(t *testing.T) {
	service, runtime, roomPath, backupRoot := newBackupService(t)
	roomID := rooms.EncodeID("room")
	manual, err := service.Create(context.Background(), roomID, "可恢复", KindManual, "")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if _, err := service.Create(context.Background(), roomID, "快照", KindSnapshot, ""); err != nil {
			t.Fatal(err)
		}
	}
	archive, _, _, err := service.Open(manual.ID)
	if err != nil {
		t.Fatal(err)
	}
	archiveBytes, err := io.ReadAll(archive)
	_ = archive.Close()
	if err != nil {
		t.Fatal(err)
	}
	beforeSource, err := os.ReadFile(filepath.Join(roomPath, "Master", "session-data"))
	if err != nil {
		t.Fatal(err)
	}
	beforeFiles, err := filepath.Glob(filepath.Join(backupRoot, "room", "*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	beforeItems, err := service.List(roomID)
	if err != nil {
		t.Fatal(err)
	}
	beforeCommands := len(runtime.sent)
	service.ConfigureMutationGuard(deniedBackupMutationGuard{})

	assertBlocked := func(operation string, err error) {
		t.Helper()
		if !errors.Is(err, runtimeguard.ErrRemoteMutationUnavailable) {
			t.Fatalf("%s error = %v", operation, err)
		}
	}
	_, err = service.Create(context.Background(), roomID, "禁止创建", KindManual, "")
	assertBlocked("create", err)
	_, _, err = service.ValidateRestore(context.Background(), roomID, manual.ID, "Test Room")
	assertBlocked("validate restore", err)
	_, err = service.Restore(context.Background(), roomID, manual.ID, "Test Room", "")
	assertBlocked("restore", err)
	_, err = service.Delete(manual.ID)
	assertBlocked("delete", err)
	_, _, err = service.PruneSnapshots(context.Background(), roomID, 1)
	assertBlocked("prune", err)

	afterSource, err := os.ReadFile(filepath.Join(roomPath, "Master", "session-data"))
	if err != nil || !bytes.Equal(afterSource, beforeSource) {
		t.Fatalf("room files changed: before=%q after=%q err=%v", beforeSource, afterSource, err)
	}
	afterFiles, err := filepath.Glob(filepath.Join(backupRoot, "room", "*.zip"))
	if err != nil || len(afterFiles) != len(beforeFiles) {
		t.Fatalf("backup files changed: before=%v after=%v err=%v", beforeFiles, afterFiles, err)
	}
	afterItems, err := service.List(roomID)
	if err != nil || len(afterItems) != len(beforeItems) {
		t.Fatalf("backup records changed: before=%#v after=%#v err=%v", beforeItems, afterItems, err)
	}
	if len(runtime.sent) != beforeCommands {
		t.Fatalf("runtime commands changed: before=%d after=%d", beforeCommands, len(runtime.sent))
	}

	if _, err := service.Get(manual.ID); err != nil {
		t.Fatalf("get was blocked: %v", err)
	}
	opened, _, _, err := service.Open(manual.ID)
	if err != nil {
		t.Fatalf("open was blocked: %v", err)
	}
	_ = opened.Close()
	renamed, err := service.Rename(manual.ID, "远端归档备注")
	if err != nil || renamed.Name != "远端归档备注" {
		t.Fatalf("rename result=%#v err=%v", renamed, err)
	}
	uploaded, err := service.Import(context.Background(), roomID, "离线上传", "backup.zip", bytes.NewReader(archiveBytes))
	if err != nil || uploaded.Kind != KindUpload {
		t.Fatalf("upload result=%#v err=%v", uploaded, err)
	}
}

type scheduledBackupProbe struct {
	created chan Kind
	pruned  chan int
}

func (p *scheduledBackupProbe) Create(_ context.Context, _ string, _ string, kind Kind, _ string) (Backup, error) {
	p.created <- kind
	return Backup{Name: "consistent snapshot"}, nil
}
func (p *scheduledBackupProbe) PruneSnapshots(_ context.Context, _ string, keep int) (int64, int, error) {
	p.pruned <- keep
	return 0, 0, nil
}

func TestExistingPoliciesUseConfiguredConsistencyExecutor(t *testing.T) {
	service, runtime, _, _ := newBackupService(t)
	runtime.running["Master"] = true
	roomID := rooms.EncodeID("room")
	if _, err := service.SavePolicy(roomID, PolicyRequest{Enabled: true, IntervalMinute: 15, MaxSnapshots: 2}); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	store := jobs.NewStore(service.store.db, "coordinated_scheduler_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	probe := &scheduledBackupProbe{created: make(chan Kind, 1), pruned: make(chan int, 1)}
	if err := NewScheduler(service, jobService, probe).RunDue(); err != nil {
		t.Fatal(err)
	}
	select {
	case kind := <-probe.created:
		if kind != KindSnapshot {
			t.Fatal(kind)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("consistency executor was not called")
	}
	select {
	case keep := <-probe.pruned:
		if keep != 2 {
			t.Fatal(keep)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retention was not routed")
	}
	// Join the persistent job before the fixture closes its database.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		policy, err := service.Policy(roomID)
		if err != nil {
			t.Fatal(err)
		}
		if policy.LastJobID != "" {
			job, err := jobService.Get(policy.LastJobID)
			if err != nil {
				t.Fatal(err)
			}
			if job.Status == jobs.StatusSucceeded {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("scheduled backup did not finish")
}
