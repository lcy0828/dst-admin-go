package configuration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/internal/topology"
)

type publicationRoomCatalog struct{ bundle rooms.ProvisionBundle }

func (c publicationRoomCatalog) ProvisionBundle(string) (rooms.ProvisionBundle, error) {
	return c.bundle, nil
}

type publicationPlacementCatalog struct{ values []topology.ExecutionPlacement }

func (c publicationPlacementCatalog) ResolveRoomExecutions(context.Context, string) ([]topology.ExecutionPlacement, error) {
	return append([]topology.ExecutionPlacement(nil), c.values...), nil
}

type publicationRuntimeCatalog struct {
	driver  runtimedriver.Driver
	targets map[string]runtimedriver.Target
}

func (c publicationRuntimeCatalog) DriverTarget(_ context.Context, _, worldID string) (runtimedriver.Driver, runtimedriver.Target, error) {
	return c.driver, c.targets[worldID], nil
}

type publicationLeaseService struct{ token uint64 }

func (s *publicationLeaseService) Acquire(_ context.Context, roomID, key string, ttl time.Duration) (operationlease.Lease, error) {
	s.token++
	return operationlease.Lease{RoomID: roomID, OperationKey: key, LeaseID: "configuration-lease", FencingToken: s.token, ExpiresAt: time.Now().UTC().Add(ttl)}, nil
}

func (s *publicationLeaseService) Renew(_ context.Context, value operationlease.Lease, ttl time.Duration) (operationlease.Lease, error) {
	value.ExpiresAt = time.Now().UTC().Add(ttl)
	return value, nil
}

func (s *publicationLeaseService) Release(operationlease.Lease) error { return nil }

type publicationNativeControl struct{}

func (publicationNativeControl) Status(context.Context, string, string) (shards.RuntimeStatus, error) {
	return shards.RuntimeStatus{State: shards.RuntimeStopped}, nil
}
func (publicationNativeControl) Start(context.Context, string, string) error        { return nil }
func (publicationNativeControl) Stop(context.Context, string, string) error         { return nil }
func (publicationNativeControl) Send(context.Context, string, string, string) error { return nil }

func TestRemotePublisherDeduplicatesSharedTargetAndPublishesConfiguration(t *testing.T) {
	remoteRoot := t.TempDir()
	roomRoot := filepath.Join(remoteRoot, "Cluster")
	for _, shard := range []string{"Master", "Caves"} {
		if err := os.MkdirAll(filepath.Join(roomRoot, shard), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(roomRoot, "cluster.ini"), []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	driver, err := runtimedriver.NewNative(remoteRoot, publicationNativeControl{})
	if err != nil {
		t.Fatal(err)
	}
	room := rooms.Room{ID: "room", DirectoryName: "Cluster", Name: "Room", Managed: true}
	master := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", Name: "Master", IsMaster: true}
	caves := rooms.World{ID: "caves", RoomID: room.ID, DirectoryName: "Caves", Name: "Caves"}
	placements := []topology.ExecutionPlacement{
		{Room: room, World: master, Revision: "revision", AppliedTargetID: "agent:node"},
		{Room: room, World: caves, Revision: "revision", AppliedTargetID: "agent:node"},
	}
	targets := map[string]runtimedriver.Target{
		master.ID: {TargetID: "agent:node", InstallationID: "default", RoomID: room.ID, WorldID: master.ID, Cluster: room.DirectoryName, Shard: master.DirectoryName, TopologyRevision: "revision"},
		caves.ID:  {TargetID: "agent:node", InstallationID: "default", RoomID: room.ID, WorldID: caves.ID, Cluster: room.DirectoryName, Shard: caves.DirectoryName, TopologyRevision: "revision"},
	}
	publisher, err := NewRemotePublisher(
		publicationRoomCatalog{bundle: rooms.ProvisionBundle{Room: room, Shared: []rooms.ProvisionFile{{Name: "cluster.ini", Data: []byte("new\n"), Mode: 0o640}}}},
		publicationPlacementCatalog{values: placements}, publicationRuntimeCatalog{driver: driver, targets: targets}, &publicationLeaseService{},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := publisher.Publish(context.Background(), PublicationRequest{RoomID: room.ID, Scope: PublicationShared, Files: []string{"cluster.ini"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.PublishedCount != 1 || result.PublicationID == "" || len(result.Warnings) != 0 {
		t.Fatalf("publication result=%#v", result)
	}
	written, err := os.ReadFile(filepath.Join(roomRoot, "cluster.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != "new\n" {
		t.Fatalf("cluster.ini=%q", written)
	}
}
