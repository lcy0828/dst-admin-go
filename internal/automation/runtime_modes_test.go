package automation

import (
	"context"
	"strings"
	"testing"

	"dont/internal/jobs"
	"dont/internal/shards"
	"dont/shared"
)

type runtimeModePlanner struct {
	options shards.PlanOptions
	called  bool
	warning *jobs.Error
}

func (p *runtimeModePlanner) Plan(action shards.Action, room string, worlds []string) ([]jobs.TargetSpec, jobs.Runner, error) {
	return p.PlanWithOptions(action, room, worlds, shards.PlanOptions{})
}
func (p *runtimeModePlanner) PlanWithOptions(_ shards.Action, _ string, _ []string, options shards.PlanOptions) ([]jobs.TargetSpec, jobs.Runner, error) {
	p.options, p.called = options, true
	return []jobs.TargetSpec{{ID: "world", Name: "Master"}}, func(_ context.Context, report func(jobs.TargetResult)) error {
		report(jobs.TargetResult{TargetID: "world", Status: jobs.StatusSucceeded, Warning: p.warning})
		return nil
	}, nil
}

func TestScheduledStartKeepsPreferencePersistenceWarning(t *testing.T) {
	planner := &runtimeModePlanner{warning: &jobs.Error{Code: "RUNTIME_MODE_PREFERENCE_SAVE_FAILED", Message: "游戏已运行，但未能保存启动偏好"}}
	executor := &DomainExecutor{roomActions: planner}
	result, err := executor.Execute(context.Background(), Task{Action: ActionRoomStart, RoomID: "room"}, "job")
	if err != nil || !strings.Contains(result.Message, planner.warning.Message) {
		t.Fatalf("warning lost: %+v %v", result, err)
	}
}
func TestScheduledStartAndRestartAcceptAndForwardRuntimeModes(t *testing.T) {
	for _, action := range []Action{ActionRoomStart, ActionRoomRestart} {
		for _, mode := range []string{"", "game", "luajit", "arena-gc"} {
			planner := &runtimeModePlanner{}
			executor := &DomainExecutor{roomActions: planner}
			parameters := map[string]interface{}{}
			if mode != "" {
				parameters["runtimeMode"] = mode
			}
			task := Task{Action: action, RoomID: "room", Parameters: parameters}
			if err := executor.Validate(task); err != nil {
				t.Fatalf("%s mode=%q: %v", action, mode, err)
			}
			if _, err := executor.Execute(context.Background(), task, "job"); err != nil {
				t.Fatal(err)
			}
			if !planner.called || planner.options.RuntimeMode != shared.RuntimePerformanceMode(mode) {
				t.Fatalf("mode lost: %+v", planner)
			}
		}
	}
	for _, parameters := range []map[string]interface{}{{"runtimeMode": ""}, {"runtimeMode": "inherit"}, {"runtimeMode": nil}, {"runtimeMode": 1}, {"runtimeVersion": "3.4.0"}, {"runtimeMode": "unknown"}} {
		if err := (&DomainExecutor{}).Validate(Task{Action: ActionRoomRestart, Parameters: parameters}); err == nil {
			t.Fatalf("invalid mode accepted: %#v", parameters)
		}
	}
	if err := (&DomainExecutor{}).Validate(Task{Action: ActionRoomStop, Parameters: map[string]interface{}{"runtimeMode": "game"}}); err == nil {
		t.Fatal("stop accepted runtimeMode")
	}
}
