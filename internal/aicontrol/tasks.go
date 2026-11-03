package aicontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"dont/internal/apicontract"
	consoleapi "dont/internal/console"
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
var taskTerms = map[string]string{
	"room.locations":                     "有哪些地点 已保存地点 地点列表 基地列表 保存的位置 locations",
	"location.save":                      "保存位置 保存地点 记录地点 标记地点 记为基地 当前位置 mark location",
	"player.teleport":                    "传送地点 传送到地点 传送到基地 传送到 回基地 前往位置 teleport destination",
	"workbench.set_player_invincibility": "普通无敌 仅免伤 不补状态 invincibility",
	"world.player_modes":                 "模式状态 是否无敌 是否开启无敌 是否开启制造模式 invincibility mode status",
	"room.status":                        "查服 房间信息 服务器状态 一服信息 server status",
	"room.players":                       "谁在线 在线人数 名单 online players",
	"room.network":                       "卡 延迟 网络 客户端性能 netscore ping performance",
	"player.inventory":                   "背包 装备 物品栏 离线背包 inventory backpack",
	"player.resolve":                     "玩家信息 查人 昵称 profile player",
	"player.resurrect":                   "复活 救活 救人 resurrect revive",
	"player.god_mode":                    "无敌 无敌模式 上帝模式 不死 免伤 god godmode invincible",
	"player.creative_mode":               "制作模式 制造模式 创造模式 自由建造 免费制作 creative freebuild freecrafting",
	"player.announce":                    "提醒玩家 通知玩家 玩家公告 announce",
	"player.ban":                         "封禁 封号 拉黑 ban",
	"player.unban":                       "解封 解除封禁 取消封禁 unban",
	"player.kill":                        "杀死玩家 杀死 击杀玩家 kill",
	"player.reset_character":             "重选角色 换人物 重置玩家 reset_character despawn",
	"player.give_item":                   "给 发 发放 给予 物品 肉丸 give item meatballs",
	"player.kick":                        "踢人 踢出 踢掉 kick",
	"room.mods":                          "模组 mod 配置 设置",
	"room.performance":                   "性能 负载 CPU 内存 报表 卡顿 performance",
	"player.spawn_entity":                "生成 刷 生物 座狼 狗王 spawn",
	"world.entity_count":                 "多少 数量 蜘蛛巢 统计 count entity prefab",
	"player.activity":                    "活动 做了什么 干了什么 最近行为 activity",
	"player.trajectory":                  "去哪 去了哪里 位置 地点 轨迹 trajectory",
	"player.combat":                      "打了谁 打怪 打boss 战斗 combat",
	"room.backup.create":                 "备份 快照 backup snapshot",
	"room.backups.configure":             "每天 凌晨 自动备份 定时 schedule backup",
}

func TaskTools() []Tool {
	selector := botString(255)
	params := map[string]any{"type": "object", "maxProperties": 32, "additionalProperties": true, "description": "仅填写该能力返回的字段，不接受脚本或操作映射。"}
	makeTool := func(name, desc string, required []string, props map[string]any) Tool {
		return Tool{Name: name, Description: desc, Mutation: taskNames[name], InputSchema: botSchema(props, required...)}
	}
	return []Tool{
		makeTool("dst_servers", "查看房间列表；指定 room 可查看房间概况、在线玩家、模组或性能。不会按列表顺序猜测一服。", nil, map[string]any{"room": selector, "view": map[string]any{"type": "string", "enum": []string{"overview", "players", "mods", "performance"}}}),
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
	type ranked struct {
		c      BotCapability
		score  int
		action *consoleapi.Action
	}
	rows := []ranked{}
	query := strings.ToLower(strings.TrimSpace(in.Query))
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
		matched := false
		for _, action := range c.Actions {
			score := discoveryScore(query, append([]string{action.ID, action.Title}, action.SearchTerms...))
			if score > 0 {
				a := action
				rows = append(rows, ranked{c, score + 100, &a})
				matched = true
			}
		}
		if !matched {
			score := discoveryScore(query, append([]string{c.ID, c.Title}, c.SearchTerms...))
			if score > 0 {
				rows = append(rows, ranked{c, score, nil})
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].score > rows[j].score })
	items := []map[string]any{}
	for _, v := range rows[:min(len(rows), 3)] {
		input := recommendedTaskInput(v.c)
		item := map[string]any{"id": v.c.ID, "title": v.c.Title, "access": v.c.Access, "risk": v.c.Risk, "parameters": v.c.Parameters, "parameterHints": v.c.ParameterHints, "tool": recommendedTask(v.c), "input": input, "schemaRevision": v.c.SchemaRevision}
		if v.action != nil {
			item["title"], item["actionId"], item["description"] = v.action.Title, v.action.ID, v.action.Description
			input["parameters"] = v.action.Parameters
		}
		items = append(items, item)
	}
	return TaskResponse{State: "succeeded", Summary: "以下为已授权的候选；没有执行操作。需要确认的动作请在面板或可信插件中继续；不要把查询、否定或讨论当作执行请求。", Data: map[string]any{"items": items, "hasMore": len(rows) > 3}}, nil
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

// Longest matching phrase wins; generic words cannot accumulate enough points
// to outrank a specific action. Discovery never submits a command.
func discoveryScore(query string, terms []string) int {
	best := 0
	for _, term := range terms {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "" {
			continue
		}
		score := 0
		if term == query {
			score = 1000 + len([]rune(term))
		} else if strings.Contains(query, term) {
			score = 100 + len([]rune(term))
		} else if len([]rune(query)) >= 2 && strings.Contains(term, query) {
			score = len([]rune(query))
		}
		if score > best {
			best = score
		}
	}
	return best
}

func nearbyPreparation(c BotCapability, p map[string]any) bool {
	if c.ID != "workbench.act_nearby_entities" {
		return false
	}
	return consoleapi.EffectiveWorkbenchRisk(consoleapi.Definition{ID: "act_nearby_entities", Risk: consoleapi.RiskCritical}, p) == consoleapi.RiskLow
}
