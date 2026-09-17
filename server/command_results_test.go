package server

import (
	"testing"

	"dont/shared"
)

func TestCommandResultReadersReturnSnapshots(t *testing.T) {
	server := &Server{commandResults: map[string]*CommandResult{
		"command": {
			CommandID: "command", AgentID: "agent", Status: "pending", StartTime: 1,
			Progress: &shared.CommandProgressPayload{CommandID: "command", Sequence: 1, Percent: 20, Message: "downloading"},
		},
	}}

	result, err := server.GetCommandResult("command")
	if err != nil {
		t.Fatal(err)
	}
	result.Status = "changed-by-caller"
	result.Progress.Percent = 90
	if server.commandResults["command"].Status != "pending" {
		t.Fatal("single-result reader exposed the mutable internal value")
	}
	if server.commandResults["command"].Progress.Percent != 20 {
		t.Fatal("single-result reader exposed mutable progress")
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

func TestCommandProgressKeepsNewestValidatedAgentSnapshot(t *testing.T) {
	server := &Server{commandResults: map[string]*CommandResult{
		"command": {CommandID: "command", AgentID: "agent-a", Status: "received"},
	}}
	agent := &AgentConnection{AgentID: "agent-a"}
	send := func(payload shared.CommandProgressPayload) {
		message, err := shared.CreateMessage(shared.TypeCommandProgress, agent.AgentID, payload)
		if err != nil {
			t.Fatal(err)
		}
		server.handleCommandProgress(agent, message)
	}
	send(shared.CommandProgressPayload{
		CommandID: "command", Sequence: 2, Stage: "mod.cache", Percent: 40, Message: "downloading",
		CurrentBytes: 32 << 20, TotalBytes: 92 << 20, BytesPerSecond: 4_500_375,
	})
	send(shared.CommandProgressPayload{CommandID: "command", Sequence: 1, Percent: 10, Message: "stale"})
	send(shared.CommandProgressPayload{CommandID: "command", Sequence: 3, Percent: 101, Message: "invalid"})

	progress := server.commandResults["command"].Progress
	if progress == nil || progress.Sequence != 2 || progress.Percent != 40 || progress.BytesPerSecond != 4_500_375 {
		t.Fatalf("unexpected command progress: %#v", progress)
	}
	otherMessage, err := shared.CreateMessage(shared.TypeCommandProgress, "agent-b", shared.CommandProgressPayload{
		CommandID: "command", Sequence: 4, Percent: 50, Message: "wrong agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	server.handleCommandProgress(&AgentConnection{AgentID: "agent-b"}, otherMessage)
	if server.commandResults["command"].Progress.Sequence != 2 {
		t.Fatal("another agent replaced command progress")
	}
}
