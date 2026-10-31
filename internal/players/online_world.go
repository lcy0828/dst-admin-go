package players

import (
	"context"
	"errors"
	"time"

	"dont/internal/playerinventory"
	"dont/internal/rooms"
	"dont/shared"
)

// OnlineWorld confirms shard-local entity evidence, never a cluster client list.
// Only location reads may be refreshed; callers must not replay a mutation after
// it has been dispatched, including when a player migrates during execution.
func (s *Service) OnlineWorld(ctx context.Context, roomID, playerID string) (rooms.World, error) {
	if !playerinventory.ValidRequest(shared.PlayerInventoryRequest{PlayerID: playerID, Source: "live"}) {
		return rooms.World{}, &shared.InventoryError{Code: "INVALID_PLAYER_ID"}
	}
	r, err := s.managedRoom(roomID)
	if err != nil {
		return rooms.World{}, err
	}
	worlds, err := s.rooms.Worlds(r.ID)
	if err != nil {
		return rooms.World{}, err
	}
	if len(worlds) > 64 {
		return rooms.World{}, ErrInvalidFilter
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	world, err := s.inventoryLiveWorld(ctx, r, worlds, playerID, false, false)
	if errors.Is(err, errInventoryLocationChanged) {
		world, err = s.inventoryLiveWorld(ctx, r, worlds, playerID, true, false)
	}
	if err != nil {
		return rooms.World{}, err
	}
	if world == nil {
		return rooms.World{}, &shared.InventoryError{Code: "PLAYER_NOT_ONLINE"}
	}
	return *world, nil
}
