package players

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"dont/internal/rooms"
	"dont/shared"
)

type onlineRuntime interface {
	Status(context.Context, string, string) (shared.ShardRuntimeStatus, error)
}

// RuntimeOnlineCounter reads the game roster only when maintenance is requested.
// Pausing does not imply an empty room, and stored telemetry may predate a logout.
type RuntimeOnlineCounter struct {
	rooms   RoomCatalog
	runtime onlineRuntime
	probe   Probe
}

func NewRuntimeOnlineCounter(catalog RoomCatalog, runtime onlineRuntime, probe Probe) *RuntimeOnlineCounter {
	return &RuntimeOnlineCounter{rooms: catalog, runtime: runtime, probe: probe}
}

func (c *RuntimeOnlineCounter) OnlinePlayers(ctx context.Context, roomID string) (int, error) {
	room, err := c.rooms.Room(roomID)
	if err != nil {
		return 0, err
	}
	if !room.Managed {
		return 0, rooms.ErrRoomNotManaged
	}
	worlds, err := c.rooms.Worlds(room.ID)
	if err != nil {
		return 0, err
	}
	if len(worlds) == 0 {
		return 0, rooms.ErrWorldNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, consoleProbeTimeout)
	defer cancel()
	type result struct {
		players []Observation
		err     error
	}
	results := make([]result, len(worlds))
	var workers sync.WaitGroup
	slots := make(chan struct{}, 4)
	for index, world := range worlds {
		workers.Add(1)
		go func(index int, world rooms.World) {
			defer workers.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				results[index].err = ctx.Err()
				return
			}
			status, err := c.runtime.Status(ctx, room.ID, world.ID)
			if err == nil {
				switch {
				case !status.SessionExists && (status.State == "stopped" || status.State == "failed"):
					return
				case status.SessionExists && (status.State == "running" || status.State == "starting"):
					results[index].players, err = c.probe.Snapshot(ctx, room.ID, world.ID)
				default:
					err = fmt.Errorf("运行状态 %q，暂不能确认在线人数", status.State)
				}
			}
			if err != nil {
				results[index].err = fmt.Errorf("%s: %w", world.Name, err)
			}
		}(index, world)
	}
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	users := make(map[string]bool)
	var failures []error
	for _, result := range results {
		if result.err != nil {
			failures = append(failures, result.err)
		}
		for _, player := range result.players {
			users[player.ID] = true
		}
	}
	return len(users), errors.Join(failures...)
}
