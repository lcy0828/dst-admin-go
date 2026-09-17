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

type stateTestCatalog struct {
	room   rooms.Room
	worlds []rooms.World
}

func (c stateTestCatalog) Room(id string) (rooms.Room, error) {
	if id != c.room.ID {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return c.room, nil
}
func (c stateTestCatalog) Worlds(roomID string) ([]rooms.World, error) {
	if roomID != c.room.ID {
		return nil, rooms.ErrRoomNotFound
	}
	return append([]rooms.World(nil), c.worlds...), nil
}
func (c stateTestCatalog) World(roomID, worldID string) (rooms.World, error) {
	if roomID != c.room.ID {
		return rooms.World{}, rooms.ErrRoomNotFound
	}
	for _, world := range c.worlds {
		if world.ID == worldID {
			return world, nil
		}
	}
	return rooms.World{}, rooms.ErrWorldNotFound
}

type stateTestRuntime struct{ running bool }

func (r *stateTestRuntime) IsRunning(context.Context, string, string) (bool, error) {
	return r.running, nil
}

type stateStatusRuntime struct {
	status shards.RuntimeStatus
	err    error
	calls  atomic.Int32
}

func (r *stateStatusRuntime) IsRunning(context.Context, string, string) (bool, error) {
	return r.status.State == shards.RuntimeRunning, nil
}
func (r *stateStatusRuntime) Status(context.Context, string, string) (shards.RuntimeStatus, error) {
	r.calls.Add(1)
	return r.status, r.err
}

type firstCallBlockingStateStatusRuntime struct {
	calls        atomic.Int32
	firstStarted chan struct{}
}

type overlappingStateStatusRuntime struct {
	started chan string
	release chan struct{}
}

func (r *overlappingStateStatusRuntime) IsRunning(context.Context, string, string) (bool, error) {
	return true, nil
}

func (r *overlappingStateStatusRuntime) Status(ctx context.Context, _, world string) (shards.RuntimeStatus, error) {
	select {
	case r.started <- world:
	case <-ctx.Done():
		return shards.RuntimeStatus{}, ctx.Err()
	}
	select {
	case <-r.release:
		return shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}, nil
	case <-ctx.Done():
		return shards.RuntimeStatus{}, ctx.Err()
	}
}

func (r *firstCallBlockingStateStatusRuntime) IsRunning(context.Context, string, string) (bool, error) {
	return true, nil
}

func (r *firstCallBlockingStateStatusRuntime) Status(ctx context.Context, _ string, _ string) (shards.RuntimeStatus, error) {
	if r.calls.Add(1) == 1 {
		close(r.firstStarted)
		<-ctx.Done()
		return shards.RuntimeStatus{}, ctx.Err()
	}
	return shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}, nil
}

type stateTestSampler struct {
	observation Observation
	err         error
}

func (s *stateTestSampler) Snapshot(context.Context, string, string) (Observation, error) {
	return s.observation, s.err
}

func (s *stateTestSampler) CurrentSnapshot(context.Context, string, string) (Observation, error) {
	return s.observation, s.err
}

type stateTestStoppedSampler struct{ stateTestSampler }

func (s *stateTestStoppedSampler) StoppedSnapshot(context.Context, string, string) (Observation, error) {
	return s.observation, s.err
}

type stateTestCurrentSampler struct {
	stateTestSampler
	current      map[string]Observation
	currentErr   error
	currentCalls atomic.Int32
}

type stateTestFreshSampler struct {
	snapshotObservation Observation
	freshObservation    Observation
	snapshotCalls       int
	freshCalls          int
	currentCalls        int
}

func (s *stateTestFreshSampler) Snapshot(context.Context, string, string) (Observation, error) {
	s.snapshotCalls++
	return s.snapshotObservation, nil
}

func (s *stateTestFreshSampler) FreshSnapshot(context.Context, string, string) (Observation, error) {
	s.freshCalls++
	return s.freshObservation, nil
}

func (s *stateTestFreshSampler) CurrentSnapshot(context.Context, string, string) (Observation, error) {
	s.currentCalls++
	return s.snapshotObservation, nil
}

func (s *stateTestCurrentSampler) CurrentSnapshot(_ context.Context, _, worldID string) (Observation, error) {
	s.currentCalls.Add(1)
	if s.currentErr != nil {
		return Observation{}, s.currentErr
	}
	observation, exists := s.current[worldID]
	if !exists {
		return Observation{}, errors.New("current snapshot unavailable")
	}
	return observation, nil
}

