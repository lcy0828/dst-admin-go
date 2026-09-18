package gameupdate

import (
	"context"
	"errors"
	"testing"

	"dont/internal/runtimedriver"
)

type failFirstReleaseStop struct {
	*coordinatorRuntime
	failed bool
}

func (r *failFirstReleaseStop) Stop(ctx context.Context, shard ReleaseShardPlan, op runtimedriver.Operation) error {
	if !r.failed {
		r.failed = true
		return errors.New("temporary stop failure")
	}
	return r.coordinatorRuntime.Stop(ctx, shard, op)
}

func TestFailedReleaseRetainsProtectionAndRetryVerifiesBeforeMutating(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	f.coordinator.runtime = &failFirstReleaseStop{coordinatorRuntime: f.runtime}
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "release-protection", Plan: f.plan})
	if err == nil || value.Stage != ReleaseStageFailed || len(value.ProtectionBackupIDs) == 0 {
		t.Fatalf("failed release: %#v %v", value, err)
	}
	if needed, err := f.coordinator.RequiresProtection(value.ID); err != nil || !needed {
		t.Fatalf("retryable release lost deletion protection: %v %v", needed, err)
	}
	backups := f.coordinator.backups.(*coordinatorBackups)
	missing := errors.New("original protection archive is missing or corrupt")
	backups.verifyErr = missing
	before := len(f.runtime.mutationEvents())
	retried, err := f.coordinator.Retry(context.Background(), value.ID)
	if !errors.Is(err, missing) || retried.Stage != ReleaseStageRecoveryRequired {
		t.Fatalf("retry was not blocked: %#v %v", retried, err)
	}
	if len(f.runtime.mutationEvents()) != before || len(backups.ids) != len(value.ProtectionBackupIDs) {
		t.Fatal("retry mutated runtimes or replaced missing original protection")
	}
	backups.verifyErr = nil
	retried, err = f.coordinator.Retry(context.Background(), value.ID)
	if err != nil || retried.Stage != ReleaseStageSucceeded {
		t.Fatalf("valid protection did not permit retry: %#v %v", retried, err)
	}
	if needed, err := f.coordinator.RequiresProtection(value.ID); err != nil || needed {
		t.Fatalf("completed release retains protection unnecessarily: %v %v", needed, err)
	}
}
