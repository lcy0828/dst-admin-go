package worldstate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/requesttiming"
	"dont/internal/rooms"
	"dont/internal/shards"
)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
	World(string, string) (rooms.World, error)
}

type Runtime interface {
	IsRunning(context.Context, string, string) (bool, error)
}

type statusRuntime interface {
	Status(context.Context, string, string) (shards.RuntimeStatus, error)
}

type identifiedRuntime interface {
	StatusFor(context.Context, string, string) (shards.RuntimeStatus, error)
}

const liveObservationWindow = 2 * time.Minute
const listCollectionConcurrency = 4

type CurrentSampler interface {
	CurrentSnapshot(context.Context, string, string) (Observation, error)
}

type CurrentWorldSampler interface {
	CurrentWorld(context.Context, string, string) (Observation, shards.RuntimeStatus, error)
}

type StoppedSampler interface {
	StoppedSnapshot(context.Context, string, string) (Observation, error)
}

type Service struct {
	rooms   RoomCatalog
	runtime Runtime
	store   *Store
	sampler Sampler
	now     func() time.Time
}

type listWorldRead struct {
	runtimeStatus    shards.RuntimeStatus
	observation      Observation
	hasSnapshot      bool
	absent           bool
	observationState ObservationState
	observationCode  string
	observationError string
}

func NewService(roomCatalog RoomCatalog, runtime Runtime, store *Store, sampler Sampler) (*Service, error) {
	if roomCatalog == nil || runtime == nil || store == nil || sampler == nil {
		return nil, errors.New("world state dependencies are required")
	}
	return &Service{rooms: roomCatalog, runtime: runtime, store: store, sampler: sampler, now: time.Now}, nil
}

func (s *Service) List(ctx context.Context, roomID string) (List, error) {
	if err := ctx.Err(); err != nil {
		return List{}, err
	}
	finishRoom := requesttiming.Start(ctx, "state.catalog")
	room, err := s.rooms.Room(roomID)
	finishRoom()
	if err != nil {
		return List{}, err
	}
	finishWorlds := requesttiming.Start(ctx, "state.catalog")
	worlds, err := s.rooms.Worlds(roomID)
	finishWorlds()
	if err != nil {
		return List{}, err
	}
	reads := make([]listWorldRead, len(worlds))
	if room.Managed {
		finishCollect := requesttiming.Start(ctx, "state.collect")
		reads, err = s.collectCurrentWorlds(ctx, room, worlds)
		finishCollect()
		if err != nil {
			return List{}, err
		}
	}
	defer requesttiming.Start(ctx, "state.result")()
	items := make([]Snapshot, len(worlds))
	var last *time.Time
	for index, world := range worlds {
		item := s.snapshotFromRead(roomID, world, reads[index])
		items[index] = item
		if !item.ObservedAt.IsZero() && (last == nil || item.ObservedAt.After(*last)) {
			capturedAt := item.ObservedAt.UTC()
			last = &capturedAt
		}
	}
	return List{Items: items, Total: len(items), LastRefreshedAt: last}, nil
}

func (s *Service) collectCurrentWorlds(ctx context.Context, room rooms.Room, worlds []rooms.World) ([]listWorldRead, error) {
	reads := make([]listWorldRead, len(worlds))
	if len(worlds) == 0 {
		return reads, nil
	}
	indexes := make(chan int)
	var workers sync.WaitGroup
	for range min(len(worlds), listCollectionConcurrency) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range indexes {
				world := worlds[index]
				worldCtx := requesttiming.Scope(ctx, "world_"+world.ID)
				reads[index] = s.readCurrentWorld(worldCtx, room, world)
			}
		}()
	}
	for index := range worlds {
		select {
		case indexes <- index:
		case <-ctx.Done():
			close(indexes)
			workers.Wait()
			return nil, ctx.Err()
		}
	}
	close(indexes)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return reads, nil
}

