package players

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/dstruntime"
	"dont/internal/playerinventory"
	"dont/shared"
)

func entityEvidence(value bool) *bool { return &value }

const inventoryTestPlayer = "KU_01234567"

type inventoryTelemetryFixture struct {
	now        func() time.Time
	values     map[string]dstruntime.Snapshot
	refreshes  []string
	refreshErr error
	readErr    error
	onRefresh  func(string)
}

func (f *inventoryTelemetryFixture) ReadPlayers(_ context.Context, _, world string) (dstruntime.Snapshot, error) {
	return f.values[world], f.readErr
}
func (f *inventoryTelemetryFixture) RefreshSnapshots(_ context.Context, _, world string) (dstruntime.SnapshotRefreshResult, error) {
	f.refreshes = append(f.refreshes, world)
	if f.onRefresh != nil {
		f.onRefresh(world)
	}
	value := f.values[world]
	value.CapturedAt = f.now()
	f.values[world] = value
	return dstruntime.SnapshotRefreshResult{Players: value}, f.refreshErr
}
func newInventoryLocalityService(t *testing.T, age time.Duration, localWorld string) (*Service, *inventoryTelemetryFixture, *inventorySender) {
	t.Helper()
	service, _, _, _, _ := newPlayerTestService(t)
	reader := &inventoryTelemetryFixture{now: service.now, values: map[string]dstruntime.Snapshot{}}
	for _, world := range []string{"master", "caves"} {
		reader.values[world] = dstruntime.Snapshot{LocalityKnown: true, CapturedAt: service.now().Add(-age), Players: []dstruntime.SnapshotPlayer{{
			ID: inventoryTestPlayer, Name: "Player", Prefab: "willow", GameplayState: GameplayStateUnknown, LocalEntity: entityEvidence(world == localWorld),
		}}}
	}
	probe, err := NewTelemetryProbe(&telemetryTestProbe{}, reader, &telemetryTestProbe{err: errors.New("unexpected fallback")})
	if err != nil {
		t.Fatal(err)
	}
	probe.now = service.now
	service.probe = probe
	sender := &inventorySender{value: map[string]shared.PlayerInventory{
		"master": {PlayerID: inventoryTestPlayer, Source: "live", Empty: true}, "caves": {PlayerID: inventoryTestPlayer, Source: "live", Empty: true},
	}}
	service.sender = sender
	return service, reader, sender
}
func TestInventoryLocatesLocalEntityDespiteDuplicatedClusterClients(t *testing.T) {
	for _, world := range []string{"master", "caves"} {
		for _, age := range []time.Duration{0, 16 * time.Second, 61 * time.Second} {
			t.Run(world+"/"+age.String(), func(t *testing.T) {
				s, reader, sender := newInventoryLocalityService(t, age, world)
				value, err := s.Inventory(context.Background(), "room", "", shared.PlayerInventoryRequest{PlayerID: inventoryTestPlayer})
				if err != nil || value.Source != "live" || value.WorldID != world {
					t.Fatalf("value=%#v err=%v", value, err)
				}
				if len(sender.calls) != 1 || sender.calls[0] != world+":live" {
					t.Fatal(sender.calls)
				}
				expected := 0
				if age > 15*time.Second {
					expected = 2
				}
				if len(reader.refreshes) != expected {
					t.Fatalf("refreshes=%v", reader.refreshes)
				}
			})
		}
	}
}
func TestInventoryRefreshActuallyConfirmsFreshLocation(t *testing.T) {
	s, reader, _ := newInventoryLocalityService(t, 0, "master")
	_, err := s.Inventory(context.Background(), "room", "", shared.PlayerInventoryRequest{PlayerID: inventoryTestPlayer, Refresh: true})
	if err != nil || len(reader.refreshes) != 2 {
		t.Fatal(reader.refreshes, err)
	}
}
func TestInventoryExplicitWorldNeverCrossesToOtherShardOrSave(t *testing.T) {
	s, reader, sender := newInventoryLocalityService(t, 0, "caves")
	_, err := s.Inventory(context.Background(), "room", "master", shared.PlayerInventoryRequest{PlayerID: inventoryTestPlayer})
	if playerinventory.Code(err) != "PLAYER_NOT_ONLINE" || len(sender.calls) != 0 || len(reader.refreshes) != 0 {
		t.Fatal(err, sender.calls, reader.refreshes)
	}
}
func TestInventoryConnectedPlayerWithoutEntityIsNeverTreatedAsOffline(t *testing.T) {
	for _, state := range []string{GameplayStateSelectingCharacter, GameplayStateLoading, GameplayStateUnknown, GameplayStateMigrating} {
		t.Run(state, func(t *testing.T) {
			s, reader, sender := newInventoryLocalityService(t, 0, "")
			for world, value := range reader.values {
				value.Players[0].GameplayState = state
				reader.values[world] = value
			}
			_, err := s.Inventory(context.Background(), "room", "", shared.PlayerInventoryRequest{PlayerID: inventoryTestPlayer})
			code := playerinventory.Code(err)
			expected := "PLAYER_LOCATION_UNCERTAIN"
			if state == GameplayStateSelectingCharacter || state == GameplayStateLoading {
				expected = "PLAYER_NOT_READY"
			}
			if code != expected || len(sender.calls) != 0 {
				t.Fatal(code, err, sender.calls)
			}
		})
	}
}
func TestInventoryRechecksRealConflictButNeverGuesses(t *testing.T) {
	s, reader, sender := newInventoryLocalityService(t, 0, "master")
	value := reader.values["caves"]
	value.Players[0].LocalEntity = entityEvidence(true)
	reader.values["caves"] = value
	_, err := s.Inventory(context.Background(), "room", "", shared.PlayerInventoryRequest{PlayerID: inventoryTestPlayer})
	if playerinventory.Code(err) != "PLAYER_LOCATION_UNCERTAIN" || len(sender.calls) != 0 || len(reader.refreshes) != 2 {
		t.Fatal(err, sender.calls, reader.refreshes)
	}
}
func TestInventoryRetriesLocationOnceWhenPlayerMovesBeforeRead(t *testing.T) {
	s, reader, sender := newInventoryLocalityService(t, 0, "master")
	sender.err = map[string]error{"master": &shared.InventoryError{Code: "PLAYER_NOT_ONLINE"}}
	reader.onRefresh = func(world string) {
		value := reader.values[world]
		value.Players[0].LocalEntity = entityEvidence(world == "caves")
		reader.values[world] = value
	}
	value, err := s.Inventory(context.Background(), "room", "", shared.PlayerInventoryRequest{PlayerID: inventoryTestPlayer})
	if err != nil || value.WorldID != "caves" || len(sender.calls) != 2 || len(reader.refreshes) != 2 {
		t.Fatal(value, err, sender.calls, reader.refreshes)
	}
}
func TestInventoryOfflineRequiresCompleteFreshRoomEvidence(t *testing.T) {
	s, reader, sender := newInventoryLocalityService(t, 0, "")
	for world, value := range reader.values {
		value.Players = []dstruntime.SnapshotPlayer{}
		reader.values[world] = value
	}
	sender.value = map[string]shared.PlayerInventory{"master": {PlayerID: inventoryTestPlayer, Source: "save", SourceID: "one"}}
	sender.err = map[string]error{"caves": &shared.InventoryError{Code: "PLAYER_SNAPSHOT_NOT_FOUND"}}
	value, err := s.Inventory(context.Background(), "room", "", shared.PlayerInventoryRequest{PlayerID: inventoryTestPlayer})
	if err != nil || value.Source != "save" || len(sender.calls) != 2 {
		t.Fatal(value, err, sender.calls)
	}
}
func TestInventoryOldRuntimeUsesShardLocalFallbackWithoutReadingHistory(t *testing.T) {
	s, reader, sender := newInventoryLocalityService(t, time.Minute, "master")
	for world, value := range reader.values {
		value.LocalityKnown = false
		value.Players[0].LocalEntity = nil
		reader.values[world] = value
	}
	native := &telemetryTestProbe{err: errors.New("must not read logs")}
	fallback := &playerWorldProbe{items: map[string][]Observation{
		"master": {{ID: inventoryTestPlayer, Name: "Player", LocalEntity: entityEvidence(true)}},
		"caves":  {{ID: inventoryTestPlayer, Name: "Player", LocalEntity: entityEvidence(false)}},
	}}
	probe, _ := NewTelemetryProbe(native, reader, fallback)
	probe.now = s.now
	s.probe = probe
	value, err := s.Inventory(context.Background(), "room", "", shared.PlayerInventoryRequest{PlayerID: inventoryTestPlayer})
	if err != nil || value.WorldID != "master" || native.calls != 0 || len(sender.calls) != 1 {
		t.Fatal(value, err, native.calls, sender.calls)
	}
}
func TestInventoryRemoteDisconnectedNeverReadsControllerFallbackOrSavedItems(t *testing.T) {
	s, reader, sender := newInventoryLocalityService(t, time.Minute, "master")
	reader.readErr = agents.ErrAgentOffline
	native, local, remote := &telemetryTestProbe{}, &telemetryTestProbe{}, &telemetryTestProbe{}
	probe, _ := NewTelemetryProbe(native, reader, local, remotePlayerLocality{})
	probe.now = s.now
	probe.ConfigureRemoteFallback(remote)
	s.probe = probe
	_, err := s.Inventory(context.Background(), "room", "", shared.PlayerInventoryRequest{PlayerID: inventoryTestPlayer})
	if !errors.Is(err, agents.ErrAgentOffline) || len(sender.calls) != 0 || native.calls != 0 || local.calls != 0 || remote.calls != 0 {
		t.Fatal(err, sender.calls, native.calls, local.calls, remote.calls)
	}
}

