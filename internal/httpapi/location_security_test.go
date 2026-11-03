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
