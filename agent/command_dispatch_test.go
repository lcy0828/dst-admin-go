package agent

import (
	"sync/atomic"
	"testing"
	"time"

	"dont/shared"
)

func queuedCommandForTest(action shared.ShardAction) commandWorkItem {
	acknowledged := make(chan struct{})
	close(acknowledged)
	return commandWorkItem{
		payload:      shared.CommandPayload{Type: string(action)},
		acknowledged: acknowledged,
	}
}

func TestStatusCommandIsNotBlockedByStartingShard(t *testing.T) {
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	statusHandled := make(chan struct{})
	agent := &Agent{
		stopChan:        make(chan struct{}),
		statusCommands:  make(chan commandWorkItem, 2),
		generalCommands: make(chan commandWorkItem, 2),
	}
	agent.commandHandler = func(item commandWorkItem) {
		switch item.payload.Type {
		case string(shared.ShardActionStart):
			close(startEntered)
			<-releaseStart
		case string(shared.ShardActionStatus):
			close(statusHandled)
		}
	}
	agent.startCommandWorkers()
	t.Cleanup(func() {
		close(releaseStart)
		close(agent.stopChan)
		agent.commandWorkerWG.Wait()
	})

	if !agent.enqueueCommand(queuedCommandForTest(shared.ShardActionStart)) {
		t.Fatal("start command was not queued")
	}
	select {
	case <-startEntered:
	case <-time.After(time.Second):
		t.Fatal("start command did not begin")
	}
	if !agent.enqueueCommand(queuedCommandForTest(shared.ShardActionStatus)) {
		t.Fatal("status command was not queued")
	}
	select {
	case <-statusHandled:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("status command waited behind the blocking start command")
	}
}

func TestCommandQueueRejectsWorkWhenFull(t *testing.T) {
	agent := &Agent{
		stopChan:        make(chan struct{}),
		statusCommands:  make(chan commandWorkItem, 1),
		generalCommands: make(chan commandWorkItem, 1),
	}
	if !agent.enqueueCommand(queuedCommandForTest(shared.ShardActionStart)) {
		t.Fatal("first command was not queued")
	}
	if agent.enqueueCommand(queuedCommandForTest(shared.ShardActionStop)) {
		t.Fatal("full command queue accepted more work")
	}
}

func TestGeneralCommandWorkersRemainBounded(t *testing.T) {
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	agent := &Agent{
		stopChan:        make(chan struct{}),
		statusCommands:  make(chan commandWorkItem, 1),
		generalCommands: make(chan commandWorkItem, generalCommandQueueSize),
	}
	agent.commandHandler = func(commandWorkItem) {
		current := active.Add(1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		<-release
		active.Add(-1)
	}
	agent.startCommandWorkers()
	for index := 0; index < generalCommandWorkerCount*2; index++ {
		if !agent.enqueueCommand(queuedCommandForTest(shared.ShardActionStart)) {
			t.Fatalf("command %d was not queued", index)
		}
	}
	deadline := time.After(time.Second)
	for maximum.Load() < generalCommandWorkerCount {
		select {
		case <-deadline:
			t.Fatalf("only %d workers became active", maximum.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if maximum.Load() > generalCommandWorkerCount {
		t.Fatalf("worker concurrency exceeded limit: %d", maximum.Load())
	}
	close(release)
	close(agent.stopChan)
	agent.commandWorkerWG.Wait()
}
