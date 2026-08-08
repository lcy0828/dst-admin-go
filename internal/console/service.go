package console

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"dont/internal/rooms"
)

var (
	ErrCommandNotFound    = errors.New("command definition not found")
	ErrInvalidArguments   = errors.New("command arguments are invalid")
	ErrConfirmationNeeded = errors.New("command confirmation does not match")
	ErrRawCommandInvalid  = errors.New("raw command is invalid")
	ErrRoomNotManaged     = errors.New("room must be managed before commands can be sent")
)

type Sender interface {
	Send(context.Context, string, string, string) error
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
	templates map[string]template
}

func NewService(roomCatalog RoomCatalog, sender Sender, store *Store) (*Service, error) {
	if roomCatalog == nil || sender == nil || store == nil {
		return nil, errors.New("room catalog, command sender, and store are required")
	}
	return &Service{rooms: roomCatalog, sender: sender, store: store, templates: builtinTemplates()}, nil
}

func (s *Service) Definitions() []Definition {
	order := []string{"save_world", "announce", "list_players", "set_season", "rollback", "shutdown", "regenerate"}
	result := make([]Definition, 0, len(order))
	for _, id := range order {
		result = append(result, s.templates[id].definition)
	}
	return result
}

func (s *Service) Execute(ctx context.Context, roomID, worldID string, request ExecuteRequest) (Run, error) {
	tmpl, exists := s.templates[strings.TrimSpace(request.CommandID)]
	if !exists {
		return Run{}, ErrCommandNotFound
	}
	room, world, err := s.resolve(roomID, worldID)
	if err != nil {
		return Run{}, err
	}
	if confirmationRequired(tmpl.definition.Risk) && request.Confirmation != room.Name {
		return Run{}, ErrConfirmationNeeded
	}
	script, err := tmpl.render(request.Arguments)
	if err != nil {
		return Run{}, err
	}
	run, err := s.store.Create(Run{
		RoomID: room.ID, WorldID: world.ID, Mode: "builtin", CommandID: tmpl.definition.ID,
		Name: tmpl.definition.Name, Risk: tmpl.definition.Risk, Arguments: request.Arguments,
	})
	if err != nil {
		return Run{}, err
	}
	sendErr := s.sender.Send(ctx, room.DirectoryName, world.DirectoryName, markedScript(run.ID, script))
	completed, completeErr := s.store.Complete(run.ID, sendErr)
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
	sendErr := s.sender.Send(ctx, room.DirectoryName, world.DirectoryName, markedScript(run.ID, command))
	completed, completeErr := s.store.Complete(run.ID, sendErr)
	if completeErr != nil {
		return Run{}, completeErr
	}
	return completed, sendErr
}

func (s *Service) Runs(filter ListFilter) ([]Run, int, error) { return s.store.List(filter) }

func (s *Service) Run(runID string) (Run, error) { return s.store.Get(runID) }

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
	minRollback, maxRollback := 1, 5
	templates := map[string]template{
		"save_world":   simpleTemplate("save_world", "保存世界", "立即保存当前世界", "基础操作", RiskLow, "c_save()"),
		"list_players": simpleTemplate("list_players", "列出玩家", "将当前玩家列表写入服务器日志", "查询", RiskLow, `for i,v in ipairs(TheNet:GetClientTable()) do print("[DST-ADMIN-PLAYER]",i,v.userid,v.name,v.prefab) end`),
		"shutdown":     simpleTemplate("shutdown", "关闭分片", "保存并关闭当前分片", "危险操作", RiskHigh, "c_shutdown(true)"),
		"regenerate":   simpleTemplate("regenerate", "重新生成世界", "删除当前进度并重新生成世界", "危险操作", RiskCritical, "c_regenerateworld()"),
	}

	templates["announce"] = template{
		definition: Definition{ID: "announce", Name: "发送公告", Description: "向当前房间的玩家发送公告", Category: "基础操作", Risk: RiskLow, Parameters: []Parameter{{Name: "message", Label: "公告内容", Type: "string", Required: true}}},
		render: func(arguments map[string]interface{}) (string, error) {
			message, err := stringArgument(arguments, "message", 1, 500)
			if err != nil {
				return "", err
			}
			return "c_announce(" + quoteLua(message) + ")", nil
		},
	}
	templates["set_season"] = template{
		definition: Definition{ID: "set_season", Name: "设置季节", Description: "切换当前世界季节", Category: "世界控制", Risk: RiskMedium, Parameters: []Parameter{{Name: "season", Label: "季节", Type: "enum", Required: true, Options: []string{"autumn", "winter", "spring", "summer"}}}},
		render: func(arguments map[string]interface{}) (string, error) {
			season, err := enumArgument(arguments, "season", "autumn", "winter", "spring", "summer")
			if err != nil {
				return "", err
			}
			return "TheWorld:PushEvent(\"ms_setseason\"," + quoteLua(season) + ")", nil
		},
	}
	templates["rollback"] = template{
		definition: Definition{ID: "rollback", Name: "回档", Description: "将当前世界回退指定天数", Category: "危险操作", Risk: RiskCritical, Parameters: []Parameter{{Name: "days", Label: "回退天数", Type: "integer", Required: true, Minimum: &minRollback, Maximum: &maxRollback}}},
		render: func(arguments map[string]interface{}) (string, error) {
			days, err := intArgument(arguments, "days", minRollback, maxRollback)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("c_rollback(%d)", days), nil
		},
	}
	return templates
}

func simpleTemplate(id, name, description, category string, risk Risk, script string) template {
	return template{definition: Definition{ID: id, Name: name, Description: description, Category: category, Risk: risk, Parameters: []Parameter{}}, render: func(arguments map[string]interface{}) (string, error) {
		if len(arguments) > 0 {
			return "", ErrInvalidArguments
		}
		return script, nil
	}}
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

func intArgument(arguments map[string]interface{}, name string, minimum, maximum int) (int, error) {
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
	if value < minimum || value > maximum {
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
