package aicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"dont/internal/apicontract"
)

// A previously unknown API is defined only here, not in capabilities, search
// terms, permission maps or plugin code. This is the frozen-client boundary.
func newGenericCatalog(t *testing.T, extra string) *apicontract.Catalog {
	t.Helper()
	c, e := apicontract.Parse([]byte(`openapi: 3.1.0
paths:
  /rooms/{roomId}/nebula:
    parameters:
      - {name: roomId, in: path, required: true, schema: {type: string}}
    get:
      operationId: inspectNebula
      summary: 查看星云读数
      tags: [Rooms]
      x-dst-access: {profile: game-read, access: read}
      responses:
        '200': {description: Samples, content: {application/json: {schema: {type: array, items: {type: number}}}}}
    patch:
      operationId: adjustNebula
      summary: 调整星云设置
      tags: [Rooms]
      x-dst-access: {profile: game-manage, access: write, confirmation: none}
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              additionalProperties: false
              properties:
                enabled: {type: boolean, default: true}
                count: {type: integer, minimum: 0, maximum: 10, default: 5}
                label: {type: [string, 'null'], maxLength: 40}
                choice: {oneOf: [{type: string}, {type: integer}]}
      responses:
        '204': {description: Updated}
  /nebula-plan:
    post:
      operationId: startNebulaPlan
      summary: 启动全局星云任务
      tags: [System]
      x-dst-access: {profile: panel-manage, access: write, confirmation: destructive}
      responses:
        '202': {description: Accepted}
  /nebula-version:
    get:
      operationId: inspectNebulaVersion
      summary: 查看全局星云版本
      tags: [System]
      responses:
        '200': {description: Version}
` + extra))
	if e != nil {
		t.Fatal(e)
	}
	return c
}

