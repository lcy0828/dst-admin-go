package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/internal/topology"
	"dont/shared"

	"github.com/go-ini/ini"
)

type publicationPlacementCatalog struct {
	values []topology.ExecutionPlacement
	links  []topology.ShardLink
}

func (c publicationPlacementCatalog) ResolveRoomExecutions(context.Context, string) ([]topology.ExecutionPlacement, error) {
	return append([]topology.ExecutionPlacement(nil), c.values...), nil
}

func (c publicationPlacementCatalog) AppliedPlacement(roomID, worldID string) (topology.ExecutionPlacement, error) {
	for _, value := range c.values {
		if value.Room.ID == roomID && value.World.ID == worldID {
			return value, nil
		}
	}
	return topology.ExecutionPlacement{}, rooms.ErrWorldNotFound
}

func (c publicationPlacementCatalog) ResolveAppliedShardLinks(context.Context, string) ([]topology.ShardLink, error) {
	return append([]topology.ShardLink(nil), c.links...), nil
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

type corruptingConfigurationDriver struct {
	*runtimedriver.Native
	corrupt   bool
	rollbacks int
	completes int
}

func (d *corruptingConfigurationDriver) PublishConfiguration(ctx context.Context, target runtimedriver.Target, operation runtimedriver.Operation, publicationID, scope string) error {
	if err := d.Native.PublishConfiguration(ctx, target, operation, publicationID, scope); err != nil {
		return err
	}
	d.corrupt = true
	return nil
}

func (d *corruptingConfigurationDriver) ReadConfiguration(ctx context.Context, target runtimedriver.Target, scope string) (shared.RuntimeConfigurationResult, error) {
	result, err := d.Native.ReadConfiguration(ctx, target, scope)
	if err != nil || !d.corrupt {
		return result, err
	}
	for index := range result.Files {
		if result.Files[index].Exists {
			result.Files[index].Data = []byte("corrupted read-back\n")
			break
		}
	}
	return result, nil
}

func (d *corruptingConfigurationDriver) RollbackConfiguration(ctx context.Context, target runtimedriver.Target, operation runtimedriver.Operation, publicationID, scope string) error {
	d.rollbacks++
	d.corrupt = false
	return d.Native.RollbackConfiguration(ctx, target, operation, publicationID, scope)
}

func (d *corruptingConfigurationDriver) CompleteConfiguration(ctx context.Context, target runtimedriver.Target, operation runtimedriver.Operation, publicationID, scope string) error {
	d.completes++
	return d.Native.CompleteConfiguration(ctx, target, operation, publicationID, scope)
}

type publicationMutationObserver struct{ targets []string }

func (o *publicationMutationObserver) RuntimeTargetChanged(targetID string) {
	o.targets = append(o.targets, targetID)
}

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
		publicationPlacementCatalog{values: placements}, publicationRuntimeCatalog{driver: driver, targets: targets}, &publicationLeaseService{},
	)
	if err != nil {
		t.Fatal(err)
	}
	mutations := &publicationMutationObserver{}
	if err := publisher.ConfigureMutationObserver(mutations); err != nil {
		t.Fatal(err)
	}
	result, err := publisher.Publish(context.Background(), PublicationRequest{
		RoomID: room.ID, Scope: PublicationShared, Files: []string{"cluster.ini"},
		Payload: []rooms.ProvisionFile{{Name: "cluster.ini", Data: []byte("new\n"), Mode: 0o640}},
	})
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
	if len(mutations.targets) != 1 || mutations.targets[0] != "agent:node" {
		t.Fatalf("mutation targets=%v", mutations.targets)
	}
}