func (s *Service) readCurrentWorld(ctx context.Context, room rooms.Room, world rooms.World) listWorldRead {
	defer requesttiming.Start(ctx, "sample")()
	if sampler, ok := s.sampler.(CurrentWorldSampler); ok {
		observation, status, err := sampler.CurrentWorld(ctx, room.ID, world.ID)
		if !errors.Is(err, dstruntime.ErrWorldStateReadUnsupported) {
			if status.State == "" {
				status = shards.RuntimeStatus{State: shards.RuntimeUnknown, Code: "RUNTIME_STATUS_UNAVAILABLE"}
				if err != nil {
					status.Message = err.Error()
				}
			}
			return worldReadResult(status, observation, err)
		}
	}
	finishStatus := requesttiming.Start(ctx, "status")
	status, err := s.worldRuntimeStatus(ctx, room, world)
	finishStatus()
	if err != nil {
		return listWorldRead{runtimeStatus: shards.RuntimeStatus{
			State: shards.RuntimeUnknown, Code: "RUNTIME_STATUS_UNAVAILABLE", Message: err.Error(),
		}}
	}
	if status.State == shards.RuntimeStopped || status.State == shards.RuntimeFailed {
		if reader, ok := s.sampler.(StoppedSampler); ok {
			observation, err := reader.StoppedSnapshot(ctx, room.ID, world.ID)
			return worldReadResult(status, observation, err)
		}
	} else if status.State == shards.RuntimeRunning {
		if reader, ok := s.sampler.(CurrentSampler); ok {
			observation, err := reader.CurrentSnapshot(ctx, room.ID, world.ID)
			return worldReadResult(status, observation, err)
		}
	}
	return listWorldRead{runtimeStatus: status}
}

func worldReadResult(status shards.RuntimeStatus, observation Observation, err error) listWorldRead {
	read := listWorldRead{runtimeStatus: status}
	if err == nil {
		err = validateObservation(observation)
	}
	switch {
	case err == nil:
		read.observation, read.hasSnapshot = observation, true
	case errors.Is(err, dstruntime.ErrWorldStateAbsent):
		// Stopped with no JSON is an empty state, not a failed collection.
		read.absent = true
	case errors.Is(err, dstruntime.ErrWorldStatePending):
		read.observationState = ObservationStatePending
		read.observationCode = "WORLD_STATE_PENDING"
	case errors.Is(err, dstruntime.ErrRuntimeRefreshDeferred):
		read.observationState = ObservationStateDeferred
		read.observationCode = ObservationCodeRoomOperationInProgress
	default:
		read.observationState = ObservationStateFailed
		read.observationError = err.Error()
	}
	return read
}

func (s *Service) snapshotFromRead(roomID string, world rooms.World, read listWorldRead) Snapshot {
	item := Snapshot{RoomID: roomID, WorldID: world.ID, WorldName: world.Name, WorldRole: string(world.Role)}
	if read.hasSnapshot {
		item = snapshotFromObservation(roomID, world, read.observation, s.now())
	}
	if read.runtimeStatus.State == "" {
		read.runtimeStatus.State = shards.RuntimeUnknown
	}
	item.Paused = read.runtimeStatus.Paused
	item = decorateSnapshot(item, read.runtimeStatus.State, s.now())
	item.RuntimeCode, item.RuntimeMessage = read.runtimeStatus.Code, read.runtimeStatus.Message
	item.ObservationState, item.ObservationCode, item.ObservationError = read.observationState, read.observationCode, read.observationError
	return item
}