func TestGenericRecoveryAfterLostResponseNeverDispatches(t *testing.T) {
	var calls atomic.Int32
	s, _ := fixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{"httpStatus":204}`), nil
	}))
	s.ConfigureAPI(newGenericCatalog(t, ""))
	actor := genericActor(ProfileGameManage)
	ceiling := Principal{ID: "recovery-service"}
	request := GenericInput{Actor: actor, Mode: "run", Operation: "adjustNebula", RequestID: "original-message-001", Arguments: map[string]any{"path": map[string]any{"roomId": "room-test"}, "body": map[string]any{"count": 0}}}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.Generic(context.Background(), ceiling, request) }()
	}
	wg.Wait()
	for i := 0; i < 3; i++ {
		out, err := s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "recover", RequestID: request.RequestID})
		if err != nil || out.(map[string]any)["state"] != "succeeded" {
			t.Fatal(out, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("write sent %d times", calls.Load())
	}
	unknown, err := s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "recover", RequestID: "never-visible-request"})
	if err != nil || unknown.(map[string]any)["state"] != "uncertain" || calls.Load() != 1 {
		t.Fatal(unknown, err, calls.Load())
	}
	actor.Writes = nil
	if _, err = s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "recover", RequestID: request.RequestID}); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked recovery", err)
	}
}

func TestGenericWorkbenchAndRoomPolicyAreNotWidened(t *testing.T) {
	s, _ := fixture(t, nil)
	c, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(c)
	a := genericActor(ProfilePanelManage)
	p, err := s.companionPrincipal(Principal{ID: "scope-service"}, a)
	if err != nil || !p.Restricted() || p.AllowsRoom("other-room") || !p.AllowsGlobalTasks() {
		t.Fatal("room scope cleared", p, err)
	}
	for _, id := range []string{"executeBuiltinRoomCommand", "executeRoomCommand", "executeRawRoomCommand"} {
		op, _ := c.Operation(id)
		for _, mode := range []string{"off", "standard", "all"} {
			a.Workbench = mode
			if got := s.genericAllowed(p, a, op); got != (mode == "all") {
				t.Fatal(id, mode, got)
			}
		}
	}
	a = genericActor(ProfileGameManage)
	a.Categories = []string{"status"}
	a.Writes = []string{"player.god_mode"}
	p, _ = s.companionPrincipal(Principal{ID: "scope-service"}, a)
	op, _ := c.Operation("setRoomPlayerGodMode")
	if !s.genericAllowed(p, a, op) {
		t.Fatal("explicit write grant incorrectly requires public read category")
	}
}

func TestGenericFollowsOnlyOriginalTaskAndPreservesPending(t *testing.T) {
	reads, writes := 0, 0
	taskID := "task-original"
	phase := "running"
	s, _ := fixture(t, apiBackendFunc(func(_ context.Context, _ Principal, op *apicontract.Operation, _ apicontract.Input) (json.RawMessage, error) {
		if op.ID == "startTrackedSample" {
			writes++
			return json.RawMessage(`{"httpStatus":202,"data":{"id":"task-original","status":"queued"}}`), nil
		}
		reads++
		return json.Marshal(map[string]any{"httpStatus": 200, "data": map[string]any{"id": taskID, "status": phase}})
	}))
	c, err := apicontract.Parse([]byte(`openapi: 3.1.0
paths:
  /sample:
    post:
      operationId: startTrackedSample
      summary: Synthetic tracked task
      tags: [System]
      x-dst-access: {access: write, confirmation: none}
      x-dst-result-reads:
        - {operationId: readTrackedSample, parameters: {}, match: {data.id: data.id}}
      responses:
        '202': {description: Accepted, content: {application/json: {schema: {$ref: '#/components/schemas/Envelope'}}}}
    get:
      operationId: readTrackedSample
      summary: Current synthetic task
      tags: [System]
      responses:
        '200': {description: Current task, content: {application/json: {schema: {$ref: '#/components/schemas/Envelope'}}}}
components:
  schemas:
    Envelope:
      type: object
      properties:
        data: {type: object, x-dst-task-state: status, properties: {id: {type: string}, status: {type: string}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(c)
	actor := genericActor(ProfilePanelManage)
	ceiling := Principal{ID: "task-service"}
	out, err := s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "run", Operation: "startTrackedSample", RequestID: "task-message-001"})
	if err != nil {
		t.Fatal(err)
	}
	result := out.(map[string]any)
	if result["state"] != "queued" {
		t.Fatal(result)
	}
	ref := result["taskRef"].(string)
	out, err = s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "result", TaskRef: ref})
	if err != nil || out.(map[string]any)["state"] != "running" {
		t.Fatal(out, err)
	}
	phase = "succeeded"
	out, err = s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "result", TaskRef: ref})
	if err != nil || out.(map[string]any)["state"] != "succeeded" {
		t.Fatal(out, err)
	}
	taskID = "task-replacement"
	out, err = s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "result", TaskRef: ref})
	if err != nil || out.(map[string]any)["progressUnavailable"] == nil || writes != 1 || reads != 3 {
		t.Fatal(out, err, writes, reads)
	}
	actor.Deny = []string{"readTrackedSample"}
	out, err = s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "result", TaskRef: ref})
	if err != nil || reads != 3 {
		t.Fatal("revoked followup was called", out, err, reads)
	}
}
func genericActor(role string) CompanionActor {
	return CompanionActor{Identity: []string{"platform-test", "bot-test", "group-test", "user-test"}, Profile: role, Rooms: []string{"room-test"}, Categories: []string{"*"}, Writes: []string{"*"}, Workbench: "all", PanelWide: role == ProfilePanelManage}
}
func TestGenericFrozenContractDiscoveryAndPatchOmission(t *testing.T) {
	calls := 0
	s, _ := fixture(t, apiBackendFunc(func(_ context.Context, p Principal, op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		calls++
		if p.ID == "service-test" {
			t.Fatal("shared task owner")
		}
		if op.ID == "adjustNebula" {
			body := in.Body.(map[string]any)
			if len(body) != 1 || body["enabled"] != false {
				t.Fatalf("PATCH defaults applied: %#v", body)
			}
		}
		return json.RawMessage(`{"httpStatus":200,"data":[1,2]}`), nil
	}))
	s.ConfigureAPI(newGenericCatalog(t, ""))
	actor := genericActor(ProfileGameManage)
	ceiling := Principal{ID: "service-test", Profile: ProfilePanelManage}
	for _, query := range []string{"inspectNebula", "查看星云读数", "adjustNebula"} {
		out, e := s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "find", Query: query})
		if e != nil {
			t.Fatal(e)
		}
		b, _ := json.Marshal(out)
		if !strings.Contains(string(b), "api:") {
			t.Fatalf("new API absent: %s", b)
		}
	}
	in := GenericInput{Actor: actor, Mode: "run", Operation: "adjustNebula", Arguments: map[string]any{"path": map[string]any{"roomId": "room-test"}, "body": map[string]any{"enabled": false}}, RequestID: "message-test-001"}
	first, e := s.Generic(context.Background(), ceiling, in)
	if e != nil {
		t.Fatal(e)
	}
	second, e := s.Generic(context.Background(), ceiling, in)
	if e != nil {
		t.Fatal(e)
	}
	if calls != 1 || first.(map[string]any)["taskRef"] != second.(map[string]any)["taskRef"] {
		t.Fatal("write replayed", calls)
	}
	ref := first.(map[string]any)["taskRef"].(string)
	other := actor
	other.Identity = append([]string{}, actor.Identity...)
	other.Identity[3] = "other-user"
	if _, e = s.Generic(context.Background(), ceiling, GenericInput{Actor: other, Mode: "result", TaskRef: ref}); !errors.Is(e, ErrDenied) {
		t.Fatal("cross-user result", e)
	}
	actor.Deny = []string{"adjustNebula"}
	if _, e = s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "result", TaskRef: ref}); !errors.Is(e, ErrDenied) {
		t.Fatal("revocation", e)
	}
}
func TestGenericConfirmationIsOperationLocalAndNeverResendsUnknown(t *testing.T) {
	calls := 0
	s, _ := fixture(t, apiBackendFunc(func(_ context.Context, _ Principal, _ *apicontract.Operation, _ apicontract.Input) (json.RawMessage, error) {
		calls++
		return nil, ErrUncertain
	}))
	s.ConfigureAPI(newGenericCatalog(t, ""))
	actor := genericActor(ProfilePanelManage)
	ceiling := Principal{ID: "service-test"}
	prepared, e := s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "run", Operation: "startNebulaPlan", RequestID: "message-test-002"})
	if e != nil {
		t.Fatal(e)
	}
	ref := prepared.(map[string]any)["taskRef"].(string)
	if calls != 0 {
		t.Fatal("prepare executed")
	}
	s.ConfigureAPI(newGenericCatalog(t, `  /new-nebula:
    get:
      operationId: inspectNewNebula
      summary: Unrelated new API
      tags: [System]
      responses: {'200': {description: OK}}
`))
	for i := 0; i < 3; i++ {
		out, e := s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "execute", TaskRef: ref})
		if e != nil {
			t.Fatal(e)
		}
		if out.(map[string]any)["state"] != "uncertain" {
			t.Fatal(out)
		}
	}
	if calls != 1 {
		t.Fatal("unknown replayed", calls)
	}
}
func TestGenericReadBoundaryScopeAndValidation(t *testing.T) {
	s, _ := fixture(t, apiBackendFunc(func(_ context.Context, _ Principal, _ *apicontract.Operation, _ apicontract.Input) (json.RawMessage, error) {
		return json.RawMessage(`{"httpStatus":204}`), nil
	}))
	s.ConfigureAPI(newGenericCatalog(t, ""))
	ceiling := Principal{ID: "service-test"}
	for i, tc := range []struct {
		role, mode, op, room string
		body                 map[string]any
		ok                   bool
	}{
		{ProfileGameRead, "read", "inspectNebula", "room-test", nil, true},
		{ProfileGameRead, "read", "inspectNebula", "other-room", nil, false},
		{ProfileGameRead, "read", "adjustNebula", "room-test", map[string]any{}, false},
		{ProfileGameManage, "read", "adjustNebula", "room-test", map[string]any{}, false},
		{ProfileGameRead, "run", "adjustNebula", "room-test", map[string]any{}, false},
		{ProfileGameManage, "run", "adjustNebula", "room-test", map[string]any{"count": 11}, false},
		{ProfileGameManage, "run", "adjustNebula", "room-test", map[string]any{"count": 0, "label": nil}, true},
		{ProfileGameManage, "run", "adjustNebula", "room-test", map[string]any{"choice": false}, false},
		{ProfilePanelManage, "read", "inspectNebulaVersion", "", nil, true},
		{ProfileGameRead, "read", "inspectNebulaVersion", "", nil, false},
		{ProfilePanelManage, "read", "unregistered", "", nil, false},
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			args := map[string]any{}
			if tc.room != "" {
				args["path"] = map[string]any{"roomId": tc.room}
			}
			if tc.body != nil {
				args["body"] = tc.body
			}
			_, e := s.Generic(context.Background(), ceiling, GenericInput{Actor: genericActor(tc.role), Mode: tc.mode, Operation: tc.op, Arguments: args, RequestID: fmt.Sprintf("test-message-%d", i)})
			if (e == nil) != tc.ok {
				t.Fatal(e)
			}
		})
	}
	if _, e := s.Generic(context.Background(), Principal{ID: "scoped", Profile: ProfileGameRead, RoomIDs: []string{"room-test"}}, GenericInput{Actor: genericActor(ProfilePanelManage), Mode: "find", Query: "Nebula"}); !errors.Is(e, ErrDenied) {
		t.Fatal("credential elevated", e)
	}
}

