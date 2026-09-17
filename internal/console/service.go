package console

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/shared"
)

var (
	ErrCommandNotFound    = errors.New("command definition not found")
	ErrInvalidArguments   = errors.New("command arguments are invalid")
	ErrConfirmationNeeded = errors.New("command confirmation does not match")
	ErrRawCommandInvalid  = errors.New("raw command is invalid")
	ErrRoomNotManaged     = errors.New("room must be managed before commands can be sent")
	ErrInvalidDefinition  = errors.New("command definition is invalid")
	ErrBuiltinDefinition  = errors.New("builtin command definition cannot be changed")
)

var parameterNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var placeholderPattern = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)
var dstUserIDPattern = regexp.MustCompile(`^KU_[A-Za-z0-9_-]{3,64}$`)
var prefabPattern = regexp.MustCompile(`^[a-z0-9_]{1,80}$`)

type Sender interface {
	Send(context.Context, string, string, string) error
}

type routedSender interface {
	SendID(context.Context, string, string, shared.RuntimeConsoleRequest) (shared.RuntimeOperationResult, error)
}

type RuntimeCommander interface {
	ExecuteCommand(context.Context, string, string, dstruntime.CommandRequest) (dstruntime.CommandReceipt, error)
}

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
}

type template struct {
	definition Definition
	render     func(map[string]interface{}) (string, error)
}

type Service struct {
	rooms     RoomCatalog
	sender    Sender
	store     *Store
	commander RuntimeCommander
	templates map[string]template
}

func NewService(roomCatalog RoomCatalog, sender Sender, store *Store, commanders ...RuntimeCommander) (*Service, error) {
	if roomCatalog == nil || sender == nil || store == nil {
		return nil, errors.New("room catalog, command sender, and store are required")
	}
	service := &Service{rooms: roomCatalog, sender: sender, store: store, templates: builtinTemplates()}
	if len(commanders) > 0 {
		service.commander = commanders[0]
	}
	return service, nil
}

func (s *Service) Definitions() []Definition {
	definitions, _ := s.DefinitionsWithError()
	return definitions
}

func (s *Service) DefinitionsWithError() ([]Definition, error) {
	order := []string{
		"world_info", "save_world", "announce", "list_players", "give_item", "give_recipe_materials", "give_blueprint",
		"set_player_stat", "set_player_speed", "set_player_lock", "adjust_player_luck", "manage_player_skills", "set_monkey_curse",
		"clear_player_debuffs", "clear_player_inventory", "clear_player_naughtiness", "teleport_player", "relocate_players", "set_player_ability", "toggle_player_ghost",
		"set_player_attack_multiplier", "set_health_penalty", "set_character_power", "manage_followers", "spawn_domesticated_beefalo",
		"set_season", "next_phase", "set_phase", "set_clock_segments",
		"skip_days", "set_time_scale", "set_precipitation", "set_world_wetness", "set_world_temperature", "set_moon_phase",
		"trigger_world_event", "trigger_incident", "set_special_event", "stop_vote", "spawn_entity", "teleport_to_nearest_entity", "remove_nearby_entities", "act_nearby_entities",
		"rollback", "shutdown", "regenerate",
	}
	result := make([]Definition, 0, len(order))
	for _, id := range order {
		result = append(result, s.templates[id].definition)
	}
	custom, err := s.store.Definitions()
	if err != nil {
		return nil, err
	}
	return append(result, custom...), nil
}

func (s *Service) Definition(id string) (Definition, error) {
	if tmpl, exists := s.templates[strings.TrimSpace(id)]; exists {
		return tmpl.definition, nil
	}
	return s.store.Definition(strings.TrimSpace(id))
}

func (s *Service) CreateDefinition(definition Definition) (Definition, error) {
	definition.ID = ""
	definition.IsBuiltin = false
	definition.Risk = RiskCritical
	definition, err := normalizeDefinition(definition)
	if err != nil {
		return Definition{}, err
	}
	return s.store.CreateDefinition(definition)
}

func (s *Service) UpdateDefinition(id string, definition Definition) (Definition, error) {
	id = strings.TrimSpace(id)
	if _, exists := s.templates[id]; exists {
		return Definition{}, ErrBuiltinDefinition
	}
	definition.ID = id
	definition.IsBuiltin = false
	definition.Risk = RiskCritical
	definition, err := normalizeDefinition(definition)
	if err != nil {
		return Definition{}, err
	}
	return s.store.UpdateDefinition(definition)
}

func (s *Service) DeleteDefinition(id string) error {
	id = strings.TrimSpace(id)
	if _, exists := s.templates[id]; exists {
		return ErrBuiltinDefinition
	}
	return s.store.DeleteDefinition(id)
}