func decorateSnapshot(snapshot Snapshot, runtimeState shards.RuntimeState, now time.Time) Snapshot {
	age := time.Duration(0)
	if !snapshot.ObservedAt.IsZero() {
		age = now.UTC().Sub(snapshot.ObservedAt.UTC())
		if age < 0 {
			age = 0
		}
	}
	snapshot.RuntimeState = string(runtimeState)
	if runtimeState != shards.RuntimeRunning {
		snapshot.Paused = nil
	}
	snapshot.AgeSeconds = int64(age / time.Second)
	switch {
	case snapshot.ObservedAt.IsZero():
		snapshot.Freshness = FreshnessUnavailable
	case snapshot.Paused != nil && *snapshot.Paused:
		snapshot.Freshness = FreshnessPaused
	case runtimeState == shards.RuntimeRunning && age <= liveObservationWindow:
		snapshot.Freshness = FreshnessLive
	case runtimeState == shards.RuntimeRunning:
		snapshot.Freshness = FreshnessDelayed
	case runtimeState == shards.RuntimeStopped || runtimeState == shards.RuntimeFailed:
		snapshot.Freshness = FreshnessStopped
	case runtimeState == shards.RuntimeStarting:
		snapshot.Freshness = FreshnessDelayed
	default:
		snapshot.Freshness = FreshnessUnavailable
	}
	snapshot.Stale = snapshot.Freshness != FreshnessLive && snapshot.Freshness != FreshnessPaused
	return snapshot
}

func (s *Service) History(roomID, worldID string, limit int) (History, error) {
	if limit < 1 || limit > 720 || strings.TrimSpace(worldID) == "" {
		return History{}, ErrInvalidFilter
	}
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return History{}, err
	}
	world, err := s.rooms.World(roomID, worldID)
	if err != nil {
		return History{}, err
	}
	items, total, err := s.store.History(roomID, worldID, limit)
	if err != nil {
		return History{}, err
	}
	status := shards.RuntimeStatus{State: shards.RuntimeUnknown}
	if room.Managed {
		if runtime, ok := s.runtime.(identifiedRuntime); ok {
			if current, statusErr := runtime.StatusFor(context.Background(), room.ID, world.ID); statusErr == nil {
				status = current
			}
		} else if runtime, ok := s.runtime.(statusRuntime); ok {
			if current, statusErr := runtime.Status(context.Background(), room.DirectoryName, world.DirectoryName); statusErr == nil {
				status = current
			}
		} else if running, runningErr := s.runtime.IsRunning(context.Background(), room.DirectoryName, world.DirectoryName); runningErr == nil {
			if running {
				status.State = shards.RuntimeRunning
			} else {
				status.State = shards.RuntimeStopped
			}
		}
	}
	for index := range items {
		items[index] = decorateSnapshot(items[index], status.State, s.now())
		items[index].RuntimeCode = status.Code
		items[index].RuntimeMessage = status.Message
	}
	return History{Items: items, Total: total, Limit: limit, WorldID: worldID}, nil
}

func (s *Service) WorldTargets(roomID string) ([]rooms.World, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return nil, err
	}
	if !room.Managed {
		return nil, ErrRoomNotManaged
	}
	return s.rooms.Worlds(roomID)
}

func (s *Service) RefreshWorld(ctx context.Context, roomID, worldID string) (RefreshResult, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return RefreshResult{}, err
	}
	if !room.Managed {
		return RefreshResult{}, ErrRoomNotManaged
	}
	world, err := s.rooms.World(roomID, worldID)
	if err != nil {
		return RefreshResult{}, err
	}
	read := s.readCurrentWorld(ctx, room, world)
	if ctx.Err() != nil {
		return RefreshResult{}, ctx.Err()
	}
	if !read.hasSnapshot && !read.absent && read.observationState != ObservationStatePending {
		message := read.observationError
		if message == "" {
			message = read.runtimeStatus.Message
		}
		return RefreshResult{}, fmt.Errorf("%w: %s", dstruntime.ErrSnapshotUnavailable, message)
	}
	snapshot := s.snapshotFromRead(roomID, world, read)
	message := "世界状态已读取"
	if read.observationState == ObservationStatePending {
		message = "等待首次采集"
	} else if read.absent {
		message = "暂无世界状态数据"
	}
	return RefreshResult{
		WorldID: world.ID, WorldName: world.Name, ObservedAt: snapshot.ObservedAt,
		Message: message, Snapshot: snapshot,
	}, nil
}

