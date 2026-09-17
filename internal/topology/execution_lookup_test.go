package topology

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/shared"
)

type isolatedExecutionCatalog struct {
	topologyRoomCatalog
	listCalls int
	syncCalls int
}

func (c *isolatedExecutionCatalog) List() ([]rooms.Room, error) {
	c.listCalls++
	return nil, errors.New("unrelated room is unreadable")
}

func (c *isolatedExecutionCatalog) SyncRuntimeCatalog([]rooms.RuntimeCatalogSource) error {
	c.syncCalls++
	return errors.New("unexpected catalog synchronization")
}

func TestExecutionLookupsOnlyReadSelectedRoom(t *testing.T) {
	room := rooms.Room{ID: "room", DirectoryName: "all", Managed: true}
	world := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", Name: "Original", Role: rooms.WorldRoleMaster}
	catalog := &isolatedExecutionCatalog{topologyRoomCatalog: topologyRoomCatalog{
		rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}},
	}}
	target := agents.RuntimeTarget{ID: "agent:node", AgentID: "node", Kind: agents.RuntimeKindAgent,
		Online: true, Configured: true, Status: agents.RuntimeStatusReady, Capabilities: []string{"shard.control.v1"}}
	inventory := runtimeInventory(target, 4, 4, []shared.RoomInventoryReport{inventoryRoom("all", "Master")}, nil, time.Now().UTC())
	store := newTopologyTestStore(t)
	record, err := store.Ensure(room.ID, []string{world.ID})
	if err != nil {
		t.Fatal(err)
	}
	record.Placements[0].AppliedTargetID, record.Placements[0].DesiredTargetID = target.ID, target.ID
	if _, err := store.Save(room.ID, record.Revision, record.Placements); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(catalog, topologyTargetCatalog{items: []agents.RuntimeTargetInventory{inventory}}, store)
	if err != nil {
		t.Fatal(err)
	}
	lookups := map[string]func() ([]ExecutionPlacement, error){
		"applied": func() ([]ExecutionPlacement, error) {
			value, err := service.AppliedPlacement(room.ID, world.ID)
			return []ExecutionPlacement{value}, err
		},
		"execution": func() ([]ExecutionPlacement, error) {
			value, err := service.ResolveExecution(context.Background(), room.ID, world.ID)
			return []ExecutionPlacement{value}, err
		},
		"cached": func() ([]ExecutionPlacement, error) {
			value, err := service.ResolveCachedExecution(context.Background(), room.ID, world.ID)
			return []ExecutionPlacement{value}, err
		},
		"room": func() ([]ExecutionPlacement, error) {
			return service.ResolveRoomExecutions(context.Background(), room.ID)
		},
		"cached room": func() ([]ExecutionPlacement, error) {
			return service.ResolveCachedRoomExecutions(context.Background(), room.ID)
		},
	}
	for name, lookup := range lookups {
		t.Run(name, func(t *testing.T) {
			catalog.worlds[room.ID][0].Name = name
			values, err := lookup()
			if err != nil || len(values) != 1 || values[0].World.Name != name || values[0].AppliedTargetID != target.ID {
				t.Fatalf("lookup ignored current room or remote placement: %#v, %v", values, err)
			}
		})
	}
	if catalog.listCalls != 0 || catalog.syncCalls != 0 {
		t.Fatalf("global catalog calls: list=%d, sync=%d", catalog.listCalls, catalog.syncCalls)
	}
}
