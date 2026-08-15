package runtimeevents

import (
	"context"
	"testing"

	"dont/internal/dstruntime"
)

type eventSource struct{ batch dstruntime.EventBatch }

func (s eventSource) ReadEvents(context.Context, string, string) (dstruntime.EventBatch, error) {
	return s.batch, nil
}

func TestWindowSupportsResumeGapAndProducerReset(t *testing.T) {
	batch := dstruntime.EventBatch{
		ProducerInstanceID: "runtime/instance:one", FirstSequence: 4, LastSequence: 6,
		Events: []dstruntime.RuntimeEvent{{Sequence: 4}, {Sequence: 5}, {Sequence: 6}},
	}
	service, _ := New(eventSource{batch: batch})

	fresh, err := service.Window(context.Background(), "room", "world", Cursor{}, false)
	if err != nil || len(fresh.Events) != 0 || fresh.Cursor.Sequence != 6 || fresh.Reset || fresh.Gap {
		t.Fatalf("fresh=%#v err=%v", fresh, err)
	}
	parsed, err := Parse(fresh.EncodedCursor)
	if err != nil || parsed != fresh.Cursor {
		t.Fatalf("parsed=%#v cursor=%#v err=%v", parsed, fresh.Cursor, err)
	}

	resumed, err := service.Window(context.Background(), "room", "world", Cursor{ProducerInstanceID: batch.ProducerInstanceID, Sequence: 4}, true)
	if err != nil || len(resumed.Events) != 2 || resumed.Events[0].Sequence != 5 || resumed.Reset || resumed.Gap {
		t.Fatalf("resumed=%#v err=%v", resumed, err)
	}

	gap, err := service.Window(context.Background(), "room", "world", Cursor{ProducerInstanceID: batch.ProducerInstanceID, Sequence: 1}, true)
	if err != nil || !gap.Gap || gap.Reset || len(gap.Events) != 3 {
		t.Fatalf("gap=%#v err=%v", gap, err)
	}

	reset, err := service.Window(context.Background(), "room", "world", Cursor{ProducerInstanceID: "old-instance", Sequence: 20}, true)
	if err != nil || !reset.Reset || reset.Gap || len(reset.Events) != 3 {
		t.Fatalf("reset=%#v err=%v", reset, err)
	}
}

func TestCursorRejectsMalformedOrUnboundedValues(t *testing.T) {
	for _, value := range []string{"", "missing-separator", "!.1", "YQ.-1", "YQ.not-number"} {
		if _, err := Parse(value); err != ErrInvalidCursor {
			t.Fatalf("cursor %q error=%v", value, err)
		}
	}
}
