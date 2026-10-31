package aicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"dont/internal/apicontract"
)

func TestBotControlledSpawnScopeIdempotencyAndReceipt(t *testing.T) {
	writes, reads := 0, 0
	s, _ := botFixture(t, apiBackendFunc(func(_ context.Context, _ Principal, op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		if op.ID == "spawnRoomPlayerEntity" {
			writes++
			if in.Body.(map[string]any)["entity"] != "座狼" {
				t.Fatal(in)
			}
			return json.RawMessage(`{"data":{"runId":"spawn-a","roomId":"room-a","playerId":"KU_TEST123","status":"uncertain","complete":false,"executionOutcome":"unconfirmed","mayHaveExecuted":true}}`), nil
		}
		if op.ID == "getRoomPlayerEntitySpawn" {
			reads++
			if in.Path["runId"] != "spawn-a" || in.Query["refresh"] != true {
				t.Fatal(in)
			}
			return json.RawMessage(`{"data":{"runId":"spawn-a","roomId":"room-a","playerId":"KU_TEST123","status":"succeeded","complete":true,"executionOutcome":"confirmed","spawned":2}}`), nil
		}
		t.Fatal(op.ID)
		return nil, nil
	}))
	p := Principal{ID: "manager", Profile: ProfileGameManage, RoomIDs: []string{"room-a"}}
	in := botInput(t, s, p, "player.spawn_entity", "spawn-test-123")
	in.Parameters = map[string]any{"playerId": "KU_TEST123", "entity": "座狼", "count": 2}
	for i := 0; i < 2; i++ {
		r, err := s.BotExecute(context.Background(), p, in, true)
		if err != nil || r.Status != "uncertain" || r.SpawnRunID != "spawn-a" || writes != 1 {
			t.Fatal(r, err, writes)
		}
	}
	r, err := s.BotResult(context.Background(), p, "room-a", in.RequestID)
	if err != nil || r.Status != "succeeded" || writes != 1 || reads != 1 {
		t.Fatal(r, err, writes, reads)
	}
	reader := Principal{ID: "reader", Profile: ProfileGameRead, RoomIDs: []string{"room-a"}}
	if _, err = s.BotExecute(context.Background(), reader, in, true); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	if p.AllowsOperation("spawnRoomPlayerEntity", "other") || reader.AllowsOperation("spawnRoomPlayerEntity", "room-a") || p.AllowsOperation("executeRoomCommand", "room-a") {
		t.Fatal("scope bypass")
	}
	in.RequestID = "spawn-invalid-123"
	in.Parameters["commandId"] = "custom"
	if _, err = s.BotExecute(context.Background(), p, in, true); !errors.Is(err, ErrInvalid) || writes != 1 {
		t.Fatal(err, writes)
	}
}