// SampleWorld is used only by explicit history collection, not page refreshes.
func (s *Service) SampleWorld(ctx context.Context, roomID, worldID string) (RefreshResult, error) {
	result, err := s.RefreshWorld(ctx, roomID, worldID)
	if err != nil {
		return RefreshResult{}, err
	}
	if result.Snapshot.RuntimeState != string(shards.RuntimeRunning) {
		return RefreshResult{}, ErrWorldNotRunning
	}
	if result.ObservedAt.IsZero() {
		return RefreshResult{}, dstruntime.ErrWorldStatePending
	}
	stored, err := s.store.Append(result.Snapshot)
	if err != nil {
		return RefreshResult{}, err
	}
	result.Snapshot.ID = stored.ID
	result.Message = "世界状态已记录"
	return result, nil
}

func (s *Service) worldRuntimeStatus(ctx context.Context, room rooms.Room, world rooms.World) (shards.RuntimeStatus, error) {
	if runtime, ok := s.runtime.(identifiedRuntime); ok {
		return runtime.StatusFor(ctx, room.ID, world.ID)
	}
	if runtime, ok := s.runtime.(statusRuntime); ok {
		return runtime.Status(ctx, room.DirectoryName, world.DirectoryName)
	}
	running, err := s.runtime.IsRunning(ctx, room.DirectoryName, world.DirectoryName)
	if err != nil {
		return shards.RuntimeStatus{State: shards.RuntimeUnknown}, err
	}
	if running {
		return shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}, nil
	}
	return shards.RuntimeStatus{State: shards.RuntimeStopped}, nil
}

func snapshotFromObservation(roomID string, world rooms.World, observation Observation, fallbackTime time.Time) Snapshot {
	observedAt := observation.CapturedAt.UTC()
	if observedAt.IsZero() {
		observedAt = fallbackTime.UTC()
	}
	return Snapshot{
		RoomID: roomID, WorldID: world.ID, WorldName: world.Name, WorldRole: string(world.Role),
		Season: observation.Season, Phase: observation.Phase, Cycles: observation.Cycles,
		ElapsedDaysInSeason: observation.ElapsedDaysInSeason, RemainingDaysInSeason: observation.RemainingDaysInSeason,
		SeasonProgress: observation.SeasonProgress, DayProgress: observation.DayProgress, PhaseProgress: observation.PhaseProgress,
		Precipitation: observation.Precipitation, MoonPhase: observation.MoonPhase, Temperature: observation.Temperature,
		Wetness: observation.Wetness, Moisture: observation.Moisture, MoistureCeil: observation.MoistureCeil,
		PrecipitationRate: observation.PrecipitationRate, NightmarePhase: observation.NightmarePhase,
		NightmareProgress: observation.NightmareProgress, HostPerformance: observation.HostPerformance,
		ObservedAt: observedAt,
	}
}

func validateObservation(observation Observation) error {
	for _, value := range []string{observation.Season, observation.Phase, observation.Precipitation, observation.MoonPhase, observation.NightmarePhase} {
		if len([]rune(value)) > 64 || strings.ContainsRune(value, '\x00') {
			return ErrInvalidSnapshot
		}
	}
	for _, value := range []*int{observation.Cycles, observation.ElapsedDaysInSeason, observation.RemainingDaysInSeason} {
		if value != nil && *value < 0 {
			return ErrInvalidSnapshot
		}
	}
	if observation.HostPerformance != nil && (*observation.HostPerformance < 0 || *observation.HostPerformance > 2) {
		return ErrInvalidSnapshot
	}
	for _, value := range []*float64{
		observation.SeasonProgress, observation.DayProgress, observation.PhaseProgress, observation.Temperature,
		observation.Wetness, observation.Moisture, observation.MoistureCeil, observation.PrecipitationRate, observation.NightmareProgress,
	} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return ErrInvalidSnapshot
		}
	}
	return nil
}
