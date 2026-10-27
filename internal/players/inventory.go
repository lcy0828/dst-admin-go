package players

import (
	"context"
	"errors"
	"strconv"
	"time"

	"dont/internal/playerinventory"
	"dont/internal/rooms"
	"dont/internal/shards"
	"dont/shared"
)

var errInventoryLocationChanged = errors.New("player location requires confirmation")

func uncertainInventoryLocation() error {
	return errors.Join(&shared.InventoryError{Code: "PLAYER_LOCATION_UNCERTAIN"}, errInventoryLocationChanged)
}

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
	return s.inventoryReads.Do(ctx, key+"\x00"+strconv.FormatBool(q.Refresh), key, func(ctx context.Context) (shared.PlayerInventory, error) {
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
		world, locateErr := s.inventoryLiveWorld(ctx, room, worlds, q.PlayerID, q.Refresh, worldID != "")
		retried := false
		if worldID == "" && !q.Refresh && errors.Is(locateErr, errInventoryLocationChanged) {
			retried = true
			world, locateErr = s.inventoryLiveWorld(ctx, room, worlds, q.PlayerID, true, false)
		}
		if locateErr != nil {
			return shared.PlayerInventory{}, locateErr
		}
		if world != nil {
			live := q
			live.Source = "live"
			value, readErr := readInventoryWorld(ctx, reader, room.ID, *world, live)
			code := playerinventory.Code(readErr)
			// The entity may have migrated after routing. Re-locate at most
			// once, within the same deadline and the original room scope.
			if worldID == "" && !retried && (code == "PLAYER_NOT_ONLINE" || code == "PLAYER_LOCATION_UNCERTAIN") {
				world, locateErr = s.inventoryLiveWorld(ctx, room, worlds, q.PlayerID, true, false)
				if locateErr != nil {
					return shared.PlayerInventory{}, locateErr
				}
				if world == nil {
					return shared.PlayerInventory{}, &shared.InventoryError{Code: "PLAYER_NOT_ONLINE"}
				}
				return readInventoryWorld(ctx, reader, room.ID, *world, live)
			}
			return value, readErr
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

type inventoryLocationProbe interface {
	InventorySnapshot(context.Context, string, string, bool) (SnapshotResult, error)
}

func (s *Service) inventoryLiveWorld(ctx context.Context, room rooms.Room, worlds []rooms.World, player string, force, scoped bool) (*rooms.World, error) {
	known, err := s.store.Get(room.ID, player)
	if !force && err == nil && known.Online && !known.PresenceConflict && known.WorldConfirmed &&
		known.LocalEntity != nil && *known.LocalEntity && known.PresenceStatus == FreshnessLive && known.PresenceObservedAt != nil &&
		s.now().Sub(*known.PresenceObservedAt) >= -2*time.Second && s.now().Sub(*known.PresenceObservedAt) <= 15*time.Second {
		for _, world := range worlds {
			if world.ID == known.WorldID && known.GameplayState != GameplayStateMigrating {
				status, statusErr := s.worldStatus(ctx, room, world)
				if statusErr == nil && status.State == shards.RuntimeRunning {
					return &world, nil
				}
				break
			}
		}
	}
	var selected *rooms.World
	connected, selecting, migrating := false, false, false
	for _, world := range worlds {
		status, statusErr := s.worldStatus(ctx, room, world)
		if statusErr != nil {
			return nil, statusErr
		}
		if status.State == shards.RuntimeStopped || status.State == shards.RuntimeFailed {
			continue
		}
		if status.State != shards.RuntimeRunning {
			return nil, &shared.InventoryError{Code: "PLAYER_LOCATION_UNCERTAIN"}
		}
		var result SnapshotResult
		var snapshotErr error
		if probe, ok := s.probe.(inventoryLocationProbe); ok {
			result, snapshotErr = probe.InventorySnapshot(ctx, room.ID, world.ID, force)
		} else {
			result, snapshotErr = s.readSnapshot(ctx, room.ID, world.ID, s.now().UTC())
		}
		if snapshotErr != nil || !inventorySnapshotFresh(result, s.now()) {
			if errors.Is(snapshotErr, context.DeadlineExceeded) || errors.Is(snapshotErr, ErrProbeTimedOut) {
				return nil, &shared.InventoryError{Code: "INVENTORY_TIMEOUT"}
			}
			if errors.Is(snapshotErr, context.Canceled) {
				return nil, snapshotErr
			}
			if errors.Is(snapshotErr, ErrTelemetryRecoveryDeferred) {
				return nil, &shared.InventoryError{Code: "INVENTORY_BUSY"}
			}
			return nil, errors.Join(&shared.InventoryError{Code: "PLAYER_LOCATION_UNCERTAIN"}, snapshotErr)
		}
		for _, observation := range result.Observations {
			if observation.ID != player {
				continue
			}
			connected = true
			selecting = selecting || observation.GameplayState == GameplayStateSelectingCharacter || observation.GameplayState == GameplayStateLoading
			if observation.LocalEntity == nil || !*observation.LocalEntity {
				continue
			}
			migrating = migrating || observation.GameplayState == GameplayStateMigrating
			if selected != nil {
				return nil, uncertainInventoryLocation()
			}
			copy := world
			selected = &copy
		}
	}
	if migrating {
		return nil, uncertainInventoryLocation()
	}
	if selected != nil {
		return selected, nil
	}
	if connected {
		if selecting {
			return nil, &shared.InventoryError{Code: "PLAYER_NOT_READY"}
		}
		if scoped {
			return nil, &shared.InventoryError{Code: "PLAYER_NOT_ONLINE"}
		}
		return nil, uncertainInventoryLocation()
	}
	return nil, nil
}
