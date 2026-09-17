package worldstate

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/internal/shards"
)

type combinedStateSampler struct {
	stateTestSampler
	calls    atomic.Int32
	results  map[string]Observation
	errors   map[string]error
	statuses map[string]shards.RuntimeState
}

func (s *combinedStateSampler) CurrentWorld(_ context.Context, _, worldID string) (Observation, shards.RuntimeStatus, error) {
	s.calls.Add(1)
	state := s.statuses[worldID]
	if state == "" {
		state = shards.RuntimeRunning
	}
	return s.results[worldID], shards.RuntimeStatus{State: state, SessionExists: state == shards.RuntimeRunning}, s.errors[worldID]
}

func TestCurrentWorldReadUsesCombinedSamplerAndKeepsPartialResults(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	catalog := stateTestCatalog{room: rooms.Room{ID: "room", Managed: true}, worlds: []rooms.World{
		{ID: "master", Name: "Master", Role: rooms.WorldRoleMaster},
		{ID: "caves", Name: "Caves", Role: rooms.WorldRoleCaves},
	}}
	sampler := &combinedStateSampler{results: map[string]Observation{"master": {Season: "autumn", CapturedAt: now.Add(-time.Hour)}}, errors: map[string]error{"caves": errors.New("permission denied")}}
	runtime := &stateStatusRuntime{err: errors.New("unexpected separate status read")}
	store := newWorldStateStore(t)
	service, err := NewService(catalog, runtime, store, sampler)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	if _, err := store.Append(Snapshot{RoomID: "room", WorldID: "caves", Season: "winter", ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	list, err := service.List(context.Background(), "room")
	if err != nil || sampler.calls.Load() != 2 || runtime.calls.Load() != 0 || len(list.Items) != 2 {
		t.Fatalf("list=%#v error=%v calls=%d/%d", list, err, sampler.calls.Load(), runtime.calls.Load())
	}
	if list.Items[0].Season != "autumn" || list.Items[0].Freshness != FreshnessDelayed || list.Items[0].ObservationError != "" || list.Items[1].Season != "" || list.Items[1].ObservationError != "permission denied" {
		t.Fatalf("partial read=%#v", list)
	}
	sampler.errors["caves"] = dstruntime.ErrWorldStatePending
	refreshed, err := service.RefreshWorld(context.Background(), "room", "caves")
	if err != nil || refreshed.Snapshot.ObservationState != ObservationStatePending || !refreshed.ObservedAt.IsZero() {
		t.Fatalf("pending read=%#v error=%v", refreshed, err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), "room"); err != nil {
		t.Fatalf("current read still depends on history database: %v", err)
	}
}

func TestStoppedAbsentStateIsEmptyButRealReadErrorsRemainVisible(t *testing.T) {
	status := shards.RuntimeStatus{State: shards.RuntimeStopped}
	read := worldReadResult(status, Observation{}, dstruntime.ErrWorldStateAbsent)
	if read.hasSnapshot || read.observationError != "" || read.observationState == ObservationStateFailed {
		t.Fatalf("empty world: %+v", read)
	}
	read = worldReadResult(status, Observation{}, errors.New("permission denied"))
	if read.observationError != "permission denied" || read.observationState != ObservationStateFailed {
		t.Fatalf("hidden file error: %+v", read)
	}
}

func TestRefreshStoppedWorldWithoutSampleSucceedsWithoutHidingReadFailures(t *testing.T) {
	catalog := stateTestCatalog{room: rooms.Room{ID: "room", Managed: true}, worlds: []rooms.World{{ID: "master", Name: "Master"}}}
	sampler := &combinedStateSampler{statuses: map[string]shards.RuntimeState{"master": shards.RuntimeStopped}, errors: map[string]error{"master": dstruntime.ErrWorldStateAbsent}}
	service, err := NewService(catalog, &stateStatusRuntime{}, newWorldStateStore(t), sampler)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.RefreshWorld(context.Background(), "room", "master")
	if err != nil || !result.ObservedAt.IsZero() || result.Snapshot.ObservationError != "" || result.Snapshot.Freshness != FreshnessUnavailable {
		t.Fatalf("unused world: %+v %v", result, err)
	}
	sampler.errors["master"] = errors.New("permission denied")
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); !errors.Is(err, dstruntime.ErrSnapshotUnavailable) {
		t.Fatalf("lost read failure: %v", err)
	}
}