func TestInventoryTransientEntityOverlapResolvesWithSingleReconfirmation(t *testing.T) {
	s, reader, sender := newInventoryLocalityService(t, 0, "master")
	value := reader.values["caves"]
	value.Players[0].LocalEntity = entityEvidence(true)
	reader.values["caves"] = value
	reader.onRefresh = func(world string) {
		value := reader.values[world]
		value.Players[0].LocalEntity = entityEvidence(world == "caves")
		reader.values[world] = value
	}
	valueRead, err := s.Inventory(context.Background(), "room", "", shared.PlayerInventoryRequest{PlayerID: inventoryTestPlayer})
	if err != nil || valueRead.WorldID != "caves" || len(reader.refreshes) != 2 || len(sender.calls) != 1 {
		t.Fatal(valueRead, err, reader.refreshes, sender.calls)
	}
}

func TestInventoryLocalMigratingEntityNeverProducesSavedOrEmptyItems(t *testing.T) {
	s, reader, sender := newInventoryLocalityService(t, 0, "master")
	value := reader.values["master"]
	value.Players[0].GameplayState = GameplayStateMigrating
	reader.values["master"] = value
	_, err := s.Inventory(context.Background(), "room", "", shared.PlayerInventoryRequest{PlayerID: inventoryTestPlayer})
	if playerinventory.Code(err) != "PLAYER_LOCATION_UNCERTAIN" || len(reader.refreshes) != 2 || len(sender.calls) != 0 {
		t.Fatal(err, reader.refreshes, sender.calls)
	}
}
