package aicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"dont/internal/apicontract"
)

func TestBotGrantAuthorizationIdempotencyAndReceiptRecovery(t *testing.T) {
	writes, reads := 0, 0
	s, _ := botFixture(t, apiBackendFunc(func(_ context.Context, _ Principal, op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		switch op.ID {
		case "giveRoomPlayerItem":
			writes++
			if in.Body.(map[string]any)["item"] != "肉丸" || in.Path["playerId"] != "KU_TEST123" {
				t.Fatal(in)
			}
			return json.RawMessage(`{"httpStatus":201,"data":{"runId":"grant-a","roomId":"room-a","playerId":"KU_TEST123","prefab":"meatballs","status":"uncertain","complete":false,"executionOutcome":"unconfirmed","mayHaveExecuted":true}}`), nil
		case "getRoomPlayerItemGrant":
			reads++
			if in.Path["runId"] != "grant-a" || in.Query["refresh"] != true {
				t.Fatal(in)
			}
			return json.RawMessage(`{"httpStatus":200,"data":{"runId":"grant-a","roomId":"room-a","playerId":"KU_TEST123","prefab":"meatballs","status":"succeeded","complete":true,"executionOutcome":"confirmed","mayHaveExecuted":true,"requested":3,"granted":3}}`), nil
		default:
			t.Fatal(op.ID)
			return nil, nil
		}
	}))
	p := Principal{ID: "manager", Profile: ProfileGameManage, RoomIDs: []string{"room-a"}}
	in := botInput(t, s, p, "player.give_item", "grant-test-123")
	in.Parameters = map[string]any{"playerId": "KU_TEST123", "item": "肉丸", "count": 3}
	r, err := s.BotExecute(context.Background(), p, in, true)
	if err != nil || r.Status != "uncertain" || r.JobID != "" || r.GrantRunID != "grant-a" || writes != 1 {
		t.Fatal(r, err, writes)
	}
	r, err = s.BotExecute(context.Background(), p, in, true)
	if err != nil || r.Status != "uncertain" || writes != 1 || reads != 0 {
		t.Fatal(r, err, writes, reads)
	}
	r, err = s.BotResult(context.Background(), p, "room-a", in.RequestID)
	if err != nil || r.Status != "succeeded" || !r.Complete || writes != 1 || reads != 1 {
		t.Fatal(r, err, writes, reads)
	}
	if _, err = s.BotExecute(context.Background(), Principal{ID: "reader", Profile: ProfileGameRead, RoomIDs: []string{"room-a"}}, in, true); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	if p.AllowsOperation("executeRoomCommand", "room-a") || p.AllowsOperation("executeRawRoomCommand", "room-a") {
		t.Fatal("general Lua leaked")
	}
	if p.AllowsOperation("giveRoomPlayerItem", "other") {
		t.Fatal("room scope bypass")
	}
	in.RequestID = "invalid-grant-123"
	in.Parameters["readOnly"] = true
	if _, err = s.BotExecute(context.Background(), p, in, true); !errors.Is(err, ErrInvalid) || writes != 1 {
		t.Fatal(err, writes)
	}
}

func TestBotGrantPreflightRejectionAndSentAreNotSuccess(t *testing.T) {
	s, _ := botFixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		return nil, &PlatformError{Status: 409, Code: "PLAYER_NOT_ONLINE", Message: "未发放"}
	}))
	p := Principal{ID: "manager", Profile: ProfileGameManage, RoomIDs: []string{"room-a"}}
	in := botInput(t, s, p, "player.give_item", "grant-reject-123")
	in.Parameters = map[string]any{"playerId": "KU_TEST123", "item": "肉丸", "count": 3}
	r, err := s.BotExecute(context.Background(), p, in, true)
	if err != nil || r.Status != "rejected" || !r.Complete || r.MayHaveExecuted {
		t.Fatal(r, err)
	}
	for _, status := range []string{"sent", "succeeded"} {
		r = botProject(BotResult{RoomID: "room-a"}, BotCapability{ID: "player.give_item"}, map[string]any{"runId": "run", "roomId": "room-a", "status": status, "complete": false, "executionOutcome": "unconfirmed", "mayHaveExecuted": true})
		if r.Status != "uncertain" || r.Complete || r.JobID != "" {
			t.Fatal(r)
		}
	}
}
