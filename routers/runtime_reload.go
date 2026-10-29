package routers

import (
	"context"
	"fmt"

	"dont/internal/deploymentprofile"
	"dont/internal/rooms"
	"dont/internal/shards"
	"dont/internal/systemsettings"
)

// Only the gateway switch preserves the local executor and its control owner.
// Any concurrent environment change retains the stopped-world requirement.
func preservesLocalWorlds(profile deploymentprofile.Profile, changedFields []string) bool {
	return profile.LocalExecutorEnabled && !profile.MemberEnabled &&
		len(changedFields) == 1 && changedFields[0] == "fleet.controllerEnabled"
}

type reloadRoomCatalog interface {
	List() ([]rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
}

type reloadShardStatus interface {
	Status(context.Context, string, string) (shards.RuntimeStatus, error)
}

func requireStoppedLocalWorlds(ctx context.Context, catalog reloadRoomCatalog, control reloadShardStatus) error {
	localRooms, err := catalog.List()
	if err != nil {
		return err
	}
	for _, room := range localRooms {
		worlds, err := catalog.Worlds(room.ID)
		if err != nil {
			return err
		}
		for _, world := range worlds {
			status, err := control.Status(ctx, room.DirectoryName, world.DirectoryName)
			if err != nil || status.SessionExists || status.State != shards.RuntimeStopped && status.State != shards.RuntimeFailed {
				return fmt.Errorf("%w（%s / %s）", systemsettings.ErrLocalWorldsRunning, room.Name, world.Name)
			}
		}
	}
	return nil
}
