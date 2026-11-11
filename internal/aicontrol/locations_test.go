package aicontrol

import (
	"context"
	"encoding/json"
	"testing"

	"dont/internal/apicontract"
)

func TestTeleportRecipePreservesSelectedLocationIdentity(t *testing.T) {
	for _, item := range locationCapabilities() {
		if item.ID != "player.teleport" {
			continue
		}
		params := map[string]any{"playerId": "KU_EXAMPLE", "locationName": "猪王", "locationId": "saved-id", "worldId": "world"}
		in := item.build("room", params)
		body := in.Body.(map[string]any)
		if body["locationId"] != "saved-id" || body["worldId"] != "world" || body["locationName"] != nil || params["locationName"] != "猪王" {
			t.Fatalf("selected ID lost or input mutated: %+v %+v", in, params)
		}
		legacy := item.build("room", map[string]any{"playerId": "KU_EXAMPLE", "locationName": "猪王村"})
		if legacy.Body.(map[string]any)["locationName"] != "猪王村" {
			t.Fatal("legacy name-only destination changed")
		}
		return
	}
	t.Fatal("missing teleport recipe")
}

func TestTeleportRecipeSelectedIDUsesValidatedPrepareContract(t *testing.T) {
	const locationID = "00000000-0000-4000-8000-000000000001"
	calls := 0
	s, _ := botFixture(t, apiBackendFunc(func(_ context.Context, _ Principal, op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		calls++
		if op.ID != "prepareRoomTeleport" {
			t.Fatal(op.ID)
		}
		body := in.Body.(map[string]any)
		if calls == 1 && (body["locationId"] != locationID || body["locationName"] != nil) {
			t.Fatalf("selected ID not forwarded exactly: %+v", body)
		}
		if calls == 2 && (body["locationName"] != "猪王村" || body["locationId"] != nil) {
			t.Fatalf("legacy name changed: %+v", body)
		}
		return json.RawMessage(`{"httpStatus":200,"data":{"state":"prepared","id":"preview-example"}}`), nil
	}))
	p := Principal{ID: "example-manager", Profile: ProfileGameManage, RoomIDs: []string{"room-a"}}
	for i, params := range []map[string]any{
		{"playerId": "KU_EXAMPLE", "locationName": "猪王", "locationId": locationID, "worldId": "world"},
		{"playerId": "KU_EXAMPLE", "locationName": "猪王村"},
	} {
		in := botInput(t, s, p, "player.teleport", []string{"selected-location", "legacy-name"}[i])
		in.Parameters = params
		if _, err := s.BotExecute(context.Background(), p, in, true); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}
