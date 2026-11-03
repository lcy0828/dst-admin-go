package gameupdate

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"dont/internal/operationlease"
)

type optionsBackupFailure struct{ ReleaseProtectionService }

func (b optionsBackupFailure) CreateProtection(context.Context, string, string, string, *operationlease.Lease) (string, error) {
	return "", errors.New("test backup storage unavailable")
}

type optionsPartialBackupFailure struct{ ReleaseProtectionService }

func (b optionsPartialBackupFailure) CreateProtection(ctx context.Context, roomID, name, releaseID string, lease *operationlease.Lease) (string, error) {
	if roomID == "room-b" {
		return "", errors.New("test second backup failed")
	}
	return b.ReleaseProtectionService.CreateProtection(ctx, roomID, name, releaseID, lease)
}

type optionsInvalidBackup struct{ ReleaseProtectionService }

func (b optionsInvalidBackup) VerifyProtection(context.Context, string, string, string) error {
	return errors.New("test recorded backup is no longer valid")
}

type optionsCountdownNotifier struct{ announce func() }

func (n optionsCountdownNotifier) BeforeOperations(context.Context, []string, string, string, string) error {
	return errors.New("explicit delay must not run the legacy countdown")
}

func (n optionsCountdownNotifier) AnnounceGameUpdate(context.Context, []string, int, string) error {
	n.announce()
	return nil
}

func TestReleaseOptionalBackupAndCountdownValidation(t *testing.T) {
	policy, err := NormalizeReleasePolicy(ReleasePolicyInput{})
	if err != nil || policy.SkipBackup || policy.StopDelaySeconds != nil {
		t.Fatalf("legacy defaults changed: %+v %v", policy, err)
	}
	for _, delay := range []int{-1, 0, 60, 360, 361} {
		policy, err := NormalizeReleasePolicy(ReleasePolicyInput{StopDelaySeconds: &delay, SkipBackup: true})
		if delay < 0 || delay > 360 {
			if !errors.Is(err, ErrReleaseInvalid) {
				t.Fatalf("accepted invalid delay %d: %v", delay, err)
			}
		} else if err != nil || !policy.SkipBackup || *policy.StopDelaySeconds != delay {
			t.Fatalf("delay %d: %+v %v", delay, policy, err)
		}
	}
}

func TestReleaseSkipBackupDoesNotRequireBackupStorage(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	f.plan.Policy.SkipBackup = true
	f.plan.PlanHash, _ = calculateReleasePlanHash(f.plan)
	f.coordinator.backups = fullBackupVolume{f.coordinator.backups}
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "no-backup", Plan: f.plan})
	if err != nil || value.Stage != ReleaseStageSucceeded || len(value.ProtectionBackupIDs) != 0 {
		t.Fatalf("stage=%s backups=%v err=%v", value.Stage, value.ProtectionBackupIDs, err)
	}
	assertOptionsRestored(t, f)
	if len(f.runtime.updateCounts) != 2 {
		t.Fatal("did not update both installations")
	}
}

func TestReleaseBackupDecisionPersistsAndSupportsRetrySkipOrCancel(t *testing.T) {
	for _, action := range []string{"retry", "skip", "cancel"} {
		t.Run(action, func(t *testing.T) {
			f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
			backups := f.coordinator.backups
			f.plan.Policy.PauseOnBackupFailure = true
			f.plan.PlanHash, _ = calculateReleasePlanHash(f.plan)
			f.coordinator.backups = optionsBackupFailure{backups}
			value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "backup-choice", Plan: f.plan})
			if !errors.Is(err, ErrReleaseRecoveryNeeded) || value.NextAction != "backup_decision" || value.Stage != ReleaseStageRecoveryRequired || value.WritingStarted || len(f.runtime.updateCounts) != 0 {
				t.Fatalf("invalid backup pause: %+v err=%v", value, err)
			}
			if err := f.coordinator.store.Migrate(); err != nil {
				t.Fatal(err)
			}
			stored, err := f.coordinator.Get(value.ID)
			if err != nil || stored.NextAction != "backup_decision" {
				t.Fatalf("lost backup decision after restart: %+v %v", stored, err)
			}
			if err := f.coordinator.store.CheckMaintenance(context.Background(), releaseResources(f.plan), "other"); err == nil {
				t.Fatal("paused update allowed another writer")
			}
			if _, err := f.coordinator.RetryConfirmed(context.Background(), value.ID, ReleaseRetryRequest{SkipBackup: true}, "unconfirmed"); !errors.Is(err, ErrReleaseConfirmation) {
				t.Fatalf("skip accepted without confirmation: %v", err)
			}
			request := ReleaseRetryRequest{}
			switch action {
			case "retry":
				f.coordinator.backups = backups
			case "skip":
				request.Confirmed, request.SkipBackup = true, true
			case "cancel":
				request.Confirmed, request.CancelUpdate = true, true
			}
			value, err = f.coordinator.RetryConfirmed(context.Background(), value.ID, request, "backup-choice-resume")
			if err != nil {
				t.Fatal(err)
			}
			assertOptionsRestored(t, f)
			if action == "cancel" {
				if value.Stage != ReleaseStageAbandoned || len(f.runtime.updateCounts) != 0 || value.ErrorCode != "UPDATE_CANCELED" {
					t.Fatalf("cancel wrote files or lost status: %+v", value)
				}
				for _, shard := range value.Shards {
					if shard.WasRunning && (shard.RuntimeState != "running" || shard.StartedAt == nil) {
						t.Fatalf("cancel lost restoration evidence: %+v", shard)
					}
				}
			} else if value.Stage != ReleaseStageSucceeded || len(f.runtime.updateCounts) != 2 || value.BackupSkipped != (action == "skip") {
				t.Fatalf("bad resume: %+v", value)
			}
			if err := f.coordinator.store.CheckMaintenance(context.Background(), releaseResources(f.plan), "other"); err != nil {
				t.Fatalf("completed update left maintenance locked: %v", err)
			}
			stored, err = f.coordinator.Get(value.ID)
			if err != nil || stored.BackupSkipped != value.BackupSkipped || stored.CurrentJobID != "backup-choice-resume" {
				t.Fatalf("decision was not persisted: %+v %v", stored, err)
			}
		})
	}
}

