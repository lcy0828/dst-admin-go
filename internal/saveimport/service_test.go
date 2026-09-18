package saveimport

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/shirou/gopsutil/v3/disk"
)

func TestServiceRecoversAbandonedMultipartFilesOnInitialization(t *testing.T) {
	app := newApplyTestApp(t)
	dir := filepath.Join(app.importRoot, ".uploads")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "upload-interrupted")
	if err := os.WriteFile(path, []byte("partial upload"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(Config{SaveRoot: app.saveRoot, ImportRoot: app.importRoot, WorkshopRoot: app.workshopRoot}, app.store, app.rooms, app.runtime, app.backups, app.downloader); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("abandoned multipart file remains: %v", err)
	}
}

func TestServiceRecoversInterruptedAnalysisAsRetryable(t *testing.T) {
	app := newApplyTestApp(t)
	archive := createZIP(t, []archiveTestEntry{
		{name: "cluster.ini", content: clusterINI("Interrupted")},
		{name: "Master/server.ini", content: serverINI(true, 1, 10999)},
	})
	value, err := app.service.Upload(context.Background(), "中断分析", "source.zip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.MarkAnalyzing(value.ID); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(app.importRoot, value.ID, ".analyze-interrupted")
	if err := os.MkdirAll(staging, 0750); err != nil {
		t.Fatal(err)
	}

	if _, err := NewService(Config{SaveRoot: app.saveRoot, ImportRoot: app.importRoot, WorkshopRoot: app.workshopRoot}, app.store, app.rooms, app.runtime, app.backups, app.downloader); err != nil {
		t.Fatal(err)
	}
	recovered, err := app.store.Get(value.ID)
	if err != nil || recovered.Status != StatusUploaded || recovered.ErrorCode != "SERVER_RESTARTED" {
		t.Fatalf("recovered import = %#v, %v", recovered, err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("analysis staging was not removed: %v", err)
	}
}

func TestReserveRejectsOverlappingImportOperations(t *testing.T) {
	app := newApplyTestApp(t)
	release, err := app.service.Reserve("import-one", "analyze")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.service.Reserve("import-one", "apply"); !errors.Is(err, ErrImportBusy) {
		t.Fatalf("error = %v, want busy", err)
	}
	release()
	secondRelease, err := app.service.Reserve("import-one", "apply")
	if err != nil {
		t.Fatal(err)
	}
	secondRelease()
}

func TestReserveApplyRejectsOverlappingTargetRoom(t *testing.T) {
	app := newApplyTestApp(t)
	release, err := app.service.ReserveApply("import-one", "room-one")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := app.service.ReserveApply("import-two", "room-one"); !errors.Is(err, ErrImportBusy) {
		t.Fatalf("error = %v, want busy target", err)
	}
	otherRelease, err := app.service.ReserveApply("import-two", "room-two")
	if err != nil {
		t.Fatal(err)
	}
	otherRelease()
}

func TestDeleteRejectsActiveImportOperation(t *testing.T) {
	app := newApplyTestApp(t)
	archive := createZIP(t, []archiveTestEntry{
		{name: "cluster.ini", content: clusterINI("Busy")},
		{name: "Master/server.ini", content: serverINI(true, 1, 10999)},
	})
	value, err := app.service.Upload(context.Background(), "处理中", "source.zip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	release, err := app.service.Reserve(value.ID, "analyze")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := app.service.Delete(value.ID); !errors.Is(err, ErrImportBusy) {
		t.Fatalf("delete error = %v, want busy", err)
	}
	if _, err := app.service.Get(value.ID); err != nil {
		t.Fatalf("active import was deleted: %v", err)
	}
}

func TestUploadSpaceCheckKeepsSafetyHeadroom(t *testing.T) {
	app := newApplyTestApp(t)
	original := inspectDiskUsage
	inspectDiskUsage = func(string) (*disk.UsageStat, error) {
		return &disk.UsageStat{Free: minimumImportHeadroom + 1024}, nil
	}
	t.Cleanup(func() { inspectDiskUsage = original })
	if err := app.service.CheckUploadSpace(2048, false); !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("error = %v, want insufficient space", err)
	}
}
