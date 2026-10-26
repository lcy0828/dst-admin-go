package aicontrol

import (
	"context"
	"encoding/json"
	"testing"

	"dont/internal/apicontract"
)

func TestPlayerInventoryIsDiscoverableReadonlyAndWorksWithoutRunningRuntime(t *testing.T) {
	s, _ := fixture(t, apiBackendFunc(func(_ context.Context, _ Principal, op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		if op.ID != "getRoomPlayerInventory" || op.Mutation() || in.Path["playerId"] != "KU_01234567" {
			t.Fatal(op.ID, in)
		}
		return json.RawMessage(`{"data":{"source":"save","status":"partial","coverage":"standard_components","empty":false,"limitsReached":["DEPTH_LIMIT"]}}`), nil
	}))
	c, e := apicontract.Default()
	if e != nil {
		t.Fatal(e)
	}
	s.ConfigureAPI(c)
	tools, e := s.AvailableTools()
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, tool := range tools {
		if tool.Name == "api_getRoomPlayerInventory" {
			found = true
			if tool.Mutation {
				t.Fatal("inventory requires mutation permission")
			}
		}
	}
	if !found {
		t.Fatal("inventory tool missing")
	}
	v, e := s.Invoke(context.Background(), principal(), "api_getRoomPlayerInventory", json.RawMessage(`{"path":{"roomId":"room-a","playerId":"KU_01234567"},"query":{"source":"save"}}`))
	if e != nil || v.Status != "succeeded" {
		t.Fatal(v, e)
	}
}
