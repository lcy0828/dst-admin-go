package configuration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/topology"
)

type inlineConfigurationDriver struct {
	*runtimedriver.Native
	inline     int
	staged     int
	err        error
	publishErr error
}

func (d *inlineConfigurationDriver) ApplyConfiguration(ctx context.Context, target runtimedriver.Target, op runtimedriver.Operation, descriptor runtimedriver.ConfigurationDescriptor, data []byte, expected map[string]string) ([]string, error) {
	d.inline++
	if op.LeaseID == "" || op.FencingToken == 0 {
		return nil, errors.New("missing lease")
	}
	if d.err != nil {
		return nil, d.err
	}
	return d.Native.ApplyConfiguration(ctx, target, op, descriptor, data, expected)
}

func (d *inlineConfigurationDriver) BeginConfiguration(ctx context.Context, target runtimedriver.Target, op runtimedriver.Operation, descriptor runtimedriver.ConfigurationDescriptor) (int64, error) {
	d.staged++
	return d.Native.BeginConfiguration(ctx, target, op, descriptor)
}

func (d *inlineConfigurationDriver) PublishConfiguration(ctx context.Context, target runtimedriver.Target, op runtimedriver.Operation, id, scope string) error {
	if d.publishErr != nil {
		return d.publishErr
	}
	return d.Native.PublishConfiguration(ctx, target, op, id, scope)
}

type splitConfigurationRuntimes struct {
	drivers map[string]*inlineConfigurationDriver
	targets map[string]runtimedriver.Target
}

func (r splitConfigurationRuntimes) DriverTarget(_ context.Context, _, worldID string) (runtimedriver.Driver, runtimedriver.Target, error) {
	return r.drivers[worldID], r.targets[worldID], nil
}

func TestSplitConfigurationSaveKeepsNodeFieldsAndRollback(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "second target failure"}[failSecond], func(t *testing.T) {
			room := rooms.Room{ID: "room", DirectoryName: "Cluster", Managed: true}
			runtimes := splitConfigurationRuntimes{drivers: map[string]*inlineConfigurationDriver{}, targets: map[string]runtimedriver.Target{}}
			placements := publicationPlacementCatalog{}
			paths, originals := map[string]string{}, map[string]string{}
			for _, worldID := range []string{"Master", "Caves"} {
				root := t.TempDir()
				if err := os.MkdirAll(filepath.Join(root, "Cluster", worldID), 0750); err != nil {
					t.Fatal(err)
				}
				paths[worldID] = filepath.Join(root, "Cluster", "cluster.ini")
				originals[worldID] = "[NETWORK]\ncluster_name=old\n[CUSTOM]\nowner=" + worldID + "\n[SHARD]\nmaster_ip=127.0.0.1\nmaster_port=10888\n"
				if err := os.WriteFile(paths[worldID], []byte(originals[worldID]), 0640); err != nil {
					t.Fatal(err)
				}
				native, err := runtimedriver.NewNative(root, publicationNativeControl{})
				if err != nil {
					t.Fatal(err)
				}
				runtimes.drivers[worldID] = &inlineConfigurationDriver{Native: native}
				runtimes.targets[worldID] = runtimedriver.Target{TargetID: "agent:" + worldID, InstallationID: "default", RoomID: room.ID, WorldID: worldID, Cluster: "Cluster", Shard: worldID, TopologyRevision: "r1"}
				placements.values = append(placements.values, topology.ExecutionPlacement{Room: room, World: rooms.World{ID: worldID, RoomID: room.ID, DirectoryName: worldID, IsMaster: worldID == "Master"}})
			}
			if failSecond {
				runtimes.drivers["Caves"].publishErr = errors.New("Caves write failed")
			}
			publisher, err := NewRemotePublisher(placements, runtimes, &publicationLeaseService{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = publisher.Publish(context.Background(), PublicationRequest{
				RoomID: room.ID, Scope: PublicationShared, Files: []string{"cluster.ini"},
				Payload:       []rooms.ProvisionFile{{Name: "cluster.ini", Mode: 0640, Data: []byte("[NETWORK]\ncluster_name=new\n[CUSTOM]\nowner=Master\n")}},
				ExpectedFiles: map[string]string{"cluster.ini": configurationFileDigest([]byte(originals["Master"]))},
			})
			if (err != nil) != failSecond {
				t.Fatalf("save failure=%v", err)
			}
			for worldID, driver := range runtimes.drivers {
				if driver.inline != 0 || driver.staged != 1 {
					t.Fatalf("split save used inline path: inline=%d staged=%d", driver.inline, driver.staged)
				}
				data, err := os.ReadFile(paths[worldID])
				if err != nil {
					t.Fatal(err)
				}
				if failSecond {
					if string(data) != originals[worldID] {
						t.Fatalf("%s was not rolled back: %q", worldID, data)
					}
				} else if !strings.Contains(string(data), worldID) || !strings.Contains(string(data), "new") {
					t.Fatalf("%s lost node-owned INI fields: %q", worldID, data)
				}
			}
		})
	}
}

