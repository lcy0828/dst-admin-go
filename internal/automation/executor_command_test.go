package automation

import (
	"context"
	"dont/internal/console"
	"strings"
	"testing"
)

type scheduledCommands struct {
	room, world, raw string
	request          console.ExecuteRequest
}

func (*scheduledCommands) Definitions() []console.Definition {
	return []console.Definition{{ID: "shutdown", Risk: console.RiskHigh}, {ID: "custom-template", Risk: console.RiskCritical}}
}
func (c *scheduledCommands) ExecuteScheduled(_ context.Context, room, world string, request console.ExecuteRequest, raw string) (console.Run, error) {
	c.room, c.world, c.request, c.raw = room, world, request, raw
	return console.Run{Name: "command"}, nil
}
func TestScheduledCommandsAllowAdminTemplatesAndRawLua(t *testing.T) {
	for _, parameters := range []map[string]interface{}{{"commandId": "shutdown"}, {"commandId": "custom-template"}, {"rawCommand": "print('scheduled')\nc_save()"}} {
		commands := &scheduledCommands{}
		executor := &DomainExecutor{commands: commands}
		_, err := executor.Execute(context.Background(), Task{RoomID: "remote-room", WorldIDs: []string{"caves"}, Action: ActionCommandExecute, Parameters: parameters}, "job")
		if err != nil || commands.room != "remote-room" || commands.world != "caves" {
			t.Fatalf("routing failed: %#v %v", commands, err)
		}
		if raw, ok := parameters["rawCommand"]; ok && commands.raw != raw {
			t.Fatalf("script changed: %q", commands.raw)
		}
	}
}
func TestScheduledCommandsRejectInvalidTargetsAndPayloads(t *testing.T) {
	executor := &DomainExecutor{commands: &scheduledCommands{}}
	for _, parameters := range []map[string]interface{}{{"rawCommand": ""}, {"rawCommand": 5}, {"rawCommand": strings.Repeat("x", 4097)}, {"rawCommand": "print(1)\x00"}, {"rawCommand": "print(1)", "commandId": "shutdown"}, {"commandId": "missing"}} {
		if err := executor.Validate(Task{WorldIDs: []string{"world"}, Action: ActionCommandExecute, Parameters: parameters}); err == nil {
			t.Fatalf("accepted %#v", parameters)
		}
	}
	for _, worlds := range [][]string{nil, {"master", "caves"}} {
		if err := executor.Validate(Task{WorldIDs: worlds, Action: ActionCommandExecute, Parameters: map[string]interface{}{"rawCommand": "print(1)"}}); err == nil {
			t.Fatal("ambiguous target accepted")
		}
	}
}
