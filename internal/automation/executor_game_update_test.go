package automation

import (
	"context"
	"errors"
	"testing"

	"dont/internal/gameupdate"
	"dont/internal/maintenance"
)

type automaticGameUpdater struct {
	updated   bool
	err       error
	room, job string
}

func (u *automaticGameUpdater) UpdateRoomWhenEmpty(_ context.Context, room, job string) (bool, error) {
	u.room, u.job = room, job
	return u.updated, u.err
}

func TestGameMaintenanceReportsSkippedConditions(t *testing.T) {
	for _, test := range []struct {
		name             string
		updated, skipped bool
		err              error
	}{
		{"current", false, true, nil}, {"updated", true, false, nil},
		{"players", false, true, maintenance.ErrPlayersOnline},
		{"unknown", false, true, maintenance.ErrPresenceUnavailable},
		{"preflight", false, true, gameupdate.ErrReleasePreviewBlocked},
		{"recovery", false, false, gameupdate.ErrReleaseRecoveryNeeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			updater := &automaticGameUpdater{updated: test.updated, err: test.err}
			executor := &DomainExecutor{}
			executor.ConfigureGameUpdates(updater)
			result, err := executor.ExecuteScheduled(context.Background(), Task{Action: ActionGameUpdateEmpty, RoomID: "room"}, "job")
			if result.Skipped != test.skipped || updater.room != "room" || updater.job != "job" {
				t.Fatalf("result=%+v err=%v updater=%+v", result, err, updater)
			}
			if test.skipped && err != nil || !test.skipped && !errors.Is(err, test.err) {
				t.Fatal(err)
			}
		})
	}
}

func TestGameMaintenanceRejectsPartialScopeAndImmediateRetry(t *testing.T) {
	executor := &DomainExecutor{}
	for _, task := range []Task{
		{Action: ActionGameUpdateEmpty, WorldIDs: []string{"Master"}},
		{Action: ActionGameUpdateEmpty, RetryTimes: 1},
		{Action: ActionGameUpdateEmpty, Parameters: map[string]interface{}{"force": true}},
	} {
		if err := executor.Validate(task); err == nil {
			t.Fatalf("accepted unsafe automatic task: %+v", task)
		}
	}
}

func TestGameMaintenancePersistsSkipWithoutFailureOrRetry(t *testing.T) {
	executor := &DomainExecutor{}
	executor.ConfigureGameUpdates(&automaticGameUpdater{err: maintenance.ErrPlayersOnline})
	service, store, jobService := newAutomationTestService(t, executor)
	group, err := service.CreateGroup("room", GroupInput{Name: "Maintenance", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask("room", TaskInput{GroupID: group.ID, Name: "Update", Enabled: true,
		Schedule: "*/15 * * * *", Action: ActionGameUpdateEmpty, TimeoutSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.RunTask("room", task.ID, TriggerSchedule)
	if err != nil {
		t.Fatal(err)
	}
	waitAutomationJob(t, jobService, job.ID)
	runs, err := service.Runs("room", RunFilter{Limit: 25})
	if err != nil || len(runs.Items) != 1 || runs.Items[0].Status != RunSkipped || runs.Items[0].Output == "" || runs.Items[0].RetryCount != 0 {
		t.Fatalf("skip persisted as failure or retry: %+v %v", runs, err)
	}
	saved, err := store.Task("room", task.ID)
	if err != nil || saved.LastStatus != RunSkipped {
		t.Fatalf("last result lost skip: %+v %v", saved, err)
	}
}
