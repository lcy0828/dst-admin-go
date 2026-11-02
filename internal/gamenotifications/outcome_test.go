package gamenotifications

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/shared"
)

type notificationCommanderFunc func(context.Context, string, string, dstruntime.CommandRequest) (dstruntime.CommandReceipt, error)

func (f notificationCommanderFunc) ExecuteCommand(ctx context.Context, roomID, worldID string, request dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
	return f(ctx, roomID, worldID, request)
}

func TestAnnouncementConfirmsOneActualExecutionOnLocalAndAgent(t *testing.T) {
	for _, target := range []string{"local", "agent:test-node"} {
		t.Run(target, func(t *testing.T) {
			service, store, runtime, catalog := newNotificationTestService(t)
			master, caves := catalog.worlds[0], catalog.worlds[1]
			// An unrelated world must not turn a confirmed room broadcast into a
			// partial failure, and does not need another Agent status call.
			runtime.statusErrs[caves.ID] = errors.New("unrelated node is offline")
			calls := 0
			service.commander = notificationCommanderFunc(func(ctx context.Context, roomID, worldID string, request dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > broadcastTimeout || roomID != catalog.room.ID || worldID != master.ID || request.Action != "console.execute" ||
					request.Arguments["script"] != `c_announce("hello\"\nworld")` {
					t.Fatalf("wrong bounded command: %s/%s %+v", roomID, worldID, request)
				}
				return dstruntime.CommandReceipt{RequestID: request.RequestID, Action: request.Action, OK: true,
					CompletedAt: service.now().UTC(), TargetID: target, TopologyRevision: "test-revision"}, nil
			})
			plan, err := service.Prepare(catalog.room.ID, "hello\"\nworld", SourceManual)
			if err != nil {
				t.Fatal(err)
			}
			if err := plan.Runner("test-job")(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			value, err := store.Get(plan.Notification.ID)
			if err != nil || value.Outcome != OutcomeConfirmed || value.ConfirmedCount != 1 || value.SubmittedCount != 0 || value.FailureCount != 0 ||
				len(runtime.statusCalls) != 1 || len(runtime.commands) != 0 || calls != 1 {
				t.Fatalf("broadcast result=%+v err=%v calls=%d status=%v", value, err, calls, runtime.statusCalls)
			}
			if value.Deliveries[0].TargetID != target || value.Deliveries[0].TopologyRevision != "test-revision" ||
				value.Deliveries[1].SkipReason != SkipRoomBroadcast || value.Deliveries[1].BroadcastWorldID != master.ID || value.Deliveries[1].SentAt != nil {
				t.Fatalf("lost dispatch or skip evidence: %+v", value.Deliveries)
			}
			if err := plan.Runner("test-job")(context.Background(), nil); err == nil || calls != 1 {
				t.Fatal("completed announcement was executed again")
			}
		})
	}
}

func TestAnnouncementRejectsMissingInvalidOrFailedReceiptsWithoutReplay(t *testing.T) {
	for _, scenario := range []string{"timeout", "missing", "wrong-id", "wrong-action", "missing-time", "stale", "future", "rejected", "unavailable", "before-send-canceled"} {
		t.Run(scenario, func(t *testing.T) {
			service, _, runtime, catalog := newNotificationTestService(t)
			runtime.statuses[catalog.worlds[1].ID] = shared.ShardRuntimeStatus{State: "running", SessionExists: true}
			calls := 0
			service.commander = notificationCommanderFunc(func(_ context.Context, _, _ string, request dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
				calls++
				receipt := dstruntime.CommandReceipt{RequestID: request.RequestID, Action: request.Action, OK: true, CompletedAt: service.now().UTC()}
				switch scenario {
				case "timeout":
					return receipt, fmt.Errorf("%w: %w", dstruntime.ErrRuntimeCommandUnconfirmed, context.DeadlineExceeded)
				case "missing":
					return receipt, dstruntime.ErrRuntimeResultAbsent
				case "wrong-id":
					receipt.RequestID = "unrelated-request"
				case "wrong-action":
					receipt.Action = "system.ping"
				case "missing-time":
					receipt.CompletedAt = time.Time{}
				case "stale":
					receipt.CompletedAt = service.now().Add(-time.Hour)
				case "future":
					receipt.CompletedAt = service.now().Add(time.Hour)
				case "rejected":
					receipt.OK, receipt.Code, receipt.Message = false, "COMMAND_EXECUTION_FAILED", "game rejected command"
				case "unavailable":
					return receipt, dstruntime.ErrRuntimeUnavailable
				case "before-send-canceled":
					return receipt, context.Canceled
				}
				return receipt, nil
			})
			value, err := service.sendDirect(context.Background(), catalog.room.ID, "test announcement", SourceManual, "")
			want := OutcomeUnconfirmed
			if scenario == "rejected" || scenario == "unavailable" || scenario == "before-send-canceled" {
				want = OutcomeFailed
			}
			if err != nil || value.Outcome != want || calls != 1 || len(runtime.commands) != 0 || len(runtime.statusCalls) != 1 ||
				value.Deliveries[1].SkipReason != SkipBroadcastAttempted || value.ConfirmedCount != 0 {
				t.Fatalf("result=%+v err=%v commands=%v calls=%d", value, err, runtime.commands, calls)
			}
		})
	}
}

type notificationOutcomeRuntime struct {
	*notificationTestRuntime
	result shared.RuntimeOperationResult
	err    error
}

func (r notificationOutcomeRuntime) SendID(ctx context.Context, roomID, worldID string, request shared.RuntimeConsoleRequest) (shared.RuntimeOperationResult, error) {
	_, _ = r.notificationTestRuntime.SendID(ctx, roomID, worldID, request)
	return r.result, r.err
}

func TestCompatibilitySubmissionsDoNotInventExecution(t *testing.T) {
	for _, scenario := range []struct {
		transport shared.RuntimeOutcome
		err       error
		want      Outcome
	}{
		{shared.RuntimeOutcomeSent, nil, OutcomeSubmitted},
		{shared.RuntimeOutcomeConfirmed, nil, OutcomeSubmitted},
		{"", nil, OutcomeSubmitted},
		{shared.RuntimeOutcomeUnknown, nil, OutcomeUnconfirmed},
		{shared.RuntimeOutcomeFailed, nil, OutcomeFailed},
		{shared.RuntimeOutcomeSent, errors.New("connection lost"), OutcomeUnconfirmed},
	} {
		t.Run(fmt.Sprint(scenario.transport, scenario.err), func(t *testing.T) {
			service, _, runtime, catalog := newNotificationTestService(t)
			service.runtime = notificationOutcomeRuntime{runtime, shared.RuntimeOperationResult{Outcome: scenario.transport}, scenario.err}
			value, err := service.sendDirect(context.Background(), catalog.room.ID, "test announcement", SourceManual, "")
			if err != nil || value.Outcome != scenario.want || value.ConfirmedCount != 0 || len(runtime.commands) != 1 {
				t.Fatalf("result=%+v err=%v", value, err)
			}
		})
	}
}

func TestUnconfirmedAutomationDoesNotReportSuccess(t *testing.T) {
	service, _, runtime, catalog := newNotificationTestService(t)
	service.commander = notificationCommanderFunc(func(context.Context, string, string, dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
		return dstruntime.CommandReceipt{}, dstruntime.ErrRuntimeCommandUnconfirmed
	})
	_, _, _, err := service.SendAutomation(context.Background(), catalog.room.ID, "test announcement", "test-job")
	if !errors.Is(err, ErrOutcomeUnconfirmed) || len(runtime.commands) != 0 {
		t.Fatalf("uncertain automation result=%v", err)
	}
}

func TestConfirmedAutomationCanUseAnotherWorldWhenMasterStatusUnavailable(t *testing.T) {
	service, store, runtime, catalog := newNotificationTestService(t)
	runtime.statusErrs[catalog.worlds[0].ID] = errors.New("Master unavailable")
	runtime.statuses[catalog.worlds[1].ID] = shared.ShardRuntimeStatus{State: "running", SessionExists: true}
	calls := 0
	service.commander = notificationCommanderFunc(func(_ context.Context, _, worldID string, request dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
		calls++
		if worldID != catalog.worlds[1].ID {
			t.Fatal("broadcast sent on unavailable world")
		}
		return dstruntime.CommandReceipt{RequestID: request.RequestID, Action: request.Action, CompletedAt: service.now().UTC(), OK: true}, nil
	})
	success, failure, _, err := service.SendAutomation(context.Background(), catalog.room.ID, "Reminder", "test-job")
	history, readErr := store.List(ListFilter{})
	if err != nil || readErr != nil || success != 1 || failure != 0 || calls != 1 || history.Items[0].Outcome != OutcomeConfirmed {
		t.Fatalf("success=%d failure=%d calls=%d err=%v readErr=%v history=%+v", success, failure, calls, err, readErr, history)
	}
}

func TestOldNotificationRecordsRemainUnconfirmedAsExecutionEvidence(t *testing.T) {
	service, store, _, catalog := newNotificationTestService(t)
	value, err := service.sendDirect(context.Background(), catalog.room.ID, "legacy announcement", SourceManual, "")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate old rows after additive migration: no outcome or skip reason.
	if err := store.db.Table(store.deliveriesTable).Where("notification_id = ?", value.ID).Updates(map[string]interface{}{
		"outcome": nil, "skip_reason": nil, "broadcast_world_id": nil, "broadcast_world_name": nil,
	}).Error; err != nil {
		t.Fatal(err)
	}
	for _, read := range []func() (Notification, error){
		func() (Notification, error) { return store.Get(value.ID) },
		func() (Notification, error) { values, err := store.List(ListFilter{}); return values.Items[0], err },
	} {
		old, err := read()
		if err != nil || old.Outcome != OutcomeSubmitted || old.ConfirmedCount != 0 || old.Deliveries[1].SkipReason != "" ||
			strings.Contains(old.Deliveries[1].Message, "未运行") {
			t.Fatalf("old record invented evidence: %+v %v", old, err)
		}
	}
}

func TestLegacyNotificationSchemaMigratesWithoutInventingExecution(t *testing.T) {
	service, store, _, catalog := newNotificationTestService(t)
	old, err := service.sendDirect(context.Background(), catalog.room.ID, "Legacy announcement", SourceManual, "")
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild the previous schema on SQLite versions without DROP COLUMN.
	legacyTable := store.deliveriesTable + "_legacy"
	columns := "id, notification_id, world_id, world_name, target_id, agent_id, status, message, error_code, error_message, topology_revision, sent_at, observed_at"
	for _, statement := range []string{
		"CREATE TABLE " + legacyTable + ` (
			id integer PRIMARY KEY AUTOINCREMENT, notification_id char(36) NOT NULL,
			world_id varchar(255) NOT NULL, world_name varchar(255) NOT NULL,
			target_id varchar(255), agent_id varchar(255), status varchar(24) NOT NULL,
			message text, error_code varchar(64), error_message text,
			topology_revision varchar(80), sent_at datetime, observed_at datetime)`,
		"INSERT INTO " + legacyTable + " (" + columns + ") SELECT " + columns + " FROM " + store.deliveriesTable,
		"DROP TABLE " + store.deliveriesTable,
		"ALTER TABLE " + legacyTable + " RENAME TO " + store.deliveriesTable,
	} {
		if err := store.db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	value, err := store.Get(old.ID)
	if err != nil || value.Status != old.Status || value.Message != old.Message || value.Outcome != OutcomeSubmitted ||
		value.ConfirmedCount != 0 || len(value.Deliveries) != 2 || value.Deliveries[1].SkipReason != "" {
		t.Fatalf("legacy record lost or upgraded to confirmation: %+v err=%v", value, err)
	}
	newValue, err := service.sendDirect(context.Background(), catalog.room.ID, "New announcement", SourceManual, "")
	if err != nil || newValue.Outcome != OutcomeSubmitted || newValue.Deliveries[1].SkipReason != SkipRoomBroadcast {
		t.Fatalf("new fields not writable after migration: %+v err=%v", newValue, err)
	}
}

func TestReceiptWaitHonorsCancellationWithoutResending(t *testing.T) {
	service, _, runtime, catalog := newNotificationTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	service.commander = notificationCommanderFunc(func(ctx context.Context, _, _ string, _ dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
		calls++
		cancel() // Cancellation happens after dispatch, regardless of test load.
		<-ctx.Done()
		return dstruntime.CommandReceipt{}, fmt.Errorf("%w: %w", dstruntime.ErrRuntimeCommandUnconfirmed, ctx.Err())
	})
	started := time.Now()
	value, err := service.sendDirect(ctx, catalog.room.ID, "test announcement", SourceManual, "")
	if err != nil || value.Outcome != OutcomeUnconfirmed || calls != 1 || len(runtime.commands) != 0 || time.Since(started) > time.Second ||
		value.Deliveries[1].SkipReason != SkipBroadcastAttempted {
		t.Fatalf("cancelled wait=%+v err=%v calls=%d", value, err, calls)
	}
}