func TestSmallConfigurationPublicationUsesOneCallAndKeepsLegacyFallback(t *testing.T) {
	for _, scenario := range []string{"inline", "legacy", "offline", "conflict"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "Cluster", "Master"), 0750); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "Cluster", "cluster.ini")
			before, after := []byte("[CUSTOM]\nmanual=yes\n"), []byte("[CUSTOM]\nmanual=yes\n[NETWORK]\ncluster_name=new\n")
			if err := os.WriteFile(path, before, 0640); err != nil {
				t.Fatal(err)
			}
			native, err := runtimedriver.NewNative(root, publicationNativeControl{})
			if err != nil {
				t.Fatal(err)
			}
			driver := &inlineConfigurationDriver{Native: native}
			if scenario == "offline" {
				driver.err = errors.New("node disconnected")
			}
			room := rooms.Room{ID: "room", DirectoryName: "Cluster", Managed: true}
			world := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", IsMaster: true}
			target := runtimedriver.Target{TargetID: "agent:node", InstallationID: "default", RoomID: room.ID, WorldID: world.ID,
				Cluster: "Cluster", Shard: "Master", TopologyRevision: "r1", CapabilitiesKnown: true,
				Capabilities: []runtimedriver.Capability{runtimedriver.CapabilityConfigPublish, runtimedriver.CapabilityConfigRead}}
			if scenario != "legacy" {
				target.Capabilities = append(target.Capabilities, runtimedriver.CapabilityConfigApply)
			}
			publisher, err := NewRemotePublisher(publicationPlacementCatalog{values: []topology.ExecutionPlacement{{Room: room, World: world}}},
				publicationRuntimeCatalog{driver: driver, targets: map[string]runtimedriver.Target{world.ID: target}}, &publicationLeaseService{})
			if err != nil {
				t.Fatal(err)
			}
			expected := configurationFileDigest(before)
			if scenario == "conflict" {
				expected = configurationFileDigest([]byte("old page contents"))
			}
			result, err := publisher.Publish(context.Background(), PublicationRequest{
				RoomID: room.ID, Scope: PublicationShared, Files: []string{"cluster.ini"},
				Payload:       []rooms.ProvisionFile{{Name: "cluster.ini", Data: after, Mode: 0640}},
				ExpectedFiles: map[string]string{"cluster.ini": expected},
			})
			failed := scenario == "offline" || scenario == "conflict"
			if (err != nil) != failed || (result.PublishedCount == 1) == failed {
				t.Fatalf("result=%#v, error=%v", result, err)
			}
			if scenario == "conflict" && !errors.Is(err, ErrRevisionConflict) {
				t.Fatalf("revision error lost: %v", err)
			}
			if scenario == "legacy" {
				if driver.inline != 0 || driver.staged != 1 {
					t.Fatalf("legacy calls inline=%d staged=%d", driver.inline, driver.staged)
				}
			} else if driver.inline != 1 || driver.staged != 0 {
				t.Fatalf("inline calls=%d staged=%d", driver.inline, driver.staged)
			}
			data, readErr := os.ReadFile(path)
			want := after
			if failed {
				want = before
			}
			if readErr != nil || string(data) != string(want) {
				t.Fatalf("unexpected file contents: %q, %v", data, readErr)
			}
		})
	}
}
