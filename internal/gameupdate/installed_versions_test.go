package gameupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/topology"
	"dont/shared"
)

type installedTargets []agents.RuntimeTarget

func (t installedTargets) RuntimeTargets() ([]agents.RuntimeTarget, error) { return t, nil }

type installedDriverFunc func(context.Context, runtimedriver.Target) (shared.RuntimeGameVersionResult, error)

func (f installedDriverFunc) ObserveGameVersion(ctx context.Context, target runtimedriver.Target) (shared.RuntimeGameVersionResult, error) {
	return f(ctx, target)
}

func TestInstalledVersionsReadsSharedInstallationOnceAndRespectsSelectedNode(t *testing.T) {
	var remoteCalls, localCalls atomic.Int32
	room := rooms.Room{ID: "room", DirectoryName: "all", Managed: true}
	catalog := releaseSnapshotRooms{room: room, worlds: []rooms.World{{ID: "master", DirectoryName: "Master"}, {ID: "caves", DirectoryName: "Caves"}, {ID: "other", DirectoryName: "Other"}}}
	placements := releaseSnapshotPlacements{
		"master": {AppliedTargetID: "agent:node", AppliedInstallationID: "native", Revision: "revision"},
		"caves":  {AppliedTargetID: "agent:node", AppliedInstallationID: "native", Revision: "revision"},
		"other":  {AppliedTargetID: "local", AppliedInstallationID: "default", Revision: "revision"},
	}
	service := NewInstalledVersionService(catalog, placements, installedTargets{{ID: "agent:node", Online: true}, {ID: "local", Online: true}},
		installedDriverFunc(func(context.Context, runtimedriver.Target) (shared.RuntimeGameVersionResult, error) {
			localCalls.Add(1)
			return shared.RuntimeGameVersionResult{}, nil
		}),
		installedDriverFunc(func(_ context.Context, target runtimedriver.Target) (shared.RuntimeGameVersionResult, error) {
			remoteCalls.Add(1)
			if target.TargetID != "agent:node" || target.InstallationID != "native" || target.Cluster != "all" {
				return shared.RuntimeGameVersionResult{}, errors.New("wrong target")
			}
			return shared.RuntimeGameVersionResult{Installed: true, GameVersion: "747465", SteamBuild: "24700372", Branch: "public", ObservedAt: time.Now()}, nil
		}))
	for i := 0; i < 2; i++ {
		values, err := service.Installed(context.Background(), []string{"agent:node"})
		if err != nil || len(values) != 1 || values[0].GameVersion != "747465" || values[0].Error != "" {
			t.Fatalf("values=%#v err=%v", values, err)
		}
	}
	if remoteCalls.Load() != 2 || localCalls.Load() != 0 {
		t.Fatalf("calls remote=%d local=%d", remoteCalls.Load(), localCalls.Load())
	}
	placements["caves"] = topology.ExecutionPlacement{AppliedTargetID: "agent:offline", AppliedInstallationID: "native", Revision: "revision"}
	values, err := service.Installed(context.Background(), nil)
	if err != nil || len(values) != 3 {
		t.Fatalf("values=%#v err=%v", values, err)
	}
	if values[1].TargetID != "agent:offline" || values[1].Error == "" {
		t.Fatalf("offline result=%#v", values)
	}
}

func TestVersionSummaryNeverCallsSteamAndReflectsManualVersionChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "steamapps"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "steamapps", "appmanifest_343050.acf"), []byte(`"AppState" { "buildid" "24700372" "UserConfig" {} }`), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Service{config: Config{ServerPath: root, AppID: "343050"}, now: time.Now, official: fixedOfficialRelease{version: "747465"}}
	for _, version := range []string{"747465", "740477"} {
		if err := os.WriteFile(filepath.Join(root, "version.txt"), []byte(version), 0600); err != nil {
			t.Fatal(err)
		}
		report := s.VersionSummary(context.Background(), false)
		if report.GameVersion != version || report.UpToDate == nil || *report.UpToDate != (version == "747465") || report.OfficialCheckError != "" {
			t.Fatalf("report=%#v", report)
		}
	}
}
