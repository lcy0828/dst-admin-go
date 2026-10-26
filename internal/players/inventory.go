package players

import (
	"context"
	"errors"
	"time"

	"dont/internal/playerinventory"
	"dont/internal/rooms"
	"dont/internal/shards"
	"dont/shared"
)

type inventoryRuntime interface {
	ReadPlayerInventory(context.Context, string, string, shared.PlayerInventoryRequest) (shared.PlayerInventory, error)
}

// Inventory deliberately does not require a historical Player row: imported
// saves can be inspected by their known user ID before any telemetry exists.
func (s *Service) Inventory(ctx context.Context, roomID, worldID string, q shared.PlayerInventoryRequest) (shared.PlayerInventory, error) {
	if q.Source == "" {
		q.Source = "auto"
	}
	if !playerinventory.ValidRequest(q) {
		return shared.PlayerInventory{}, &shared.InventoryError{Code: "INVALID_PLAYER_ID"}
	}
	key := roomID + "\x00" + worldID + "\x00" + q.PlayerID + "\x00" + q.Source + "\x00" + q.SourceID
	return s.inventoryReads.Do(ctx, key, key, func(ctx context.Context) (shared.PlayerInventory, error) {
		return s.readInventory(ctx, roomID, worldID, q)
	})
}

func (s *Service) readInventory(ctx context.Context, roomID, worldID string, q shared.PlayerInventoryRequest) (shared.PlayerInventory, error) {
	room, err := s.managedRoom(roomID)
	if err != nil {
		return shared.PlayerInventory{}, err
	}
	worlds, err := s.rooms.Worlds(room.ID)
	if err != nil {
		return shared.PlayerInventory{}, err
	}
	if len(worlds) > 64 {
		return shared.PlayerInventory{}, ErrInvalidFilter
	}
	if worldID != "" {
		world, err := s.rooms.World(room.ID, worldID)
		if err != nil {
			return shared.PlayerInventory{}, err
		}
		worlds = []rooms.World{world}
	}
	reader, ok := s.sender.(inventoryRuntime)
	if coordinated, available := s.commander.(inventoryRuntime); available {
		reader, ok = coordinated, true
	}
	if !ok {
		return shared.PlayerInventory{}, &shared.InventoryError{Code: "INVENTORY_CAPABILITY_UNAVAILABLE"}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if q.Source == "auto" || q.Source == "live" {
		world, locateErr := s.inventoryLiveWorld(ctx, room, worlds, q.PlayerID)
		if locateErr != nil {
			return shared.PlayerInventory{}, locateErr
		}
		if world != nil {
			q.Source = "live"
			return readInventoryWorld(ctx, reader, room.ID, *world, q)
		}
		if q.Source == "live" {
			return shared.PlayerInventory{}, &shared.InventoryError{Code: "PLAYER_NOT_ONLINE"}
		}
	}
	q.Source = "save"
	var results []shared.PlayerInventory
	var candidates []shared.InventoryCandidate
	for _, world := range worlds {
		value, readErr := readInventoryWorld(ctx, reader, room.ID, world, q)
		if readErr == nil {
			results = append(results, value)
			candidates = append(candidates, shared.InventoryCandidate{SourceID: value.SourceID, WorldID: world.ID, WorldName: world.Name, SessionID: value.SessionID, FileModifiedAt: value.FileModifiedAt})
			continue
		}
		code := playerinventory.Code(readErr)
		if code == "SOURCE_AMBIGUOUS" {
			candidates = append(candidates, value.Candidates...)
			continue
		}
		if code == "PLAYER_SNAPSHOT_NOT_FOUND" {
			continue
		}
		return value, readErr
	}
	if len(results) == 1 && len(candidates) == 1 {
		return results[0], nil
	}
	if len(candidates) > 0 {
		return shared.PlayerInventory{}, &shared.InventoryError{Code: "SOURCE_AMBIGUOUS", Candidates: candidates}
	}
	return shared.PlayerInventory{}, &shared.InventoryError{Code: "PLAYER_SNAPSHOT_NOT_FOUND"}
}

func readInventoryWorld(ctx context.Context, reader inventoryRuntime, roomID string, world rooms.World, q shared.PlayerInventoryRequest) (shared.PlayerInventory, error) {
	value, err := reader.ReadPlayerInventory(ctx, roomID, world.ID, q)
	value.RoomID, value.WorldID, value.WorldName = roomID, world.ID, world.Name
	for i := range value.Candidates {
		value.Candidates[i].WorldID = world.ID
		value.Candidates[i].WorldName = world.Name
	}
	var typed *shared.InventoryError
	if errors.As(err, &typed) && len(value.Candidates) > 0 {
		err = &shared.InventoryError{Code: typed.Code, Candidates: value.Candidates}
	}
	return value, err
}

func (s *Service) inventoryLiveWorld(ctx context.Context, room rooms.Room, worlds []rooms.World, player string) (*rooms.World, error) {
	known, err := s.store.Get(room.ID, player)
	if err == nil && known.Online && known.PresenceConflict {
		return nil, &shared.InventoryError{Code: "PLAYER_LOCATION_UNCERTAIN"}
	}
	if err == nil && known.Online && known.WorldConfirmed && known.PresenceStatus == FreshnessLive && known.PresenceObservedAt != nil && s.now().Sub(*known.PresenceObservedAt) >= -2*time.Second && s.now().Sub(*known.PresenceObservedAt) <= 15*time.Second {
		for _, world := range worlds {
			if world.ID == known.WorldID {
				if known.GameplayState == GameplayStateMigrating {
					return nil, &shared.InventoryError{Code: "PLAYER_LOCATION_UNCERTAIN"}
				}
				if known.GameplayState == GameplayStateLoading || known.GameplayState == GameplayStateSelectingCharacter {
					return nil, &shared.InventoryError{Code: "PLAYER_NOT_READY"}
				}
				return &world, nil
			}
		}
	}
	var selected *rooms.World
	for _, world := range worlds {
		status, err := s.worldStatus(ctx, room, world)
		if err != nil {
			return nil, err
		}
		if status.State == shards.RuntimeStopped || status.State == shards.RuntimeFailed {
			continue
		}
		if status.State != shards.RuntimeRunning {
			return nil, &shared.InventoryError{Code: "PLAYER_LOCATION_UNCERTAIN"}
		}
		observations, _, freshness, _, observedAt, _, err := s.readSnapshot(ctx, room.ID, world.ID, s.now().UTC())
		age := s.now().Sub(observedAt)
		if err != nil || freshness != FreshnessLive || age < -2*time.Second || age > 15*time.Second {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, &shared.InventoryError{Code: "INVENTORY_TIMEOUT"}
			}
			return nil, &shared.InventoryError{Code: "PLAYER_LOCATION_UNCERTAIN"}
		}
		for _, observation := range observations {
			if observation.ID != player {
				continue
			}
			if selected != nil || observation.GameplayState == GameplayStateMigrating {
				return nil, &shared.InventoryError{Code: "PLAYER_LOCATION_UNCERTAIN"}
			}
			if observation.GameplayState == GameplayStateLoading || observation.GameplayState == GameplayStateSelectingCharacter {
				return nil, &shared.InventoryError{Code: "PLAYER_NOT_READY"}
			}
			copy := world
			selected = &copy
		}
	}
	return selected, nil
}