func TestReleaseInvalidPartialBackupKeepsDecisionBeforeWriting(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	f.plan.Policy.PauseOnBackupFailure = true
	f.plan.PlanHash, _ = calculateReleasePlanHash(f.plan)
	backups := f.coordinator.backups
	f.coordinator.backups = optionsPartialBackupFailure{backups}
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "partial-backup-choice", Plan: f.plan})
	if !errors.Is(err, ErrReleaseRecoveryNeeded) || value.NextAction != "backup_decision" || len(value.ProtectionBackupIDs) != 1 {
		t.Fatalf("partial backup was not preserved: %+v %v", value, err)
	}
	f.coordinator.backups = optionsInvalidBackup{backups}
	value, err = f.coordinator.Retry(context.Background(), value.ID, "retry-invalid-backup")
	if !errors.Is(err, ErrReleaseRecoveryNeeded) || value.NextAction != "backup_decision" || value.WritingStarted || len(f.runtime.updateCounts) != 0 {
		t.Fatalf("invalid backup lost decision or wrote game files: %+v %v", value, err)
	}
	value, err = f.coordinator.RetryConfirmed(context.Background(), value.ID, ReleaseRetryRequest{Confirmed: true, SkipBackup: true}, "skip-invalid-backup")
	if err != nil || value.Stage != ReleaseStageSucceeded || !value.BackupSkipped {
		t.Fatalf("explicit skip could not resume: %+v %v", value, err)
	}
	assertOptionsRestored(t, f)
}

func assertOptionsRestored(t *testing.T, f releaseCoordinatorFixture) {
	t.Helper()
	for key, state := range f.runtime.states {
		expected := "running"
		if key == "room-b\x00Archive" {
			expected = "stopped"
		}
		if state.State != expected {
			t.Fatalf("world %s: expected %s, got %s", key, expected, state.State)
		}
	}
}

func TestReleaseSkipBackupStillRequiresStoppedWorldsAndVerifiedBuild(t *testing.T) {
	for _, failure := range []string{"stop", "version"} {
		t.Run(failure, func(t *testing.T) {
			f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
			f.plan.Policy.SkipBackup = true
			f.plan.PlanHash, _ = calculateReleasePlanHash(f.plan)
			if failure == "stop" {
				f.coordinator.runtime = &releaseFaultRuntime{coordinatorRuntime: f.runtime, failedStop: true}
			} else {
				f.runtime.updateVersions["agent:node-a\x00primary"] = "700"
			}
			value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "skip-failure", Plan: f.plan})
			if err == nil || value.Stage == ReleaseStageSucceeded {
				t.Fatalf("skip bypassed %s failure: %+v %v", failure, value, err)
			}
			if failure == "stop" && len(f.runtime.updateCounts) != 0 {
				t.Fatal("game write after stop failure")
			}
		})
	}
}