func TestGenericAllNativeOperationsHaveAnAuthorizedDiscoveryPath(t *testing.T) {
	s, _ := fixture(t, nil)
	catalog, e := apicontract.Default()
	if e != nil {
		t.Fatal(e)
	}
	s.ConfigureAPI(catalog)
	ceiling := Principal{ID: "coverage-service"}
	actor := genericActor(ProfilePanelManage)
	checked := 0
	for _, op := range catalog.Operations() {
		if !op.Callable() || strings.Contains(op.Path, "/bot/") {
			continue
		}
		t.Run(op.ID, func(t *testing.T) {
			out, e := s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "find", Query: op.ID})
			if e != nil {
				t.Fatal(e)
			}
			data, _ := json.Marshal(out)
			var result struct {
				Items []struct {
					ID         string         `json:"id"`
					Parameters map[string]any `json:"parameters"`
				} `json:"items"`
			}
			_ = json.Unmarshal(data, &result)
			found := false
			for _, item := range result.Items {
				if item.ID == "api:"+op.ID {
					found = true
					if item.Parameters == nil {
						t.Error("input contract missing")
					}
				}
			}
			if !found {
				t.Fatalf("native API disappeared: %s", data)
			}
		})
		checked++
	}
	if checked < 300 {
		t.Fatal("unexpectedly incomplete native coverage", checked)
	}
	t.Logf("Verified %d distinct native API discovery paths; not a claim of QQ dialogue or game execution coverage.", checked)
}

