package automation

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/rooms"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type automationTestCatalog struct{}

func (automationTestCatalog) Room(id string) (rooms.Room, error) {
	if id != "room" {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return rooms.Room{ID: id, Name: "Test Room", Managed: true}, nil
}

type automationTestExecutor struct {
	started chan struct{}
	release chan struct{}
	err     error
}

func (e *automationTestExecutor) Validate(Task) error { return nil }
func (e *automationTestExecutor) Execute(ctx context.Context, _ Task, _ string) (ExecutionResult, error) {
	if e.started != nil {
		select {
		case e.started <- struct{}{}:
		default:
		}
	}
	if e.release != nil {
		select {
		case <-e.release:
		case <-ctx.Done():
			return ExecutionResult{}, ctx.Err()
		}
	}
	return ExecutionResult{Message: "action complete"}, e.err
}

func newAutomationTestService(t *testing.T, executor ActionExecutor) (*Service, *Store, *jobs.Service) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "automation_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobStore := jobs.NewStore(db, "automation_test_")
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(automationTestCatalog{}, store, jobService, executor)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	store.now = service.now
	return service, store, jobService
}

func createAutomationFixture(t *testing.T, service *Service) (Group, Task) {
	t.Helper()
	group, err := service.CreateGroup("room", GroupInput{Name: "Daily", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask("room", TaskInput{GroupID: group.ID, Name: "Morning refresh", Enabled: true, Schedule: "0 10 * * *", Timezone: "Asia/Shanghai", Action: ActionWorldStateRefresh, Parameters: map[string]interface{}{}, TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	return group, task
}

func TestTaskLifecycleRunStatsAndRevisionProtection(t *testing.T) {
	service, _, jobService := newAutomationTestService(t, &automationTestExecutor{})
	group, task := createAutomationFixture(t, service)
	if task.NextRunAt == nil {
		t.Fatal("enabled task has no next run")
	}
	if err := service.DeleteGroup("room", group.ID); !errors.Is(err, ErrGroupNotEmpty) {
		t.Fatalf("non-empty group delete error = %v", err)
	}
	if _, err := service.UpdateTask("room", task.ID, TaskInput{GroupID: group.ID, Name: task.Name, Enabled: true, Schedule: task.Schedule, Timezone: task.Timezone, Action: task.Action, Parameters: map[string]interface{}{}, TimeoutSeconds: 30, ExpectedRevision: "00000000-0000-0000-0000-000000000000"}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale update error = %v", err)
	}
	job, err := service.RunTask("room", task.ID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	waitAutomationJob(t, jobService, job.ID)
	runs, err := service.Runs("room", RunFilter{Limit: 25})
	if err != nil || len(runs.Items) != 1 || runs.Items[0].Status != RunSucceeded || runs.Items[0].JobID != job.ID || runs.Items[0].Output != "action complete" {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
	stats, err := service.Stats("room", 7)
	if err != nil || stats.Total != 1 || stats.Succeeded != 1 || stats.SuccessRate != 1 {
		t.Fatalf("stats=%#v err=%v", stats, err)
	}
}

func TestConcurrentRunIsSkippedAndSchedulerRecoversInterruptedRuns(t *testing.T) {
	executor := &automationTestExecutor{started: make(chan struct{}, 1), release: make(chan struct{})}
	service, store, jobService := newAutomationTestService(t, executor)
	_, task := createAutomationFixture(t, service)
	job, err := service.RunTask("room", task.ID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("automation run did not start")
	}
	if _, err := service.RunTask("room", task.ID, TriggerSchedule); !errors.Is(err, ErrTaskRunning) {
		t.Fatalf("overlapping run error = %v", err)
	}
	close(executor.release)
	waitAutomationJob(t, jobService, job.ID)
	runs, err := service.Runs("room", RunFilter{Limit: 25})
	foundSkipped := false
	for _, item := range runs.Items {
		foundSkipped = foundSkipped || item.Status == RunSkipped
	}
	if err != nil || len(runs.Items) != 2 || !foundSkipped {
		t.Fatalf("overlap runs=%#v err=%v", runs, err)
	}
	queued := Run{ID: "queued-run", TaskID: task.ID, TaskName: task.Name, GroupID: task.GroupID, GroupName: task.GroupName, RoomID: task.RoomID, Action: task.Action, Trigger: TriggerSchedule, Status: RunQueued, CreatedAt: service.now()}
	if _, err := store.CreateRun(queued); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, service)
	if err := scheduler.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()
	if scheduler.engine == nil || len(scheduler.engine.Entries()) != 1 {
		t.Fatalf("scheduler entries = %#v", scheduler.engine)
	}
	runs, _ = service.Runs("room", RunFilter{Limit: 25})
	var recovered *Run
	for index := range runs.Items {
		if runs.Items[index].ID == "queued-run" {
			recovered = &runs.Items[index]
		}
	}
	if recovered == nil || recovered.Status != RunFailed || recovered.Error == "" {
		t.Fatalf("interrupted run not recovered: %#v", recovered)
	}
}

func TestImportPreviewDigestAndReplace(t *testing.T) {
	service, _, _ := newAutomationTestService(t, &automationTestExecutor{})
	document := Document{Version: 1, Groups: []DocumentGroup{{Key: "daily", Name: "Daily", Enabled: true}}, Tasks: []DocumentTask{{GroupKey: "daily", Name: "State refresh", Enabled: true, Schedule: "0 10 * * *", Timezone: "Asia/Shanghai", Action: ActionWorldStateRefresh, Parameters: map[string]interface{}{}, TimeoutSeconds: 30}}}
	preview, err := service.PreviewImport("room", document)
	if err != nil || !preview.Valid || preview.Digest == "" {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	if _, err := service.Import("room", ImportRequest{Document: document, Digest: "stale", Replace: true}); !errors.Is(err, ErrImportDigest) {
		t.Fatalf("stale digest error = %v", err)
	}
	result, err := service.Import("room", ImportRequest{Document: document, Digest: preview.Digest, Replace: true})
	if err != nil || result.GroupsCreated != 1 || result.TasksCreated != 1 {
		t.Fatalf("import=%#v err=%v", result, err)
	}
	exported, err := service.Export("room")
	if err != nil || len(exported.Groups) != 1 || len(exported.Tasks) != 1 || exported.Tasks[0].Action != ActionWorldStateRefresh {
		t.Fatalf("export=%#v err=%v", exported, err)
	}
}

func waitAutomationJob(t *testing.T, service *jobs.Service, jobID string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := service.Get(jobID)
		if err == nil && (job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusFailed || job.Status == jobs.StatusCanceled) {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("automation job %q did not finish", jobID)
	return jobs.Job{}
}
