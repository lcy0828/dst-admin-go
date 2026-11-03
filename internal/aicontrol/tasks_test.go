package aicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dont/internal/apicontract"
)

func taskFixture(t *testing.T, write func(*apicontract.Operation, apicontract.Input) (json.RawMessage, error)) (*Service, *Store) {
	return botFixture(t, apiBackendFunc(func(_ context.Context, _ Principal, op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		switch op.ID {
		case "listRooms":
			return json.RawMessage(`{"httpStatus":200,"data":{"items":[{"id":"room-a","name":"测试房间"},{"id":"room-b","name":"其他房间"}]}}`), nil
		case "resolveRoomPlayer":
			if in.Query["query"] == "同名玩家" {
				return json.RawMessage(`{"httpStatus":200,"data":{"status":"ambiguous","total":2,"items":[{"id":"KU_A","name":"同名玩家"},{"id":"KU_B","name":"同名玩家"}]}}`), nil
			}
			return json.RawMessage(`{"httpStatus":200,"data":{"status":"resolved","total":1,"items":[{"id":"KU_EXAMPLE","name":"示例玩家"}]}}`), nil
		case "getRoomPlayerInventory":
			return json.RawMessage(`{"httpStatus":200,"data":{"status":"complete","source":"save","items":[]}}`), nil
		case "listRoomWorlds":
			return json.RawMessage(`{"httpStatus":200,"data":{"items":[{"id":"world-a","name":"Master","role":"master"}]}}`), nil
		case "getJob":
			return json.RawMessage(`{"httpStatus":200,"data":{"id":"job-a","roomId":"room-a","status":"succeeded","outcome":"full"}}`), nil
		default:
			if write != nil {
				return write(op, in)
			}
			t.Fatalf("unexpected operation %s", op.ID)
			return nil, nil
		}
	}))
}
func taskPrincipal(profile string) Principal {
	return Principal{ID: "task-client", Profile: profile, RoomIDs: []string{"room-a"}}
}
func callTask(t *testing.T, s *Service, p Principal, name string, in any) TaskResponse {
	t.Helper()
	raw, _ := json.Marshal(in)
	out, e := s.InvokeTask(context.Background(), p, name, raw)
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func TestTaskFriendlyQueriesResolveAndNeverChooseNamesakes(t *testing.T) {
	s, _ := taskFixture(t, nil)
	p := taskPrincipal(ProfileGameRead)
	r := callTask(t, s, p, "dst_player", map[string]any{"room": "测试房间", "player": "示例玩家", "view": "inventory"})
	if r.State != "succeeded" || r.Result.Source != "save" || r.TaskRef == "" {
		t.Fatal(r)
	}
	r = callTask(t, s, p, "dst_player", map[string]any{"player": "同名玩家", "view": "inventory"})
	if r.State != "needs_input" || r.TaskRef != "" {
		t.Fatal(r)
	}
	r = callTask(t, s, p, "dst_servers", map[string]any{})
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), "room-b") {
		t.Fatal(string(encoded))
	}
}
func TestTaskPreparedWriteIsDurableAndNeverReplayed(t *testing.T) {
	var calls atomic.Int32
	s, store := taskFixture(t, func(op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		if op.ID != "createRoomBackup" {
			t.Fatal(op.ID)
		}
		calls.Add(1)
		return json.RawMessage(`{"httpStatus":202,"data":{"id":"job-a","roomId":"room-a","status":"queued"}}`), nil
	})
	p := taskPrincipal(ProfileGameManage)
	r := callTask(t, s, p, "dst_server_action", map[string]any{"room": "room-a", "action": "backup"})
	if r.State != "prepared" || calls.Load() != 0 {
		t.Fatal(r, calls.Load())
	}
	in := map[string]any{"mode": "execute", "taskRef": r.TaskRef}
	first := callTask(t, s, p, "dst_run", in)
	if first.State != "pending" {
		t.Fatal(first)
	}
	if e := store.Migrate(); e != nil {
		t.Fatal(e)
	}
	second := callTask(t, s, p, "dst_run", in)
	if second.State != "succeeded" || calls.Load() != 1 {
		t.Fatal(second, calls.Load())
	}
	other := p
	other.ID = "other-client"
	raw, _ := json.Marshal(map[string]any{"taskRef": r.TaskRef})
	if _, e := s.InvokeTask(context.Background(), other, "dst_result", raw); !errors.Is(e, ErrDenied) {
		t.Fatal(e)
	}
	p.RoomIDs = []string{"room-b"}
	if _, e := s.InvokeTask(context.Background(), p, "dst_result", raw); !errors.Is(e, ErrDenied) {
		t.Fatal(e)
	}
}
func TestTaskUncertainDispatchAndInterruptedReservationNeverRetry(t *testing.T) {
	var calls atomic.Int32
	s, store := taskFixture(t, func(*apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		calls.Add(1)
		return nil, errors.New("connection lost")
	})
	p := taskPrincipal(ProfileGameManage)
	r := callTask(t, s, p, "dst_server_action", map[string]any{"action": "backup"})
	in := map[string]any{"mode": "execute", "taskRef": r.TaskRef}
	if got := callTask(t, s, p, "dst_run", in); got.State != "uncertain" {
		t.Fatal(got)
	}
	callTask(t, s, p, "dst_run", in)
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	r = callTask(t, s, p, "dst_server_action", map[string]any{"action": "backup"})
	store.db.Table(store.taskTable).Where("id = ?", r.TaskRef).UpdateColumn("state", "submitted")
	if got := callTask(t, s, p, "dst_run", map[string]any{"mode": "execute", "taskRef": r.TaskRef}); got.State != "uncertain" {
		t.Fatal(got)
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}
func TestTaskValidationPermissionAndExpiry(t *testing.T) {
	s, store := taskFixture(t, nil)
	reader := taskPrincipal(ProfileGameRead)
	manager := taskPrincipal(ProfileGameManage)
	for _, tc := range []struct {
		p         Principal
		name, raw string
	}{
		{reader, "dst_run", `{"mode":"prepare","capabilityId":"room.backup.create"}`},
		{reader, "dst_read", `{"capabilityId":"room.backup.create"}`},
		{manager, "dst_read", `{"capabilityId":"custom.anything","readOnly":true}`},
		{manager, "dst_world", `{"world":"地面","view":"entity_count","parameters":{"prefab":"spiderden\");print(1);--"}}`},
		{manager, "dst_run", `{"mode":"execute","taskRef":"invalid","parameters":{"count":9}}`},
		{manager, "dst_player_action", `{"player":"示例玩家","action":"give_item","parameters":{"item":"肉丸","count":41}}`},
		{manager, "dst_find", `{"query":"a","query":"b"}`},
	} {
		if _, e := s.InvokeTask(context.Background(), tc.p, tc.name, json.RawMessage(tc.raw)); e == nil {
			t.Fatal(tc)
		}
	}
	r := callTask(t, s, manager, "dst_server_action", map[string]any{"action": "backup"})
	store.db.Table(store.taskTable).Where("id = ?", r.TaskRef).UpdateColumn("expires_at", time.Now().Add(-time.Hour))
	if got := callTask(t, s, manager, "dst_run", map[string]any{"mode": "execute", "taskRef": r.TaskRef}); got.State != "expired" {
		t.Fatal(got)
	}
}
func TestTaskDiscoveryAndAdmissionBounded(t *testing.T) {
	s, _ := taskFixture(t, nil)
	reader := taskPrincipal(ProfileGameRead)
	r := callTask(t, s, reader, "dst_find", map[string]any{"query": "查看玩家背包"})
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), "dst_player") || !strings.Contains(string(raw), `"input":{"view":"inventory"}`) || strings.Contains(string(raw), "give_item") {
		t.Fatal(string(raw))
	}
	s.taskSlots <- struct{}{}
	s.taskSlots <- struct{}{}
	if _, e := s.InvokeTask(context.Background(), reader, "dst_servers", json.RawMessage(`{}`)); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	<-s.taskSlots
	<-s.taskSlots
}

