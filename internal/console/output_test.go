package console

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"dont/internal/dstruntime"
)

func TestOutputPersistsWithItsRunIncludingFailure(t *testing.T) {
	s := newConsoleService(t, &captureSender{})
	s.commander = &captureCommander{replies: []dstruntime.CommandReceipt{
		{OK: true, Details: map[string]interface{}{"output": map[string]interface{}{"text": "first\n", "truncated": false}}},
		{OK: false, Code: "COMMAND_EXECUTION_FAILED", Message: "boom", Details: map[string]interface{}{"output": map[string]interface{}{"text": "before boom\n", "truncated": true}}},
		{OK: true}, // Older Runtime does not claim an empty captured result.
	}}
	var runs []Run
	for _, code := range []string{`print("first")`, `print("before boom"); error("boom")`, `print("legacy")`} {
		run, err := s.ExecuteRaw(context.Background(), "room-id", "world-id", RawRequest{Command: code, Confirmation: "周末服"})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	for i, run := range runs {
		saved, err := s.store.Get(run.ID)
		if err != nil || saved.ID != run.ID || saved.RawCommand != run.RawCommand {
			t.Fatalf("saved run %d: %#v %v", i, saved, err)
		}
		if i == 2 {
			if saved.Output != nil {
				t.Fatalf("legacy output: %#v", saved.Output)
			}
			continue
		}
		if saved.Output == nil || *saved.Output != *run.Output || saved.Output.Text != []string{"first\n", "before boom\n"}[i] {
			t.Fatalf("saved output %d: %#v", i, saved)
		}
	}
	if runs[1].Status != RunFailed || runs[1].ErrorMessage != "boom" {
		t.Fatalf("failure lost: %#v", runs[1])
	}
	items, total, err := s.Runs(ListFilter{RoomID: "room-id", WorldID: "world-id"})
	if err != nil || total != 3 || len(items) != 3 || items[1].Output == nil {
		t.Fatalf("history: %#v %d %v", items, total, err)
	}
}

func TestOutputRejectsMismatchedCommandReceipt(t *testing.T) {
	s := newConsoleService(t, &captureSender{})
	s.commander = &captureCommander{replies: []dstruntime.CommandReceipt{
		{RequestID: "someone-elses-command", OK: true, Details: map[string]interface{}{"output": map[string]interface{}{"text": "wrong output", "truncated": false}}},
		{OK: true},
	}}
	run, err := s.ExecuteRaw(context.Background(), "room-id", "world-id", RawRequest{Command: `print("mine")`, Confirmation: "周末服"})
	if err != nil || run.Status != RunUncertain || run.Output != nil || !run.MayHaveExecuted {
		t.Fatalf("mismatched run: %#v %v", run, err)
	}
}

func TestOutputDefensivelyBoundsAgentText(t *testing.T) {
	output := commandOutput(map[string]interface{}{"output": map[string]interface{}{"text": strings.Repeat("中", 10000), "truncated": false}})
	if output == nil || !output.Truncated || len(output.Text) > maxCommandOutputBytes || !utf8.ValidString(output.Text) {
		t.Fatalf("output invalid: %#v", output)
	}
	if commandOutput(map[string]interface{}{"output": map[string]interface{}{"text": 123, "truncated": false}}) != nil {
		t.Fatal("accepted invalid output")
	}
}

func TestSavedCommandRecordsRenderedScript(t *testing.T) {
	s := newConsoleService(t, &captureSender{})
	s.commander = &captureCommander{replies: []dstruntime.CommandReceipt{{OK: true}}}
	run, err := s.Execute(context.Background(), "room-id", "world-id", ExecuteRequest{CommandID: "announce", Arguments: map[string]interface{}{"message": "hello"}})
	if err != nil || !strings.Contains(run.Script, `c_announce("hello")`) {
		t.Fatalf("code snapshot missing: %#v %v", run, err)
	}
}
