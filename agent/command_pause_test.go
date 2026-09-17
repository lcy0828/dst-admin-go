package agent

import (
	"dont/shared"
	"testing"
)

func TestPauseCommandsRequiresEmptyQueueAndRejectsNewWork(t *testing.T) {
	a := &Agent{stopChan: make(chan struct{}), generalCommands: make(chan commandWorkItem, 1)}
	resume, err := a.PauseCommandsIfIdle()
	if err != nil {
		t.Fatal(err)
	}
	if a.enqueueCommand(queuedCommandForTest(shared.ShardActionStart)) {
		t.Fatal("admitted command while paused")
	}
	resume()
	if !a.enqueueCommand(queuedCommandForTest(shared.ShardActionStart)) {
		t.Fatal("queue did not resume")
	}
	if _, err := a.PauseCommandsIfIdle(); err == nil {
		t.Fatal("ignored queued command")
	}
}