func TestTaskDirectoryProtocolVersionsCannotReuseEachOthersCursor(t *testing.T) {
	s, _ := taskFixture(t, nil)
	p := taskPrincipal(ProfileGameRead)
	first, err := s.BotDirectory(p, 1, 2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.BotDirectory(p, 2, 2, first["revision"].(string), "")
	if err != nil || second["notModified"] == true || second["revision"] == first["revision"] {
		t.Fatal(second, err)
	}
	if _, err := s.BotDirectory(p, 2, 2, "", first["nextCursor"].(string)); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	rollback, err := s.BotDirectory(p, 1, 2, second["revision"].(string), "")
	if err != nil || rollback["notModified"] == true || rollback["revision"] != first["revision"] {
		t.Fatal(rollback, err)
	}
}

func TestTaskReadProjectionSurvivesResultLookup(t *testing.T) {
	s, _ := taskFixture(t, func(op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		if op.ID != "listRoomPlayers" {
			t.Fatal(op.ID)
		}
		return json.RawMessage(`{"httpStatus":200,"data":{"total":2,"online":2,"items":[{"id":"KU_EXAMPLE","name":"示例玩家","netScore":0},{"id":"KU_OTHER","name":"其他玩家","netScore":2}]}}`), nil
	})
	p := taskPrincipal(ProfileGameRead)
	first := callTask(t, s, p, "dst_player", map[string]any{"player": "示例玩家", "view": "network"})
	for _, got := range []TaskResponse{first, callTask(t, s, p, "dst_result", map[string]any{"taskRef": first.TaskRef})} {
		data, _ := json.Marshal(got)
		if strings.Contains(string(data), "KU_OTHER") || !strings.Contains(string(data), "raw_scores") {
			t.Fatal(string(data))
		}
		projection := got.Result.Data.(map[string]any)
		if _, ok := projection["total"]; ok || projection["roomSummary"].(map[string]any)["total"] != float64(2) {
			t.Fatalf("room totals must not describe the selected player's rows: %s", data)
		}
	}
}

func TestTaskMissingParametersAndExpiredReadCleanupKeepWrites(t *testing.T) {
	s, store := taskFixture(t, nil)
	p := taskPrincipal(ProfileGameManage)
	missing := callTask(t, s, p, "dst_player_action", map[string]any{"action": "give_item", "player": "示例玩家"})
	if missing.State != "needs_input" || missing.TaskRef != "" {
		t.Fatal(missing)
	}
	write := callTask(t, s, p, "dst_server_action", map[string]any{"action": "backup"})
	read := callTask(t, s, p, "dst_player", map[string]any{"player": "示例玩家", "view": "inventory"})
	past := time.Now().Add(-48 * time.Hour)
	store.db.Table(store.taskTable).Where("id IN (?)", []string{write.TaskRef, read.TaskRef}).UpdateColumn("created_at", past)
	callTask(t, s, p, "dst_player", map[string]any{"player": "示例玩家", "view": "inventory"})
	if _, _, e := s.loadTask(p, read.TaskRef); e == nil {
		t.Fatal("expired read kept")
	}
	if _, _, e := s.loadTask(p, write.TaskRef); e != nil {
		t.Fatal("write identity removed", e)
	}
}

func TestLocationDiscoveryUsesCallableTaskRoutesAndReadScope(t *testing.T) {
	s, _ := botFixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		t.Fatal("discovery must not execute an API")
		return nil, nil
	}))
	for _, tc := range []struct{ query, capability, tool, profile string }{
		{"已保存地点", "room.locations", "dst_read", ProfileGameRead},
		{"有哪些地点", "room.locations", "dst_read", ProfileGameRead},
		{"保存地点", "location.save", "dst_run", ProfileGameManage},
		{"传送到基地", "player.teleport", "dst_run", ProfileGameManage},
		{"把玩家甲传送到营地", "player.teleport", "dst_run", ProfileGameManage},
	} {
		t.Run(tc.query, func(t *testing.T) {
			r := callTask(t, s, taskPrincipal(tc.profile), "dst_find", map[string]any{"query": tc.query})
			items := r.Data.(map[string]any)["items"].([]map[string]any)
			found := false
			for _, item := range items {
				if item["id"] != tc.capability {
					continue
				}
				found = true
				if item["tool"] != tc.tool {
					t.Fatalf("uncallable route: %#v", item)
				}
				in := item["input"].(map[string]any)
				if tc.tool == "dst_run" && (in["mode"] != "prepare" || in["capabilityId"] != tc.capability) {
					t.Fatal(in)
				}
			}
			if !found {
				t.Fatalf("missing %s: %#v", tc.capability, items)
			}
		})
	}
	read := callTask(t, s, taskPrincipal(ProfileGameRead), "dst_find", map[string]any{"query": "传送到基地"})
	for _, item := range read.Data.(map[string]any)["items"].([]map[string]any) {
		if item["access"] != "read" {
			t.Fatal("discovery widened permissions", item)
		}
	}
	for _, c := range BotCapabilities() {
		if c.ID == "room.locations" && (c.Category != "status" || c.Access != "read") {
			t.Fatal(c)
		}
		// New player.* actions must not be routed to an unsupported enum merely
		// because their ID happens to share the player prefix.
		if recommendedTask(c) != "dst_player_action" {
			continue
		}
		in := recommendedTaskInput(c)
		in["player"] = "KU_EXAMPLE"
		raw, _ := json.Marshal(in)
		if _, err := parseTask("dst_player_action", raw); err != nil {
			t.Fatalf("%s: %v", c.ID, err)
		}
	}
}
