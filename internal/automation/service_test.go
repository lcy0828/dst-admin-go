package automation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/rooms"

	"github.com/google/uuid"
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

func (automationTestCatalog) List() ([]rooms.Room, error) {
	return []rooms.Room{{ID: "room", DirectoryName: "survival", Name: "Test Room", Managed: true}}, nil
}

func (automationTestCatalog) Worlds(roomID string) ([]rooms.World, error) {
	if roomID != "room" {
		return nil, rooms.ErrRoomNotFound
	}
	return []rooms.World{{ID: "Master", RoomID: roomID, DirectoryName: "Master", Name: "Master"}}, nil
}

type automationTestExecutor struct {
	started  chan struct{}
	release  chan struct{}
	err      error
	failures int
	attempts int
}

func (e *automationTestExecutor) Validate(Task) error { return nil }
func (e *automationTestExecutor) Execute(ctx context.Context, _ Task, _ string) (ExecutionResult, error) {
	e.attempts++
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
	if e.failures > 0 {
		e.failures--
		return ExecutionResult{}, errors.New("temporary failure")
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

func TestMigrateLegacyAutomationTablesPreservesData(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	legacySchema := []string{
		`CREATE TABLE legacy_automation_group (id char(36) PRIMARY KEY, room_id varchar(255) NOT NULL, name varchar(80) NOT NULL, description varchar(300) NOT NULL, enabled bool NOT NULL, revision char(36) NOT NULL, created_at datetime, updated_at datetime)`,
		`CREATE TABLE legacy_automation_task (id char(36) PRIMARY KEY, room_id varchar(255) NOT NULL, group_id char(36) NOT NULL, name varchar(80) NOT NULL, description varchar(300) NOT NULL, enabled bool NOT NULL, schedule varchar(128) NOT NULL, timezone varchar(64) NOT NULL, action varchar(64) NOT NULL, world_ids text NOT NULL, parameters text NOT NULL, timeout_seconds integer NOT NULL, last_run_at datetime, last_status varchar(20), last_job_id char(36), revision char(36) NOT NULL, created_at datetime, updated_at datetime)`,
		`CREATE TABLE legacy_automation_run (id char(36) PRIMARY KEY, task_id char(36) NOT NULL, task_name varchar(80) NOT NULL, group_id char(36) NOT NULL, group_name varchar(80) NOT NULL, room_id varchar(255) NOT NULL, action varchar(64) NOT NULL, trigger varchar(20) NOT NULL, status varchar(20) NOT NULL, job_id char(36), output text, error text, started_at datetime, finished_at datetime, duration_ms bigint, created_at datetime NOT NULL)`,
	}
	for _, statement := range legacySchema {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	if err := db.Exec(`INSERT INTO legacy_automation_group (id, room_id, name, description, enabled, revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, "group", "room", "Legacy", "kept", true, "group-revision", now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO legacy_automation_task (id, room_id, group_id, name, description, enabled, schedule, timezone, action, world_ids, parameters, timeout_seconds, revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, "task", "room", "group", "Legacy task", "kept", true, "0 10 * * *", "Asia/Shanghai", string(ActionWorldStateRefresh), "[]", "{}", 30, "task-revision", now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO legacy_automation_run (id, task_id, task_name, group_id, group_name, room_id, action, trigger, status, duration_ms, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, "run", "task", "Legacy task", "group", "Legacy", "room", string(ActionWorldStateRefresh), string(TriggerManual), string(RunSucceeded), 25, now).Error; err != nil {
		t.Fatal(err)
	}

	store := NewStore(db, "legacy_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	group, err := store.Group("room", "group")
	if err != nil || group.Type != "custom" || group.Description != "kept" {
		t.Fatalf("migrated group=%#v err=%v", group, err)
	}
	task, err := store.Task("room", "task")
	if err != nil || task.RetryTimes != 0 || task.RetryInterval != 60 || len(task.Dependencies) != 0 || task.Description != "kept" {
		t.Fatalf("migrated task=%#v err=%v", task, err)
	}
	run, err := store.Run("room", "run")
	if err != nil || run.RetryCount != 0 || run.DurationMs != 25 {
		t.Fatalf("migrated run=%#v err=%v", run, err)
	}
}

func TestMigrateLegacyCronTasksOnlyImportsResolvableTargets(t *testing.T) {
	service, store, _ := newAutomationTestService(t, &automationTestExecutor{})
	if err := store.db.Exec(`CREATE TABLE legacy_cron_task (
		id integer primary key, name varchar(255), description varchar(255), spec varchar(255),
		type varchar(255), target varchar(255), args varchar(255), dependencies varchar(255),
		timeout integer, retry_times integer, retry_interval integer, status integer
	)`).Error; err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		id     int
		name   string
		target string
		args   string
	}{
		{1, "refresh state", "read_world_state", `["survival","Master"]`},
		{2, "missing target", "read_player_config", `["missing","Master"]`},
		{3, "unsupported", "cleanupLogs", `[30]`},
	}
	for _, row := range rows {
		if err := store.db.Exec(`INSERT INTO legacy_cron_task
			(id, name, description, spec, type, target, args, dependencies, timeout, retry_times, retry_interval, status)
			VALUES (?, ?, '', '*/30 * * * * *', 'function', ?, ?, '', 0, 0, 0, 1)`,
			row.id, row.name, row.target, row.args).Error; err != nil {
			t.Fatal(err)
		}
	}

	report, err := MigrateLegacyCronTasks(store.db, "legacy_", automationTestCatalog{}, service)
	if err != nil {
		t.Fatal(err)
	}
	if report.Examined != 3 || report.Migrated != 1 || report.Existing != 0 || len(report.Skipped) != 2 {
		t.Fatalf("unexpected migration report: %+v", report)
	}
	tasks, err := service.Tasks("room")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Action != ActionWorldStateRefresh || len(tasks[0].WorldIDs) != 1 || tasks[0].WorldIDs[0] != "Master" {
		t.Fatalf("unexpected migrated tasks: %+v", tasks)
	}

	repeated, err := MigrateLegacyCronTasks(store.db, "legacy_", automationTestCatalog{}, service)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Migrated != 0 || repeated.Existing != 1 || len(repeated.Skipped) != 2 {
		t.Fatalf("migration is not idempotent: %+v", repeated)
	}
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

func TestTaskCollectionsRemainJSONCollectionsWhenEmpty(t *testing.T) {
	service, _, _ := newAutomationTestService(t, &automationTestExecutor{})
	group, err := service.CreateGroup("room", GroupInput{Name: "Daily", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask("room", TaskInput{
		GroupID: group.ID, Name: "Refresh", Enabled: true, Schedule: "0 10 * * *",
		Timezone: "Asia/Shanghai", Action: ActionWorldStateRefresh, TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.WorldIDs == nil || task.Dependencies == nil || task.Parameters == nil {
		t.Fatalf("empty collections must remain non-nil: %#v", task)
	}
	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, expected := range []string{`"worldIds":[]`, `"parameters":{}`, `"dependencies":[]`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("task JSON %s does not contain %s", text, expected)
		}
	}
}

func TestEnsureDefaultPlayerRefreshIsIdempotent(t *testing.T) {
	service, _, _ := newAutomationTestService(t, &automationTestExecutor{})
	created, wasCreated, err := service.EnsureDefaultPlayerRefresh("room")
	if err != nil || !wasCreated {
		t.Fatalf("create default refresh: task=%#v created=%v err=%v", created, wasCreated, err)
	}
	if created.Action != ActionPlayerRefresh || created.Schedule != defaultPlayerRefreshSchedule || len(created.WorldIDs) != 0 || !created.Enabled {
		t.Fatalf("unexpected default task: %#v", created)
	}
	repeated, wasCreated, err := service.EnsureDefaultPlayerRefresh("room")
	if err != nil || wasCreated || repeated.ID != created.ID {
		t.Fatalf("default refresh is not idempotent: first=%#v repeated=%#v created=%v err=%v", created, repeated, wasCreated, err)
	}
	groups, err := service.Groups("room")
	if err != nil || len(groups) != 1 || groups[0].Name != playerManagementGroupName || groups[0].Type != "system" {
		t.Fatalf("unexpected default group: groups=%#v err=%v", groups, err)
	}
}

func TestEnsureDefaultPlayerRefreshReactivatesBuiltInTaskAndGroup(t *testing.T) {
	service, store, _ := newAutomationTestService(t, &automationTestExecutor{})
	task, _, err := service.EnsureDefaultPlayerRefresh("room")
	if err != nil {
		t.Fatal(err)
	}
	group, err := store.Group("room", task.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	task, err = service.UpdateTask("room", task.ID, TaskInput{
		GroupID: task.GroupID, Name: task.Name, Description: task.Description, Enabled: false,
		Schedule: task.Schedule, Timezone: task.Timezone, Action: task.Action, WorldIDs: task.WorldIDs,
		Parameters: task.Parameters, TimeoutSeconds: task.TimeoutSeconds, RetryTimes: task.RetryTimes,
		RetryInterval: task.RetryInterval, Dependencies: task.Dependencies, ExpectedRevision: task.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateGroup("room", group.ID, GroupInput{
		Name: group.Name, Description: group.Description, Type: group.Type, Enabled: false, ExpectedRevision: group.Revision,
	}); err != nil {
		t.Fatal(err)
	}

	restored, changed, err := service.EnsureDefaultPlayerRefresh("room")
	if err != nil || !changed || !restored.Enabled {
		t.Fatalf("restore default refresh: task=%#v changed=%v err=%v", restored, changed, err)
	}
	restoredGroup, err := store.Group("room", restored.GroupID)
	if err != nil || !restoredGroup.Enabled {
		t.Fatalf("default group was not restored: group=%#v err=%v", restoredGroup, err)
	}
	scheduled, err := store.ScheduledTasks()
	if err != nil || len(scheduled) != 1 || scheduled[0].ID != restored.ID {
		t.Fatalf("restored default is not schedulable: tasks=%#v err=%v", scheduled, err)
	}
}

func TestEnsureDefaultPlayerRefreshDoesNotEnableDisabledCustomGroup(t *testing.T) {
	service, store, _ := newAutomationTestService(t, &automationTestExecutor{})
	custom, err := service.CreateGroup("room", GroupInput{Name: playerManagementGroupName, Type: "custom", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	task, changed, err := service.EnsureDefaultPlayerRefresh("room")
	if err != nil || !changed || task.GroupID == custom.ID {
		t.Fatalf("default reused disabled custom group: task=%#v changed=%v err=%v", task, changed, err)
	}
	custom, err = store.Group("room", custom.ID)
	if err != nil || custom.Enabled {
		t.Fatalf("disabled custom group was modified: group=%#v err=%v", custom, err)
	}
	systemGroup, err := store.Group("room", task.GroupID)
	if err != nil || !systemGroup.Enabled || systemGroup.Type != "system" || systemGroup.Name != playerRefreshSystemGroupName {
		t.Fatalf("unexpected fallback group: group=%#v err=%v", systemGroup, err)
	}
}

func TestEnsureDefaultPlayerRefreshReplacesMigratedLegacyRefresh(t *testing.T) {
	service, store, _ := newAutomationTestService(t, &automationTestExecutor{})
	group, err := service.CreateGroup("room", GroupInput{Name: legacyMigrationGroupName, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := service.CreateTask("room", TaskInput{
		GroupID: group.ID, Name: "Legacy player refresh", Description: "由旧版任务 #52 迁移", Enabled: true,
		Schedule: "*/30 * * * * *", Timezone: "Asia/Shanghai", Action: ActionPlayerRefresh,
		WorldIDs: []string{"Master"}, Parameters: map[string]interface{}{}, TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	current, changed, err := service.EnsureDefaultPlayerRefresh("room")
	if err != nil || !changed || current.ID == legacy.ID || len(current.WorldIDs) != 0 {
		t.Fatalf("legacy refresh was not replaced: task=%#v changed=%v err=%v", current, changed, err)
	}
	legacy, err = store.Task("room", legacy.ID)
	if err != nil || legacy.Enabled {
		t.Fatalf("legacy refresh remains enabled: task=%#v err=%v", legacy, err)
	}
	scheduled, err := store.ScheduledTasks()
	if err != nil || len(scheduled) != 1 || scheduled[0].ID != current.ID {
		t.Fatalf("unexpected scheduled refreshes: tasks=%#v err=%v", scheduled, err)
	}
}

func TestDeleteRoomTasksRemovesSchedulesAndGroups(t *testing.T) {
	service, store, _ := newAutomationTestService(t, &automationTestExecutor{})
	if _, _, err := service.EnsureDefaultPlayerRefresh("room"); err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteRoomTasks("room"); err != nil {
		t.Fatal(err)
	}
	tasks, taskErr := store.Tasks("room")
	groups, groupErr := store.Groups("room")
	if taskErr != nil || groupErr != nil || len(tasks) != 0 || len(groups) != 0 {
		t.Fatalf("room automation was not removed: tasks=%#v groups=%#v taskErr=%v groupErr=%v", tasks, groups, taskErr, groupErr)
	}
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

func TestSixFieldScheduleDependenciesRetryAndRunCleanup(t *testing.T) {
	executor := &automationTestExecutor{}
	service, store, jobService := newAutomationTestService(t, executor)
	group, dependency := createAutomationFixture(t, service)
	task, err := service.CreateTask("room", TaskInput{
		GroupID: group.ID, Name: "Dependent task", Enabled: true,
		Schedule: "0 0 10 * * *", Timezone: "Asia/Shanghai",
		Action: ActionWorldStateRefresh, Parameters: map[string]interface{}{},
		TimeoutSeconds: 30, RetryTimes: 1, RetryInterval: 1,
		Dependencies: []string{dependency.ID},
	})
	if err != nil || task.NextRunAt == nil {
		t.Fatalf("six-field task=%#v err=%v", task, err)
	}
	if _, err := service.RunTask("room", task.ID, TriggerManual); !errors.Is(err, ErrDependencies) {
		t.Fatalf("unsatisfied dependency error=%v", err)
	}
	job, err := service.RunTask("room", dependency.ID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	waitAutomationJob(t, jobService, job.ID)
	executor.failures = 1
	job, err = service.RunTask("room", task.ID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	waitAutomationJob(t, jobService, job.ID)
	runs, err := service.Runs("room", RunFilter{TaskID: task.ID, Limit: 25})
	var successful *Run
	for index := range runs.Items {
		if runs.Items[index].Status == RunSucceeded {
			successful = &runs.Items[index]
		}
	}
	if err != nil || len(runs.Items) != 2 || successful == nil || successful.RetryCount != 1 {
		t.Fatalf("dependent runs=%#v err=%v", runs, err)
	}
	detail, err := service.Run("room", successful.ID)
	if err != nil || detail.ID != successful.ID {
		t.Fatalf("run detail=%#v err=%v", detail, err)
	}
	old := service.now().AddDate(0, 0, -40)
	if _, err := store.CreateRun(Run{ID: uuid.NewString(), TaskID: task.ID, TaskName: task.Name, GroupID: group.ID, GroupName: group.Name, RoomID: "room", Action: task.Action, Trigger: TriggerManual, Status: RunFailed, CreatedAt: old}); err != nil {
		t.Fatal(err)
	}
	cleared, err := service.ClearRuns("room", ClearRunsInput{KeepDays: 30, TaskID: task.ID, Status: RunFailed})
	if err != nil || cleared.DeletedCount != 1 {
		t.Fatalf("clear=%#v err=%v", cleared, err)
	}
	update := TaskInput{GroupID: group.ID, Name: task.Name, Enabled: true, Schedule: task.Schedule, Timezone: task.Timezone, Action: task.Action, Parameters: map[string]interface{}{}, TimeoutSeconds: 30, RetryInterval: 60, Dependencies: []string{task.ID}, ExpectedRevision: task.Revision}
	if _, err := service.UpdateTask("room", task.ID, update); err == nil {
		t.Fatal("self dependency was accepted")
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