func (s *Service) Execute(ctx context.Context, roomID, worldID string, request ExecuteRequest) (Run, error) {
	commandID := strings.TrimSpace(request.CommandID)
	tmpl, builtin := s.templates[commandID]
	var definition Definition
	if builtin {
		definition = tmpl.definition
	} else {
		var err error
		definition, err = s.store.Definition(commandID)
		if errors.Is(err, ErrDefinitionNotFound) {
			return Run{}, ErrCommandNotFound
		}
		if err != nil {
			return Run{}, err
		}
	}
	room, world, err := s.resolve(roomID, worldID)
	if err != nil {
		return Run{}, err
	}
	if confirmationRequired(definition.Risk) && request.Confirmation != room.Name {
		return Run{}, ErrConfirmationNeeded
	}
	var script string
	if builtin {
		script, err = tmpl.render(request.Arguments)
	} else {
		script, err = renderCustomDefinition(definition, request.Arguments)
	}
	if err != nil {
		return Run{}, err
	}
	run, err := s.store.Create(Run{
		RoomID: room.ID, WorldID: world.ID, Mode: map[bool]string{true: "builtin", false: "custom"}[builtin], CommandID: definition.ID,
		Name: definition.Name, Risk: definition.Risk, Arguments: request.Arguments,
	})
	if err != nil {
		return Run{}, err
	}
	if s.commander != nil {
		return s.executeVerified(ctx, room, world, run, script)
	}
	delivery, sendErr := s.send(ctx, room, world, markedScript(run.ID, script), shared.ConsoleModeManaged)
	completed, completeErr := s.store.CompleteDelivery(run.ID, delivery, sendErr)
	if completeErr != nil {
		return Run{}, completeErr
	}
	return completed, sendErr
}

func (s *Service) ExecuteRaw(ctx context.Context, roomID, worldID string, request RawRequest) (Run, error) {
	room, world, err := s.resolve(roomID, worldID)
	if err != nil {
		return Run{}, err
	}
	command := strings.TrimSpace(request.Command)
	if command == "" || len(command) > 4096 || !utf8.ValidString(command) || strings.ContainsRune(command, '\x00') {
		return Run{}, ErrRawCommandInvalid
	}
	if request.Confirmation != room.Name {
		return Run{}, ErrConfirmationNeeded
	}
	run, err := s.store.Create(Run{
		RoomID: room.ID, WorldID: world.ID, Mode: "raw", Name: "原始命令", Risk: RiskCritical, RawCommand: command,
	})
	if err != nil {
		return Run{}, err
	}
	if s.commander != nil {
		return s.executeVerified(ctx, room, world, run, command)
	}
	delivery, sendErr := s.send(ctx, room, world, markedScript(run.ID, command), shared.ConsoleModeRaw)
	completed, completeErr := s.store.CompleteDelivery(run.ID, delivery, sendErr)
	if completeErr != nil {
		return Run{}, completeErr
	}
	return completed, sendErr
}

func (s *Service) executeVerified(ctx context.Context, room rooms.Room, world rooms.World, run Run, script string) (Run, error) {
	receipt, err := s.commander.ExecuteCommand(ctx, room.ID, world.ID, dstruntime.CommandRequest{
		RequestID: run.ID,
		Action:    "console.execute",
		Arguments: map[string]interface{}{"script": script},
	})
	if err == nil {
		observedAt := receipt.CompletedAt.UTC()
		completion := ExecutionCompletion{
			Status: RunSucceeded, TransportOutcome: "sent", ExecutionOutcome: "confirmed",
			Message: "命令已由 DST 执行并返回确认", ObservedAt: &observedAt,
		}
		if !receipt.OK {
			completion.Status = RunFailed
			completion.ExecutionOutcome = "rejected"
			completion.Message = "DST 未能执行命令"
			completion.ErrorCode = receipt.Code
			completion.ErrorMessage = strings.TrimSpace(receipt.Message)
			if completion.ErrorMessage == "" {
				completion.ErrorMessage = receipt.Code
			}
		}
		completed, completeErr := s.store.CompleteExecution(run.ID, completion)
		if completeErr != nil {
			return Run{}, completeErr
		}
		return completed, nil
	}
	if errors.Is(err, dstruntime.ErrRuntimeResultAbsent) {
		return s.completeMissingReceipt(ctx, room, world, run, err)
	}
	code, message := verifiedCommandFailure(err)
	completed, completeErr := s.store.CompleteExecution(run.ID, ExecutionCompletion{
		Status: RunFailed, TransportOutcome: "failed", ExecutionOutcome: "none",
		Message: message, ErrorCode: code, ErrorMessage: err.Error(),
	})
	if completeErr != nil {
		return Run{}, completeErr
	}
	return completed, nil
}

