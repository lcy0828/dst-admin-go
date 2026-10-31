package automation

import (
	"context"
	"errors"
	"testing"

	"dont/internal/backups"
	"dont/internal/distributedbackup"
)

func TestBackupSchedulesPersistModeAndPreserveExistingParameters(t *testing.T) {
	s, _, _ := newAutomationTestService(t, &automationTestExecutor{})
	for _, mode := range []string{distributedbackup.ModeAutomatic, distributedbackup.ModeHot, distributedbackup.ModeCold} {
		created, err := s.ConfigureBackupSchedule("room", BackupScheduleInput{CreateNew: true, Name: &mode, Mode: &mode}, nil)
		if err != nil || created.Task.Parameters["mode"] != mode {
			t.Fatalf("mode %s: %+v %v", mode, created, err)
		}
		changed, err := s.ConfigureBackupSchedule("room", BackupScheduleInput{TaskID: created.Task.ID, Name: ptr("renamed " + mode), ExpectedRevision: created.Task.Revision}, nil)
		if err != nil || changed.Task.Parameters["mode"] != mode || changed.Task.Schedule != created.Task.Schedule {
			t.Fatalf("rename changed backup method: %+v %v", changed, err)
		}
	}
	legacy, err := s.ConfigureBackupSchedule("room", BackupScheduleInput{CreateNew: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an older persisted task, including a parameter unknown to this editor.
	legacy.Task.Parameters = map[string]any{"keep": 4, "futureSetting": "preserved"}
	if _, err := s.store.UpdateTask(*legacy.Task, legacy.Task.Revision); err != nil {
		t.Fatal(err)
	}
	changed, err := s.ConfigureBackupSchedule("room", BackupScheduleInput{TaskID: legacy.Task.ID, Name: ptr("older plan")}, nil)
	if err != nil || changed.Task.Parameters["futureSetting"] != "preserved" || changed.Task.Parameters["mode"] != nil {
		t.Fatalf("old plan was rewritten: %+v %v", changed, err)
	}
	changed, err = s.ConfigureBackupSchedule("room", BackupScheduleInput{TaskID: legacy.Task.ID, Mode: ptr(distributedbackup.ModeCold), ExpectedRevision: changed.Task.Revision}, nil)
	if err != nil || changed.Task.Parameters["mode"] != distributedbackup.ModeCold || changed.Task.Parameters["futureSetting"] != "preserved" {
		t.Fatalf("explicit method change lost parameters: %+v %v", changed, err)
	}
}

func TestBackupScheduleRejectsUnknownModeBeforeWriting(t *testing.T) {
	s, _, _ := newAutomationTestService(t, &automationTestExecutor{})
	for _, mode := range []string{"", "hot", "unknown"} {
		if _, err := s.ConfigureBackupSchedule("room", BackupScheduleInput{Mode: &mode}, nil); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("accepted %q: %v", mode, err)
		}
	}
	tasks, _ := s.Tasks("room")
	groups, _ := s.Groups("room")
	if len(tasks) != 0 || len(groups) != 0 {
		t.Fatal("invalid method left persisted data")
	}
}

func TestBackupExecutorRoutesExplicitMethodsWithoutFallback(t *testing.T) {
	for _, mode := range []string{"", distributedbackup.ModeAutomatic, distributedbackup.ModeHot, distributedbackup.ModeCold} {
		remote := &scheduledRemoteBackup{}
		executor := &DomainExecutor{backups: BackupRouter{Distributed: remote}}
		params := map[string]any{}
		if mode != "" {
			params["mode"] = mode
		}
		if _, err := executor.Execute(context.Background(), Task{RoomID: "room", Action: ActionBackupCreate, Parameters: params}, "job"); err != nil {
			t.Fatal(err)
		}
		expected := mode
		if expected == "" {
			expected = distributedbackup.ModeAutomatic
		}
		if remote.calls != 1 || remote.mode != expected {
			t.Fatalf("method changed: %+v", remote)
		}
	}
	remote := &scheduledRemoteBackup{err: distributedbackup.ErrHotUnavailable}
	executor := &DomainExecutor{backups: BackupRouter{Distributed: remote}}
	_, err := executor.Execute(context.Background(), Task{RoomID: "room", Action: ActionBackupCreate, Parameters: map[string]any{"mode": distributedbackup.ModeHot}}, "job")
	if !errors.Is(err, distributedbackup.ErrHotUnavailable) || remote.calls != 1 || remote.mode != distributedbackup.ModeHot {
		t.Fatalf("hot failure fell back: %+v %v", remote, err)
	}
	for _, mode := range []any{nil, true, 1, "", "unsupported"} {
		if err := executor.Validate(Task{Action: ActionBackupCreate, Parameters: map[string]any{"mode": mode}}); err == nil {
			t.Fatalf("accepted mode %#v", mode)
		}
	}
	legacy := &retentionBackupExecutor{}
	executor.backups = legacy
	_, err = executor.Execute(context.Background(), Task{RoomID: "room", Action: ActionBackupCreate, Parameters: map[string]any{"mode": distributedbackup.ModeCold}}, "job")
	if !errors.Is(err, ErrDependencies) || legacy.creates != 0 {
		t.Fatalf("unsupported method silently ignored: %+v %v", legacy, err)
	}
	router := BackupRouter{Distributed: remote}
	if _, err := router.CreateWithMode(context.Background(), "room", "snapshot", backups.KindSnapshot, "job", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("router accepted empty explicit mode", err)
	}
}
