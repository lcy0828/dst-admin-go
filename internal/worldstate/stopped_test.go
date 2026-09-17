package worldstate

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/internal/shards"
)

type stoppedWorldStateReader struct {
	refreshableWorldStateReader
	stopped      dstruntime.WorldStateSnapshot
	stoppedErr   error
	stoppedCalls int
}

func (r *stoppedWorldStateReader) ReadStoppedWorldState(context.Context, string, string) (dstruntime.WorldStateSnapshot, error) {
	r.stoppedCalls++
	return r.stopped, r.stoppedErr
}

func TestServiceReadsStoppedWorldFromRuntimeWithoutRefreshingOrPersisting(t *testing.T) {
	now := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)
	capturedAt := now.Add(-time.Hour)
	cycles := 200
	reader := &stoppedWorldStateReader{
		refreshableWorldStateReader: refreshableWorldStateReader{runtimeWorldStateReader: &runtimeWorldStateReader{}},
		stopped:                     dstruntime.WorldStateSnapshot{CapturedAt: capturedAt, Season: "summer", Phase: "day", Cycles: &cycles},
	}
	fallback := &countingWorldStateSampler{}
	sampler, err := NewRuntimeSampler(reader)
	if err != nil {
		t.Fatal(err)
	}
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "all", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", Name: "Master", DirectoryName: "Master", Role: rooms.WorldRoleMaster}},
	}
	store := newWorldStateStore(t)
	if _, err := store.Append(Snapshot{RoomID: "room", WorldID: "master", Season: "autumn", ObservedAt: now.Add(-72 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	runtime := &stateStatusRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}}
	service, err := NewService(catalog, runtime, store, sampler)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	for _, state := range []shards.RuntimeState{shards.RuntimeStopped, shards.RuntimeFailed} {
		runtime.status.State = state
		list, err := service.List(context.Background(), "room")
		if err != nil || len(list.Items) != 1 {
			t.Fatalf("list=%#v err=%v", list, err)
		}
		item := list.Items[0]
		if item.Season != "summer" || item.Cycles == nil || *item.Cycles != cycles || !item.ObservedAt.Equal(capturedAt) || item.AgeSeconds != 3600 || item.Freshness != FreshnessStopped {
			t.Fatalf("stopped state=%#v", item)
		}
	}
	if reader.stoppedCalls != 2 || reader.calls != 0 || reader.refreshCalls != 0 || fallback.calls != 0 {
		t.Fatalf("unexpected runtime calls: %+v, fallback=%d", reader, fallback.calls)
	}
	stored, err := store.Current("room")
	if err != nil || len(stored) != 1 || stored[0].Season != "autumn" {
		t.Fatalf("read-only list modified history: %#v, %v", stored, err)
	}

	reader.stoppedErr = errors.New("runtime file unavailable")
	list, err := service.List(context.Background(), "room")
	if err != nil || list.Items[0].Freshness != FreshnessUnavailable || !list.Items[0].ObservedAt.IsZero() || list.Items[0].ObservationError != reader.stoppedErr.Error() {
		t.Fatalf("failed runtime read silently used database history: %#v, %v", list, err)
	}
}
