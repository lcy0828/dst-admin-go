package console

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dont/internal/dstruntime"
)

type lateCommander struct {
	captureCommander
	reads atomic.Int32
	read  func(context.Context, string, string, dstruntime.CommandRequest, time.Time) (dstruntime.CommandReceipt, error)
}

func (c *lateCommander) ReadCommandReceipt(ctx context.Context, room, world string, request dstruntime.CommandRequest, since time.Time) (dstruntime.CommandReceipt, error) {
	c.reads.Add(1)
	return c.read(ctx, room, world, request, since)
}
func uncertainRun(t *testing.T, s *Service) Run {
	t.Helper()
	run, err := s.store.Create(Run{RoomID: "room", WorldID: "Master", Mode: "raw", Name: "count", RawCommand: `print(0)`})
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.store.CompleteExecution(run.ID, ExecutionCompletion{Status: RunUncertain, TransportOutcome: "sent", ExecutionOutcome: "unknown", MayHaveExecuted: true, ErrorCode: "COMMAND_OUTCOME_UNKNOWN"})
	if err != nil {
		t.Fatal(err)
	}
	return run
}
func lateReply(request dstruntime.CommandRequest, now time.Time, ok bool, output interface{}) dstruntime.CommandReceipt {
	return dstruntime.CommandReceipt{RequestID: request.RequestID, Action: request.Action, OK: ok, Code: "COMMAND_EXECUTED", CompletedAt: now, Details: map[string]interface{}{"output": output}}
}
func TestLateReceiptPersistsExactOutputAndDoesNotReplay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output interface{}
		ok     bool
		state  string
	}{
		{"zero", map[string]interface{}{"text": "0\n", "truncated": false}, true, "output"},
		{"empty", map[string]interface{}{"text": "", "truncated": false}, true, "no_output"},
		{"legacy", nil, true, "output_unavailable"},
		{"partial failure", map[string]interface{}{"text": "partial\n", "truncated": false}, false, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sender := &captureSender{}
			s := newConsoleService(t, sender)
			run := uncertainRun(t, s)
			c := &lateCommander{read: func(_ context.Context, room, world string, request dstruntime.CommandRequest, since time.Time) (dstruntime.CommandReceipt, error) {
				if room != run.RoomID || world != run.WorldID || !since.Equal(run.CreatedAt) {
					t.Error("wrong target or time")
				}
				return lateReply(request, time.Now(), tc.ok, tc.output), nil
			}}
			s.commander = c
			got, err := s.RefreshRun(context.Background(), run.RoomID, run.ID)
			if err != nil || got.ResultState != tc.state || got.ReceiptCheck.Status != "found" || got.ReceiptRefreshUntil != nil || got.MayHaveExecuted || got.ErrorCode == "COMMAND_OUTCOME_UNKNOWN" {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			persisted, err := s.Run(run.ID)
			if err != nil || persisted.ResultState != tc.state || persisted.Status != got.Status || persisted.ReceiptCheck != nil {
				t.Fatalf("persisted=%+v err=%v", persisted, err)
			}
			if _, err = s.RefreshRun(context.Background(), run.RoomID, run.ID); err != nil {
				t.Fatal(err)
			}
			if c.reads.Load() != 1 || len(c.requests) != 0 || sender.script != "" {
				t.Fatal("re-read/replayed command")
			}
		})
	}
}
func TestReceiptLookupStaysUnknownAndReadsAreOptIn(t *testing.T) {
	s := newConsoleService(t, &captureSender{})
	run := uncertainRun(t, s)
	c := &lateCommander{read: func(_ context.Context, _, _ string, req dstruntime.CommandRequest, _ time.Time) (dstruntime.CommandReceipt, error) {
		return dstruntime.CommandReceipt{}, dstruntime.ErrRuntimeResultAbsent
	}}
	s.commander = c
	if _, err := s.Run(run.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Runs(ListFilter{RoomID: run.RoomID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshRun(context.Background(), "another-room", run.ID); !errors.Is(err, ErrRunNotFound) {
		t.Fatal(err)
	}
	if c.reads.Load() != 0 {
		t.Fatal("default/list/wrong-room read called Runtime")
	}
	for _, tc := range []struct {
		name, status string
		readErr      error
	}{
		{"missing", "not_found", dstruntime.ErrRuntimeResultAbsent}, {"offline", "unavailable", errors.New("Agent disconnected")}, {"canceled", "unavailable", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.read = func(context.Context, string, string, dstruntime.CommandRequest, time.Time) (dstruntime.CommandReceipt, error) {
				return dstruntime.CommandReceipt{}, tc.readErr
			}
			got, err := s.RefreshRun(context.Background(), run.RoomID, run.ID)
			if err != nil || got.Status != RunUncertain || got.Output != nil || got.ResultState != "unconfirmed" || got.ReceiptCheck.Status != tc.status {
				t.Fatalf("got=%+v err=%v", got, err)
			}
		})
	}
	c.read = func(_ context.Context, _, _ string, r dstruntime.CommandRequest, _ time.Time) (dstruntime.CommandReceipt, error) {
		r.RequestID = "other-request"
		return lateReply(r, time.Now(), true, nil), nil
	}
	got, err := s.RefreshRun(context.Background(), run.RoomID, run.ID)
	if err != nil || got.Status != RunUncertain || got.ReceiptCheck.Status != "not_found" {
		t.Fatalf("mismatch accepted: %+v %v", got, err)
	}
	s.store.now = func() time.Time { return run.CreatedAt.Add(receiptRefreshWindow) }
	before := c.reads.Load()
	got, err = s.RefreshRun(context.Background(), run.RoomID, run.ID)
	if err != nil || got.ReceiptCheck.Status != "expired" || c.reads.Load() != before {
		t.Fatalf("expired read=%+v %v", got, err)
	}
}
func TestConcurrentLateReadsShareOneReadAndCannotOverwriteConfirmation(t *testing.T) {
	s := newConsoleService(t, &captureSender{})
	run := uncertainRun(t, s)
	started, release := make(chan struct{}), make(chan struct{})
	c := &lateCommander{read: func(_ context.Context, _, _ string, r dstruntime.CommandRequest, _ time.Time) (dstruntime.CommandReceipt, error) {
		close(started)
		<-release
		return lateReply(r, time.Now(), true, map[string]interface{}{"text": "late", "truncated": false}), nil
	}}
	s.commander = c
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.RefreshRun(context.Background(), run.RoomID, run.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	<-started
	// A different valid receipt wins before the in-flight lookup completes.
	if _, err := s.store.confirmLateExecution(run.ID, ExecutionCompletion{Status: RunFailed, ExecutionOutcome: "rejected", Output: &Output{Text: "original"}}); err != nil {
		t.Fatal(err)
	}
	close(release)
	wg.Wait()
	got, err := s.Run(run.ID)
	if err != nil || got.Status != RunFailed || got.Output.Text != "original" || c.reads.Load() != 1 {
		t.Fatalf("overwritten=%+v reads=%d err=%v", got, c.reads.Load(), err)
	}
}
func TestLegacySentIsUnconfirmedWithoutLateRead(t *testing.T) {
	s := newConsoleService(t, &captureSender{})
	run, err := s.ExecuteRaw(context.Background(), "room", "world", RawRequest{Command: `print(0)`, Confirmation: "周末服"})
	if err != nil || run.ResultState != "unconfirmed" || run.ReceiptRefreshUntil != nil {
		t.Fatalf("legacy=%+v %v", run, err)
	}
}
