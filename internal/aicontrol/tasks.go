package aicontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"dont/internal/apicontract"
	consoleapi "dont/internal/console"
	"dont/internal/intent"
	"github.com/google/jsonschema-go/jsonschema"
)

// Task selectors are user-facing names or real IDs, never filesystem paths or Lua.
type TaskInput struct {
	Tool         string         `json:"tool,omitempty"`
	Room         string         `json:"room,omitempty"`
	Player       string         `json:"player,omitempty"`
	World        string         `json:"world,omitempty"`
	View         string         `json:"view,omitempty"`
	Action       string         `json:"action,omitempty"`
	Query        string         `json:"query,omitempty"`
	CapabilityID string         `json:"capabilityId,omitempty"`
	Parameters   map[string]any `json:"parameters,omitempty"`
	TaskRef      string         `json:"taskRef,omitempty"`
	Mode         string         `json:"mode,omitempty"`
}
type TaskResponse struct {
	State        string         `json:"state"`
	Summary      string         `json:"summary"`
	TaskRef      string         `json:"taskRef,omitempty"`
	RoomID       string         `json:"roomId,omitempty"`
	CapabilityID string         `json:"capabilityId,omitempty"`
	Parameters   map[string]any `json:"parameters,omitempty"`
	ExpiresAt    *time.Time     `json:"expiresAt,omitempty"`
	Data         any            `json:"data,omitempty"`
	Result       *BotResult     `json:"result,omitempty"`
}
type taskRecord struct {
	ReadOnly  bool      `gorm:"index"`
	PlayerID  string    `gorm:"type:varchar(128)"`
	ID        string    `gorm:"primary_key;type:varchar(64)"`
	Source    string    `gorm:"index;type:varchar(64)"`
	RoomID    string    `gorm:"type:varchar(255)"`
	Input     string    `gorm:"type:text"`
	State     string    `gorm:"type:varchar(24);index"`
	ExpiresAt time.Time `gorm:"index"`
	CreatedAt time.Time `gorm:"index"`
}

var taskNames = map[string]bool{
	"dst_servers": false, "dst_player": false, "dst_world": false, "dst_find": false,
	"dst_read": false, "dst_result": false, "dst_player_action": true,
	"dst_server_action": true, "dst_run": true,
}

