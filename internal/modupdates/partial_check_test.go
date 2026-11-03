package modupdates

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/mods"
)

func TestPartialCheckAllowsOnlyConfirmedUpdatesAndPersistsWarnings(t *testing.T) {
	f := newUpdateTestFixture(t)
	last := f.now.Add(-time.Hour)
	if _, err := f.store.SaveState(State{RoomID: "room", Status: StatusAvailable, AvailableModIDs: []string{"stale"}, LastCheckedAt: &last}); err != nil {
		t.Fatal(err)
	}
	f.catalog.updates = []string{"200"}
	f.catalog.checkSummary = &mods.ModUpdateCheck{CheckedAt: f.now, Complete: false, Total: 2, Unknown: 1, Outdated: 1, Items: []mods.ModUpdateCheckItem{{ModID: "100", Status: "unknown", Reason: "metadata_timeout"}, {ModID: "200", Status: "outdated"}}}
	state, err := f.service.check(context.Background(), "room", "check", false)
	if err != nil || state.Status != StatusAvailable || len(state.AvailableModIDs) != 1 || state.AvailableModIDs[0] != "200" || !state.LastCheckedAt.Equal(last) {
		t.Fatal(state, err)
	}
	state, err = f.service.applyNow(context.Background(), "room", "apply")
	if err != nil || state.Status != StatusLoaded || len(f.catalog.updateCalls) != 1 || f.catalog.updateCalls[0] != "200" || f.restarter.calls != 1 {
		t.Fatal(state, err, f.catalog.updateCalls, f.restarter.calls)
	}
	reloaded, err := f.store.State("room")
	if err != nil || reloaded.CheckSummary == nil || reloaded.CheckSummary.Complete || reloaded.CheckSummary.Unknown != 1 {
		t.Fatal(reloaded, err)
	}
}

func TestUnknownOnlyCheckNeverMeansLatestOrRestarts(t *testing.T) {
	f := newUpdateTestFixture(t)
	f.catalog.updates = nil
	f.catalog.checkSummary = &mods.ModUpdateCheck{CheckedAt: f.now, Complete: false, Total: 1, Unknown: 1, Items: []mods.ModUpdateCheckItem{{ModID: "100", Status: "unknown"}}}
	state, err := f.service.applyNow(context.Background(), "room", "apply")
	if err != nil || state.Status != StatusBlocked || state.ErrorCode != "MOD_UPDATE_VERSION_UNCONFIRMED" || state.LastCheckedAt != nil || f.restarter.calls != 0 || len(f.catalog.updateCalls) != 0 {
		t.Fatal(state, err)
	}
}

func TestPendingDownloadProgressSurvivesReopenAndScopeChanges(t *testing.T) {
	f := newUpdateTestFixture(t)
	f.service.catalog = sharedRoomCatalog{f.catalog}
	state := DefaultState("room", f.now)
	ctx, err := f.service.trackDownloads(context.Background(), &state, []string{"100"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mods.RecordUpdateTarget(ctx, mods.ModUpdateTarget{TargetID: "local", InstallationID: "default", ModIDs: []string{"100"}, Status: "uncertain"}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(f.db, "test_").State("room")
	if err != nil || reloaded.PendingUpdate == nil || reloaded.PendingUpdate.Targets[0].Status != "uncertain" {
		t.Fatal(reloaded, err)
	}
	if _, err := f.service.trackDownloads(context.Background(), &reloaded, []string{"100"}); err != nil {
		t.Fatal(err)
	}
	f.catalog.installationRooms = []string{"room", "new-room"}
	if _, err := f.service.trackDownloads(context.Background(), &reloaded, []string{"100"}); err == nil {
		t.Fatal("expanded scope accepted")
	}
}

func TestUncertainRestartNeverRepeatsAcrossCheckAndManualRetry(t *testing.T) {
	f := newUpdateTestFixture(t)
	f.restarter.err = errors.New("connection lost")
	state, err := f.service.applyNow(context.Background(), "room", "job")
	if err == nil || state.PendingUpdate.RestartingRoomID != "room" {
		t.Fatal(state, err)
	}
	f.restarter.err = nil
	if _, err = f.service.check(context.Background(), "room", "check", false); err == nil {
		t.Fatal("lost uncertainty")
	}
	if _, err = f.service.applyNow(context.Background(), "room", "retry"); err == nil {
		t.Fatal("repeated uncertain restart")
	}
	if f.restarter.calls != 1 {
		t.Fatal(f.restarter.calls)
	}
}

func TestApplyRejectsChangedConfirmationBeforeWrites(t *testing.T) {
	for _, request := range []ApplyRequest{{ExpectedScopeRevision: "old"}, {ModIDs: []string{"100"}}} {
		f := newUpdateTestFixture(t)
		f.catalog.updates = []string{"200"}
		f.catalog.checkSummary = &mods.ModUpdateCheck{Complete: true, Scope: mods.ModUpdateScope{Revision: "new"}, Items: []mods.ModUpdateCheckItem{{ModID: "200", Status: "outdated"}}}
		ctx := context.WithValue(context.Background(), applyRequestKey{}, request)
		state, err := f.service.applyNow(ctx, "room", "job")
		if err == nil || state.ErrorCode != "MOD_UPDATE_PLAN_CHANGED" || len(f.catalog.updateCalls) != 0 || f.restarter.calls != 0 {
			t.Fatal(state, err)
		}
	}
}

func TestNewDownloadFailureCannotKeepPreviousPreparedMarker(t *testing.T) {
	f := newUpdateTestFixture(t)
	state := State{RoomID: "room", Status: StatusPrepared, PreparedAt: &f.now, AvailableModIDs: []string{"100"}, PendingUpdate: &PendingUpdate{ModIDs: []string{"100"}, AffectedRoomIDs: []string{"room"}}}
	f.catalog.updateErr = errors.New("download failed")
	state, err := f.service.downloadUpdates(context.Background(), state, []string{"200"})
	if err == nil || state.PreparedAt != nil || state.PendingUpdate == nil || len(state.PendingUpdate.ModIDs) != 2 {
		t.Fatal(state, err)
	}
}

func TestPersistedTerminalWorldResultsRecoverBeforeRoomCheckpoint(t *testing.T) {
	f := newUpdateTestFixture(t)
	state := State{RoomID: "room", PendingUpdate: &PendingUpdate{RestartingRoomID: "room", RestartWorlds: map[string][]RestartWorld{"room": {{WorldID: "master", Status: "ready"}, {WorldID: "caves", Status: "ready"}}}}}
	if err := f.service.validatePendingScope(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if state.PendingUpdate.RestartingRoomID != "" || len(state.PendingUpdate.RestartedRoomIDs) != 1 {
		t.Fatal(state.PendingUpdate)
	}
}