func (s *Service) completeMissingReceipt(ctx context.Context, room rooms.Room, world rooms.World, run Run, commandErr error) (Run, error) {
	recoveryContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 6*time.Second)
	defer cancel()
	beacon, beaconErr := s.commander.ExecuteCommand(recoveryContext, room.ID, world.ID, dstruntime.CommandRequest{
		RequestID: run.ID + "-beacon",
		Action:    "system.ping",
		Arguments: map[string]interface{}{},
	})
	completion := ExecutionCompletion{
		Status: RunUnresponsive, TransportOutcome: "sent", ExecutionOutcome: "unresponsive",
		Message:   "分片控制台未响应，恢复探针也未返回结果",
		ErrorCode: "CONSOLE_UNRESPONSIVE", ErrorMessage: commandErr.Error(),
		MayHaveExecuted: true, RecoveryOutcome: "failed",
	}
	if beaconErr == nil && beacon.OK {
		observedAt := beacon.CompletedAt.UTC()
		completion.Status = RunUncertain
		completion.ExecutionOutcome = "unknown"
		completion.Message = "未收到原命令回执；控制台已自动恢复，本次命令不会自动重放"
		completion.ErrorCode = "COMMAND_OUTCOME_UNKNOWN"
		completion.ErrorMessage = "本次命令可能已经执行，请核对游戏状态后再决定是否重新执行"
		completion.RecoveryOutcome = "succeeded"
		completion.ObservedAt = &observedAt
	} else if errors.Is(beaconErr, dstruntime.ErrRuntimeUnavailable) || errors.Is(beaconErr, dstruntime.ErrRuntimeNotInstalled) {
		completion.Status = RunUncertain
		completion.ExecutionOutcome = "unknown"
		completion.Message = "未收到原命令回执，且目标分片已无法执行恢复探针"
		completion.ErrorCode = "COMMAND_OUTCOME_UNKNOWN"
		completion.ErrorMessage = "本次命令可能已经执行；目标分片当前不可用"
		completion.RecoveryOutcome = "unavailable"
	} else if beaconErr != nil {
		completion.ErrorMessage = commandErr.Error() + "; recovery beacon: " + beaconErr.Error()
	} else {
		completion.ErrorMessage = "恢复探针被 Runtime 拒绝: " + beacon.Code + ": " + beacon.Message
	}
	completed, completeErr := s.store.CompleteExecution(run.ID, completion)
	if completeErr != nil {
		return Run{}, completeErr
	}
	return completed, nil
}

func verifiedCommandFailure(err error) (string, string) {
	switch {
	case errors.Is(err, dstruntime.ErrRuntimeUnavailable), errors.Is(err, dstruntime.ErrRuntimeNotInstalled):
		return "RUNTIME_UNAVAILABLE", "目标分片 Runtime 尚未就绪，命令没有发送"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "COMMAND_CANCELED", "命令请求已取消"
	default:
		return "COMMAND_SEND_FAILED", "命令发送失败"
	}
}

func (s *Service) send(ctx context.Context, room rooms.Room, world rooms.World, command string, mode shared.ConsoleMode) (Delivery, error) {
	if sender, ok := s.sender.(routedSender); ok {
		result, err := sender.SendID(ctx, room.ID, world.ID, shared.RuntimeConsoleRequest{Mode: mode, Command: command})
		observedAt := result.ObservedAt.UTC()
		delivery := Delivery{
			TransportOutcome: string(result.Outcome), ExecutionOutcome: "unknown", Message: result.Message,
			OperationID: result.OperationID, OperationKey: result.OperationKey, TargetID: result.TargetID,
			AgentID: result.AgentID, TopologyRevision: result.TopologyRevision,
		}
		if !observedAt.IsZero() {
			delivery.ObservedAt = &observedAt
		}
		return delivery, err
	}
	err := s.sender.Send(ctx, room.DirectoryName, world.DirectoryName, command)
	delivery := Delivery{TransportOutcome: "sent", ExecutionOutcome: "unknown"}
	return delivery, err
}

func (s *Service) Runs(filter ListFilter) ([]Run, int, error) { return s.store.List(filter) }

func (s *Service) Run(runID string) (Run, error) { return s.store.Get(runID) }

func (s *Service) DeleteRuns(filter ListFilter) (int64, error) { return s.store.DeleteRuns(filter) }

func (s *Service) resolve(roomID, worldID string) (rooms.Room, rooms.World, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return rooms.Room{}, rooms.World{}, err
	}
	if !room.Managed {
		return rooms.Room{}, rooms.World{}, ErrRoomNotManaged
	}
	world, err := s.rooms.World(roomID, worldID)
	return room, world, err
}

