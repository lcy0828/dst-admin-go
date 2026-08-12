package worldstate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"dont/internal/rooms"
)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
	World(string, string) (rooms.World, error)
}

type Runtime interface {
	IsRunning(context.Context, string, string) (bool, error)
}

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
			running, runningErr := s.runtime.IsRunning(ctx, room.DirectoryName, world.DirectoryName)
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
	var last *time.Time
	for _, item := range items {
		if last == nil || item.ObservedAt.After(*last) {
			value := item.ObservedAt.UTC()
			last = &value
		}
	}
	return List{Items: items, Total: len(items), LastRefreshedAt: last}, nil
}

func (s *Service) History(roomID, worldID string, limit int) (History, error) {
	if limit < 1 || limit > 720 || strings.TrimSpace(worldID) == "" {
		return History{}, ErrInvalidFilter
	}
	if _, err := s.rooms.World(roomID, worldID); err != nil {
		return History{}, err
	}
	items, total, err := s.store.History(roomID, worldID, limit)
	if err != nil {
		return History{}, err
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
	running, err := s.runtime.IsRunning(ctx, room.DirectoryName, world.DirectoryName)
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