func TestRuntimePublisherRejectsControllerTemplateFallback(t *testing.T) {
	publisher, err := NewRemotePublisher(publicationPlacementCatalog{}, publicationRuntimeCatalog{}, &publicationLeaseService{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = publisher.Publish(context.Background(), PublicationRequest{RoomID: "room", Scope: PublicationShared, Files: []string{"cluster.ini"}})
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("expected explicit payload error, got %v", err)
	}
}

func TestRuntimePublisherRollsBackWhenDiskReadBackDiffers(t *testing.T) {
	remoteRoot := t.TempDir()
	roomRoot := filepath.Join(remoteRoot, "Cluster")
	if err := os.MkdirAll(filepath.Join(roomRoot, "Master"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roomRoot, "cluster.ini"), []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	native, err := runtimedriver.NewNative(remoteRoot, publicationNativeControl{})
	if err != nil {
		t.Fatal(err)
	}
	driver := &corruptingConfigurationDriver{Native: native}
	room := rooms.Room{ID: "room", DirectoryName: "Cluster", Name: "Room", Managed: true}
	world := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", Name: "Master", IsMaster: true}
	publisher, err := NewRemotePublisher(
		publicationPlacementCatalog{values: []topology.ExecutionPlacement{{
			Room: room, World: world, Revision: "revision", AppliedTargetID: "agent:node",
		}}},
		publicationRuntimeCatalog{driver: driver, targets: map[string]runtimedriver.Target{
			world.ID: {TargetID: "agent:node", InstallationID: "native", RoomID: room.ID, WorldID: world.ID, Cluster: room.DirectoryName, Shard: world.DirectoryName, TopologyRevision: "revision"},
		}},
		&publicationLeaseService{},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = publisher.Publish(context.Background(), PublicationRequest{
		RoomID: room.ID, Scope: PublicationShared, Files: []string{"cluster.ini"},
		Payload: []rooms.ProvisionFile{{Name: "cluster.ini", Data: []byte("new\n"), Mode: 0o640}},
	})
	if err == nil {
		t.Fatal("expected read-back mismatch")
	}
	if driver.rollbacks != 1 || driver.completes != 0 {
		t.Fatalf("rollbacks=%d completes=%d", driver.rollbacks, driver.completes)
	}
	written, readErr := os.ReadFile(filepath.Join(roomRoot, "cluster.ini"))
	if readErr != nil || string(written) != "old\n" {
		t.Fatalf("rollback content=%q err=%v", written, readErr)
	}
}

func TestRuntimePublisherCanPublishExplicitPayloadToLocalTarget(t *testing.T) {
	root := t.TempDir()
	roomRoot := filepath.Join(root, "Cluster")
	if err := os.MkdirAll(filepath.Join(roomRoot, "Master"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roomRoot, "cluster.ini"), []byte("[NETWORK]\ncluster_name=Room\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	driver, err := runtimedriver.NewNative(root, publicationNativeControl{})
	if err != nil {
		t.Fatal(err)
	}
	room := rooms.Room{ID: "room", DirectoryName: "Cluster", Name: "Room", Managed: true}
	world := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", Name: "Master", IsMaster: true}
	publisher, err := NewRemotePublisher(
		publicationPlacementCatalog{values: []topology.ExecutionPlacement{{
			Room: room, World: world, Revision: "revision", AppliedTargetID: "local",
		}}},
		publicationRuntimeCatalog{driver: driver, targets: map[string]runtimedriver.Target{
			world.ID: {TargetID: "local", InstallationID: "default", RoomID: room.ID, WorldID: world.ID, Cluster: room.DirectoryName, Shard: world.DirectoryName, TopologyRevision: "revision"},
		}},
		&publicationLeaseService{},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := publisher.Publish(context.Background(), PublicationRequest{
		RoomID: room.ID, Scope: PublicationShared, Files: []string{"blocklist.txt"}, IncludeLocal: true,
		Payload: []rooms.ProvisionFile{{Name: "blocklist.txt", Data: []byte("KU_BLOCKED\n"), Mode: 0o640}},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(roomRoot, "blocklist.txt"))
	if err != nil || string(data) != "KU_BLOCKED\n" || result.PublishedCount != 1 {
		t.Fatalf("data=%q result=%#v err=%v", data, result, err)
	}
}

func TestRuntimePublisherPublishesOnlySelectedWorldModOverrides(t *testing.T) {
	root := t.TempDir()
	roomRoot := filepath.Join(root, "Cluster")
	for _, shard := range []string{"Master", "Caves"} {
		if err := os.MkdirAll(filepath.Join(roomRoot, shard), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(roomRoot, shard, "modoverrides.lua"), []byte("return {}\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	driver, err := runtimedriver.NewNative(root, publicationNativeControl{})
	if err != nil {
		t.Fatal(err)
	}
	room := rooms.Room{ID: "room", DirectoryName: "Cluster", Name: "Room", Managed: true}
	master := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", Name: "Master", IsMaster: true}
	caves := rooms.World{ID: "caves", RoomID: room.ID, DirectoryName: "Caves", Name: "Caves"}
	publisher, err := NewRemotePublisher(
		publicationPlacementCatalog{values: []topology.ExecutionPlacement{
			{Room: room, World: master, Revision: "revision", AppliedTargetID: "local"},
			{Room: room, World: caves, Revision: "revision", AppliedTargetID: "local"},
		}},
		publicationRuntimeCatalog{driver: driver, targets: map[string]runtimedriver.Target{
			master.ID: {TargetID: "local", InstallationID: "default", RoomID: room.ID, WorldID: master.ID, Cluster: room.DirectoryName, Shard: master.DirectoryName, TopologyRevision: "revision"},
			caves.ID:  {TargetID: "local", InstallationID: "default", RoomID: room.ID, WorldID: caves.ID, Cluster: room.DirectoryName, Shard: caves.DirectoryName, TopologyRevision: "revision"},
		}},
		&publicationLeaseService{},
	)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("return { [\"workshop-100\"] = { enabled = true } }\n")
	digest := sha256.Sum256([]byte("return {}\n"))
	published, err := publisher.PublishModOverrides(context.Background(), room.ID, []runtimedriver.ModOverridesUpdate{{
		Target:         runtimedriver.Target{TargetID: "local", InstallationID: "default", RoomID: room.ID, WorldID: caves.ID, Cluster: room.DirectoryName, Shard: caves.DirectoryName, TopologyRevision: "revision"},
		ExpectedSHA256: hex.EncodeToString(digest[:]), Content: content,
	}})
	if err != nil {
		t.Fatal(err)
	}
	masterContent, _ := os.ReadFile(filepath.Join(roomRoot, "Master", "modoverrides.lua"))
	cavesContent, _ := os.ReadFile(filepath.Join(roomRoot, "Caves", "modoverrides.lua"))
	if published != 1 || string(masterContent) != "return {}\n" || string(cavesContent) != string(content) {
		t.Fatalf("published=%d master=%q caves=%q", published, masterContent, cavesContent)
	}
}

func TestRenderPublicationPreservesRuntimeShardRouteWithoutAppliedSnapshot(t *testing.T) {
	publisher := &RemotePublisher{}
	base, _, err := publisher.archive(PublicationRequest{
		RoomID: "room", Scope: PublicationShared, Files: []string{"cluster.ini"},
		Payload: []rooms.ProvisionFile{{
			Name: "cluster.ini", Mode: 0o640,
			Data: []byte("[NETWORK]\ncluster_name = Updated\n[SHARD]\nbind_ip = 0.0.0.0\nmaster_ip = future.example\nmaster_port = 20889\n"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	current := []byte("[NETWORK]\ncluster_name = Current\nfuture_option = keep\n[CUSTOM]\nmanual_flag = keep\n[SHARD]\nbind_ip = 192.168.2.23\nmaster_ip = 192.168.2.42\nmaster_port = 10889\n")
	rendered, err := renderPublicationArchiveForTarget(
		base, runtimedriver.Target{TargetID: "agent:secondary", InstallationID: "native"},
		nil, false, true, current,
	)
	if err != nil {
		t.Fatal(err)
	}
	files, err := publicationArchiveFiles(rendered)
	if err != nil {
		t.Fatal(err)
	}
	config, err := ini.Load(files["cluster.ini"])
	if err != nil {
		t.Fatal(err)
	}
	if config.Section("NETWORK").Key("cluster_name").String() != "Updated" ||
		config.Section("NETWORK").Key("future_option").String() != "keep" ||
		config.Section("CUSTOM").Key("manual_flag").String() != "keep" ||
		config.Section("SHARD").Key("bind_ip").String() != "192.168.2.23" ||
		config.Section("SHARD").Key("master_ip").String() != "192.168.2.42" ||
		config.Section("SHARD").Key("master_port").String() != "10889" {
		t.Fatalf("rendered cluster.ini=%s", files["cluster.ini"])
	}
}

func TestRenderPublicationUsesAppliedShardRouteWhenPresent(t *testing.T) {
	publisher := &RemotePublisher{}
	base, _, err := publisher.archive(PublicationRequest{
		RoomID: "room", Scope: PublicationShared, Files: []string{"cluster.ini"},
		Payload: []rooms.ProvisionFile{{Name: "cluster.ini", Mode: 0o640, Data: []byte("[SHARD]\nmaster_ip = future.example\nmaster_port = 20889\n")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderPublicationArchiveForTarget(
		base, runtimedriver.Target{TargetID: "agent:secondary", InstallationID: "native"},
		[]topology.ShardLink{{
			SourceTargetID: "agent:secondary", SourceInstallationID: "native",
			MasterTargetID: "local", MasterInstallationID: "default",
			Address: "192.168.2.42", Port: 10889, Mode: topology.ShardLinkLAN,
		}}, false, true, []byte("[NETWORK]\nfuture_option = keep\n[CUSTOM]\nmanual_flag = keep\n[SHARD]\nmaster_ip = stale.example\nmaster_port = 30889\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := publicationArchiveFiles(rendered)
	config, err := ini.Load(files["cluster.ini"])
	if err != nil {
		t.Fatal(err)
	}
	if config.Section("SHARD").Key("bind_ip").String() != "0.0.0.0" ||
		config.Section("SHARD").Key("master_ip").String() != "192.168.2.42" ||
		config.Section("SHARD").Key("master_port").String() != "10889" ||
		config.Section("NETWORK").Key("future_option").String() != "keep" ||
		config.Section("CUSTOM").Key("manual_flag").String() != "keep" {
		t.Fatalf("rendered cluster.ini=%s", files["cluster.ini"])
	}
}
