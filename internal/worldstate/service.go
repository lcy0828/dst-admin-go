package worldstate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

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

type CurrentSampler interface {
	CurrentSnapshot(context.Context, string, string) (Observation, error)
}

type Service struct {
	rooms   RoomCatalog
	runtime Runtime
	store   *Store
	sampler Sampler
	now     func() time.Time
}

func NewService(roomCatalog RoomCatalog, runtime Runtime, store *Store, sampler Sampler) (*Service, error) {
	if roomCatalog == nil || runtime == nil || store == nil || sampler == nil {
		return nil, errors.New("world state dependencies are required")
	}
	return &Service{rooms: roomCatalog, runtime: runtime, store: store, sampler: sampler, now: time.Now}, nil
}

func (s *Service) List(ctx context.Context, roomID string) (List, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return List{}, err
	}
	items, err := s.store.Current(roomID)
	if err != nil {
		return List{}, err
	}
	if current, ok := s.sampler.(CurrentSampler); ok && room.Managed {
		worlds, worldsErr := s.rooms.Worlds(roomID)
		if worldsErr != nil {
			return List{}, worldsErr
		}
		byWorld := make(map[string]int, len(items))
		for index, item := range items {
			byWorld[item.WorldID] = index
		}
		for _, world := range worlds {
			if err := ctx.Err(); err != nil {
				return List{}, err
			}
			running, runningErr := s.worldRunning(ctx, room, world)
			if runningErr != nil || !running {
				continue
			}
			observation, snapshotErr := current.CurrentSnapshot(ctx, roomID, world.ID)
			if snapshotErr != nil || validateObservation(observation) != nil {
				continue
			}
			live := snapshotFromObservation(roomID, world, observation, s.now())
			if index, exists := byWorld[world.ID]; exists {
				if live.ObservedAt.After(items[index].ObservedAt) {
					items[index] = live
				}
				continue
			}
			byWorld[world.ID] = len(items)
			items = append(items, live)
		}
	}
	worlds, worldsErr := s.rooms.Worlds(roomID)
	if worldsErr != nil {
		return List{}, worldsErr
	}
	worldByID := make(map[string]rooms.World, len(worlds))
	for _, world := range worlds {
		worldByID[world.ID] = world
	}
	for index := range items {
		world, exists := worldByID[items[index].WorldID]
		if !exists || !room.Managed {
			items[index] = decorateSnapshot(items[index], shards.RuntimeUnknown, s.now())
			continue
		}
		state := shards.RuntimeUnknown
		if runtime, ok := s.runtime.(identifiedRuntime); ok {
			status, statusErr := runtime.StatusFor(ctx, room.ID, world.ID)
			if statusErr == nil {
				state = status.State
			}
		} else if runtime, ok := s.runtime.(statusRuntime); ok {
			status, statusErr := runtime.Status(ctx, room.DirectoryName, world.DirectoryName)
			if statusErr == nil {
				state = status.State
			}
		} else if running, runningErr := s.runtime.IsRunning(ctx, room.DirectoryName, world.DirectoryName); runningErr == nil {
			if running {
				state = shards.RuntimeRunning
			} else {
				state = shards.RuntimeStopped
			}
		}
		items[index] = decorateSnapshot(items[index], state, s.now())
	}
	var last *time.Time
	for _, item := range items {
		if last == nil || item.ObservedAt.After(*last) {
			value := item.ObservedAt.UTC()
			last = &value
		}
	}
	return List{Items: items, Total: len(items), LastRefreshedAt: last}, nil
}

func decorateSnapshot(snapshot Snapshot, runtimeState shards.RuntimeState, now time.Time) Snapshot {
	age := now.UTC().Sub(snapshot.ObservedAt.UTC())
	if age < 0 {
		age = 0
	}
	snapshot.RuntimeState = string(runtimeState)
	snapshot.AgeSeconds = int64(age / time.Second)
	switch {
	case snapshot.ObservedAt.IsZero():
		snapshot.Freshness = FreshnessUnavailable
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
	snapshot.Stale = snapshot.Freshness != FreshnessLive
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
	state := shards.RuntimeUnknown
	if room.Managed {
		if runtime, ok := s.runtime.(identifiedRuntime); ok {
			if status, statusErr := runtime.StatusFor(context.Background(), room.ID, world.ID); statusErr == nil {
				state = status.State
			}
		} else if runtime, ok := s.runtime.(statusRuntime); ok {
			if status, statusErr := runtime.Status(context.Background(), room.DirectoryName, world.DirectoryName); statusErr == nil {
				state = status.State
			}
		} else if running, runningErr := s.runtime.IsRunning(context.Background(), room.DirectoryName, world.DirectoryName); runningErr == nil {
			if running {
				state = shards.RuntimeRunning
			} else {
				state = shards.RuntimeStopped
			}
		}
	}
	for index := range items {
		items[index] = decorateSnapshot(items[index], state, s.now())
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
	running, err := s.worldRunning(ctx, room, world)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("inspect world runtime: %w", err)
	}
	if !running {
		return RefreshResult{}, ErrWorldNotRunning
	}
	observation, err := s.sampler.Snapshot(ctx, roomID, worldID)
	if err != nil {
		return RefreshResult{}, err
	}
	if err := validateObservation(observation); err != nil {
		return RefreshResult{}, err
	}
	observedAt := observation.CapturedAt.UTC()
	if observedAt.IsZero() {
		observedAt = s.now().UTC()
	}
	snapshot := snapshotFromObservation(roomID, world, observation, observedAt)
	stored, err := s.store.Append(snapshot)
	if err != nil {
		return RefreshResult{}, err
	}
	return RefreshResult{WorldID: world.ID, WorldName: world.Name, ObservedAt: stored.ObservedAt, Message: "世界状态已采样"}, nil
}

func (s *Service) worldRunning(ctx context.Context, room rooms.Room, world rooms.World) (bool, error) {
	if runtime, ok := s.runtime.(identifiedRuntime); ok {
		status, err := runtime.StatusFor(ctx, room.ID, world.ID)
		return status.State == shards.RuntimeRunning, err
	}
	return s.runtime.IsRunning(ctx, room.DirectoryName, world.DirectoryName)
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
		NightmareProgress: observation.NightmareProgress, ObservedAt: observedAt,
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
