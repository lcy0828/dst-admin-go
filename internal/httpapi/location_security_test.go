package httpapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"dont/internal/aicontrol"
	"dont/internal/apicontract"
	"github.com/gin-gonic/gin"
)

func TestModelCannotApproveTeleportOrUseLegacyMovement(t *testing.T) {
	catalog, e := apicontract.Default()
	if e != nil {
		t.Fatal(e)
	}
	ctx := aicontrol.WithModelChannel(context.Background())
	backend := &AIBackend{}
	for _, id := range []string{"executeRoomTeleport", "executeRoomCommand", "executeRawRoomCommand", "executeBuiltinRoomCommand"} {
		op, ok := catalog.Operation(id)
		if !ok {
			t.Fatal(id)
		}
		for _, command := range []string{"teleport_player", "relocate_players", "teleport_to_nearest_entity"} {
			_, e := backend.InvokeAPI(ctx, aicontrol.Principal{Profile: aicontrol.ProfilePanelManage}, op, apicontract.Input{Body: map[string]any{"commandId": command, "confirmed": true, "legacy": true}})
			if !errors.Is(e, aicontrol.ErrDenied) {
				t.Fatalf("%s %s bypass: %v", id, command, e)
			}
		}
	}
}
func TestLocationBodiesRejectCallerProofScriptsAndDuplicateKeys(t *testing.T) {
	for _, body := range []string{`{"name":"x","name":"y"}`, `{"name":"x","sessionId":"forged"}`, `{"name":"x","readOnly":true}`, `{"name":"x","script":"c_godmode()"}`, `{"name":"x","x":"0);c_shutdown()"}`} {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest("POST", "/", strings.NewReader(body))
		var input struct {
			Name string  `json:"name"`
			X    float64 `json:"x"`
		}
		if e := locationBody(ctx, &input); e == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
func TestDirectApprovalHandlerAlsoRejectsModelContext(t *testing.T) {
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"confirmed":true}`)).WithContext(aicontrol.WithModelChannel(context.Background()))
	(&LocationsHandler{}).execute(ctx)
	if w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestModelMayPrepareButCannotApproveAnyFixedInteraction(t *testing.T) {
	catalog, _ := apicontract.Default()
	op, _ := catalog.Operation("executeBuiltinRoomCommand")
	backend := &AIBackend{}
	ctx := aicontrol.WithModelChannel(context.Background())
	for _, id := range []string{"act_nearby_entities", "set_player_hunger_rate", "set_wanda_age", "restore_woodie_human"} {
		actions := []string{""}
		if id == "act_nearby_entities" {
			actions = []string{"freeze", "delete", "pick", "repair", "fertilize", "grow", "ripen", "harvest", "repair_burnt"}
		}
		for _, action := range actions {
			for _, phase := range []string{"preview", "execute", "cancel"} {
				_, err := backend.InvokeAPI(ctx, aicontrol.Principal{Profile: aicontrol.ProfilePanelManage}, op, apicontract.Input{Body: map[string]any{"commandId": id, "confirmed": true, "arguments": map[string]any{"action": action, "phase": phase, "preview_id": "00000000-0000-4000-8000-000000000001"}}})
				if phase == "execute" && !errors.Is(err, aicontrol.ErrDenied) {
					t.Fatalf("%s %s bypassed confirmation: %v", id, action, err)
				}
				if phase != "execute" && errors.Is(err, aicontrol.ErrDenied) {
					t.Fatalf("%s preparation denied", id)
				}
			}
		}
	}
}

func TestDirectWorkbenchApprovalHandlerRejectsModelContext(t *testing.T) {
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"commandId":"act_nearby_entities","arguments":{"action":"pick","phase":"execute"},"confirmed":true}`)).WithContext(aicontrol.WithModelChannel(context.Background()))
	(&ConsoleHandler{}).workbench(ctx)
	if w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
}