func TestGenericUserReportedDiscoveryScenarios(t *testing.T) {
	s, _ := fixture(t, nil)
	c, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(c)
	actor := genericActor(ProfilePanelManage)
	cases := []struct {
		query    string
		expected []string
	}{
		{"复活玩家", []string{"player.resurrect", "api:executeBuiltinRoomCommand"}},
		{"复活单个玩家 阿青", []string{"player.resurrect", "api:resurrectRoomPlayer"}},
		{"玩家网络档位 netScore", []string{"api:getRoomPlayer", "api:listRoomPlayers"}},
		{"查看房间列表，以及每个房间当前在线玩家人数/在线玩家", []string{"api:listRooms"}},
		{"列出所有房间及房间ID，房间列表", []string{"api:listRooms"}},
		{"读取全部房间列表名称和ID", []string{"api:listRooms"}},
		{"给玩家开启无敌模式", []string{"api:setRoomPlayerGodMode", "workbench.set_player_mode", "workbench.set_player_invincibility"}},
		{"开启玩家创造模式", []string{"api:setRoomPlayerCreativeMode", "workbench.set_player_mode"}},
		{"跳过一天", []string{"workbench.skip_days", "workbench.skip_day"}},
		{"重置世界", []string{"api:resetRoomWorlds", "workbench.regenerate"}},
		{"最近一天服务器 CPU 内存指标", []string{"api:getRoomPerformanceReport"}},
		{"查看地图", []string{"api:listRoomMaps", "api:getMapManifest", "api:getMapLayerImage"}},
		{"查看模组配置", []string{"api:getModConfiguration", "api:getRoomModProfile"}},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			value, err := s.Generic(context.Background(), Principal{ID: "scenario-service"}, GenericInput{Actor: actor, Mode: "find", Query: tc.query})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(value)
			var envelope struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
			}
			_ = json.Unmarshal(raw, &envelope)
			ids := []string{}
			matched := false
			for _, item := range envelope.Items {
				ids = append(ids, item.ID)
				for _, want := range tc.expected {
					matched = matched || item.ID == want
				}
			}
			if !matched {
				t.Fatalf("no relevant result: %v", ids)
			}
		})
	}
}

