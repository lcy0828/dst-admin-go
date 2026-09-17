package topology

import (
	"context"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/shared"
)

func TestNextAppliedShardLinksDoesNotBorrowFutureTopology(t *testing.T) {
	current := []storedShardLink{{
		SourceTargetID: "agent:old-secondary", MasterTargetID: "local",
		Address: "192.168.2.42", Port: 10888, Mode: ShardLinkLAN,
	}}
	desired := []storedShardLink{{
		SourceTargetID: "agent:new-secondary", MasterTargetID: "agent:new-master",
		Address: "192.168.2.50", Port: 10888, Mode: ShardLinkLAN,
	}}
	pending := []storedPlacement{
		{WorldID: "master", WorldRole: rooms.WorldRoleMaster, DesiredTargetID: "agent:new-master", AppliedTargetID: "local"},
		{WorldID: "caves", WorldRole: rooms.WorldRoleCaves, DesiredTargetID: "agent:new-secondary", AppliedTargetID: "agent:old-secondary"},
	}
	if links := nextAppliedShardLinks(current, desired, pending); len(links) != 1 || links[0].Address != "192.168.2.42" {
		t.Fatalf("pending applied links=%#v", links)
	}
	for index := range pending {
		pending[index].AppliedTargetID = pending[index].DesiredTargetID
	}
	if links := nextAppliedShardLinks(current, desired, pending); len(links) != 1 || links[0].Address != "192.168.2.50" {
		t.Fatalf("aligned applied links=%#v", links)
	}
}

type shardLinkTargetCatalog struct {
	items     []agents.RuntimeTargetInventory
	reachable map[string]int64
}

func TestNewServiceUsesExplicitNetworkDiscovery(t *testing.T) {
	discovery := shardLinkTargetCatalog{}
	service, err := NewService(topologyRoomCatalog{}, topologyTargetCatalog{}, newTopologyTestStore(t), discovery)
	if err != nil {
		t.Fatal(err)
	}
	if service.egress == nil || service.endpointProbe == nil {
		t.Fatal("explicit network discovery was not wired into the topology service")
	}
}

func (c shardLinkTargetCatalog) RuntimeTargetInventories(context.Context) ([]agents.RuntimeTargetInventory, error) {
	return append([]agents.RuntimeTargetInventory(nil), c.items...), nil
}

func (c shardLinkTargetCatalog) DetectEgress(context.Context, string, shared.RuntimeNetworkRegion) (shared.RuntimeNetworkResult, error) {
	return shared.RuntimeNetworkResult{Address: "203.0.113.20", Region: shared.RuntimeNetworkRegionGlobal, ObservedAt: time.Now().UTC()}, nil
}

func (c shardLinkTargetCatalog) ListenNetworkEndpoint(ctx context.Context, _ string, _ string, _ int, tokens []string, _ time.Duration) ([]string, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(300 * time.Millisecond):
		return append([]string(nil), tokens...), nil
	}
}

func (c shardLinkTargetCatalog) ProbeNetworkEndpoints(_ context.Context, _ string, endpoints []shared.RuntimeNetworkEndpointRequest, _ time.Duration) ([]shared.RuntimeNetworkEndpointResult, error) {
	result := make([]shared.RuntimeNetworkEndpointResult, 0, len(endpoints))
	for _, endpoint := range endpoints {
		latency, reachable := c.reachable[endpoint.Address]
		value := shared.RuntimeNetworkEndpointResult{Address: endpoint.Address, Port: endpoint.Port, Reachable: reachable, LatencyMillis: latency}
		if !reachable {
			value.Error = "endpoint probe timed out"
		}
		result = append(result, value)
	}
	return result, nil
}

