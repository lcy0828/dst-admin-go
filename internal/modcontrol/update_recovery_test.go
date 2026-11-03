package modcontrol

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/shared"
)

type recoveryFiles struct {
	committed bool
	*testRuntimeFileObserver
	mu       sync.Mutex
	versions map[string]string
	calls    map[string]int
	fail     string
}

func (f *recoveryFiles) ObserveRuntimeModInventory(_ context.Context, id, installation string) (*shared.RuntimeModFilesObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	manifest := ""
	if f.committed {
		manifest = f.versions[id]
	}
	return &shared.RuntimeModFilesObservation{InstallationID: installation, Mods: map[string]shared.RuntimeModFileState{"100": {Status: shared.RuntimeModFileReady, Version: f.versions[id], SteamManifestID: manifest}}}, nil
}
func (f *recoveryFiles) LinkInstallationMods(context.Context, string, string, []string) error {
	return nil
}
func (f *recoveryFiles) UpdateInstallationMods(_ context.Context, id, _ string, _ []string, _ io.Writer) (mods.ActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[id]++
	if f.fail == id {
		return mods.ActionResult{}, errors.New("download failed")
	}
	f.versions[id] = "2"
	return mods.ActionResult{ModIDs: []string{"100"}}, nil
}
func TestPartialDownloadRetryVerifiesCompletedTargetAndNeverRepeatsUncertainTarget(t *testing.T) {
	files := &recoveryFiles{committed: true, testRuntimeFileObserver: &testRuntimeFileObserver{}, versions: map[string]string{"a": "1", "b": "1"}, calls: map[string]int{}, fail: "b"}
	s := &Service{installer: files, runtimeFiles: files, mods: &testModCatalog{described: map[string]mods.SteamMod{"100": {ID: "100", Version: "2", SteamManifestID: "2"}}}}
	targets := map[string]modpublication.AppliedPlacement{"a": {TargetID: "a", InstallationID: "default"}, "b": {TargetID: "b", InstallationID: "default"}}
	var mu sync.Mutex
	states := map[string]mods.ModUpdateTarget{}
	record := func(target mods.ModUpdateTarget) error {
		mu.Lock()
		defer mu.Unlock()
		states[target.TargetID] = target
		return nil
	}
	ctx := mods.WithUpdateTracking(context.Background(), nil, record)
	if _, _, err := s.downloadRoomInstallations(ctx, targets, []string{"100"}, io.Discard); err == nil {
		t.Fatal("partial failure lost")
	}
	if states["a"].Status != "verified" || states["b"].Status != "failed" {
		t.Fatal(states)
	}
	files.fail = ""
	previous := []mods.ModUpdateTarget{states["a"], states["b"]}
	ctx = mods.WithUpdateTracking(context.Background(), previous, record)
	if _, _, err := s.downloadRoomInstallations(ctx, targets, []string{"100"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if files.calls["a"] != 1 || files.calls["b"] != 2 {
		t.Fatal(files.calls)
	}
	files.versions["b"] = "1"
	previous[1].Status = "uncertain"
	ctx = mods.WithUpdateTracking(context.Background(), previous, record)
	if _, _, err := s.downloadRoomInstallations(ctx, targets, []string{"100"}, io.Discard); err == nil {
		t.Fatal("uncertain target repeated")
	}
	if files.calls["b"] != 2 {
		t.Fatal(files.calls)
	}
	// Read-only verification can resolve the uncertain download without replay.
	files.versions["b"] = "2"
	files.committed = false
	if _, _, err := s.downloadRoomInstallations(ctx, targets, []string{"100"}, io.Discard); err == nil {
		t.Fatal("version alone cleared an uncertain receipt")
	}
	files.committed = true
	if _, _, err := s.downloadRoomInstallations(ctx, targets, []string{"100"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if files.calls["b"] != 2 || states["b"].Status != "verified" {
		t.Fatal(files.calls, states)
	}
}

func TestUpdateScopeChangesOnConfigurationAndPlacementButNotMetadata(t *testing.T) {
	f := newSnapshotFixture(t)
	s, err := NewService(f.source, f.mods, &snapshotCoordinator{source: f.source})
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.ModUpdateScope(context.Background(), f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.mods.described = map[string]mods.SteamMod{"100": {ID: "100", Version: "new"}}
	after, err := s.ModUpdateScope(context.Background(), f.room.ID)
	if err != nil || before.Revision != after.Revision {
		t.Fatal(after, err)
	}
	f.driver.overrides[f.room.DirectoryName+"\x00"+f.caves.DirectoryName] = []byte(`return { ["workshop-100"]={enabled=false} }`)
	after, err = s.ModUpdateScope(context.Background(), f.room.ID)
	if err != nil || before.Revision == after.Revision {
		t.Fatal(after, err)
	}
	before = after
	f.placements.executions[f.room.ID][1].Target.Config.InstallationID = "changed"
	after, err = s.ModUpdateScope(context.Background(), f.room.ID)
	if err == nil && before.Revision == after.Revision {
		t.Fatal("placement change ignored")
	}
}
