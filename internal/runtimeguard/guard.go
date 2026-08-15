package runtimeguard

import (
	"errors"
	"fmt"

	"dont/internal/rooms"
)

const ErrorCode = "REMOTE_RUNTIME_MUTATION_UNAVAILABLE"

var ErrRemoteMutationUnavailable = errors.New("room contains remotely placed shards; distributed file mutation is unavailable")

type WorldCatalog interface {
	Worlds(string) ([]rooms.World, error)
}

type PlacementResolver interface {
	IsLocalPlacement(string, string) (bool, error)
}

type MutationGuard interface {
	RequireRoom(string) error
	RequireWorld(string, string) error
}

type Guard struct {
	worlds     WorldCatalog
	placements PlacementResolver
}

func New(worlds WorldCatalog, placements PlacementResolver) (*Guard, error) {
	if worlds == nil || placements == nil {
		return nil, errors.New("runtime mutation guard dependencies are required")
	}
	return &Guard{worlds: worlds, placements: placements}, nil
}

func (g *Guard) RequireRoom(roomID string) error {
	worlds, err := g.worlds.Worlds(roomID)
	if err != nil {
		return err
	}
	for _, world := range worlds {
		if err := g.RequireWorld(roomID, world.ID); err != nil {
			return err
		}
	}
	return nil
}

func (g *Guard) RequireWorld(roomID, worldID string) error {
	local, err := g.placements.IsLocalPlacement(roomID, worldID)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("%w: world %s", ErrRemoteMutationUnavailable, worldID)
	}
	return nil
}

var _ MutationGuard = (*Guard)(nil)