func builtinTemplates() map[string]template {
	minRollback := 1
	minCount, maxItemCount, maxEntityCount := 1, 40, 20
	minRadius, maxRadius, maxRemoveCount := 1, 30, 100
	minSegment, maxSegment := 0, 16
	templates := map[string]template{
		"world_info": simpleTemplate(
			"world_info", "查询世界信息", "将天数、季节、昼夜、月相、温度和降水状态写入服务器日志",
			"查询", RiskLow,
			`print("[DST-ADMIN-WORLD]",(TheWorld.state.cycles or 0)+1,TheWorld.state.season or "unknown",TheWorld.state.remainingdaysinseason or -1,TheWorld.state.phase or "unknown",TheWorld.state.moonphase or "unknown",TheWorld.state.temperature or 0,tostring(TheWorld.state.israining))`,
		),
		"save_world":   simpleTemplate("save_world", "保存世界", "立即保存当前世界", "基础操作", RiskLow, "c_save()"),
		"list_players": simpleTemplate("list_players", "列出玩家", "将当前玩家列表写入服务器日志", "查询", RiskLow, `for i,v in ipairs(TheNet:GetClientTable()) do print("[DST-ADMIN-PLAYER]",i,v.userid,v.name,v.prefab) end`),
		"shutdown":     simpleTemplate("shutdown", "关闭分片", "保存并关闭当前分片", "危险操作", RiskHigh, "c_shutdown(true)"),
		"regenerate":   simpleTemplate("regenerate", "重新生成世界", "删除当前进度并重新生成世界", "危险操作", RiskCritical, "c_regenerateworld()"),
		"stop_vote":    simpleTemplate("stop_vote", "停止投票", "立即终止当前服务器投票", "世界控制", RiskHigh, "c_stopvote()"),
	}

	templates["announce"] = template{
		definition: Definition{ID: "announce", Name: "发送公告", Description: "向当前房间的玩家发送公告", Category: "基础操作", Risk: RiskLow, Parameters: []Parameter{{Name: "message", Label: "公告内容", Type: "string", Required: true}}, Script: `c_announce("{message}")`, IsBuiltin: true},
		render: func(arguments map[string]interface{}) (string, error) {
			message, err := stringArgument(arguments, "message", 1, 500)
			if err != nil {
				return "", err
			}
			return "c_announce(" + quoteLua(message) + ")", nil
		},
	}
	templates["set_season"] = template{
		definition: Definition{ID: "set_season", Name: "设置季节", Description: "切换当前世界季节", Category: "世界控制", Risk: RiskMedium, Parameters: []Parameter{{Name: "season", Label: "季节", Type: "enum", Required: true, Options: []string{"autumn", "winter", "spring", "summer"}}}, Script: `TheWorld:PushEvent("ms_setseason","{season}")`, IsBuiltin: true},
		render: func(arguments map[string]interface{}) (string, error) {
			season, err := enumArgument(arguments, "season", "autumn", "winter", "spring", "summer")
			if err != nil {
				return "", err
			}
			return "TheWorld:PushEvent(\"ms_setseason\"," + quoteLua(season) + ")", nil
		},
	}
	templates["next_phase"] = simpleTemplate(
		"next_phase", "推进昼夜阶段", "将当前世界从白天推进到黄昏、从黄昏推进到夜晚，或从夜晚推进到白天",
		"世界控制", RiskMedium, `TheWorld:PushEvent("ms_nextphase")`,
	)
	templates["set_phase"] = template{
		definition: Definition{ID: "set_phase", Name: "设置昼夜阶段", Description: "将当前世界直接切换到白天、黄昏或夜晚", Category: "世界控制", Risk: RiskMedium, Parameters: []Parameter{{Name: "phase", Label: "昼夜阶段", Type: "enum", Required: true, Options: []string{"day", "dusk", "night"}}}, Script: `TheWorld:PushEvent("ms_setphase","{phase}")`, IsBuiltin: true},
		render: func(arguments map[string]interface{}) (string, error) {
			phase, err := enumArgument(arguments, "phase", "day", "dusk", "night")
			if err != nil {
				return "", err
			}
			return "TheWorld:PushEvent(\"ms_setphase\"," + quoteLua(phase) + ")", nil
		},
	}
	templates["give_item"] = template{
		definition: Definition{
			ID: "give_item", Name: "给予玩家物品", Description: "按 KU ID 向当前分片的在线玩家发放物品", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true, Description: "玩家必须在线且位于当前分片"},
				{Name: "prefab", Label: "物品 Prefab", Type: "string", Required: true, Description: "例如 flint、goldnugget"},
				{Name: "count", Label: "数量", Type: "integer", Required: true, Minimum: &minCount, Maximum: &maxItemCount, Default: 1},
				{Name: "quantity_mode", Label: "数量方式", Type: "enum", Options: []string{"units", "stacks"}, Default: "units", Description: "units 按个数发放；stacks 按物品实际堆叠上限发放整组"},
			},
			Script: `按 KU ID 查找玩家并安全调用 SpawnPrefab 与 inventory:GiveItem`, IsBuiltin: true,
		},
		render: func(arguments map[string]interface{}) (string, error) {
			playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
			if err != nil {
				return "", err
			}
			prefab, err := matchedArgument(arguments, "prefab", prefabPattern)
			if err != nil {
				return "", err
			}
			count, err := intArgument(arguments, "count", minCount, maxItemCount)
			if err != nil {
				return "", err
			}
			quantityMode, err := optionalEnumArgument(arguments, "quantity_mode", "units", "units", "stacks")
			if err != nil {
				return "", err
			}
			return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p and p.components and p.components.inventory then local mode=%s;local n=0;for i=1,%d do local item=SpawnPrefab(%s);if item then local granted=1;local stack=item.components and item.components.stackable;if mode=="stacks"and stack then stack:SetStackSize(stack.maxsize or 1);granted=stack:StackSize()end;p.components.inventory:GiveItem(item);n=n+granted end end;print("[DST-ADMIN-GIVE]",%s,%s,mode,n)else print("[DST-ADMIN-GIVE]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), quoteLua(quantityMode), count, quoteLua(prefab), quoteLua(playerID), quoteLua(prefab), quoteLua(playerID)), nil
		},
	}
	templates["give_recipe_materials"] = template{
		definition: Definition{
			ID: "give_recipe_materials", Name: "给予制作材料", Description: "按目标配方向玩家给予一份或多份完整制作材料", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "prefab", Label: "配方 Prefab", Type: "string", Required: true},
				{Name: "count", Label: "配方份数", Type: "integer", Required: true, Minimum: &minCount, Maximum: &maxItemCount, Default: 1},
			},
			Script: `从 AllRecipes 白名单查询配方并逐项生成 ingredients`, IsBuiltin: true,
		},
		render: func(arguments map[string]interface{}) (string, error) {
			playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
			if err != nil {
				return "", err
			}
			prefab, err := matchedArgument(arguments, "prefab", prefabPattern)
			if err != nil {
				return "", err
			}
			count, err := intArgument(arguments, "count", minCount, maxItemCount)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;local recipe=(AllRecipes or {})[%s];if p and recipe then local n=0;local function give(item)if not item then return end;if item.components and item.components.inventoryitem and p.components and p.components.inventory then p.components.inventory:GiveItem(item)else local x,y,z=p.Transform:GetWorldPosition();item.Transform:SetPosition(x,y,z)end;n=n+1 end;for i=1,%d do for _,ingredient in pairs(recipe.ingredients or {})do for k=1,ingredient.amount or 0 do give(SpawnPrefab(ingredient.type))end end end;print("[DST-ADMIN-GIVE-MATERIALS]",%s,%s,n)else print("[DST-ADMIN-GIVE-MATERIALS]","PLAYER_OR_RECIPE_NOT_FOUND",%s,%s)end`, quoteLua(playerID), quoteLua(prefab), count, quoteLua(playerID), quoteLua(prefab), quoteLua(playerID), quoteLua(prefab)), nil
		},
	}
	templates["give_blueprint"] = template{
		definition: Definition{
			ID: "give_blueprint", Name: "给予配方蓝图", Description: "向玩家给予目标 Prefab 对应的蓝图实体", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "prefab", Label: "配方 Prefab", Type: "string", Required: true},
				{Name: "count", Label: "数量", Type: "integer", Required: true, Minimum: &minCount, Maximum: &maxItemCount, Default: 1},
			},
			Script: `生成经过校验的 {prefab}_blueprint 并放入玩家物品栏`, IsBuiltin: true,
		},
		render: func(arguments map[string]interface{}) (string, error) {
			playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
			if err != nil {
				return "", err
			}
			prefab, err := matchedArgument(arguments, "prefab", prefabPattern)
			if err != nil {
				return "", err
			}
			count, err := intArgument(arguments, "count", minCount, maxItemCount)
			if err != nil {
				return "", err
			}
			blueprint := prefab + "_blueprint"
			return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p and p.components and p.components.inventory then local n=0;for i=1,%d do local item=SpawnPrefab(%s);if item then p.components.inventory:GiveItem(item);n=n+1 end end;print("[DST-ADMIN-GIVE-BLUEPRINT]",%s,%s,n)else print("[DST-ADMIN-GIVE-BLUEPRINT]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), count, quoteLua(blueprint), quoteLua(playerID), quoteLua(prefab), quoteLua(playerID)), nil
		},
	}
	templates["set_clock_segments"] = template{
		definition: Definition{
			ID: "set_clock_segments", Name: "设置昼夜时段", Description: "调整一天中白天、黄昏和夜晚的时长，总和必须为 16", Category: "世界控制", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "day", Label: "白天段数", Type: "integer", Required: true, Minimum: &minSegment, Maximum: &maxSegment, Default: 10},
				{Name: "dusk", Label: "黄昏段数", Type: "integer", Required: true, Minimum: &minSegment, Maximum: &maxSegment, Default: 4},
				{Name: "night", Label: "夜晚段数", Type: "integer", Required: true, Minimum: &minSegment, Maximum: &maxSegment, Default: 2},
			},
			Script: `TheWorld:PushEvent("ms_setclocksegs", {day={day}, dusk={dusk}, night={night}})`, IsBuiltin: true,
		},
		render: func(arguments map[string]interface{}) (string, error) {
			day, err := intArgument(arguments, "day", minSegment, maxSegment)
			if err != nil {
				return "", err
			}
			dusk, err := intArgument(arguments, "dusk", minSegment, maxSegment)
			if err != nil {
				return "", err
			}
			night, err := intArgument(arguments, "night", minSegment, maxSegment)
			if err != nil || day+dusk+night != 16 {
				return "", ErrInvalidArguments
			}
			return fmt.Sprintf(`TheWorld:PushEvent("ms_setclocksegs",{day=%d,dusk=%d,night=%d})`, day, dusk, night), nil
		},
	}
	templates["spawn_entity"] = template{
		definition: Definition{
			ID: "spawn_entity", Name: "在玩家附近生成实体", Description: "按 KU ID 在当前分片玩家附近生成指定 Prefab", Category: "世界控制", Risk: RiskCritical,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true, Description: "实体将在该玩家附近生成"},
				{Name: "prefab", Label: "实体 Prefab", Type: "string", Required: true, Description: "例如 pigman、hound"},
				{Name: "count", Label: "数量", Type: "integer", Required: true, Minimum: &minCount, Maximum: &maxEntityCount, Default: 1},
			},
			Script: `按 KU ID 查找玩家，在其附近安全调用 SpawnPrefab`, IsBuiltin: true,
		},
		render: func(arguments map[string]interface{}) (string, error) {
			playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
			if err != nil {
				return "", err
			}
			prefab, err := matchedArgument(arguments, "prefab", prefabPattern)
			if err != nil {
				return "", err
			}
			count, err := intArgument(arguments, "count", minCount, maxEntityCount)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p and p.Transform then local x,y,z=p.Transform:GetWorldPosition();local n=0;for i=1,%d do local e=SpawnPrefab(%s);if e and e.Transform then local a=(i-1)*6.28318530718/%d;e.Transform:SetPosition(x+math.cos(a)*2,y,z+math.sin(a)*2);n=n+1 end end;print("[DST-ADMIN-SPAWN]",%s,%s,n)else print("[DST-ADMIN-SPAWN]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), count, quoteLua(prefab), count, quoteLua(playerID), quoteLua(prefab), quoteLua(playerID)), nil
		},
	}
	templates["teleport_to_nearest_entity"] = template{
		definition: Definition{
			ID: "teleport_to_nearest_entity", Name: "传送到最近实体", Description: "将在线玩家传送到当前分片最近的指定 Prefab", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "prefab", Label: "目标 Prefab", Type: "string", Required: true},
			},
			Script: `在 Ents 中精确匹配 Prefab 并选择距玩家最近的有效实体`, IsBuiltin: true,
		},
		render: func(arguments map[string]interface{}) (string, error) {
			playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
			if err != nil {
				return "", err
			}
			prefab, err := matchedArgument(arguments, "prefab", prefabPattern)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;local target=nil;local distance=nil;if p and p.Transform then local x,y,z=p.Transform:GetWorldPosition();for _,e in pairs(Ents or {})do if e~=p and e.prefab==%s and e:IsValid()and e.Transform and not e:HasTag("INLIMBO")then local ex,ey,ez=e.Transform:GetWorldPosition();local d=(ex-x)*(ex-x)+(ez-z)*(ez-z);if distance==nil or d<distance then target=e;distance=d end end end;if target then local tx,ty,tz=target.Transform:GetWorldPosition();if p.Physics then p.Physics:Teleport(tx,ty,tz)else p.Transform:SetPosition(tx,ty,tz)end end end;print("[DST-ADMIN-TELEPORT-PREFAB]",%s,%s,target~=nil)`, quoteLua(playerID), quoteLua(prefab), quoteLua(playerID), quoteLua(prefab)), nil
		},
	}
	templates["remove_nearby_entities"] = template{
		definition: Definition{
			ID: "remove_nearby_entities", Name: "清理玩家附近实体", Description: "按 KU ID 清理玩家附近指定 Prefab，并限制半径和最大数量", Category: "世界控制", Risk: RiskCritical,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true, Description: "以该在线玩家为清理中心"},
				{Name: "prefab", Label: "实体 Prefab", Type: "string", Required: true, Description: "只移除完全匹配的 Prefab"},
				{Name: "radius", Label: "半径", Type: "integer", Required: true, Minimum: &minRadius, Maximum: &maxRadius, Default: 10},
				{Name: "maximum", Label: "最大数量", Type: "integer", Required: true, Minimum: &minCount, Maximum: &maxRemoveCount, Default: 20},
			},
			Script: `按 KU ID 查找玩家，在限定半径内移除最多指定数量的同名 Prefab`, IsBuiltin: true,
		},
		render: func(arguments map[string]interface{}) (string, error) {
			playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
			if err != nil {
				return "", err
			}
			prefab, err := matchedArgument(arguments, "prefab", prefabPattern)
			if err != nil {
				return "", err
			}
			radius, err := intArgument(arguments, "radius", minRadius, maxRadius)
			if err != nil {
				return "", err
			}
			maximum, err := intArgument(arguments, "maximum", minCount, maxRemoveCount)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p and p.Transform then local x,_,z=p.Transform:GetWorldPosition();local n=0;for _,e in pairs(Ents or {})do if n>=%d then break end;if e~=p and e.prefab==%s and e.Transform and e:IsValid()then local ex,_,ez=e.Transform:GetWorldPosition();local dx,dz=ex-x,ez-z;if dx*dx+dz*dz<=%d then e:Remove();n=n+1 end end end;print("[DST-ADMIN-REMOVE]",%s,%s,n)else print("[DST-ADMIN-REMOVE]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), maximum, quoteLua(prefab), radius*radius, quoteLua(playerID), quoteLua(prefab), quoteLua(playerID)), nil
		},
	}
	templates["rollback"] = template{
		definition: Definition{ID: "rollback", Name: "回档", Description: "将当前世界回退指定数量的存档快照", Category: "危险操作", Risk: RiskCritical, Parameters: []Parameter{{Name: "days", Label: "回档点数", Type: "integer", Required: true, Minimum: &minRollback}}, Script: "c_rollback({days})", IsBuiltin: true},
		render: func(arguments map[string]interface{}) (string, error) {
			days, err := intArgumentAtLeast(arguments, "days", minRollback)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("c_rollback(%d)", days), nil
		},
	}
	addGameToolTemplates(templates)
	return templates
}