func TestServiceHistorySamplingPersistsTimeSeriesAndRejectsStoppedWorld(t *testing.T) {
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}},
	}
	runtime := &stateTestRuntime{running: true}
	progress := .25
	cycles := 12
	sampler := &stateTestStoppedSampler{stateTestSampler{observation: Observation{Season: "autumn", Phase: "day", Cycles: &cycles, SeasonProgress: &progress}}}
	service, err := NewService(catalog, runtime, newWorldStateStore(t), sampler)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	if _, err := service.SampleWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	list, err := service.List(context.Background(), "room")
	if err != nil || len(list.Items) != 1 || list.Items[0].Season != "autumn" || list.LastRefreshedAt == nil || !list.LastRefreshedAt.Equal(now) {
		t.Fatalf("list=%#v err=%v", list, err)
	}
	history, err := service.History("room", "master", 120)
	if err != nil || history.Total != 1 {
		t.Fatalf("history=%#v err=%v", history, err)
	}
	runtime.running = false
	if _, err := service.SampleWorld(context.Background(), "room", "master"); !errors.Is(err, ErrWorldNotRunning) {
		t.Fatalf("stopped world error = %v", err)
	}
	if _, err := service.History("room", "master", 0); !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("invalid limit error = %v", err)
	}
}

func TestServiceRefreshUsesRuntimeCaptureTimeWhenProvided(t *testing.T) {
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}},
	}
	capturedAt := time.Date(2026, 8, 12, 10, 0, 1, 0, time.UTC)
	service, err := NewService(catalog, &stateTestRuntime{running: true}, newWorldStateStore(t), &stateTestSampler{
		observation: Observation{Season: "winter", CapturedAt: capturedAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return capturedAt.Add(4 * time.Second) }
	result, err := service.RefreshWorld(context.Background(), "room", "master")
	if err != nil || !result.ObservedAt.Equal(capturedAt) {
		t.Fatalf("refresh result = %#v, error = %v", result, err)
	}
	list, err := service.List(context.Background(), "room")
	if err != nil || len(list.Items) != 1 || !list.Items[0].ObservedAt.Equal(capturedAt) {
		t.Fatalf("stored capture time = %#v, error = %v", list, err)
	}
}

func TestServiceRefreshReadsFilesWithoutActiveSamplingOrPersistence(t *testing.T) {
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}},
	}
	capturedAt := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	sampler := &stateTestFreshSampler{
		snapshotObservation: Observation{Season: "autumn", Phase: "day", CapturedAt: capturedAt},
		freshObservation:    Observation{Season: "winter", Phase: "night", CapturedAt: capturedAt},
	}
	service, err := NewService(catalog, &stateTestRuntime{running: true}, newWorldStateStore(t), sampler)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return capturedAt.Add(time.Second) }
	result, err := service.RefreshWorld(context.Background(), "room", "master")
	if err != nil || sampler.freshCalls != 0 || sampler.snapshotCalls != 0 || sampler.currentCalls != 1 {
		t.Fatalf("refresh result = %#v, error = %v, fresh calls = %d, snapshot calls = %d", result, err, sampler.freshCalls, sampler.snapshotCalls)
	}
	if result.Snapshot.ID != 0 || result.Snapshot.Season != "autumn" || result.Snapshot.Phase != "day" || result.Snapshot.Freshness != FreshnessLive || result.Snapshot.RuntimeState != string(shards.RuntimeRunning) || result.Snapshot.Stale {
		t.Fatalf("returned snapshot = %#v", result.Snapshot)
	}
	history, err := service.History("room", "master", 120)
	if err != nil || history.Total != 0 {
		t.Fatalf("page refresh wrote history: %#v, %v", history, err)
	}
}

func TestServiceListMergesLiveRuntimeStateWithoutPersistingIt(t *testing.T) {
	catalog := stateTestCatalog{
		room: rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{
			{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster},
			{ID: "caves", RoomID: "room", DirectoryName: "Caves", Name: "Caves", Role: rooms.WorldRoleCaves},
		},
	}
	capturedAt := time.Date(2026, 8, 13, 2, 20, 0, 0, time.UTC)
	masterCycles, cavesCycles := 161, 162
	sampler := &stateTestCurrentSampler{current: map[string]Observation{
		"master": {Season: "winter", Phase: "day", Cycles: &masterCycles, CapturedAt: capturedAt},
		"caves":  {Season: "winter", Phase: "night", Cycles: &cavesCycles, CapturedAt: capturedAt.Add(time.Second)},
	}}
	store := newWorldStateStore(t)
	service, err := NewService(catalog, &stateTestRuntime{running: true}, store, sampler)
	if err != nil {
		t.Fatal(err)
	}
	list, err := service.List(context.Background(), "room")
	if err != nil || list.Total != 2 || sampler.currentCalls.Load() != 2 || list.LastRefreshedAt == nil || !list.LastRefreshedAt.Equal(capturedAt.Add(time.Second)) {
		t.Fatalf("live list = %#v, calls = %d, error = %v", list, sampler.currentCalls.Load(), err)
	}
	byWorld := make(map[string]Snapshot, len(list.Items))
	for _, item := range list.Items {
		byWorld[item.WorldID] = item
	}
	if byWorld["master"].Cycles == nil || *byWorld["master"].Cycles != 161 || byWorld["master"].Season != "winter" || byWorld["caves"].Cycles == nil || *byWorld["caves"].Cycles != 162 {
		t.Fatalf("live snapshots were not merged: %#v", list.Items)
	}
	stored, err := store.Current("room")
	if err != nil || len(stored) != 0 {
		t.Fatalf("read-only list persisted snapshots: %#v, error = %v", stored, err)
	}
}

