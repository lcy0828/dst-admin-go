package aicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"dont/internal/apicontract"
)

func TestChatStreamPreservesChatHistoryPermissionsAndResourceOwnership(t *testing.T) {
	invoked := 0
	service, store := fixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		invoked++
		return json.RawMessage(`{"resource":{"kind":"stream","fetched":false}}`), nil
	}))
	catalog, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	service.ConfigureAPI(catalog)
	principal := Principal{ID: "synthetic-reader", Profile: ProfilePanelManage, RoomIDs: []string{"allowed"}}
	for _, tc := range []struct {
		p       Principal
		room    string
		allowed bool
	}{
		{principal, "allowed", true}, {principal, "other", false},
		{Principal{ID: "public-reader", Profile: ProfileGameRead, RoomIDs: []string{"allowed"}}, "allowed", false},
		{Principal{ID: "game-manager", Profile: ProfileGameManage, RoomIDs: []string{"allowed"}}, "allowed", false},
	} {
		raw, _ := json.Marshal(map[string]any{"path": map[string]any{"roomId": tc.room}})
		before := invoked
		_, err := service.Invoke(context.Background(), tc.p, "api_streamRoomChatLogs", raw)
		if tc.allowed && (err != nil || invoked != before+1) {
			t.Fatal("authorized stream denied", err)
		}
		if !tc.allowed && (!errors.Is(err, ErrDenied) || invoked != before) {
			t.Fatal("unauthorized stream reached backend", err)
		}
		if tc.p.AllowsOperation("streamRoomChatLogs", tc.room) != tc.p.AllowsOperation("listRoomChatLogs", tc.room) {
			t.Fatal("chat scope broadened")
		}
	}
	op, _ := catalog.Operation("streamRoomChatLogs")
	resource, err := store.CreateResource(principal, op, apicontract.Input{Path: map[string]any{"roomId": "allowed"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resource(Principal{ID: "other"}, resource.ID); !errors.Is(err, ErrDenied) {
		t.Fatal("resource crossed principals")
	}
	if _, err := store.CreateResource(principal, op, apicontract.Input{Path: map[string]any{"roomId": "other"}}); !errors.Is(err, ErrDenied) {
		t.Fatal("resource crossed rooms")
	}
}