func simpleTemplate(id, name, description, category string, risk Risk, script string) template {
	return template{definition: Definition{ID: id, Name: name, Description: description, Category: category, Risk: risk, Parameters: []Parameter{}, Script: script, IsBuiltin: true}, render: func(arguments map[string]interface{}) (string, error) {
		if len(arguments) > 0 {
			return "", ErrInvalidArguments
		}
		return script, nil
	}}
}

func normalizeDefinition(definition Definition) (Definition, error) {
	definition.Name = strings.TrimSpace(definition.Name)
	definition.Description = strings.TrimSpace(definition.Description)
	definition.Category = strings.TrimSpace(definition.Category)
	definition.Script = strings.TrimSpace(definition.Script)
	if definition.Name == "" || len([]rune(definition.Name)) > 80 || len([]rune(definition.Description)) > 300 ||
		definition.Category == "" || len([]rune(definition.Category)) > 80 || definition.Script == "" || len(definition.Script) > 4096 ||
		len(definition.Parameters) > 20 || !utf8.ValidString(definition.Script) || strings.ContainsRune(definition.Script, '\x00') {
		return Definition{}, ErrInvalidDefinition
	}
	seen := make(map[string]struct{}, len(definition.Parameters))
	for index := range definition.Parameters {
		parameter := &definition.Parameters[index]
		parameter.Name = strings.TrimSpace(parameter.Name)
		parameter.Label = strings.TrimSpace(parameter.Label)
		parameter.Description = strings.TrimSpace(parameter.Description)
		if !parameterNamePattern.MatchString(parameter.Name) {
			return Definition{}, ErrInvalidDefinition
		}
		if _, exists := seen[parameter.Name]; exists {
			return Definition{}, ErrInvalidDefinition
		}
		seen[parameter.Name] = struct{}{}
		switch parameter.Type {
		case "", "string", "number", "integer", "boolean", "enum":
		default:
			return Definition{}, ErrInvalidDefinition
		}
		if parameter.Type == "enum" && len(parameter.Options) == 0 {
			return Definition{}, ErrInvalidDefinition
		}
	}
	for _, match := range placeholderPattern.FindAllStringSubmatch(definition.Script, -1) {
		if _, exists := seen[match[1]]; !exists {
			return Definition{}, ErrInvalidDefinition
		}
	}
	return definition, nil
}