func TestServiceListUsesSuccessfulLiveStateWhenStoredTimestampIsNewer(t *testing.T) {
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}},
	}
	store := newWorldStateStore(t)
	liveAt := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	if _, err := store.Append(Snapshot{
		RoomID: "room", WorldID: "master", WorldName: "Master", WorldRole: "master",
		Season: "autumn", ObservedAt: liveAt.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	sampler := &stateTestCurrentSampler{current: map[string]Observation{
		"master": {Season: "winter", Phase: "night", CapturedAt: liveAt},
	}}
	service, err := NewService(catalog, &stateStatusRuntime{
		status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true},
	}, store, sampler)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return liveAt.Add(time.Second) }

	list, err := service.List(context.Background(), "room")
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list=%#v error=%v", list, err)
	}
	if item := list.Items[0]; item.Season != "winter" || item.Phase != "night" || !item.ObservedAt.Equal(liveAt) {
		t.Fatalf("live state did not replace future stored state: %#v", item)
	}
}

func TestServiceListChecksEachWorldRuntimeOnlyOnce(t *testing.T) {
	catalog := stateTestCatalog{
		room: rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{
			{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster},
			{ID: "caves", RoomID: "room", DirectoryName: "Caves", Name: "Caves", Role: rooms.WorldRoleCaves},
		},
	}
	runtime := &stateStatusRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	sampler := &stateTestCurrentSampler{current: map[string]Observation{}}
	service, err := NewService(catalog, runtime, newWorldStateStore(t), sampler)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), "room"); err != nil {
		t.Fatal(err)
	}
	if runtime.calls.Load() != int32(len(catalog.worlds)) {
		t.Fatalf("runtime checked %d times for %d worlds", runtime.calls.Load(), len(catalog.worlds))
	}
}

func TestServiceListCollectsWorldsConcurrentlyAndKeepsCatalogOrder(t *testing.T) {
	catalog := stateTestCatalog{
		room: rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{
			{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster},
			{ID: "caves", RoomID: "room", DirectoryName: "Caves", Name: "Caves", Role: rooms.WorldRoleCaves},
		},
	}
	runtime := &overlappingStateStatusRuntime{started: make(chan string, 2), release: make(chan struct{})}
	service, err := NewService(catalog, runtime, newWorldStateStore(t), &stateTestSampler{})
	if err != nil {
		t.Fatal(err)
	}

	type listResult struct {
		value List
		err   error
	}
	done := make(chan listResult, 1)
	go func() {
		value, listErr := service.List(context.Background(), "room")
		done <- listResult{value: value, err: listErr}
	}()
	for range 2 {
		select {
		case <-runtime.started:
		case <-time.After(2 * time.Second):
			close(runtime.release)
			t.Fatal("world runtime reads did not overlap")
		}
	}
	close(runtime.release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if len(result.value.Items) != 2 || result.value.Items[0].WorldID != "master" || result.value.Items[1].WorldID != "caves" {
		t.Fatalf("parallel collection changed catalog order: %#v", result.value.Items)
	}
}

func TestServiceListRequestsDoNotShareTheFirstRequestContext(t *testing.T) {
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}},
	}
	runtime := &firstCallBlockingStateStatusRuntime{firstStarted: make(chan struct{})}
	service, err := NewService(catalog, runtime, newWorldStateStore(t), &stateTestSampler{})
	if err != nil {
		t.Fatal(err)
	}

	firstContext, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstDone := make(chan error, 1)
	go func() {
		_, listErr := service.List(firstContext, "room")
		firstDone <- listErr
	}()
	<-runtime.firstStarted

	secondContext, cancelSecond := context.WithTimeout(context.Background(), time.Second)
	defer cancelSecond()
	if _, err := service.List(secondContext, "room"); err != nil {
		t.Fatalf("second request inherited the first request context: %v", err)
	}
	if runtime.calls.Load() != 2 {
		t.Fatalf("runtime status calls = %d, want 2 independent reads", runtime.calls.Load())
	}
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first request error = %v, want context canceled", err)
	}
}

