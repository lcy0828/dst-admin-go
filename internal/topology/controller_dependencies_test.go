package topology

import (
	"testing"

	"dont/internal/rooms"
)

func TestControllerDependenciesUsePlacementAndRetainOfflineRooms(t *testing.T) {
	for _, test := range []struct {
		name      string
		observed  []string
		applied   string
		desired   string
		dependent bool
	}{
		{"local catalog", []string{"local"}, "", "", false},
		{"offline remote catalog", []string{"agent:offline"}, "", "", true},
		{"unplaced world", nil, "", "", false},
		{"pending remote migration", []string{"local"}, "local", "agent:offline", true},
		{"return migration pending", []string{"local"}, "agent:offline", "local", true},
		{"remote placement", []string{"local"}, "agent:offline", "agent:offline", true},
		{"migrated locally with stale inventory", []string{"agent:offline"}, "local", "local", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newTopologyTestStore(t)
			world := rooms.World{ID: "master", DirectoryName: "Master", TargetIDs: test.observed}
			catalog := topologyRoomCatalog{
				rooms:  []rooms.Room{{ID: "room", Name: "测试房间", TargetIDs: test.observed}},
				worlds: map[string][]rooms.World{"room": {world}},
			}
			if test.applied != "" {
				if err := store.create(record{RoomID: "room", Revision: "test", Placements: []storedPlacement{{
					WorldID: world.ID, AppliedTargetID: test.applied, DesiredTargetID: test.desired,
				}}}); err != nil {
					t.Fatal(err)
				}
			}
			// A removed room's stale placement must not prevent the switch.
			if err := store.create(record{RoomID: "removed", Revision: "removed", Placements: []storedPlacement{{
				WorldID: "master", AppliedTargetID: "agent:removed", DesiredTargetID: "agent:removed",
			}}}); err != nil {
				t.Fatal(err)
			}
			service, err := NewService(catalog, topologyTargetCatalog{}, store)
			if err != nil {
				t.Fatal(err)
			}
			dependent, err := service.RemoteDeploymentRooms()
			if err != nil || (len(dependent) != 0) != test.dependent {
				t.Fatalf("dependencies=%+v err=%v", dependent, err)
			}
		})
	}
}