func renderCustomDefinition(definition Definition, arguments map[string]interface{}) (string, error) {
	values := make(map[string]string, len(definition.Parameters))
	for _, parameter := range definition.Parameters {
		value, exists := arguments[parameter.Name]
		if (!exists || value == nil || value == "") && parameter.Default != nil {
			value, exists = parameter.Default, true
		}
		if parameter.Required && (!exists || value == nil || value == "") {
			return "", ErrInvalidArguments
		}
		if !exists || value == nil {
			values[parameter.Name] = ""
			continue
		}
		rendered, err := renderCustomArgument(parameter, value)
		if err != nil {
			return "", err
		}
		values[parameter.Name] = rendered
	}
	script := definition.Script
	for name, value := range values {
		script = strings.ReplaceAll(script, "{"+name+"}", value)
	}
	return script, nil
}

func renderCustomArgument(parameter Parameter, value interface{}) (string, error) {
	switch parameter.Type {
	case "", "string":
		text, ok := value.(string)
		if !ok || !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') || len([]rune(text)) > 1000 {
			return "", ErrInvalidArguments
		}
		return strings.TrimSuffix(strings.TrimPrefix(quoteLua(text), `"`), `"`), nil
	case "enum":
		text, ok := value.(string)
		if !ok {
			return "", ErrInvalidArguments
		}
		for _, option := range parameter.Options {
			if text == option {
				return strings.TrimSuffix(strings.TrimPrefix(quoteLua(text), `"`), `"`), nil
			}
		}
	case "boolean":
		switch typed := value.(type) {
		case bool:
			return strconv.FormatBool(typed), nil
		case string:
			if typed == "true" || typed == "false" {
				return typed, nil
			}
		}
	case "integer":
		integer, err := intArgument(map[string]interface{}{"value": value}, "value", -1000000, 1000000)
		if err == nil {
			return strconv.Itoa(integer), nil
		}
	case "number":
		var number float64
		switch typed := value.(type) {
		case float64:
			number = typed
		case int:
			number = float64(typed)
		case string:
			parsed, err := strconv.ParseFloat(typed, 64)
			if err != nil {
				return "", ErrInvalidArguments
			}
			number = parsed
		default:
			return "", ErrInvalidArguments
		}
		if number >= -1000000 && number <= 1000000 {
			return strconv.FormatFloat(number, 'f', -1, 64), nil
		}
	}
	return "", ErrInvalidArguments
}

