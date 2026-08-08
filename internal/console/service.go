package console

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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
	ErrInvalidDefinition  = errors.New("command definition is invalid")
	ErrBuiltinDefinition  = errors.New("builtin command definition cannot be changed")
)

var parameterNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var placeholderPattern = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

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
	definitions, _ := s.DefinitionsWithError()
	return definitions
}

func (s *Service) DefinitionsWithError() ([]Definition, error) {
	order := []string{"save_world", "announce", "list_players", "set_season", "rollback", "shutdown", "regenerate"}
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
	minRollback, maxRollback := 1, 5
	templates := map[string]template{
		"save_world":   simpleTemplate("save_world", "保存世界", "立即保存当前世界", "基础操作", RiskLow, "c_save()"),
		"list_players": simpleTemplate("list_players", "列出玩家", "将当前玩家列表写入服务器日志", "查询", RiskLow, `for i,v in ipairs(TheNet:GetClientTable()) do print("[DST-ADMIN-PLAYER]",i,v.userid,v.name,v.prefab) end`),
		"shutdown":     simpleTemplate("shutdown", "关闭分片", "保存并关闭当前分片", "危险操作", RiskHigh, "c_shutdown(true)"),
		"regenerate":   simpleTemplate("regenerate", "重新生成世界", "删除当前进度并重新生成世界", "危险操作", RiskCritical, "c_regenerateworld()"),
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
	templates["rollback"] = template{
		definition: Definition{ID: "rollback", Name: "回档", Description: "将当前世界回退指定天数", Category: "危险操作", Risk: RiskCritical, Parameters: []Parameter{{Name: "days", Label: "回退天数", Type: "integer", Required: true, Minimum: &minRollback, Maximum: &maxRollback}}, Script: "c_rollback({days})", IsBuiltin: true},
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
