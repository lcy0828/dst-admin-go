package automation

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"dont/internal/gamenotifications"
	"dont/internal/jobs"
)

type automationNotificationSender struct {
	roomID, message, jobID string
	success, failure, skip int
	err                    error
	calls                  atomic.Int32
}

func (s *automationNotificationSender) SendAutomation(_ context.Context, roomID, message, jobID string) (int, int, int, error) {
	s.calls.Add(1)
	s.roomID, s.message, s.jobID = roomID, message, jobID
	return s.success, s.failure, s.skip, s.err
}

func TestNotificationAutomationNeverReplaysAfterExecutionError(t *testing.T) {
	for _, sendErr := range []error{gamenotifications.ErrOutcomeUnconfirmed, errors.New("could not persist delivery result")} {
		t.Run(sendErr.Error(), func(t *testing.T) {
			sender := &automationNotificationSender{failure: 1, err: sendErr}
			executor := &DomainExecutor{}
			if err := executor.ConfigureNotifications(sender); err != nil {
				t.Fatal(err)
			}
			service, _, jobService := newAutomationTestService(t, executor)
			group, err := service.CreateGroup("room", GroupInput{Name: "Announcements", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			task, err := service.CreateTask("room", TaskInput{
				GroupID: group.ID, Name: "Reminder", Enabled: true, Schedule: "0 10 * * *", Timezone: "Asia/Shanghai",
				Action: ActionNotificationSend, Parameters: map[string]interface{}{"message": "Maintenance reminder"},
				TimeoutSeconds: 30, RetryTimes: 3, RetryInterval: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			job, err := service.RunTask("room", task.ID, TriggerSchedule)
			if err != nil {
				t.Fatal(err)
			}
			completed := waitAutomationJob(t, jobService, job.ID)
			runs, err := service.Runs("room", RunFilter{TaskID: task.ID, Limit: 25})
			if err != nil || completed.Status != jobs.StatusFailed || sender.calls.Load() != 1 || len(runs.Items) != 1 ||
				runs.Items[0].Status != RunFailed || runs.Items[0].RetryCount != 0 || runs.Items[0].Error != sendErr.Error() {
				t.Fatalf("job=%+v runs=%+v calls=%d err=%v", completed, runs, sender.calls.Load(), err)
			}
		})
	}
}

func TestDomainExecutorSendsAutomationNotificationsThroughConfiguredService(t *testing.T) {
	sender := &automationNotificationSender{success: 1, skip: 1}
	executor := &DomainExecutor{}
	if err := executor.ConfigureNotifications(sender); err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), Task{
		RoomID: "room", Action: ActionNotificationSend, Parameters: map[string]interface{}{"message": "  十分钟后维护  "},
	}, "job-notification")
	if err != nil || sender.roomID != "room" || sender.message != "十分钟后维护" || sender.jobID != "job-notification" || !strings.Contains(result.Message, "已提交 1") {
		t.Fatalf("sender=%#v result=%#v err=%v", sender, result, err)
	}
}

func TestDomainExecutorRejectsInvalidOrFailedAutomationNotifications(t *testing.T) {
	executor := &DomainExecutor{}
	invalid := Task{RoomID: "room", Action: ActionNotificationSend, Parameters: map[string]interface{}{"message": ""}}
	if err := executor.Validate(invalid); err == nil {
		t.Fatal("empty notification was accepted")
	}
	sender := &automationNotificationSender{failure: 1, err: errors.New("console unavailable")}
	if err := executor.ConfigureNotifications(sender); err != nil {
		t.Fatal(err)
	}
	_, err := executor.Execute(context.Background(), Task{
		RoomID: "room", Action: ActionNotificationSend, Parameters: map[string]interface{}{"message": "维护"},
	}, "job-failed")
	if err == nil || !strings.Contains(err.Error(), "console unavailable") {
		t.Fatalf("error=%v", err)
	}
}
