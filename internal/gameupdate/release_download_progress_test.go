package gameupdate

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/operationprogress"
	"dont/internal/runtimedriver"
	"dont/shared"
)

type releaseStreamingRunner struct {
	base  CommandRunner
	check func()
}

func (r releaseStreamingRunner) Run(ctx context.Context, executable string, args []string, out io.Writer) error {
	_, _ = io.WriteString(out, "Update state (0x61) down")
	_, _ = io.WriteString(out, "loading, progress: 50 (50 / 100)\r")
	r.check()
	return r.base.Run(ctx, executable, args, out)
}

func TestNativeReleaseReportsDownloadBeforeSteamCMDReturns(t *testing.T) {
	s, _, _, _, _, _ := newUpdateService(t, nil)
	steamcmd := filepath.Join(t.TempDir(), "steamcmd")
	if err := os.WriteFile(steamcmd, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.config.SteamCMDPath = steamcmd
	var progress operationprogress.Update
	ctx := operationprogress.WithReporter(context.Background(), func(p operationprogress.Update) { progress = p })
	s.runner = releaseStreamingRunner{base: s.runner, check: func() {
		if progress.Stage != "game.download" || progress.CurrentBytes != 50 || progress.TotalBytes != 100 || progress.Percent == 100 {
			t.Fatal(progress)
		}
	}}
	result, err := s.UpdateReleaseInstallation(ctx, "200", false)
	if err != nil || result.CurrentVersion != "200" {
		t.Fatal(result, err)
	}
}

type downloadProgressRuntime struct {
	*coordinatorRuntime
	during func(context.Context, ReleaseInstallationPlan, runtimedriver.Operation)
	late   context.Context
}

func (r *downloadProgressRuntime) UpdateInstallation(ctx context.Context, target ReleaseInstallationPlan, op runtimedriver.Operation, version string, clean bool) (shared.RuntimeGameVersionResult, error) {
	r.late = ctx
	r.during(ctx, target, op)
	return r.coordinatorRuntime.UpdateInstallation(ctx, target, op, version, clean)
}

func TestDownloadProgressIsPersistedBeforeReturnAndNeverConfirmsWrites(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	r := &downloadProgressRuntime{coordinatorRuntime: f.runtime}
	r.during = func(ctx context.Context, target ReleaseInstallationPlan, op runtimedriver.Operation) {
		for _, update := range []operationprogress.Update{
			{Stage: "game.download", Attempt: 1, CurrentBytes: 50, TotalBytes: 100, BytesPerSecond: 10},
			{Stage: "game.validate", Attempt: 1, CurrentBytes: 1, TotalBytes: 100, BytesPerSecond: 99},
			{Stage: "game.prepare", Attempt: 2},
			{Stage: "game.download", Attempt: 2, CurrentBytes: 100, TotalBytes: 100},
		} {
			operationprogress.Report(ctx, update)
			v, err := f.coordinator.store.Get("download-progress")
			if err != nil {
				t.Fatal(err)
			}
			i := releaseInstallationResult(&v, target.TargetID, target.InstallationID)
			if v.Stage != ReleaseStageUpdating || i.Progress == nil || i.Progress.OperationID != op.ID || i.WriteState != "dispatched" || i.AfterVersion != "" {
				t.Fatalf("progress turned into completion: %+v", i)
			}
			if i.Progress.Attempt != update.Attempt || i.Progress.CurrentBytes != update.CurrentBytes || i.Progress.Phase != releaseDownloadPhase(update.Stage) {
				t.Fatal(i.Progress, update)
			}
			if update.Stage == "game.validate" && i.Progress.BytesPerSecond != 0 {
				t.Fatal("validation retained download speed")
			}
		}
	}
	f.coordinator.runtime = r
	v, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "download-progress", Plan: f.plan})
	if err != nil || v.Stage != ReleaseStageSucceeded {
		t.Fatal(v.Stage, err)
	}
	before, err := f.coordinator.store.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	operationprogress.Report(r.late, operationprogress.Update{Stage: "game.validate", Attempt: 2, CurrentBytes: 5, TotalBytes: 100})
	after, err := f.coordinator.store.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, item := range after.Installations {
		if item.Progress == nil || item.Progress.Phase != "downloading" || item.Progress.CurrentBytes != 100 || !item.Progress.ObservedAt.Equal(before.Installations[i].Progress.ObservedAt) {
			t.Fatal("late callback changed final evidence", item)
		}
	}
}

func TestDownloadCollectorRejectsInvalidAndThrottlesRemoteProgress(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	now := time.Now().UTC()
	f.coordinator.now = func() time.Time { return now }
	r := &downloadProgressRuntime{coordinatorRuntime: f.runtime}
	r.during = func(ctx context.Context, target ReleaseInstallationPlan, op runtimedriver.Operation) {
		operationprogress.Report(ctx, operationprogress.Update{Stage: "untrusted", CurrentBytes: 1, TotalBytes: 10})
		operationprogress.Report(ctx, operationprogress.Update{Stage: "game.download", CurrentBytes: 100, TotalBytes: 10})
		v, _ := f.coordinator.store.Get("bounded-progress")
		if releaseInstallationResult(&v, target.TargetID, target.InstallationID).Progress != nil {
			t.Fatal("invalid progress persisted")
		}
		operationprogress.Report(ctx, operationprogress.Update{Stage: "game.download", CurrentBytes: 1, TotalBytes: 10})
		operationprogress.Report(ctx, operationprogress.Update{Stage: "game.download", CurrentBytes: 9, TotalBytes: 10})
		v, _ = f.coordinator.store.Get("bounded-progress")
		if releaseInstallationResult(&v, target.TargetID, target.InstallationID).Progress.CurrentBytes != 1 {
			t.Fatal("not coalesced")
		}
		operationprogress.Report(ctx, operationprogress.Update{Stage: "game.download", CurrentBytes: 10, TotalBytes: 10})
		v, _ = f.coordinator.store.Get("bounded-progress")
		if releaseInstallationResult(&v, target.TargetID, target.InstallationID).Progress.CurrentBytes != 10 {
			t.Fatal("final byte count lost")
		}
	}
	f.coordinator.runtime = r
	if _, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "bounded-progress", Plan: f.plan}); err != nil {
		t.Fatal(err)
	}
}