func TaskTools() []Tool {
	selector := botString(255)
	params := map[string]any{"type": "object", "maxProperties": 32, "additionalProperties": true, "description": "仅填写该能力返回的字段，不接受脚本或操作映射。"}
	makeTool := func(name, desc string, required []string, props map[string]any) Tool {
		return Tool{Name: name, Description: desc, Mutation: taskNames[name], InputSchema: botSchema(props, required...)}
	}
	return []Tool{
		makeTool("dst_servers", "查看房间列表；指定 room 可查看房间概况、在线玩家、模组或性能。性能可用 parameters.window（1h/6h/24h/7d，默认 24h）或 RFC3339 from/to 指定时间范围。不会按列表顺序猜测一服。", nil, map[string]any{"room": selector, "view": map[string]any{"type": "string", "enum": []string{"overview", "players", "mods", "performance"}}, "parameters": params}),
		makeTool("dst_player", "按昵称或 KU ID 查询玩家。重名返回候选。network 是原始网络/客户端性能档位，不等于毫秒；缺失信息不视为零。", []string{"player"}, map[string]any{"room": selector, "player": selector, "view": map[string]any{"type": "string", "enum": []string{"profile", "inventory", "network", "activity", "trajectory", "combat"}}, "parameters": params}),
		makeTool("dst_world", "读取指定世界信息、在线玩家、玩家模式状态或实体数量。entity_count 的 parameters.prefab 是实体代码；无输出不是零。", []string{"world", "view"}, map[string]any{"room": selector, "world": selector, "view": map[string]any{"type": "string", "enum": []string{"world_info", "online_players", "entity_count", "player_modes"}}, "parameters": params}),
		makeTool("dst_find", "按中文需求寻找已授权能力，最多返回三个候选及完整的小段参数结构。优先调用推荐的常用工具。", []string{"query"}, map[string]any{"query": botString(128), "room": selector}),
		makeTool("dst_read", "执行 dst_find 找到的固定只读能力。room/player/world 可填写名字；parameters 为 JSON 对象，不能提交 Lua。", []string{"capabilityId"}, map[string]any{"capabilityId": botString(64), "room": selector, "player": selector, "world": selector, "parameters": params}),
		makeTool("dst_result", "查看原任务的进度和证据。绝不重新执行；结果不确定时继续查原任务。", []string{"taskRef"}, map[string]any{"taskRef": botString(64)}),
		makeTool("dst_player_action", "准备玩家管理任务：无敌/上帝模式 god_mode、制作/创造模式 creative_mode（均须 parameters.enabled 明确开关），以及复活、物品、生物、踢人、封禁/解封、提醒、死亡或重选角色。用 dst_find 获取参数；准备不会修改游戏，随后用 dst_run execute 提交同一 taskRef。", []string{"action", "player"}, map[string]any{"room": selector, "player": selector, "action": map[string]any{"type": "string", "enum": []string{"resurrect", "give_item", "spawn_entity", "kick", "god_mode", "creative_mode", "announce", "ban", "unban", "kill", "reset_character"}}, "parameters": params}),
		makeTool("dst_server_action", "准备房间管理任务，返回固定 taskRef；准备不会停服或更新。后续用 dst_run execute 提交。", []string{"action"}, map[string]any{"room": selector, "action": map[string]any{"type": "string", "enum": []string{"start", "stop", "restart", "save", "backup", "configure_backup"}}, "parameters": params}),
		makeTool("dst_run", "mode=prepare 准备扩展管理能力；mode=execute 只携带原 taskRef。重试相同 taskRef 返回原操作，不能修改目标或参数；不确定时不新建任务。", []string{"mode"}, map[string]any{"mode": map[string]any{"type": "string", "enum": []string{"prepare", "execute"}}, "taskRef": botString(64), "capabilityId": botString(64), "room": selector, "player": selector, "world": selector, "parameters": params}),
	}
}

func parseTask(name string, raw json.RawMessage) (TaskInput, error) {
	if len(raw) > 12<<10 {
		return TaskInput{}, ErrInvalid
	}
	fields, err := StrictBotJSON(raw)
	if err != nil {
		return TaskInput{}, err
	}
	var tool *Tool
	for _, t := range TaskTools() {
		if t.Name == name {
			v := t
			tool = &v
			break
		}
	}
	if tool == nil {
		return TaskInput{}, ErrInvalid
	}
	schemaRaw, _ := json.Marshal(tool.InputSchema)
	var sch jsonschema.Schema
	if json.Unmarshal(schemaRaw, &sch) != nil {
		return TaskInput{}, ErrInvalid
	}
	validator, err := sch.Resolve(nil)
	if err != nil || validator.Validate(fields) != nil {
		return TaskInput{}, ErrInvalid
	}
	var in TaskInput
	if json.Unmarshal(raw, &in) != nil {
		return in, ErrInvalid
	}
	for _, s := range []string{in.Room, in.Player, in.World, in.Query} {
		if strings.ContainsAny(s, "\x00\r\n") {
			return in, ErrInvalid
		}
	}
	in.Tool = name
	return in, nil
}

