package configuration

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/runtimefiles"
	"dont/internal/topology"
)

type modPublicationRuntimeProbe struct {
	publicationRuntimeCatalog
	resolveError error
	resolved     int
	trusted      int
}

func (p *modPublicationRuntimeProbe) DriverTarget(ctx context.Context, roomID, worldID string) (runtimedriver.Driver, runtimedriver.Target, error) {
	p.resolved++
	if p.resolveError != nil {
		return nil, runtimedriver.Target{}, p.resolveError
	}
	return p.publicationRuntimeCatalog.DriverTarget(ctx, roomID, worldID)
}

func (p *modPublicationRuntimeProbe) TrustedTarget(runtimedriver.Target) (runtimedriver.Driver, error) {
	p.trusted++
	return p.driver, nil
}

func TestModPublicationResolvesLegacyInstallationBeforeWriting(t *testing.T) {
	offline := errors.New("remote target offline")
	for _, tc := range []struct {
		name                 string
		targetID             string
		storedInstallation   string
		resolvedInstallation string
		resolvedTarget       string
		resolvedRevision     string
		resolveError         error
		wantError            error
		staleFile            bool
		wantResolves         int
	}{
		{name: "legacy_remote", targetID: "agent:node", resolvedInstallation: "native", wantResolves: 1},
		{name: "legacy_local_named_default", targetID: "local", resolvedInstallation: "native", wantResolves: 1},
		{name: "explicit_remote_fast_path", targetID: "agent:node", storedInstallation: "native", resolvedInstallation: "native"},
		{name: "explicit_local_fast_path", targetID: "local", storedInstallation: "native", resolvedInstallation: "native"},
		{name: "different_explicit_installation", targetID: "agent:node", storedInstallation: "other", wantError: runtimedriver.ErrTopologyChanged},
		{name: "legacy_default_changed", targetID: "agent:node", resolvedInstallation: "other", wantError: runtimedriver.ErrTopologyChanged, wantResolves: 1},
		{name: "legacy_target_changed", targetID: "agent:node", resolvedInstallation: "native", resolvedTarget: "agent:other", wantError: runtimedriver.ErrTopologyChanged, wantResolves: 1},
		{name: "legacy_revision_changed", targetID: "agent:node", resolvedInstallation: "native", resolvedRevision: "new-revision", wantError: runtimedriver.ErrTopologyChanged, wantResolves: 1},
		{name: "remote_offline_does_not_fall_back", targetID: "agent:node", resolveError: offline, wantError: offline, wantResolves: 1},
		{name: "manual_edit_still_protected", targetID: "agent:node", storedInstallation: "native", staleFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			worldRoot := filepath.Join(root, "Cluster", "Master")
			if err := os.MkdirAll(worldRoot, 0o750); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(worldRoot, "modoverrides.lua")
			original := []byte("return { [\"workshop-100\"] = { enabled = true, configuration_options = { custom = 42 } } }\n")
			if err := os.WriteFile(path, original, 0o640); err != nil {
				t.Fatal(err)
			}
			driver, err := runtimedriver.NewNative(root, publicationNativeControl{})
			if err != nil {
				t.Fatal(err)
			}
			room := rooms.Room{ID: "room", DirectoryName: "Cluster", Managed: true}
			world := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", IsMaster: true}
			target := runtimedriver.Target{
				TargetID: tc.targetID, InstallationID: "native", RoomID: room.ID, WorldID: world.ID,
				Cluster: room.DirectoryName, Shard: world.DirectoryName, TopologyRevision: "revision",
			}
			resolved := target
			resolved.InstallationID = tc.resolvedInstallation
			if tc.resolvedTarget != "" {
				resolved.TargetID = tc.resolvedTarget
			}
			if tc.resolvedRevision != "" {
				resolved.TopologyRevision = tc.resolvedRevision
			}
			runtimes := &modPublicationRuntimeProbe{
				publicationRuntimeCatalog: publicationRuntimeCatalog{driver: driver, targets: map[string]runtimedriver.Target{world.ID: resolved}},
				resolveError:              tc.resolveError,
			}
			publisher, err := NewRemotePublisher(publicationPlacementCatalog{values: []topology.ExecutionPlacement{{
				Room: room, World: world, Revision: "revision", AppliedTargetID: tc.targetID, AppliedInstallationID: tc.storedInstallation,
			}}}, runtimes, &publicationLeaseService{})
			if err != nil {
				t.Fatal(err)
			}
			current := original
			for index, enabled := range []bool{false, true} {
				next := []byte(fmt.Sprintf("return { [\"workshop-100\"] = { enabled = %t, configuration_options = { custom = 42 } } }\n", enabled))
				expected := fmt.Sprintf("%x", sha256.Sum256(current))
				if tc.staleFile {
					expected = fmt.Sprintf("%x", sha256.Sum256([]byte("stale")))
				}
				count, err := publisher.PublishModOverrides(context.Background(), room.ID, []runtimedriver.ModOverridesUpdate{{
					Target: target, ExpectedSHA256: expected, Content: next,
				}})
				if tc.wantError != nil || tc.staleFile {
					if tc.staleFile {
						var conflict *runtimefiles.ConfigurationConflictError
						if !errors.As(err, &conflict) {
							t.Fatalf("expected file conflict, got %v", err)
						}
					} else if !errors.Is(err, tc.wantError) {
						t.Fatalf("error=%v, want %v", err, tc.wantError)
					}
					if count != 0 {
						t.Fatalf("unexpected writes: %d", count)
					}
				} else if err != nil || count != 1 {
					t.Fatalf("enabled=%t writes=%d error=%v", enabled, count, err)
				} else {
					current = next
				}
				data, readErr := os.ReadFile(path)
				if readErr != nil || string(data) != string(current) {
					t.Fatalf("unexpected file content: %q, error=%v", data, readErr)
				}
				if runtimes.resolved != (index+1)*tc.wantResolves || tc.wantResolves > 0 && runtimes.trusted != 0 {
					t.Fatalf("unnecessary target resolution: resolved=%d trusted=%d", runtimes.resolved, runtimes.trusted)
				}
				if tc.wantError != nil || tc.staleFile {
					break
				}
			}
		})
	}
}
