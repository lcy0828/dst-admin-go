package aicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"dont/internal/apicontract"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPlayerManagementCatalogAndScopeCoverage(t *testing.T) {
	catalog, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	manager, reader := taskPrincipal(ProfileGameManage), taskPrincipal(ProfileGameRead)
	count := 0
	for _, op := range catalog.Operations() {
		if !strings.HasPrefix(op.Path, "/rooms/{roomId}/players/{playerId}/actions/") {
			continue
		}
		count++
		if !manager.AllowsTool(op.ToolName()) || !manager.AllowsOperation(op.ID, "room-a") {
			t.Errorf("fixed player endpoint omitted from game management: %s", op.ID)
		}
		if reader.AllowsTool(op.ToolName()) || reader.AllowsOperation(op.ID, "room-a") || manager.AllowsOperation(op.ID, "other") {
			t.Errorf("player action escaped profile/room scope: %s", op.ID)
		}
		if op.ToolMetadata().Access != "write" {
			t.Errorf("player action marked read-only: %s", op.ID)
		}
	}
	if count < 9 {
		t.Fatal("player endpoints not covered", count)
	}
	for _, id := range []string{"getRoomPlayer", "refreshRoomPlayers", "listRoomBackups"} {
		if !manager.AllowsOperation(id, "room-a") || reader.AllowsOperation(id, "room-a") || manager.AllowsOperation(id, "other") {
			t.Fatal("management dependency scope", id)
		}
	}
	for _, id := range []string{"executeRoomCommand", "executeRawRoomCommand", "applyRoomAccessLists", "revealClusterToken", "applyRoomTopologyPlacement", "deleteRoom"} {
		if manager.AllowsOperation(id, "room-a") {
			t.Fatal("panel-only operation granted", id)
		}
	}
}

