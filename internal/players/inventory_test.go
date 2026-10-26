package players

import (
	"context"
	"testing"

	"dont/internal/dstruntime"
	"dont/internal/playerinventory"
	"dont/shared"
)

type inventorySender struct {
	playerTestSender
	calls []string
	value map[string]shared.PlayerInventory
	err   map[string]error
}

func (f *inventorySender) ReadPlayerInventory(_ context.Context, _ string, world string, q shared.PlayerInventoryRequest) (shared.PlayerInventory, error) {
	f.calls = append(f.calls, world+":"+q.Source)
	return f.value[world], f.err[world]
}
func TestInventoryStoppedImportedPlayerNeedsNeitherHistoryNorTelemetry(t *testing.T) {
	s, r, _, _, p := newPlayerTestService(t)
	r.running = map[string]bool{}
	p.err = dstruntime.ErrRuntimeUnavailable
	f := &inventorySender{value: map[string]shared.PlayerInventory{"master": {PlayerID: "KU_01234567", Source: "save", SourceID: "one", Empty: true}}, err: map[string]error{"caves": &shared.InventoryError{Code: "PLAYER_SNAPSHOT_NOT_FOUND"}}}
	s.sender = f
	v, e := s.Inventory(context.Background(), "room", "", shared.PlayerInventoryRequest{PlayerID: "KU_01234567", Source: "auto"})
	if e != nil || v.WorldID != "master" || v.Source != "save" || !v.Empty {
		t.Fatal(v, e)
	}
	if len(f.calls) != 2 || f.calls[0] != "master:save" || f.calls[1] != "caves:save" {
		t.Fatal(f.calls)
	}
}
func TestInventoryDoesNotGuessAcrossSavedWorlds(t *testing.T) {
	s, _, _, _, _ := newPlayerTestService(t)
	s.sender = &inventorySender{value: map[string]shared.PlayerInventory{"master": {SourceID: "one"}, "caves": {SourceID: "two"}}}
	_, e := s.Inventory(context.Background(), "room", "", shared.PlayerInventoryRequest{PlayerID: "KU_01234567", Source: "save"})
	if playerinventory.Code(e) != "SOURCE_AMBIGUOUS" {
		t.Fatal(e)
	}
	typed := e.(*shared.InventoryError)
	if len(typed.Candidates) != 2 || typed.Candidates[0].WorldName != "地面" || typed.Candidates[1].WorldID != "caves" {
		t.Fatal(typed)
	}
}
func TestInventoryUncertainPresenceNeverFallsBackToSavedItems(t *testing.T) {
	s, _, _, _, p := newPlayerTestService(t)
	p.err = dstruntime.ErrRuntimeUnavailable
	f := &inventorySender{}
	s.sender = f
	_, e := s.Inventory(context.Background(), "room", "", shared.PlayerInventoryRequest{PlayerID: "KU_01234567", Source: "auto"})
	if playerinventory.Code(e) != "PLAYER_LOCATION_UNCERTAIN" || len(f.calls) != 0 {
		t.Fatal(e, f.calls)
	}
}
