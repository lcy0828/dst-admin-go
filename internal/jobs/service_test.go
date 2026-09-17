package jobs

import (
	"context"
	"dont/shared"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func newTestJobService(t *testing.T) (*Service, *Store) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(store, NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	return service, store
}

func TestPersistsPartialTargetResultsAndReplayableEvents(t *testing.T) {
	service, _ := newTestJobService(t)
	job, err := service.Submit("room.start", "room-1", "", []TargetSpec{{ID: "master", Name: "Master"}, {ID: "caves", Name: "Caves"}}, func(_ context.Context, report func(TargetResult)) error {
		report(TargetResult{TargetID: "master", Status: StatusSucceeded, Message: "已启动"})
		report(TargetResult{TargetID: "caves", Status: StatusFailed, Error: &Error{Code: "START_FAILED", Message: "端口被占用"}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	completed := waitForJob(t, service, job.ID, StatusFailed)
	if completed.Outcome != OutcomePartial || completed.Progress != 100 || completed.Error == nil || completed.Error.Code != "PARTIAL_FAILURE" {
		t.Fatalf("unexpected completed job: %#v", completed)
	}
	if completed.Targets[0].Status != StatusSucceeded || completed.Targets[1].Status != StatusFailed {
		t.Fatalf("target results lost: %#v", completed.Targets)
	}
	events, err := service.EventsAfter(0, 100)
	if err != nil || len(events) < 5 {
		t.Fatalf("events = %#v, %v", events, err)
	}
	last := events[len(events)-1]
	if last.Type != "job.completed" || last.Data.ID != job.ID || last.Data.Outcome != OutcomePartial {
		t.Fatalf("unexpected last event: %#v", last)
	}
}

func TestCancelMarksUnfinishedTargetsAndJob(t *testing.T) {
	service, _ := newTestJobService(t)
	started := make(chan struct{})
	job, err := service.Submit("room.stop", "room-1", "", []TargetSpec{{ID: "master", Name: "Master"}}, func(ctx context.Context, _ func(TargetResult)) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := service.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	completed := waitForJob(t, service, job.ID, StatusCanceled)
	if completed.Outcome != OutcomeNone || completed.Targets[0].Status != StatusCanceled || !completed.CancelRequested {
		t.Fatalf("unexpected canceled job: %#v", completed)
	}
	if _, err := service.Cancel(job.ID); !errors.Is(err, ErrNotCancelable) {
		t.Fatalf("second cancel error = %v", err)
	}
}

func TestAllFailedJobExposesTargetError(t *testing.T) {
	service, _ := newTestJobService(t)
	job, err := service.Submit("mod.install", "room-1", "", []TargetSpec{{ID: "1392778117", Name: "Workshop 1392778117"}}, func(_ context.Context, report func(TargetResult)) error {
		report(TargetResult{TargetID: "1392778117", Status: StatusFailed, Error: &Error{Code: "WORKSHOP_DOWNLOAD_MISSING", Message: "未找到下载后的模组文件"}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	completed := waitForJob(t, service, job.ID, StatusFailed)
	if completed.Error == nil || completed.Error.Code != "WORKSHOP_DOWNLOAD_MISSING" || completed.Error.Message != "未找到下载后的模组文件" {
		t.Fatalf("target failure was not promoted to the job: %#v", completed)
	}
}

func TestSucceededJobPersistsTargetWarning(t *testing.T) {
	service, _ := newTestJobService(t)
	job, err := service.Submit("room.start", "room-1", "", []TargetSpec{{ID: "master", Name: "Master"}}, func(_ context.Context, report func(TargetResult)) error {
		report(TargetResult{
			TargetID: "master", Status: StatusSucceeded, Message: "分片已启动",
			Warning: &Error{Code: "MOD_LOAD_CONFIRMATION_FAILED", Message: "模组 3687959533 未确认加载"},
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	completed := waitForJob(t, service, job.ID, StatusSucceeded)
	if completed.Outcome != OutcomeFull || completed.Targets[0].Warning == nil ||
		completed.Targets[0].Warning.Code != "MOD_LOAD_CONFIRMATION_FAILED" ||
		!strings.Contains(completed.Message, "1 个目标存在警告") {
		t.Fatalf("warning was not persisted: %#v", completed)
	}
	events, err := service.EventsAfter(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Type != "job.completed" || last.Data.Targets[0].Warning == nil {
		t.Fatalf("warning missing from completion event: %#v", last)
	}
}

func TestProgressUpdatesAreVisibleAndDoNotRegressOnTargetCompletion(t *testing.T) {
	service, store := newTestJobService(t)
	job, _, err := store.Create("room.start", "room-1", "", []TargetSpec{{ID: "master", Name: "Master"}, {ID: "caves", Name: "Caves"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.MarkRunning(job.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := service.UpdateProgress(job.ID, 70, "正在启动世界")
	if err != nil || updated.Progress != 70 || updated.Message != "正在启动世界" {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	if _, _, err := store.RecordTarget(job.ID, TargetResult{TargetID: "master", Status: StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	current, err := service.Get(job.ID)
	if err != nil || current.Progress != 70 {
		t.Fatalf("target completion regressed progress: %#v err=%v", current, err)
	}
	stale, err := service.UpdateProgress(job.ID, 60, "过期阶段")
	if err != nil || stale.Progress != 70 || stale.Message != "正在启动世界" {
		t.Fatalf("stale=%#v err=%v", stale, err)
	}
}

func TestProgressTransferSnapshotIsPersistedAndCleared(t *testing.T) {
	service, store := newTestJobService(t)
	job, _, err := store.Create("mod.download", "", "", []TargetSpec{{ID: "1392778117", Name: "Workshop 1392778117"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.MarkRunning(job.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := service.UpdateProgressDetail(job.ID, ProgressUpdate{
		Progress: 37, Message: "正在下载模组", CurrentBytes: 32 << 20, TotalBytes: 92 << 20, BytesPerSecond: 4_500_375,
		Detail: &ProgressDetail{Stage: "mod.cache", WorkshopID: "1392778117", CurrentItem: 2, TotalItems: 3, TargetID: "agent:test", InstallationID: "native"},
	})
	if err != nil || updated.Transfer == nil || updated.Transfer.CurrentBytes != 32<<20 ||
		updated.Transfer.TotalBytes != 92<<20 || updated.Transfer.BytesPerSecond != 4_500_375 {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	persisted, err := store.Get(job.ID)
	if err != nil || persisted.ProgressDetail == nil || !reflect.DeepEqual(persisted.ProgressDetail, updated.ProgressDetail) {
		t.Fatalf("item progress missing after read: %+v err=%v", persisted.ProgressDetail, err)
	}
	events, err := service.EventsAfter(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type == "job.progress" && event.Data.ID == job.ID && event.Data.Transfer != nil && event.Data.Transfer.BytesPerSecond == 4_500_375 {
			if event.Data.ProgressDetail == nil || event.Data.ProgressDetail.CurrentItem != 2 || event.Data.ProgressDetail.TotalItems != 3 {
				t.Fatalf("missing batch item in event: %+v", event.Data)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("transfer snapshot missing from job.progress event: %#v", events)
	}
	cleared, err := service.UpdateProgress(job.ID, 38, "正在校验模组")
	if err != nil || cleared.Transfer != nil || cleared.ProgressDetail != nil {
		t.Fatalf("cleared=%#v err=%v", cleared, err)
	}
}

func TestModDownloadResultsSurvivePhaseChangesAndJobFailure(t *testing.T) {
	service, store := newTestJobService(t)
	job, _, _ := store.Create("mod.update.activate", "room", "", []TargetSpec{{ID: "room", Name: "room"}})
	if _, _, err := store.MarkRunning(job.ID); err != nil {
		t.Fatal(err)
	}
	items := []shared.ModDownloadProgress{{WorkshopID: "111", TargetID: "local", Status: "succeeded"}, {WorkshopID: "222", TargetID: "local", Status: "failed", Message: "I/O Operation Failed"}}
	if _, err := service.UpdateProgressDetail(job.ID, ProgressUpdate{Progress: 50, Message: "下载结束", Detail: &ProgressDetail{Items: items}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateProgress(job.ID, 65, "准备重启"); err != nil {
		t.Fatal(err)
	}
	worlds := []shared.WorldOperationProgress{{WorldID: "master", Name: "Master", Stage: "ready", Percent: 100}, {WorldID: "caves", Name: "Caves", Stage: "loading_world", Percent: 80}}
	if _, err := service.UpdateProgressDetail(job.ID, ProgressUpdate{Progress: 90, Message: "正在重启", Detail: &ProgressDetail{Worlds: worlds}}); err != nil {
		t.Fatal(err)
	}
	worlds[1].Stage, worlds[1].Message = "failed", "port in use"
	// A world may report after another progress source advanced the workflow.
	if _, err := service.UpdateProgressDetail(job.ID, ProgressUpdate{Progress: 89, Message: "部分失败", Detail: &ProgressDetail{Worlds: worlds}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateProgress(job.ID, 95, "正在结束任务"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Complete(job.ID, errors.New("restart failed"), false); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get(job.ID)
	if err != nil || loaded.ProgressDetail == nil || !reflect.DeepEqual(loaded.ProgressDetail.Items, items) || !reflect.DeepEqual(loaded.ProgressDetail.Worlds, worlds) {
		t.Fatalf("lost item results: %+v %v", loaded, err)
	}
}

func TestRecoversQueuedAndRunningJobsAfterRestart(t *testing.T) {
	_, store := newTestJobService(t)
	job, _, err := store.Create("server.update", "", "", []TargetSpec{{ID: "local", Name: "当前节点"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.MarkRunning(job.ID); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(store, NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := service.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != StatusFailed || recovered.Error == nil || recovered.Error.Code != "SERVER_RESTARTED" || recovered.Targets[0].Status != StatusFailed {
		t.Fatalf("unexpected recovered job: %#v", recovered)
	}
}

func TestRetentionPrunesTerminalJobsAndEventsButKeepsRunningJobs(t *testing.T) {
	service, store := newTestJobService(t)
	base := time.Unix(1_786_500_000, 0).UTC()
	store.now = func() time.Time { return base.Add(-100 * 24 * time.Hour) }
	old, _, err := store.Create("backup.create", "room-1", "", []TargetSpec{{ID: "room-1", Name: "旧备份"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.MarkRunning(old.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RecordTarget(old.ID, TargetResult{TargetID: "room-1", Status: StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Complete(old.ID, nil, false); err != nil {
		t.Fatal(err)
	}

	store.now = func() time.Time { return base }
	kept, _, err := store.Create("map.generate", "room-1", "", []TargetSpec{{ID: "master", Name: "运行中地图"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.MarkRunning(kept.ID); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		job, _, createErr := store.Create("system.refresh", "", "", []TargetSpec{{ID: fmt.Sprintf("target-%d", index), Name: "刷新"}})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, _, createErr = store.MarkRunning(job.ID); createErr != nil {
			t.Fatal(createErr)
		}
		if _, _, createErr = store.RecordTarget(job.ID, TargetResult{TargetID: fmt.Sprintf("target-%d", index), Status: StatusSucceeded}); createErr != nil {
			t.Fatal(createErr)
		}
		if _, _, createErr = store.Complete(job.ID, nil, false); createErr != nil {
			t.Fatal(createErr)
		}
	}

	result, err := service.Prune(RetentionPolicy{EventMaxAge: 7 * 24 * time.Hour, EventLimit: 6, JobMaxAge: 90 * 24 * time.Hour, JobLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.JobsDeleted != 3 || result.TargetsDeleted != 3 || result.EventsDeleted == 0 {
		t.Fatalf("retention result = %#v", result)
	}
	if _, err := service.Get(old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old job error = %v", err)
	}
	if running, err := service.Get(kept.ID); err != nil || running.Status != StatusRunning {
		t.Fatalf("running job = %#v, error = %v", running, err)
	}
	events, err := service.EventsAfter(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) > 6 {
		t.Fatalf("retained event count = %d, want at most 6", len(events))
	}
	for _, event := range events {
		if event.JobID == old.ID {
			t.Fatal("old job event survived retention")
		}
	}
}

func waitForJob(t *testing.T, service *Service, jobID string, status Status) Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := service.Get(jobID)
		if err == nil && job.Status == status {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, err := service.Get(jobID)
	t.Fatalf("job did not reach %s: %#v, %v", status, job, err)
	return Job{}
}
