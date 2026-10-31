package console

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWorkbenchValidatesCodeOwnedRecipesAndNeverTogglesLivingPlayers(t *testing.T) {
	s := newConsoleService(t, &captureSender{})
	for _, tc := range []struct {
		id        string
		args      map[string]interface{}
		confirmed bool
	}{
		{"custom", map[string]interface{}{}, true},
		{"resurrect_player", map[string]interface{}{"player_id": `KU_X\");c_shutdown()--`}, false},
		{"resurrect_player", map[string]interface{}{"player_id": "KU_TEST123", "readOnly": true}, false},
		{"resurrect_player", map[string]interface{}{"player_id": 123}, false},
		{"set_player_speed", map[string]interface{}{"player_id": "KU_TEST123", "multiplier": 101}, false},
		{"rollback", map[string]interface{}{"days": 1}, false},
	} {
		if _, err := s.ExecuteWorkbench(context.Background(), "room", "caves", tc.id, tc.args, tc.confirmed); err == nil {
			t.Fatal("accepted", tc)
		}
	}
	r, err := s.ExecuteWorkbench(context.Background(), "room", "caves", "resurrect_player", map[string]interface{}{"player_id": "KU_TEST123"}, false)
	if err != nil || r.Complete {
		t.Fatal(r, err)
	} // legacy delivery is not an execution receipt
	run, err := s.Run(r.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(run.Script, `if p:HasTag("playerghost")then`) || !strings.Contains(run.Script, `respawnfromghost`) || strings.Contains(run.Script, `ms_becameghost`) || strings.Contains(run.Script, `:Kill(`) {
		t.Fatal(run.Script)
	}
	if _, err = s.WorkbenchRun(context.Background(), "other", r.RunID, true); !errors.Is(err, ErrRunNotFound) {
		t.Fatal(err)
	}
	defs := BuiltinWorkbenchDefinitions()
	for _, d := range defs {
		if !d.IsBuiltin || d.Script != "" {
			t.Fatal(d)
		}
	}
	custom, err := s.CreateDefinition(Definition{Name: "外部命令", Category: "自定义", ScriptMode: "literal", Script: "c_shutdown()", Risk: RiskCritical})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ValidateBuiltinWorkbench(custom.ID, map[string]interface{}{}, true); !errors.Is(err, ErrCommandNotFound) {
		t.Fatal(err)
	}
}
func TestWorkbenchLateReceiptOnlyReadsOriginalAndExcludesCustomRuns(t *testing.T) {
	s := newConsoleService(t, &captureSender{})
	commander := &grantLateReceipt{}
	s.commander = commander
	r, err := s.ExecuteWorkbench(context.Background(), "room", "caves", "resurrect_player", map[string]interface{}{"player_id": "KU_TEST123"}, false)
	if err != nil || r.Complete || commander.sends != 1 {
		t.Fatal(r, err)
	}
	r, err = s.WorkbenchRun(context.Background(), "room", r.RunID, true)
	if err != nil || !r.Complete || commander.sends != 1 || commander.reads != 1 {
		t.Fatal(r, err, commander)
	}
	raw, err := s.ExecuteRaw(context.Background(), "room", "caves", RawRequest{Command: "print(1)", Confirmation: "测试房间"})
	if err == nil {
		if _, err = s.WorkbenchRun(context.Background(), "room", raw.ID, false); !errors.Is(err, ErrRunNotFound) {
			t.Fatal(err)
		}
	}
}

func TestResurrectionLuaEmitsOnlyForGhostAndDoesNotKillLivingPlayer(t *testing.T) {
	script, err := builtinTemplates()["resurrect_player"].render(map[string]interface{}{"player_id": "KU_TEST123"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ghost := range []bool{false, true} {
		expected := "ALREADY_ALIVE"
		state := "false"
		events := "0"
		if ghost {
			expected = "REQUESTED"
			state = "true"
			events = "1"
		}
		setup := `events=0;AllPlayers={{userid="KU_TEST123",HasTag=function(self,tag) return ` + state + ` end,PushEvent=function(self,event) assert(event=="respawnfromghost");events=events+1 end}}`
		output, err := auditLuaOutput(t, script+`;assert(events==`+events+`)`, setup)
		if err != nil || !strings.Contains(output, expected) {
			t.Fatal(ghost, output, err)
		}
	}
}