func TestDiscoverShardLinksAutoSelectsOnlyReachableCandidate(t *testing.T) {
	service, room, master, caves := newShardLinkDiscoveryService(t, map[string]int64{"192.168.2.20": 2})
	snapshot, err := service.Topology(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.DiscoverShardLinks(context.Background(), room.ID, ShardLinkDiscoveryRequest{
		ExpectedRevision: snapshot.Revision,
		Placements: []PlacementInput{
			{WorldID: master.ID, TargetID: "agent:master"},
			{WorldID: caves.ID, TargetID: "agent:caves"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ActiveProbe || result.MasterTargetID != "agent:master" || result.MasterPort == 0 || len(result.Links) != 1 {
		t.Fatalf("discovery=%#v", result)
	}
	link := result.Links[0]
	if link.AutoSelected == nil || link.AutoSelected.Address != "192.168.2.20" || !link.AutoSelected.Reachable {
		t.Fatalf("link=%#v", link)
	}
	if len(link.Candidates) < 2 {
		t.Fatalf("expected interface and public candidates: %#v", link.Candidates)
	}
}

func TestDiscoverShardLinksLeavesMultipleReachableCandidatesForUserChoice(t *testing.T) {
	service, room, master, caves := newShardLinkDiscoveryService(t, map[string]int64{
		"192.168.2.20": 2,
		"100.64.0.20":  8,
	})
	snapshot, err := service.Topology(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.DiscoverShardLinks(context.Background(), room.ID, ShardLinkDiscoveryRequest{
		ExpectedRevision: snapshot.Revision,
		Placements: []PlacementInput{
			{WorldID: master.ID, TargetID: "agent:master"},
			{WorldID: caves.ID, TargetID: "agent:caves"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Links) != 1 || result.Links[0].AutoSelected != nil {
		t.Fatalf("multiple reachable candidates must require a choice: %#v", result.Links)
	}
}

func TestTopologyUpdatePersistsSelectedShardLink(t *testing.T) {
	service, room, master, caves := newShardLinkDiscoveryService(t, map[string]int64{"192.168.2.20": 2})
	snapshot, err := service.Topology(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.Update(context.Background(), room.ID, UpdateRequest{
		ExpectedRevision: snapshot.Revision,
		Placements: []PlacementInput{
			{WorldID: master.ID, TargetID: "agent:master"},
			{WorldID: caves.ID, TargetID: "agent:caves"},
		},
		ShardLinks: []ShardLinkInput{{
			SourceTargetID: "agent:caves", Address: "192.168.2.20", Port: inventoryRoom(room.DirectoryName).MasterPort, Mode: ShardLinkLAN,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.ShardLinks) != 1 || updated.ShardLinks[0].Address != "192.168.2.20" || updated.ShardLinks[0].MasterTargetID != "agent:master" {
		t.Fatalf("updated shard links=%#v", updated.ShardLinks)
	}
	reloaded, err := service.Topology(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.ShardLinks) != 1 || reloaded.ShardLinks[0] != updated.ShardLinks[0] {
		t.Fatalf("reloaded shard links=%#v", reloaded.ShardLinks)
	}
}

func newShardLinkDiscoveryService(t *testing.T, reachable map[string]int64) (*Service, rooms.Room, rooms.World, rooms.World) {
	t.Helper()
	now := time.Now().UTC()
	room := rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "房间", Managed: true}
	master := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster, IsMaster: true}
	caves := rooms.World{ID: "caves", RoomID: room.ID, DirectoryName: "Caves", Name: "洞穴", Role: rooms.WorldRoleCaves}
	masterTarget := agents.RuntimeTarget{
		ID: "agent:master", AgentID: "master", Name: "Master 主机", Kind: agents.RuntimeKindAgent,
		Status: agents.RuntimeStatusReady, Online: true, Configured: true,
		IPAddresses: []string{"192.168.2.20", "100.64.0.20"},
	}
	cavesTarget := agents.RuntimeTarget{
		ID: "agent:caves", AgentID: "caves", Name: "Caves 主机", Kind: agents.RuntimeKindAgent,
		Status: agents.RuntimeStatusReady, Online: true, Configured: true,
		IPAddresses: []string{"192.168.2.21"},
	}
	localTarget := agents.RuntimeTarget{ID: localTargetID, Name: "本机", Kind: agents.RuntimeKindLocal, Status: agents.RuntimeStatusReady, Online: true, Configured: true}
	localInventory := runtimeInventory(localTarget, 4, 4, []shared.RoomInventoryReport{inventoryRoom(room.DirectoryName, "Master", "Caves")}, nil, now)
	masterInventory := runtimeInventory(masterTarget, 4, 4, nil, nil, now)
	cavesInventory := runtimeInventory(cavesTarget, 4, 4, nil, nil, now)
	targets := shardLinkTargetCatalog{items: []agents.RuntimeTargetInventory{localInventory, masterInventory, cavesInventory}, reachable: reachable}
	service, err := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {master, caves}}},
		targets,
		newTopologyTestStore(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	return service, room, master, caves
}