func TestServiceListReadsRuntimeOnEveryCompletedRequest(t *testing.T) {
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}},
	}
	runtime := &stateStatusRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	sampler := &stateTestCurrentSampler{
		stateTestSampler: stateTestSampler{observation: Observation{Season: "autumn", Phase: "day"}},
		current:          map[string]Observation{"master": {Season: "autumn", Phase: "day"}},
	}
	service, err := NewService(catalog, runtime, newWorldStateStore(t), sampler)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), "room"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), "room"); err != nil {
		t.Fatal(err)
	}
	if runtime.calls.Load() != 2 {
		t.Fatalf("two completed list requests checked runtime %d times", runtime.calls.Load())
	}
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), "room"); err != nil {
		t.Fatal(err)
	}
	if runtime.calls.Load() != 4 {
		t.Fatalf("runtime calls after refresh = %d, want 4", runtime.calls.Load())
	}
}

func TestServiceListDecoratesFreshnessFromRuntimeAndObservationAge(t *testing.T) {
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}},
	}
	store := newWorldStateStore(t)
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	if _, err := store.Append(Snapshot{RoomID: "room", WorldID: "master", WorldName: "Master", WorldRole: "master", Season: "autumn", ObservedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	runtime := &stateStatusRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	service, err := NewService(catalog, runtime, store, &stateTestStoppedSampler{stateTestSampler{
		observation: Observation{Season: "autumn", CapturedAt: now.Add(-time.Minute)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	currentTime := now
	service.now = func() time.Time { return currentTime }
	list, err := service.List(context.Background(), "room")
	if err != nil || len(list.Items) != 1 || list.Items[0].Freshness != FreshnessLive || list.Items[0].Stale || list.Items[0].RuntimeState != "running" || list.Items[0].AgeSeconds != 60 {
		t.Fatalf("live snapshot = %#v, error = %v", list, err)
	}
	currentTime = now.Add(3 * time.Minute)
	list, _ = service.List(context.Background(), "room")
	if list.Items[0].Freshness != FreshnessDelayed || !list.Items[0].Stale {
		t.Fatalf("delayed snapshot = %#v", list.Items[0])
	}
	runtime.status = shards.RuntimeStatus{State: shards.RuntimeStopped}
	currentTime = currentTime.Add(2 * time.Second)
	list, _ = service.List(context.Background(), "room")
	if list.Items[0].Freshness != FreshnessStopped || list.Items[0].RuntimeState != "stopped" || !list.Items[0].Stale {
		t.Fatalf("stopped snapshot = %#v", list.Items[0])
	}
	runtime.status = shards.RuntimeStatus{State: shards.RuntimeUnknown}
	currentTime = currentTime.Add(2 * time.Second)
	list, _ = service.List(context.Background(), "room")
	if list.Items[0].Freshness != FreshnessUnavailable || list.Items[0].RuntimeState != "unknown" || !list.Items[0].Stale {
		t.Fatalf("unavailable snapshot = %#v", list.Items[0])
	}
}

func TestServiceListIncludesWorldsWithoutSnapshots(t *testing.T) {
	catalog := stateTestCatalog{
		room: rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{
			{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster},
			{ID: "caves", RoomID: "room", DirectoryName: "Caves", Name: "Caves", Role: rooms.WorldRoleCaves},
		},
	}
	store := newWorldStateStore(t)
	now := time.Date(2026, 8, 16, 1, 0, 0, 0, time.UTC)
	if _, err := store.Append(Snapshot{
		RoomID: "room", WorldID: "master", WorldName: "Master", WorldRole: "master",
		Season: "autumn", ObservedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(catalog, &stateStatusRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}}, store, &stateTestSampler{})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	list, err := service.List(context.Background(), "room")
	if err != nil || list.Total != 2 || len(list.Items) != 2 {
		t.Fatalf("world state list = %#v, error = %v", list, err)
	}
	if list.Items[0].WorldID != "master" || list.Items[1].WorldID != "caves" {
		t.Fatalf("world order = %#v", list.Items)
	}
	missing := list.Items[1]
	if !missing.ObservedAt.IsZero() || missing.Freshness != FreshnessUnavailable || missing.RuntimeState != "stopped" || missing.AgeSeconds != 0 || !missing.Stale {
		t.Fatalf("missing world placeholder = %#v", missing)
	}
	if list.LastRefreshedAt != nil || list.Items[0].Season != "" {
		t.Fatalf("last refreshed = %#v", list.LastRefreshedAt)
	}
}

func TestServiceListExposesRuntimeAndObservationDiagnostics(t *testing.T) {
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}},
	}
	runtime := &stateStatusRuntime{status: shards.RuntimeStatus{
		State: shards.RuntimeFailed, Code: "UNMANAGED_DST_PROCESS_CONFLICT", Message: "DST process is outside the managed socket",
	}}
	service, err := NewService(catalog, runtime, newWorldStateStore(t), &stateTestCurrentSampler{current: map[string]Observation{}})
	if err != nil {
		t.Fatal(err)
	}
	list, err := service.List(context.Background(), "room")
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list=%#v error=%v", list, err)
	}
	item := list.Items[0]
	if item.RuntimeState != "failed" || item.RuntimeCode != "UNMANAGED_DST_PROCESS_CONFLICT" || item.RuntimeMessage == "" || item.ObservationError != "" {
		t.Fatalf("runtime diagnostic=%#v", item)
	}

	runtime.status = shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}
	list, err = service.List(context.Background(), "room")
	if err != nil || list.Items[0].ObservationError != "current snapshot unavailable" {
		t.Fatalf("observation diagnostic=%#v error=%v", list, err)
	}
}

