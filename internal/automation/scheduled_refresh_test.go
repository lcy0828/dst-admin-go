package automation

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"dont/internal/jobs"
	"dont/internal/players"
)

type automationSamplingPlayers struct {
	active    bool
	scheduled atomic.Int32
	manual    atomic.Int32
}

func (*automationSamplingPlayers) WorldTargets(string) ([]players.WorldTarget, error) {
	return []players.WorldTarget{{ID: "Master"}, {ID: "Caves"}}, nil
}

func (*automationSamplingPlayers) RefreshWorld(context.Context, string, string) (players.RefreshResult, error) {
	return players.RefreshResult{}, errors.New("expected room batch refresh")
}

func (p *automationSamplingPlayers) PrepareScheduledRefresh(context.Context, string, []string) (bool, error) {
	return p.active, nil
}

func (p *automationSamplingPlayers) RefreshScheduledWorlds(context.Context, string, []string) ([]players.RefreshOutcome, error) {
	p.scheduled.Add(1)
	return nil, nil
}

func (p *automationSamplingPlayers) RefreshWorlds(context.Context, string, []string) ([]players.RefreshOutcome, error) {
	p.manual.Add(1)
	return nil, nil
}

func TestPlayerSchedulesUsePauseAwareSamplingAndManualRunsRemainExplicit(t *testing.T) {
	playerService := &automationSamplingPlayers{}
	service, store, jobService := newAutomationTestService(t, &DomainExecutor{players: playerService})
	builtIn, _, err := service.EnsureDefaultPlayerRefresh("room")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunScheduledTask("room", builtIn.ID); !errors.Is(err, ErrNoRunningWorlds) {
		t.Fatalf("inactive scheduled run: %v", err)
	}
	unchanged, err := store.Task("room", builtIn.ID)
	if err != nil || unchanged.LastRunAt != nil || playerService.scheduled.Load() != 0 || playerService.manual.Load() != 0 {
		t.Fatalf("inactive schedule performed work: task=%+v err=%v", unchanged, err)
	}
	playerService.active = true
	if err := service.RunScheduledTask("room", builtIn.ID); err != nil {
		t.Fatal(err)
	}
	custom, err := service.CreateTask("room", TaskInput{
		GroupID: builtIn.GroupID, Name: "Custom player sampling", Enabled: true,
		Schedule: "*/5 * * * *", Action: ActionPlayerRefresh, WorldIDs: []string{"Master"},
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.RunTask("room", custom.ID, TriggerSchedule)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAutomationJob(t, jobService, job.ID); result.Status != jobs.StatusSucceeded {
		t.Fatalf("custom sampling failed: %+v", result)
	}
	if playerService.scheduled.Load() != 2 || playerService.manual.Load() != 0 {
		t.Fatalf("schedule used explicit refresh: scheduled=%d manual=%d", playerService.scheduled.Load(), playerService.manual.Load())
	}
	playerService.active = false
	job, err = service.RunTask("room", builtIn.ID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAutomationJob(t, jobService, job.ID); result.Status != jobs.StatusSucceeded {
		t.Fatalf("manual paused refresh failed: %+v", result)
	}
	if playerService.scheduled.Load() != 2 || playerService.manual.Load() != 1 {
		t.Fatalf("manual refresh was suppressed: scheduled=%d manual=%d", playerService.scheduled.Load(), playerService.manual.Load())
	}
}