func TestGenericGameReleaseDiscoveryRespectsGrantAndPrefersCurrentFlow(t *testing.T) {
	s, _ := fixture(t, nil)
	c, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(c)
	actor := genericActor(ProfilePanelManage)
	actor.Workbench = "standard"
	actor.Writes = []string{"api:previewGameRelease", "api:publishGameRelease", "api:retryGameRelease"}
	ceiling := Principal{ID: "game-release-discovery"}
	for _, query := range []string{"更新游戏", "直接通过面板更新游戏", "直接通过面板直接更新", "把游戏服务端更新到最新版本"} {
		out, e := s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "find", Query: query})
		if e != nil {
			t.Fatal(e)
		}
		items := out.(map[string]any)["items"].([]any)
		if len(items) == 0 || items[0].(map[string]any)["operationId"] != "publishGameRelease" {
			t.Fatalf("%q selected %v instead of game release", query, items)
		}
	}
	actor.Writes = []string{"*"}
	actor.Workbench = "all"
	out, err := s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "find", Query: "更新游戏"})
	if err != nil {
		t.Fatal(err)
	}
	items := out.(map[string]any)["items"].([]any)
	if len(items) == 0 || items[0].(map[string]any)["operationId"] != "publishGameRelease" {
		t.Fatalf("legacy update outranked current release: %v", items)
	}
	actor.Writes = nil
	out, err = s.Generic(context.Background(), ceiling, GenericInput{Actor: actor, Mode: "find", Query: "更新游戏"})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range out.(map[string]any)["items"].([]any) {
		if row.(map[string]any)["operationId"] == "publishGameRelease" {
			t.Fatal("game update visible without a write grant")
		}
	}
}

func TestGenericGameReleasePreviewIsReadOnlyDespitePost(t *testing.T) {
	var calls atomic.Int32
	s, _ := fixture(t, apiBackendFunc(func(_ context.Context, _ Principal, op *apicontract.Operation, _ apicontract.Input) (json.RawMessage, error) {
		if op.ID != "previewGameRelease" {
			t.Fatalf("unexpected operation %s", op.ID)
		}
		calls.Add(1)
		return json.RawMessage(`{"httpStatus":200,"data":{"ready":true,"updateRequired":true,"installations":[]}}`), nil
	}))
	c, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(c)
	actor := genericActor(ProfilePanelManage)
	actor.Writes = nil
	actor.Workbench = "off"
	ceiling := Principal{ID: "read-only-preview-service"}
	in := GenericInput{Actor: actor, Mode: "read", Operation: "previewGameRelease", RequestID: "read-only-preview-001", Arguments: map[string]any{"body": map[string]any{}}}
	out, err := s.Generic(context.Background(), ceiling, in)
	if err != nil || out.(map[string]any)["state"] != "succeeded" || calls.Load() != 1 {
		t.Fatalf("preview was not an immediate read: %v, %v, calls=%d", out, err, calls.Load())
	}
	in.Mode = "run"
	if _, err := s.Generic(context.Background(), ceiling, in); !errors.Is(err, ErrDenied) || calls.Load() != 1 {
		t.Fatalf("preview accepted as a write: %v, calls=%d", err, calls.Load())
	}
}

func TestGenericSingleResurrectionGrantIsDiscoverableWithoutBulkExecutor(t *testing.T) {
	s, _ := fixture(t, nil)
	c, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(c)
	actor := genericActor(ProfileGameManage)
	actor.Workbench = "off"
	actor.Writes = []string{"player.resurrect"}
	value, err := s.Generic(context.Background(), Principal{ID: "single-action-service"}, GenericInput{Actor: actor, Mode: "find", Query: "复活玩家"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	var result struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Items {
		found = found || item.ID == "player.resurrect"
		if item.ID == "workbench.apply_all_players" || item.ID == "api:executeBuiltinRoomCommand" {
			t.Fatal("single-player grant exposed aggregate executor", item.ID)
		}
	}
	if !found {
		t.Fatal("single-player resurrection missing", string(raw))
	}
}

func TestGenericTaskOutcomeAndSentAreNotFullSuccess(t *testing.T) {
	c, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	op, _ := c.Operation("createRoomBackup")
	for _, tc := range []struct{ outcome, want string }{{"full", "succeeded"}, {"partial", "partial"}, {"none", "failed"}, {"", "uncertain"}} {
		out := map[string]any{"state": "succeeded"}
		raw, _ := json.Marshal(map[string]any{"httpStatus": 202, "data": map[string]any{"status": "succeeded", "outcome": tc.outcome}})
		applyTaskState(out, op, raw)
		if out["state"] != tc.want {
			t.Fatal(tc, out)
		}
	}
	op, _ = c.Operation("executeBuiltinRoomCommand")
	out := map[string]any{"state": "succeeded"}
	applyTaskState(out, op, json.RawMessage(`{"httpStatus":201,"data":{"status":"sent"}}`))
	if out["state"] != "accepted" {
		t.Fatal(out)
	}
}
