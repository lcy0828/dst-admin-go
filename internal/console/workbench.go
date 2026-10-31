package console

import (
	"context"
	"math"
	"sort"
	"strings"
)

// BuiltinWorkbenchDefinitions is code-owned. Editable command rows and caller
// metadata cannot add recipes or change their scripts, risk or argument schema.
func BuiltinWorkbenchDefinitions() []Definition {
	templates := builtinTemplates()
	out := make([]Definition, 0, len(templates))
	for _, tmpl := range templates {
		d := tmpl.definition
		d.Script = ""
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func ValidateBuiltinWorkbench(id string, arguments map[string]interface{}, confirmed bool) (Definition, error) {
	tmpl, ok := builtinTemplates()[id]
	if !ok {
		return Definition{}, ErrCommandNotFound
	}
	d := tmpl.definition
	if d.Risk == RiskCritical && !confirmed {
		return d, ErrConfirmationNeeded
	}
	props := map[string]Parameter{}
	for _, p := range d.Parameters {
		props[p.Name] = p
		if p.Required {
			if _, ok := arguments[p.Name]; !ok {
				return d, ErrInvalidArguments
			}
		}
	}
	for key, value := range arguments {
		p, ok := props[key]
		if !ok {
			return d, ErrInvalidArguments
		}
		switch p.Type {
		case "string", "enum":
			text, ok := value.(string)
			if !ok || len(text) > 2048 || strings.ContainsRune(text, 0) {
				return d, ErrInvalidArguments
			}
			if p.Type == "enum" {
				found := false
				for _, v := range p.Options {
					if v == text {
						found = true
					}
				}
				if !found {
					return d, ErrInvalidArguments
				}
			}
		case "boolean":
			if _, ok := value.(bool); !ok {
				return d, ErrInvalidArguments
			}
		case "integer", "number":
			number, ok := numericArgument(value)
			if !ok || math.IsNaN(number) || math.IsInf(number, 0) || p.Type == "integer" && math.Trunc(number) != number || p.Minimum != nil && number < float64(*p.Minimum) || p.Maximum != nil && number > float64(*p.Maximum) {
				return d, ErrInvalidArguments
			}
		default:
			return d, ErrInvalidArguments
		}
	}
	if _, err := tmpl.render(arguments); err != nil {
		return d, err
	}
	return d, nil
}

func numericArgument(value interface{}) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

type WorkbenchResult struct {
	RunID            string    `json:"runId"`
	RoomID           string    `json:"roomId"`
	WorldID          string    `json:"worldId"`
	CommandID        string    `json:"commandId"`
	Status           RunStatus `json:"status"`
	Complete         bool      `json:"complete"`
	ExecutionOutcome string    `json:"executionOutcome"`
	MayHaveExecuted  bool      `json:"mayHaveExecuted"`
	Message          string    `json:"message"`
	Output           *Output   `json:"output,omitempty"`
}

func workbenchResult(run Run) WorkbenchResult {
	r := WorkbenchResult{RunID: run.ID, RoomID: run.RoomID, WorldID: run.WorldID, CommandID: run.CommandID, Status: run.Status, ExecutionOutcome: run.ExecutionOutcome, MayHaveExecuted: run.MayHaveExecuted, Message: run.Message, Output: run.Output}
	r.Complete = run.Status == RunFailed || run.Status == RunSucceeded && run.ExecutionOutcome == "confirmed"
	if r.Complete && run.Status == RunSucceeded {
		r.Message = "固定工作台指令已执行并收到回执；具体效果以输出和最新游戏状态为准"
	}
	return r
}

func (s *Service) ExecuteWorkbench(ctx context.Context, room, world, id string, args map[string]interface{}, confirmed bool) (WorkbenchResult, error) {
	if _, err := ValidateBuiltinWorkbench(id, args, confirmed); err != nil {
		return WorkbenchResult{}, err
	}
	r, err := s.rooms.Room(room)
	if err != nil {
		return WorkbenchResult{}, err
	}
	run, err := s.Execute(ctx, room, world, ExecuteRequest{CommandID: id, Arguments: args, Confirmation: r.Name})
	if err != nil {
		return WorkbenchResult{}, err
	}
	return workbenchResult(run), nil
}

func (s *Service) WorkbenchRun(ctx context.Context, room, id string, refresh bool) (WorkbenchResult, error) {
	run, err := s.Run(id)
	if err != nil {
		return WorkbenchResult{}, err
	}
	if _, ok := s.templates[run.CommandID]; !ok || run.RoomID != room || run.Mode != "builtin" {
		return WorkbenchResult{}, ErrRunNotFound
	}
	if refresh {
		run, err = s.RefreshRun(ctx, room, id)
		if err != nil {
			return WorkbenchResult{}, err
		}
	}
	return workbenchResult(run), nil
}