func markedScript(runID, script string) string {
	return "print(" + quoteLua("[DST-ADMIN-COMMAND "+runID+" START]") + "); " + script + "; print(" + quoteLua("[DST-ADMIN-COMMAND "+runID+" DONE]") + ")"
}

func confirmationRequired(risk Risk) bool { return risk == RiskHigh || risk == RiskCritical }

func stringArgument(arguments map[string]interface{}, name string, minimum, maximum int) (string, error) {
	value, ok := arguments[name].(string)
	value = strings.TrimSpace(value)
	if !ok || len([]rune(value)) < minimum || len([]rune(value)) > maximum || strings.ContainsRune(value, '\x00') {
		return "", ErrInvalidArguments
	}
	return value, nil
}

func enumArgument(arguments map[string]interface{}, name string, options ...string) (string, error) {
	value, err := stringArgument(arguments, name, 1, 32)
	if err != nil {
		return "", err
	}
	for _, option := range options {
		if value == option {
			return value, nil
		}
	}
	return "", ErrInvalidArguments
}

func matchedArgument(arguments map[string]interface{}, name string, pattern *regexp.Regexp) (string, error) {
	value, err := stringArgument(arguments, name, 1, 80)
	if err != nil || !pattern.MatchString(value) {
		return "", ErrInvalidArguments
	}
	return value, nil
}

func intArgument(arguments map[string]interface{}, name string, minimum, maximum int) (int, error) {
	value, err := integerArgument(arguments, name)
	if err != nil || value < minimum || value > maximum {
		return 0, ErrInvalidArguments
	}
	return value, nil
}

func intArgumentAtLeast(arguments map[string]interface{}, name string, minimum int) (int, error) {
	value, err := integerArgument(arguments, name)
	if err != nil || value < minimum {
		return 0, ErrInvalidArguments
	}
	return value, nil
}

func integerArgument(arguments map[string]interface{}, name string) (int, error) {
	var value int
	switch typed := arguments[name].(type) {
	case float64:
		value = int(typed)
		if typed != float64(value) {
			return 0, ErrInvalidArguments
		}
	case int:
		value = typed
	case string:
		parsed, err := strconv.Atoi(typed)
		if err != nil {
			return 0, ErrInvalidArguments
		}
		value = parsed
	default:
		return 0, ErrInvalidArguments
	}
	return value, nil
}

func quoteLua(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, character := range value {
		switch character {
		case '\\':
			builder.WriteString(`\\`)
		case '"':
			builder.WriteString(`\"`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			builder.WriteRune(character)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}