func TestReleaseExplicitCountdownIsDurableAndCancelableBeforeStop(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "finish", true: "cancel"}[cancel], func(t *testing.T) {
			f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
			delay := 360
			f.plan.Policy.StopDelaySeconds = &delay
			f.plan.PlanHash, _ = calculateReleasePlanHash(f.plan)
			waited := false
			f.coordinator.wait = func(ctx context.Context, duration time.Duration) error {
				waited = true
				value, err := f.coordinator.Get("countdown")
				if err != nil || value.Stage != ReleaseStageNotifying || value.CountdownEndsAt == nil || duration > 360*time.Second || duration < 359*time.Second || len(f.runtime.mutationEvents()) != 0 {
					t.Fatalf("countdown not durable before stopping: %+v duration=%v err=%v", value, duration, err)
				}
				if cancel {
					return context.Canceled
				}
				return ctx.Err()
			}
			value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "countdown", Plan: f.plan})
			if !waited {
				t.Fatal("explicit delay was skipped")
			}
			if cancel {
				if !errors.Is(err, context.Canceled) || len(f.runtime.mutationEvents()) != 0 || value.Stage != ReleaseStageFailed {
					t.Fatalf("canceled countdown mutated runtime: %+v %v", value, err)
				}
			} else if err != nil || value.Stage != ReleaseStageSucceeded {
				t.Fatalf("stage=%s err=%v", value.Stage, err)
			}
		})
	}
}

func TestLegacyAndUnattendedBackupFailureRestoresWorlds(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	f.coordinator.backups = optionsBackupFailure{f.coordinator.backups}
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "unattended-backup-failure", Plan: f.plan})
	if err == nil || value.Stage != ReleaseStageFailed || value.NextAction == "backup_decision" || len(f.runtime.updateCounts) != 0 {
		t.Fatalf("unattended failure left worlds paused: %+v %v", value, err)
	}
	assertOptionsRestored(t, f)
}

func TestReleaseCountdownStartsAfterSlowAnnouncement(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	delay := 60
	f.plan.Policy.StopDelaySeconds = &delay
	f.plan.PlanHash, _ = calculateReleasePlanHash(f.plan)
	base := time.Now().UTC()
	var elapsed atomic.Int64
	f.coordinator.now = func() time.Time { return base.Add(time.Duration(elapsed.Load()) * time.Second) }
	f.coordinator.ConfigureNotifier(optionsCountdownNotifier{announce: func() {
		value, err := f.coordinator.Get("slow-announcement")
		if err != nil || value.Stage != ReleaseStageNotifying || value.CountdownEndsAt != nil {
			t.Fatalf("announcement should be visible before countdown starts: %+v %v", value, err)
		}
		elapsed.Store(45)
	}})
	waited := false
	f.coordinator.wait = func(_ context.Context, duration time.Duration) error {
		waited = true
		value, err := f.coordinator.Get("slow-announcement")
		if err != nil || duration != 60*time.Second || value.CountdownEndsAt == nil || !value.CountdownEndsAt.Equal(base.Add(105*time.Second)) || len(f.runtime.mutationEvents()) != 0 {
			t.Fatalf("announcement shortened warning time: duration=%s value=%+v err=%v", duration, value, err)
		}
		return nil
	}
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "slow-announcement", Plan: f.plan})
	if err != nil || value.Stage != ReleaseStageSucceeded || !waited {
		t.Fatalf("update did not finish after countdown: %+v %v", value, err)
	}
}

func TestReleaseBackupSpaceDecisionBeforeWorldsStop(t *testing.T) {
	for _, action := range []string{"skip", "cancel"} {
		t.Run(action, func(t *testing.T) {
			f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
			f.plan.Policy.PauseOnBackupFailure = true
			f.plan.PlanHash, _ = calculateReleasePlanHash(f.plan)
			f.coordinator.backups = fullBackupVolume{f.coordinator.backups}
			value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "space-choice", Plan: f.plan})
			if !errors.Is(err, ErrReleaseRecoveryNeeded) || value.NextAction != "backup_decision" || len(f.runtime.mutationEvents()) != 0 {
				t.Fatalf("space failure stopped worlds before decision: %+v %v", value, err)
			}
			value, err = f.coordinator.RetryConfirmed(context.Background(), value.ID, ReleaseRetryRequest{Confirmed: true, SkipBackup: action == "skip", CancelUpdate: action == "cancel"}, "space-choice-resume")
			if err != nil {
				t.Fatal(err)
			}
			if action == "cancel" && (value.Stage != ReleaseStageAbandoned || len(f.runtime.mutationEvents()) != 0) {
				t.Fatalf("cancel restarted worlds that never stopped: %+v", value)
			}
			if action == "skip" && (value.Stage != ReleaseStageSucceeded || len(f.runtime.updateCounts) != 2) {
				t.Fatalf("explicit skip did not update: %+v", value)
			}
			assertOptionsRestored(t, f)
		})
	}
}
