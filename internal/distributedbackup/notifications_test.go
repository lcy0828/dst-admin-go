package distributedbackup

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/operationlease"
	"dont/internal/shards"
)

type backupNotifierFunc func(context.Context, string, string, string, string) error

func (f backupNotifierFunc) BeforeOperation(ctx context.Context, room, action, source, job string) error {
	return f(ctx, room, action, source, job)
}

func TestColdBackupNotifiesBeforeStoppingAndCancellationLeavesWorldsUntouched(t *testing.T) {
	for _, local := range []bool{true, false} {
		for _, canceled := range []bool{true, false} {
			f := newBackupPlacementFixture(t, local)
			calls := 0
			f.coordinator.ConfigureNotifier(backupNotifierFunc(func(_ context.Context, room, action, source, job string) error {
				calls++
				if room != "room" || action != "backup" || job != "backup-job" || len(f.master.stops) != 0 || len(f.caves.stops) != 0 {
					t.Fatal("notice ran after stopping or lost scope")
				}
				if canceled {
					return context.Canceled
				}
				return nil
			}))
			value, err := f.coordinator.CreateWithMode(context.Background(), "room", "backup", "snapshot", "backup-job", ModeCold)
			if calls != 1 {
				t.Fatalf("notification count %d", calls)
			}
			if canceled {
				if !errors.Is(err, context.Canceled) || value.Status != StatusFailed || value.FailurePhase != "notifying" || len(f.master.stops)+len(f.caves.stops)+len(f.master.starts)+len(f.caves.starts) != 0 {
					t.Fatalf("canceled notice touched worlds: %+v %v", value, err)
				}
			} else if err != nil || value.Status != StatusVerified || len(f.master.stops) != 1 || len(f.caves.stops) != 1 || len(f.master.starts) != 1 || len(f.caves.starts) != 1 {
				t.Fatalf("cold backup did not resume: %+v %v", value, err)
			}
		}
	}
}

func TestBackupAnnouncementsRespectMethodAndProtectionOwner(t *testing.T) {
	for _, scenario := range []string{"hot", "stopped", "protection", "partial"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDistributedBackupFixture(t)
			mode, kind := ModeAutomatic, "snapshot"
			calls := 0
			f.coordinator.ConfigureNotifier(backupNotifierFunc(func(context.Context, string, string, string, string) error { calls++; return nil }))
			switch scenario {
			case "hot":
				installHotBarrierDrivers(t, f, []int64{41, 41})
			case "stopped", "partial":
				f.caves.states["Cluster_1\x00Caves"] = shards.RuntimeStatus{State: shards.RuntimeStopped}
				if scenario == "stopped" {
					f.master.states["Cluster_1\x00Master"] = shards.RuntimeStatus{State: shards.RuntimeStopped}
				}
			case "protection":
				mode, kind = ModeCold, "protection"
			}
			_, err := f.coordinator.CreateWithMode(context.Background(), "room", "backup", kind, "job", mode)
			want := 0
			if scenario == "partial" {
				want = 1
			}
			if err != nil || calls != want {
				t.Fatalf("calls=%d want=%d err=%v", calls, want, err)
			}
		})
	}
}

type noticeLeaseService struct {
	LeaseService
	renew func(operationlease.Lease) (operationlease.Lease, error)
}

func (s noticeLeaseService) Renew(_ context.Context, lease operationlease.Lease, _ time.Duration) (operationlease.Lease, error) {
	return s.renew(lease)
}

func TestBackupCountdownRenewsLeaseAndCancelsOnLeaseLoss(t *testing.T) {
	for _, lost := range []bool{false, true} {
		f := newDistributedBackupFixture(t)
		lease := operationlease.Lease{ExpiresAt: time.Now().Add(leaseTTL)}
		renewed := make(chan struct{}, 1)
		cause := errors.New("lease lost")
		f.coordinator.leases = noticeLeaseService{renew: func(value operationlease.Lease) (operationlease.Lease, error) {
			if lost {
				return value, cause
			}
			value.ExpiresAt = time.Now().Add(2 * leaseTTL)
			select {
			case renewed <- struct{}{}:
			default:
			}
			return value, nil
		}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := f.coordinator.runWithLeaseRenewal(ctx, &lease, time.Millisecond, func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-renewed:
				return nil
			}
		})
		cancel()
		if lost && !errors.Is(err, cause) || !lost && (err != nil || time.Until(lease.ExpiresAt) < leaseTTL) {
			t.Fatalf("lease renewal lost=%v err=%v lease=%+v", lost, err, lease)
		}
	}
}

func TestRecoverInterruptedBackupWarningDoesNotRestartWorlds(t *testing.T) {
	f := newDistributedBackupFixture(t)
	lease, err := f.leases.Acquire(context.Background(), "room", "notice", leaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	room, parts, revision, running, err := f.coordinator.plan(context.Background(), "room")
	if err != nil {
		t.Fatal(err)
	}
	value, operation, err := f.coordinator.initializeSet(room, parts, revision, running, "backup", "snapshot", "job", "notice", lease, ModeCold)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.coordinator.saveOperationPhase(&operation, "notifying", OperationRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.leases.Release(lease); err != nil {
		t.Fatal(err)
	}
	f.master.states["Cluster_1\x00Master"] = shards.RuntimeStatus{State: shards.RuntimeStopped}
	f.caves.states["Cluster_1\x00Caves"] = shards.RuntimeStatus{State: shards.RuntimeStopped}
	if err := f.coordinator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	value, err = f.store.GetSet(value.ID)
	if err != nil || value.FailureCode != "CONTROLLER_RESTARTED" || value.Status != StatusFailed || len(f.master.starts)+len(f.caves.starts)+len(f.master.stops)+len(f.caves.stops) != 0 {
		t.Fatalf("warning recovery touched worlds: %+v %v", value, err)
	}
}