func TestServiceListDoesNotPresentStoredStateWhenRuntimeStatusReadFails(t *testing.T) {
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}},
	}
	store := newWorldStateStore(t)
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	if _, err := store.Append(Snapshot{
		RoomID: "room", WorldID: "master", WorldName: "Master", WorldRole: "master",
		Season: "autumn", ObservedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(catalog, &stateStatusRuntime{
		err: errors.New("I/O operation failed"),
	}, store, &stateTestCurrentSampler{current: map[string]Observation{}})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }

	list, err := service.List(context.Background(), "room")
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list=%#v error=%v", list, err)
	}
	item := list.Items[0]
	if item.Season != "" || !item.ObservedAt.IsZero() || item.Freshness != FreshnessUnavailable ||
		item.RuntimeCode != "RUNTIME_STATUS_UNAVAILABLE" || item.RuntimeMessage != "I/O operation failed" {
		t.Fatalf("runtime status failure exposed stored state: %#v", item)
	}
}

func TestServiceListDoesNotPresentStoredStateAsCurrentWhileRefreshIsDeferred(t *testing.T) {
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}},
	}
	store := newWorldStateStore(t)
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	if _, err := store.Append(Snapshot{
		RoomID: "room", WorldID: "master", WorldName: "Master", WorldRole: "master",
		Season: "autumn", ObservedAt: now.Add(-3 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	sampler := &stateTestCurrentSampler{
		currentErr: errors.Join(dstruntime.ErrSnapshotStale, dstruntime.ErrRuntimeRefreshDeferred),
	}
	service, err := NewService(catalog, &stateStatusRuntime{
		status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true},
	}, store, sampler)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }

	list, err := service.List(context.Background(), "room")
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list=%#v error=%v", list, err)
	}
	item := list.Items[0]
	if item.Season != "" || !item.ObservedAt.IsZero() || item.Freshness != FreshnessUnavailable ||
		item.ObservationState != ObservationStateDeferred ||
		item.ObservationCode != ObservationCodeRoomOperationInProgress || item.ObservationError != "" {
		t.Fatalf("deferred observation=%#v", item)
	}
}

func TestServiceListNeverUsesFreshSamplerForRunningWorld(t *testing.T) {
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}},
	}
	capturedAt := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	sampler := &stateTestFreshSampler{
		snapshotObservation: Observation{Season: "winter", Phase: "night", CapturedAt: capturedAt},
		freshObservation:    Observation{Season: "spring", Phase: "day", CapturedAt: capturedAt},
	}
	service, err := NewService(catalog, &stateStatusRuntime{
		status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true},
	}, newWorldStateStore(t), sampler)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return capturedAt.Add(time.Second) }

	list, err := service.List(context.Background(), "room")
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list=%#v error=%v", list, err)
	}
	if sampler.freshCalls != 0 || sampler.snapshotCalls != 0 || sampler.currentCalls != 1 || list.Items[0].Season != "winter" || list.Items[0].Freshness != FreshnessLive {
		t.Fatalf("fresh list=%#v fresh calls=%d snapshot calls=%d", list, sampler.freshCalls, sampler.snapshotCalls)
	}
}
