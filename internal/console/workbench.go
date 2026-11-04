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
	if (EffectiveWorkbenchRisk(d, arguments) == RiskCritical || RequiresHumanConfirmation(id, arguments)) && !confirmed {
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
	Movement         *MovementResult `json:"movement,omitempty"`
	Batch            *BatchResult    `json:"batch,omitempty"`
	RunID            string          `json:"runId"`
	RoomID           string          `json:"roomId"`
	WorldID          string          `json:"worldId"`
	CommandID        string          `json:"commandId"`
	Status           RunStatus       `json:"status"`
	Complete         bool            `json:"complete"`
	ExecutionOutcome string          `json:"executionOutcome"`
	MayHaveExecuted  bool            `json:"mayHaveExecuted"`
	Message          string          `json:"message"`
	Output           *Output         `json:"output,omitempty"`
}

func workbenchResult(run Run) WorkbenchResult {
	r := WorkbenchResult{RunID: run.ID, RoomID: run.RoomID, WorldID: run.WorldID, CommandID: run.CommandID, Status: run.Status, ExecutionOutcome: run.ExecutionOutcome, MayHaveExecuted: run.MayHaveExecuted, Message: run.Message, Output: run.Output}
	r.Complete = run.Status == RunFailed || run.Status == RunSucceeded && run.ExecutionOutcome == "confirmed"
	if r.Complete && run.Status == RunSucceeded {
		r.Message = "固定工作台指令已执行并收到回执；具体效果以输出和最新游戏状态为准"
	}
	r.Batch = batchResult(run)
	if movementCommand(run.CommandID) && (run.Status == RunSucceeded || run.Movement != nil) {
		r.Movement = run.Movement
		if r.Movement == nil {
			r.Movement = movementResult(run)
		}
		if r.Movement == nil {
			r.Status, r.Complete, r.ExecutionOutcome = RunUncertain, false, "unconfirmed"
			r.Message = "缺少有效的传送位置回执，请查看原结果，不要重复提交。"
		} else if r.Movement.State == "partial" {
			r.Status = "partial"
			r.Complete = true
			r.Message = "操作已结束，部分玩家未完成或位置未确认，请查看逐人结果，不要重复提交。"
		} else if r.Movement.State == "confirmed" {
			r.Message = "传送已完成，玩家位置已确认。"
		} else if r.Movement.State == "preview" {
			r.Message = "已检查并固定本次玩家和落点，请在 60 秒内确认。尚未传送。"
		} else {
			r.Message = "已取消本次传送确认。"
		}
		return r
	}
	if r.Batch != nil && r.Complete && run.Status == RunSucceeded {
		switch r.Batch.State {
		case "preview":
			r.Message = "预览完成，尚未修改游戏。30 秒内携带预览编号确认或取消。"
		case "cancelled":
			r.Message = "已取消预览，没有执行批量操作。"
		case "partial":
			r.Status = "partial"
			r.Message = "操作已结束，部分对象未完成或效果未确认；请查看逐项统计，不要重复提交。"
		case "completed":
			r.Message = "操作已完成，效果已确认。"
			if r.Batch.Matched == 0 {
				r.Message = "附近没有符合条件的对象，没有执行修改。"
			}
		}
	} else if r.Complete && run.Status == RunSucceeded && (strings.Contains(run.Script, "w.Nearby(") || strings.Contains(run.Script, "w.NearbyV2(") || strings.Contains(run.Script, "w.Setting(") || guardedExpansion(run.CommandID) && strings.Contains(run.Script, "w.Expansion(")) {
		r.Status = RunUncertain
		r.Complete = false
		r.Message = "缺少有效的范围操作结果，请读取原回执，不要重复提交。"
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
