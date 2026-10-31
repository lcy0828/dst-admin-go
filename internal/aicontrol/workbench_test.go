package aicontrol

import (
	"context"
	"dont/internal/apicontract"
	"encoding/json"
	"testing"
)

func TestWorkbenchBotPermissionsOriginalReceiptAndIdempotency(t *testing.T) {
	sends, reads := 0, 0
	s, _ := botFixture(t, apiBackendFunc(func(_ context.Context, _ Principal, op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		switch op.ID {
		case "executeBuiltinRoomCommand":
			sends++
			body := in.Body.(map[string]any)
			if body["commandId"] != "resurrect_player" || body["arguments"].(map[string]any)["player_id"] != "KU_TEST123" {
				t.Fatal(in)
			}
			return json.RawMessage(`{"httpStatus":201,"data":{"runId":"fixed-run","commandId":"resurrect_player","roomId":"room-a","status":"sent","complete":false,"executionOutcome":"unconfirmed","mayHaveExecuted":true}}`), nil
		case "getBuiltinRoomCommandRun":
			reads++
			if _, ok := in.Path["playerId"]; ok {
				t.Fatal(in)
			}
			return json.RawMessage(`{"httpStatus":200,"data":{"runId":"fixed-run","commandId":"resurrect_player","roomId":"room-a","status":"succeeded","complete":true,"executionOutcome":"confirmed","mayHaveExecuted":true,"message":"指令执行，效果以输出为准","output":{"text":"REQUESTED","truncated":false}}}`), nil
		default:
			t.Fatal(op.ID)
			return nil, nil
		}
	}))
	p := Principal{ID: "manager", Profile: ProfileGameManage, RoomIDs: []string{"room-a"}}
	in := botInput(t, s, p, "player.resurrect", "revive-request-123")
	in.Parameters = map[string]any{"player_id": "KU_TEST123"}
	r, err := s.BotExecute(context.Background(), p, in, true)
	if err != nil || r.Complete || r.Status != "uncertain" || r.WorkbenchRunID != "fixed-run" || r.ResultKind != "workbench-run" {
		t.Fatal(r, err)
	}
	if _, err = s.BotExecute(context.Background(), p, in, true); err != nil || sends != 1 {
		t.Fatal(err, sends)
	}
	r, err = s.BotResult(context.Background(), p, "room-a", in.RequestID)
	if err != nil || !r.Complete || r.Status != "succeeded" || reads != 1 || sends != 1 {
		t.Fatal(r, err, reads, sends)
	}
	reader := Principal{ID: "reader", Profile: ProfileGameRead, RoomIDs: []string{"room-a"}}
	for _, op := range []string{"executeBuiltinRoomCommand", "getBuiltinRoomCommandRun"} {
		if reader.AllowsOperation(op, "room-a") || p.AllowsOperation(op, "other") {
			t.Fatal("permission leak", op)
		}
	}
	for _, op := range []string{"executeRoomCommand", "executeRawRoomCommand"} {
		if p.AllowsOperation(op, "room-a") {
			t.Fatal("general script leak")
		}
	}
	in.RequestID = "spoof-request-123"
	in.Parameters["readOnly"] = true
	if _, err = s.BotExecute(context.Background(), p, in, true); err == nil || sends != 1 {
		t.Fatal(err, sends)
	}
}