func TestPlayerModeMCPListingDiscoveryAndDispatch(t *testing.T) {
	sends := 0
	s, store := botFixture(t, apiBackendFunc(func(_ context.Context, _ Principal, op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		if op.ID != "setRoomPlayerGodMode" && op.ID != "setRoomPlayerCreativeMode" {
			t.Fatal(op.ID)
		}
		if in.Path["roomId"] != "room-a" || in.Body.(map[string]any)["enabled"] != false {
			t.Fatal(in)
		}
		sends++
		return json.RawMessage(`{"httpStatus":202,"data":{"id":"job-a","roomId":"room-a","status":"queued"}}`), nil
	}))
	for _, profile := range []string{ProfileGameManage, ProfileGameRead} {
		_, secret, err := store.CreateToken(TokenInput{Name: "test", Days: 1, Profile: profile, RoomIDs: []string{"room-a"}})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/mcp?view=all&pagination=off", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		w := httptest.NewRecorder()
		s.MCP(w, req)
		var response struct {
			Result mcp.ListToolsResult `json:"result"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
			t.Fatal(w.Body.String())
		}
		for _, id := range []string{"setRoomPlayerGodMode", "setRoomPlayerCreativeMode"} {
			found := false
			for _, tool := range response.Result.Tools {
				if tool.Name == "api_"+id {
					found = true
					if tool.Annotations == nil || tool.Annotations.ReadOnlyHint {
						t.Fatal("mutation advertised read-only")
					}
				}
			}
			if found != (profile == ProfileGameManage) {
				t.Fatal(profile, id, found)
			}
			p := taskPrincipal(profile)
			query, _ := json.Marshal(map[string]any{"query": id, "includeSchema": true})
			result, err := s.Invoke(context.Background(), p, "find_api", query)
			if err != nil || strings.Contains(string(result.Data), `"operationId":"`+id+`"`) != found {
				t.Fatal(string(result.Data), err)
			}
			for index, room := range []string{"room-a", "other"} {
				args := map[string]any{"path": map[string]any{"roomId": room, "playerId": "KU_EXAMPLE"}, "body": map[string]any{"enabled": false}, "requestId": profile + id + room}
				raw, _ := json.Marshal(args)
				_, err = s.Invoke(context.Background(), p, "api_"+id, raw)
				allowed := profile == ProfileGameManage && index == 0
				if allowed && err != nil || !allowed && !errors.Is(err, ErrDenied) {
					t.Fatal(profile, room, id, err)
				}
				args["requestId"] = "dispatch-" + profile + id + room
				raw, _ = json.Marshal(map[string]any{"operationId": id, "arguments": args})
				_, err = s.dispatchMCP(context.Background(), p, raw)
				if allowed && err != nil || !allowed && !errors.Is(err, ErrDenied) {
					t.Fatal("dispatcher", profile, room, err)
				}
			}
		}
	}
	if sends != 4 {
		t.Fatal("denied requests reached backend", sends)
	}
}

func TestPlayerModesTaskDiscoveryValidationAndOriginalResult(t *testing.T) {
	for _, action := range []string{"god_mode", "creative_mode"} {
		for _, enabled := range []bool{true, false} {
			t.Run(action+map[bool]string{true: "-on", false: "-off"}[enabled], func(t *testing.T) {
				sends := 0
				s, _ := taskFixture(t, func(op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
					want := map[string]string{"god_mode": "setRoomPlayerGodMode", "creative_mode": "setRoomPlayerCreativeMode"}[action]
					if op.ID != want || in.Path["playerId"] != "KU_EXAMPLE" || in.Path["roomId"] != "room-a" || in.Body.(map[string]any)["enabled"] != enabled {
						t.Fatal(op.ID, in)
					}
					sends++
					return json.RawMessage(`{"httpStatus":202,"data":{"id":"job-a","roomId":"room-a","status":"queued"}}`), nil
				})
				p := taskPrincipal(ProfileGameManage)
				terms := map[string][]string{"god_mode": {"上帝模式", "无敌", "godmode"}, "creative_mode": {"制作模式", "制造模式", "创造模式", "自由建造"}}[action]
				for _, term := range terms {
					result := callTask(t, s, p, "dst_find", map[string]any{"query": term})
					items := result.Data.(map[string]any)["items"].([]map[string]any)
					if len(items) == 0 || items[0]["id"] != "player."+action {
						t.Fatal(term, result)
					}
					result = callTask(t, s, taskPrincipal(ProfileGameRead), "dst_find", map[string]any{"query": term})
					for _, item := range result.Data.(map[string]any)["items"].([]map[string]any) {
						if item["access"] == "write" {
							t.Fatal("reader sees write", item)
						}
					}
				}
				base := map[string]any{"room": "测试房间", "player": "示例玩家", "action": action}
				missing := callTask(t, s, p, "dst_player_action", base)
				if missing.State != "needs_input" || missing.TaskRef != "" || sends != 0 {
					t.Fatal(missing, sends)
				}
				for _, params := range []map[string]any{{"enabled": "false"}, {"enabled": true, "readOnly": true}, {"enabled": true, "lua": "print(1)"}, {"enabled": true, "playerId": "KU_OTHER"}} {
					base["parameters"] = params
					raw, _ := json.Marshal(base)
					if _, err := s.InvokeTask(context.Background(), p, "dst_player_action", raw); !errors.Is(err, ErrInvalid) {
						t.Fatal(params, err)
					}
				}
				base["parameters"] = map[string]any{"enabled": enabled}
				prepared := callTask(t, s, p, "dst_player_action", base)
				if prepared.State != "prepared" || prepared.TaskRef == "" || sends != 0 {
					t.Fatal(prepared, sends)
				}
				for attempt := range 2 {
					result := callTask(t, s, p, "dst_run", map[string]any{"mode": "execute", "taskRef": prepared.TaskRef})
					if result.Result == nil || sends != 1 || attempt == 0 && (result.Result.Complete || result.Result.Status != "pending") {
						t.Fatal(result)
					}
				}
				result := callTask(t, s, p, "dst_result", map[string]any{"taskRef": prepared.TaskRef})
				if result.Result == nil || !result.Result.Complete || sends != 1 {
					t.Fatal(result, sends)
				}
			})
		}
	}
}

func TestOtherPlayerRecipesKeepExplicitConfirmationAndBounds(t *testing.T) {
	s, _ := botFixture(t, nil)
	p := taskPrincipal(ProfileGameManage)
	for _, tc := range []struct {
		id, operation string
		parameters    map[string]any
	}{
		{"announce", "announceToRoomPlayer", map[string]any{"message": "测试提醒"}},
		{"ban", "banRoomPlayer", map[string]any{"confirmation": "测试房间", "reason": "测试原因", "duration": "1h"}},
		{"unban", "unbanRoomPlayer", map[string]any{"confirmation": "测试房间"}},
		{"kill", "killRoomPlayer", map[string]any{"confirmed": true}},
		{"reset_character", "resetRoomPlayerCharacter", map[string]any{"confirmed": true}},
	} {
		c, err := s.botCapability(p, "player."+tc.id, "room-a")
		if err != nil || c.Operation != tc.operation {
			t.Fatal(tc.id, c, err)
		}
		tc.parameters["playerId"] = "KU_EXAMPLE"
		input := BotInput{RoomID: "room-a", CapabilityID: c.ID, Parameters: tc.parameters}
		if err = validateTaskCapability(s, p, c, input); err != nil {
			t.Fatal(tc.id, err)
		}
		body := c.build("room-a", tc.parameters).Body.(map[string]any)
		if _, ok := body["confirmed"]; ok {
			t.Fatal("task-only parameter leaked")
		}
		if tc.id == "kill" || tc.id == "reset_character" {
			if body["confirmation"] != "KU_EXAMPLE" {
				t.Fatal(body)
			}
			tc.parameters["confirmed"] = false
		} else if tc.id == "announce" {
			tc.parameters["message"] = strings.Repeat("字", 301)
		} else {
			delete(tc.parameters, "confirmation")
		}
		if err = validateTaskCapability(s, p, c, input); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid confirmation or bound accepted", tc.id, err)
		}
	}
}