func (s *Service) InvokeTask(ctx context.Context, p Principal, name string, raw json.RawMessage) (TaskResponse, error) {
	if !s.Enabled() || p.ID == "" || !p.AllowsTool(name) || s.catalog == nil {
		return TaskResponse{}, ErrDenied
	}
	in, err := parseTask(name, raw)
	if err != nil {
		return TaskResponse{}, err
	}
	select {
	case s.taskSlots <- struct{}{}:
		defer func() { <-s.taskSlots }()
	default:
		return TaskResponse{}, ErrBusy
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if name == "dst_result" {
		return s.taskResult(ctx, p, in.TaskRef)
	}
	if name == "dst_run" && in.Mode == "execute" {
		if in.TaskRef == "" || in.CapabilityID != "" || in.Room != "" || in.Player != "" || in.World != "" || in.Parameters != nil {
			return TaskResponse{}, ErrInvalid
		}
		return s.executeTask(ctx, p, in.TaskRef)
	}
	if in.TaskRef != "" {
		return TaskResponse{}, ErrInvalid
	}
	if name == "dst_find" {
		if strings.TrimSpace(in.Query) == "" {
			return TaskResponse{}, ErrInvalid
		}
		if in.Room != "" {
			rooms, e := s.taskRooms(ctx, p)
			if e != nil {
				return TaskResponse{}, e
			}
			room, choices := selectTaskTarget(rooms, in.Room)
			if room == "" {
				return taskChoice("请选择房间。", choices), nil
			}
			in.Room = room
		}
		return s.findTasks(p, in)
	}
	rooms, err := s.taskRooms(ctx, p)
	if err != nil {
		return TaskResponse{}, err
	}
	if name == "dst_servers" && in.Room == "" && in.View == "" {
		return TaskResponse{State: "succeeded", Summary: "以下为当前允许访问的房间。", Data: map[string]any{"items": rooms}}, nil
	}
	room, choices := selectTaskTarget(rooms, in.Room)
	if room == "" {
		return taskChoice("请选择要操作的房间。", choices), nil
	}
	in.Room = room
	capability := in.CapabilityID
	switch name {
	case "dst_servers":
		capability = "room.status"
		if in.View != "" && in.View != "overview" {
			capability = "room." + in.View
		}
	case "dst_player":
		if in.View == "" {
			in.View = "profile"
		}
		capability = map[string]string{"profile": "player.resolve", "inventory": "player.inventory", "network": "room.network", "activity": "player.activity", "trajectory": "player.trajectory", "combat": "player.combat"}[in.View]
	case "dst_world":
		capability = "world." + in.View
	case "dst_player_action":
		capability = "player." + in.Action
	case "dst_server_action":
		capability = "room." + in.Action
		if in.Action == "backup" {
			capability = "room.backup.create"
		}
		if in.Action == "configure_backup" {
			capability = "room.backups.configure"
		}
	}
	c, err := s.botCapability(p, capability, room)
	if err != nil {
		return TaskResponse{}, err
	}
	write := taskNames[name]
	if (c.Access == "write") != write {
		return TaskResponse{}, ErrDenied
	}
	if c.Risk == "critical" && !nearbyPreparation(c, in.Parameters) {
		return TaskResponse{State: "needs_confirmation", Summary: "此操作需要用户在面板或可信插件中确认。", CapabilityID: c.ID}, nil
	}
	params := in.Parameters
	if params == nil {
		params = map[string]any{}
	}
	if c.ID == "player.resolve" {
		params["query"] = in.Player
	}
	if in.Player != "" && c.ID != "player.resolve" {
		data, e := s.taskRead(ctx, p, "resolveRoomPlayer", apicontract.Input{Path: map[string]any{"roomId": room}, Query: map[string]any{"query": in.Player}})
		if e != nil {
			return TaskResponse{}, e
		}
		if data["status"] == "not_found" && strings.HasPrefix(in.Player, "玩家") {
			if nick := strings.TrimSpace(strings.TrimPrefix(in.Player, "玩家")); nick != "" {
				if retry, err := s.taskRead(ctx, p, "resolveRoomPlayer", apicontract.Input{Path: map[string]any{"roomId": room}, Query: map[string]any{"query": nick}}); err == nil {
					data = retry
				}
			}
		}
		rows := taskRows(data)
		if data["status"] != "resolved" || data["hasMore"] == true || len(rows) != 1 {
			return taskChoice("请确认要查询或操作的玩家；未找到记录不代表玩家从未进入。", rows), nil
		}
		player, _ := rows[0]["id"].(string)
		if player == "" {
			return TaskResponse{}, ErrUnavailable
		}
		if c.ID == "room.network" {
			params["playerId"] = player
		} else {
			for key, hint := range c.ParameterHints {
				if hint == "player" && (key == "playerId" || key == "player_id") {
					if v, exists := params[key]; exists && v != player {
						return TaskResponse{}, ErrInvalid
					}
					params[key] = player
				}
			}
		}
	}
	if in.World != "" {
		data, e := s.taskRead(ctx, p, "listRoomWorlds", apicontract.Input{Path: map[string]any{"roomId": room}})
		if e != nil {
			return TaskResponse{}, e
		}
		rows := taskRows(data)
		world, choices := selectTaskWorld(rows, in.World)
		if world == "" {
			return taskChoice("请选择要查询的世界。", choices), nil
		}
		if v, exists := params["worldId"]; exists && v != world {
			return TaskResponse{}, ErrInvalid
		}
		params["worldId"] = world
	}
	// Network is a fixed directory projection, not arbitrary per-player Lua.
	networkPlayer, _ := params["playerId"].(string)
	if c.ID == "room.network" {
		delete(params, "playerId")
	}
	input := BotInput{RoomID: room, CapabilityID: c.ID, ContractVersion: 1, SchemaRevision: c.SchemaRevision, Parameters: params}
	if required, ok := c.Parameters["required"].([]string); ok {
		for _, key := range required {
			if _, present := params[key]; !present {
				label := map[string]string{"playerId": "玩家昵称或 KU ID", "player_id": "玩家昵称或 KU ID", "worldId": "世界", "prefab": "实体代码", "count": "数量", "item": "物品", "entity": "生物", "enabled": "是否启用", "confirmation": "完整房间名称", "reason": "封禁原因", "duration": "封禁时长", "message": "提醒内容", "confirmed": "用户明确确认"}[key]
				if label == "" {
					label = key
				}
				return TaskResponse{State: "needs_input", Summary: "请补充" + label + "。", RoomID: room, CapabilityID: c.ID, Data: map[string]any{"field": key, "parameters": c.Parameters}}, nil
			}
		}
	}
	if err = validateTaskCapability(s, p, c, input); err != nil {
		return TaskResponse{}, err
	}
	record, e := s.prepareTask(p, input, write, networkPlayer)
	if e != nil {
		return TaskResponse{}, e
	}
	if write {
		return preparedTask(record, input), nil
	}
	result, e := s.BotExecute(ctx, p, inputWithTask(input, record.ID), false)
	if e != nil {
		return TaskResponse{State: "uncertain", TaskRef: record.ID, Summary: "查询结果尚未确认，请读取原任务。"}, nil
	}
	response := projectTaskResponse(record, result)
	return response, nil
}

func (s *Service) taskRead(ctx context.Context, p Principal, id string, in apicontract.Input) (map[string]any, error) {
	return s.botBackend(ctx, p, BotCapability{Operation: id, Access: "read"}, in)
}
func taskRows(data map[string]any) []map[string]any {
	rows := []map[string]any{}
	values, _ := data["items"].([]any)
	for _, v := range values {
		if row, ok := v.(map[string]any); ok {
			rows = append(rows, row)
		}
	}
	return rows
}
func (s *Service) taskRooms(ctx context.Context, p Principal) ([]map[string]any, error) {
	data, e := s.taskRead(ctx, p, "listRooms", apicontract.Input{})
	if e != nil {
		return nil, e
	}
	rows := taskRows(data)
	if len(rows) > 256 || data["hasMore"] == true {
		return nil, ErrUnavailable
	}
	if total, ok := data["total"].(float64); ok && total > float64(len(rows)) {
		return nil, ErrUnavailable
	}
	out := []map[string]any{}
	for _, r := range rows {
		id, _ := r["id"].(string)
		if id != "" && p.AllowsRoom(id) {
			out = append(out, botPick(r, "id", "name"))
		}
	}
	return out, nil
}
func selectTaskTarget(rows []map[string]any, query string) (string, []map[string]any) {
	matches := []map[string]any{}
	for _, r := range rows {
		if query == "" || r["id"] == query || r["name"] == query {
			matches = append(matches, r)
		}
	}
	if len(matches) == 1 {
		id, _ := matches[0]["id"].(string)
		return id, matches
	}
	if len(matches) == 0 {
		matches = rows
	}
	return "", matches[:min(len(matches), 5)]
}
func selectTaskWorld(rows []map[string]any, query string) (string, []map[string]any) {
	if query == "地面" || query == "洞穴" {
		matches := []map[string]any{}
		for _, r := range rows {
			if query == "地面" && r["role"] == "master" || query == "洞穴" && r["role"] == "caves" {
				matches = append(matches, r)
			}
		}
		return selectTaskTarget(matches, "")
	}
	return selectTaskTarget(rows, query)
}
func taskChoice(summary string, rows []map[string]any) TaskResponse {
	choices := []map[string]any{}
	for _, r := range rows[:min(len(rows), 5)] {
		choices = append(choices, botPick(r, "id", "name", "worldName", "online"))
	}
	return TaskResponse{State: "needs_input", Summary: summary, Data: map[string]any{"choices": choices}}
}
func recommendedTask(c BotCapability) string {
	switch c.ID {
	case "room.status", "room.players", "room.mods", "room.performance":
		return "dst_servers"
	case "player.inventory", "player.resolve", "room.network", "player.activity", "player.trajectory", "player.combat":
		return "dst_player"
	}
	if strings.HasPrefix(c.ID, "world.") {
		return "dst_world"
	}
	if slices.Contains([]string{"player.resurrect", "player.give_item", "player.spawn_entity", "player.kick", "player.god_mode", "player.creative_mode", "player.announce", "player.ban", "player.unban", "player.kill", "player.reset_character"}, c.ID) && c.Access == "write" {
		return "dst_player_action"
	}
	if slices.Contains([]string{"room.start", "room.stop", "room.restart", "room.save", "room.backup.create", "room.backups.configure"}, c.ID) {
		return "dst_server_action"
	}
	if c.Access == "write" {
		return "dst_run"
	}
	return "dst_read"
}

func recommendedTaskInput(c BotCapability) map[string]any {
	input := map[string]any{}
	switch recommendedTask(c) {
	case "dst_servers":
		input["view"] = strings.TrimPrefix(c.ID, "room.")
		if c.ID == "room.status" {
			input["view"] = "overview"
		}
	case "dst_player":
		input["view"] = strings.Split(c.ID, ".")[1]
		if c.ID == "player.resolve" {
			input["view"] = "profile"
		}
	case "dst_world":
		input["view"] = strings.TrimPrefix(c.ID, "world.")
	case "dst_player_action", "dst_server_action":
		input["action"] = strings.Split(c.ID, ".")[1]
		if c.ID == "room.backups.configure" {
			input["action"] = "configure_backup"
		}
	default:
		input["capabilityId"] = c.ID
		if c.Access == "write" {
			input["mode"] = "prepare"
		}
	}
	return input
}
func (s *Service) findTasks(p Principal, in TaskInput) (TaskResponse, error) {
	byID := map[string]BotCapability{}
	docs := []intent.CapabilityDoc{}
	for _, c := range BotCapabilities() {
		allowed := false
		scope := append(append([]string{}, p.RoomIDs...), p.RoomID, "__unscoped__")
		if in.Room != "" {
			scope = []string{in.Room}
		}
		for _, room := range scope {
			if _, e := s.botCapability(p, c.ID, room); e == nil {
				allowed = true
				break
			}
		}
		if !allowed {
			continue
		}
		byID[c.ID] = c
		actions := make([]intent.ActionDoc, len(c.Actions))
		for i, a := range c.Actions {
			actions[i] = intent.ActionDoc{
				ID:          a.ID,
				Title:       a.Title,
				Description: a.Description,
				SearchTerms: a.SearchTerms,
				Parameters:  a.Parameters,
			}
		}
		docs = append(docs, intent.CapabilityDoc{
			ID:             c.ID,
			Title:          c.Title,
			Category:       c.Category,
			Access:         c.Access,
			Risk:           c.Risk,
			Tier:           c.Tier,
			Confirm:        c.Confirm,
			SearchTerms:    c.SearchTerms,
			Aliases:        c.Aliases,
			Examples:       c.Examples,
			Negatives:      c.Negatives,
			Prefer:         c.Prefer,
			Parameters:     c.Parameters,
			ParameterHints: c.ParameterHints,
			SlotRoles:      c.SlotRoles,
			Actions:        actions,
			ActionPolicies: c.ActionPolicies,
		})
	}
	ranked := s.rankDiscovery(context.Background(), in.Query, docs)
	rows := ranked.Candidates
	items := []map[string]any{}
	for _, v := range rows[:min(len(rows), 3)] {
		c := byID[v.Capability.ID]
		input := recommendedTaskInput(c)
		item := map[string]any{"id": c.ID, "title": c.Title, "access": c.Access, "risk": c.Risk, "tier": c.Tier, "confirm": c.Confirm, "parameters": modelParameters(c), "parameterHints": c.ParameterHints, "tool": recommendedTask(c), "input": input, "schemaRevision": c.SchemaRevision}
		if v.Action != nil {
			item["title"], item["actionId"], item["description"] = v.Action.Title, v.Action.ID, v.Action.Description
			if policy, ok := c.ActionPolicies[v.Action.ID]; ok {
				item["tier"], item["confirm"] = string(policy.Tier), string(policy.Confirm)
			}
			input["parameters"] = v.Action.Parameters
		} else if len(v.ExtractedSlots) > 0 {
			item["extractedParameters"] = v.ExtractedSlots
		}
		items = append(items, item)
	}
	return TaskResponse{State: "succeeded", Summary: "以下为已授权的候选；没有执行操作。需要确认的动作请在面板或可信插件中继续；不要把查询、否定或讨论当作执行请求。", Data: map[string]any{"items": items, "hasMore": len(rows) > 3}}, nil
}

// modelParameters removes internal and presentation fields (such as feedback,
// preview_id and confirmation) from discovery cards so models do not ask the
// user to choose panel-internal enums.
func modelParameters(c BotCapability) map[string]any {
	if len(c.SlotRoles) == 0 {
		return c.Parameters
	}
	props, ok := c.Parameters["properties"].(map[string]any)
	if !ok {
		return c.Parameters
	}
	filteredProps := map[string]any{}
	for k, v := range props {
		if slot, found := c.SlotRoles[k]; found && !slot.Model {
			continue
		}
		filteredProps[k] = v
	}
	out := map[string]any{}
	for k, v := range c.Parameters {
		out[k] = v
	}
	out["properties"] = filteredProps
	if req, ok := c.Parameters["required"].([]string); ok {
		filteredReq := []string{}
		for _, k := range req {
			if slot, found := c.SlotRoles[k]; found && !slot.Model {
				continue
			}
			filteredReq = append(filteredReq, k)
		}
		out["required"] = filteredReq
	}
	return out
}
func validateTaskCapability(s *Service, p Principal, c BotCapability, in BotInput) error {
	encoded, _ := json.Marshal(in.Parameters)
	params, e := StrictBotJSON(encoded)
	if e != nil {
		return e
	}
	schemaRaw, _ := json.Marshal(c.Parameters)
	var sch jsonschema.Schema
	if json.Unmarshal(schemaRaw, &sch) != nil {
		return ErrInvalid
	}
	validator, e := sch.Resolve(nil)
	if e != nil || validator.Validate(params) != nil {
		return fmt.Errorf("%w: 请按能力说明补全参数", ErrInvalid)
	}
	op, ok := s.catalog.Operation(c.Operation)
	if !ok {
		return ErrDenied
	}
	target := c.build(in.RoomID, params)
	if !p.AllowsAPI(op, target) {
		return ErrDenied
	}
	if op.Mutation() {
		target.RequestID = "task-parameter-validation"
	}
	raw, _ := json.Marshal(target)
	_, e = op.Validate(raw)
	return e
}
func inputWithTask(in BotInput, id string) BotInput { in.RequestID = "task-" + id; return in }

func nearbyPreparation(c BotCapability, p map[string]any) bool {
	if c.ID != "workbench.act_nearby_entities" {
		return false
	}
	return consoleapi.EffectiveWorkbenchRisk(consoleapi.Definition{ID: "act_nearby_entities", Risk: consoleapi.RiskCritical}, p) == consoleapi.RiskLow
}
