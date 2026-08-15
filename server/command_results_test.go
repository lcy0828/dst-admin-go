package server

import "testing"

func TestCommandResultReadersReturnSnapshots(t *testing.T) {
	server := &Server{commandResults: map[string]*CommandResult{
		"command": {CommandID: "command", AgentID: "agent", Status: "pending", StartTime: 1},
	}}

	result, err := server.GetCommandResult("command")
	if err != nil {
		t.Fatal(err)
	}
	result.Status = "changed-by-caller"
	if server.commandResults["command"].Status != "pending" {
		t.Fatal("single-result reader exposed the mutable internal value")
	}

	results := server.GetCommandResults("agent", 10)
	if len(results) != 1 {
		t.Fatalf("results=%d", len(results))
	}
	results[0].Status = "changed-by-list-caller"
	if server.commandResults["command"].Status != "pending" {
		t.Fatal("list reader exposed the mutable internal value")
	}
}
