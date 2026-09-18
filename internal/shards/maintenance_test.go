package shards

import (
	"context"
	"errors"
	"testing"

	"dont/internal/jobs"
	"dont/internal/maintenance"
	"dont/internal/rooms"
)

func TestUnattendedStopRechecksAfterNotification(t *testing.T) {
	for _, action := range []Action{ActionStop, ActionRestart} {
		t.Run(string(action), func(t *testing.T) {
			control := &fakeControl{running: map[string]bool{"summer_2026/Master": true}, fail: map[string]error{}}
			operations := testOperations(control)
			notifier := &fakeOperationNotifier{}
			operations.ConfigureNotifier(notifier)
			_, runner, err := operations.Plan(action, rooms.EncodeID("summer_2026"), []string{rooms.EncodeID("Master")})
			if err != nil {
				t.Fatal(err)
			}
			ctx := maintenance.WithCheck(context.Background(), func(context.Context) error {
				if len(notifier.calls) != 1 {
					t.Fatal("guard ran before notification")
				}
				return maintenance.ErrPlayersOnline
			})
			err = runner(ctx, func(jobs.TargetResult) {})
			if !errors.Is(err, maintenance.ErrPlayersOnline) || len(control.calls) != 0 {
				t.Fatalf("guard ignored: err=%v calls=%v", err, control.calls)
			}
		})
	}
}
