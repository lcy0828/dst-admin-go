package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dont/shared"
)

func TestWaitCommandResultAlreadyCompleted(t *testing.T) {
	server, agent := commandWaitFixture()
	completeWaitingCommand(t, server, agent)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := server.WaitCommandResult(ctx, "command", nil)
	if err != nil || !result.Success || result.Output != "result" {
		t.Fatalf("completed result was not returned: %#v, %v", result, err)
	}
	if server.commandResults["command"].changed != nil || result.changed != nil {
		t.Fatal("already completed result allocated a notification channel")
	}
	result.Output = "caller change"
	if server.commandResults["command"].Output != "result" {
		t.Fatal("waiter received a mutable internal result")
	}
}

func TestWaitCommandResultDoesNotMissReplyDuringProgress(t *testing.T) {
	server, agent := commandWaitFixture()
	server.commandResults["command"].Progress = &shared.CommandProgressPayload{Sequence: 1}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var sequences []uint64
	result, err := server.WaitCommandResult(ctx, "command", func(progress shared.CommandProgressPayload) {
		sequences = append(sequences, progress.Sequence)
		if progress.Sequence != 1 {
			return
		}
		// Deliver a reply after the waiter read pending, but before its select.
		message, err := shared.CreateMessage(shared.TypeCommandProgress, agent.AgentID, shared.CommandProgressPayload{
			CommandID: "command", Sequence: 2, Percent: 100, Message: "done",
		})
		if err != nil {
			t.Fatal(err)
		}
		server.handleCommandProgress(agent, message)
		completeWaitingCommand(t, server, agent)
	})
	if err != nil || !result.Success || len(sequences) != 2 || sequences[0] != 1 || sequences[1] != 2 {
		t.Fatalf("reply or final progress lost: result=%#v, sequences=%v, err=%v", result, sequences, err)
	}
}

func TestWaitCommandResultCancellationDoesNotAffectOtherWaiters(t *testing.T) {
	server, agent := commandWaitFixture()
	server.commandResults["command"].Progress = &shared.CommandProgressPayload{Sequence: 1}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	firstCtx, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	ready := make(chan struct{}, 2)
	start := func(ctx context.Context) <-chan error {
		completed := make(chan error, 1)
		go func() {
			_, err := server.WaitCommandResult(ctx, "command", func(shared.CommandProgressPayload) { ready <- struct{}{} })
			completed <- err
		}()
		return completed
	}
	first := start(firstCtx)
	second := start(ctx)
	for count := 0; count < 2; count++ {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("waiters did not register")
		}
	}
	cancelFirst()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation returned %v", err)
	}
	completeWaitingCommand(t, server, agent)
	if err := <-second; err != nil {
		t.Fatalf("cancelling another waiter lost the result: %v", err)
	}
	if server.commandResults["command"].changed != nil {
		t.Fatal("completed result retained the notification channel")
	}
}

func TestCommandNotificationsRespectAgentAndTerminalState(t *testing.T) {
	server, agent := commandWaitFixture()
	changed := make(chan struct{})
	server.commandResults["command"].changed = changed
	ack := func(agent *AgentConnection, status string) {
		message, err := shared.CreateMessage(shared.TypeCommandAck, agent.AgentID, map[string]string{"command_id": "command", "status": status})
		if err != nil {
			t.Fatal(err)
		}
		server.handleCommandAck(agent, message)
	}
	other := &AgentConnection{AgentID: "other"}
	ack(other, "rejected")
	completeWaitingCommand(t, server, other)
	if server.commandResults["command"].Status != "pending" {
		t.Fatal("another Agent replaced the command result")
	}
	select {
	case <-changed:
		t.Fatal("another Agent woke the command waiter")
	default:
	}
	ack(agent, "received")
	ack(agent, "rejected")
	select {
	case <-changed:
	default:
		t.Fatal("rejection did not notify the waiter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := server.WaitCommandResult(ctx, "command", nil)
	if err != nil || result.Status != "failed" || result.EndTime == 0 || result.ErrorMsg == "" {
		t.Fatalf("rejection was not returned: %#v, %v", result, err)
	}
	ack(agent, "received")
	if server.commandResults["command"].Status != "failed" {
		t.Fatal("late acknowledgement reverted a terminal result")
	}
	completeWaitingCommand(t, server, agent)
	ack(agent, "received")
	if server.commandResults["command"].Status != "completed" {
		t.Fatal("late acknowledgement reverted a completed result")
	}
}

func TestWaitCommandResultMissingCancelledAndStopped(t *testing.T) {
	server, _ := commandWaitFixture()
	if _, err := server.WaitCommandResult(context.Background(), "missing", nil); err == nil {
		t.Fatal("missing command must return an error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := server.WaitCommandResult(ctx, "command", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter returned %v", err)
	}
	if server.commandResults["command"].changed != nil {
		t.Fatal("pre-cancelled waiter allocated a notification channel")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	server.commandResults["command"].Progress = &shared.CommandProgressPayload{Sequence: 1}
	_, err := server.WaitCommandResult(ctx, "command", func(shared.CommandProgressPayload) { server.Stop() })
	if err == nil || !strings.Contains(err.Error(), "网关已停止") {
		t.Fatalf("stopping the gateway did not wake the waiter: %v", err)
	}
}

func commandWaitFixture() (*Server, *AgentConnection) {
	return &Server{
		stopChan: make(chan struct{}),
		commandResults: map[string]*CommandResult{
			"command": {CommandID: "command", AgentID: "agent", Status: "pending"},
		},
	}, &AgentConnection{AgentID: "agent"}
}

func completeWaitingCommand(t *testing.T, server *Server, agent *AgentConnection) {
	t.Helper()
	message, err := shared.CreateMessage(shared.TypeCommandResp, agent.AgentID, shared.CommandResponsePayload{
		CommandID: "command", Success: true, Output: "result",
	})
	if err != nil {
		t.Fatal(err)
	}
	server.handleCommandResponse(agent, message)
}
