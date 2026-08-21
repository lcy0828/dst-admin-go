package automation

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type automationNotificationSender struct {
	roomID, message, jobID string
	success, failure, skip int
	err                    error
}

func (s *automationNotificationSender) SendAutomation(_ context.Context, roomID, message, jobID string) (int, int, int, error) {
	s.roomID, s.message, s.jobID = roomID, message, jobID
	return s.success, s.failure, s.skip, s.err
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
	if err != nil || sender.roomID != "room" || sender.message != "十分钟后维护" || sender.jobID != "job-notification" || !strings.Contains(result.Message, "成功 1") {
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
