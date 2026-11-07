package distributedbackup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dont/internal/operationlease"
	"dont/internal/runtimedriver"

	lua "github.com/yuin/gopher-lua"
)

func TestHotBarrierConsoleRecoveryNeverReportsSuccessFromDeliveryAlone(t *testing.T) {
	for _, tc := range []struct {
		name, setup string
		accepted    bool
	}{
		{"drained", `return true`, true},
		{"method rejected", `return false`, false},
		{"method threw", `error("unpause failed")`, false},
		{"write pending", `status.inFlight=true;status.busy=true;return true`, true},
		{"receipt writing", `status.writing=true;return true`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := lua.NewState()
			defer state.Close()
			if err := state.DoString(`
status={barrierId="hot-test-recovery"}
output=""
print=function(value)output=output..value end
DSTAdmin={Barriers={Status=function()return status end,ForceRollback=function(id) assert(id=="hot-test-recovery");` + tc.setup + ` end}}
-- A failed or mismatched recovery must never bypass the barrier's ownership.
TheNet={EndWorldSave=function()error("unscoped native call")end,SetServerPaused=function()error("unscoped native call")end}
SetServerPaused=function()error("unscoped global call")end
`); err != nil {
				t.Fatal(err)
			}
			if err := state.DoString(forceRollbackBarrierCommand("hot-test-recovery")); err != nil {
				t.Fatal(err)
			}
			output := state.GetGlobal("output").String()
			if strings.Contains(output, "snapshot barrier recovered") {
				t.Fatalf("console delivery was used as recovery proof: %q", output)
			}
			if tc.accepted && !strings.Contains(output, "snapshot barrier recovery requested") {
				t.Fatalf("accepted recovery omitted: %q", output)
			}
			if !tc.accepted && !strings.Contains(output, "SNAPSHOT_BARRIER_RECOVERY_UNCONFIRMED") {
				t.Fatalf("uncertainty omitted from output: %q", output)
			}
		})
	}
}

func TestHotBarrierFallbackRequiresReceiptAndDoesNotBlockHealthyShard(t *testing.T) {
	for _, tc := range []struct {
		name      string
		confirmed bool
		sendFails bool
	}{
		{"verified fallback", true, false},
		{"sent without receipt", false, false},
		{"console transport failure", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newDistributedBackupFixture(t)
			drivers := installHotBarrierDrivers(t, fixture, nil)
			parts := make([]runtimePart, len(drivers))
			const id = "hot-test-recovery"
			for i, driver := range drivers {
				driver.receipt = runtimedriver.SnapshotBarrierReceipt{
					BarrierID: id, State: "completed", Busy: true, Holding: true,
					ProducerInstanceID: "instance", SessionID: "session", ShardID: "shard",
				}
				parts[i] = runtimePart{driver: driver, part: Part{
					WorldName: "example", BarrierInstance: "instance", BarrierSessionID: "session", BarrierShardID: "shard",
				}}
			}
			cancelFailure := errors.New("cancel transport unavailable")
			drivers[0].cancelErr = cancelFailure
			sendFailure := errors.New("console transport unavailable")
			if tc.sendFails {
				drivers[0].consoleErr = sendFailure
			}
			if tc.confirmed {
				drivers[0].consoleHook = func() {
					drivers[0].observeErr = nil
					drivers[0].receipt.State = "cancelled"
					drivers[0].receipt.Busy, drivers[0].receipt.Holding = false, false
				}
			}
			lease := operationlease.Lease{ExpiresAt: time.Now().Add(time.Hour)}
			err := fixture.coordinator.forceRollbackHotBarriers(parts, Operation{ID: "test"}, &lease, id, nil)
			if (err == nil) != tc.confirmed {
				t.Fatalf("confirmed=%v err=%v", tc.confirmed, err)
			}
			if !tc.confirmed && !errors.Is(err, cancelFailure) {
				t.Fatalf("cancel failure was discarded: %v", err)
			}
			if tc.sendFails && !errors.Is(err, sendFailure) {
				t.Fatalf("console failure was discarded: %v", err)
			}
			if len(drivers[0].commands) != 1 || len(drivers[1].commands) != 0 || !drivers[1].released {
				t.Fatalf("unexpected retry or blocked healthy shard: commands=(%v,%v), healthy released=%v", drivers[0].commands, drivers[1].commands, drivers[1].released)
			}
		})
	}
}

func TestHotBarrierDrainRejectsStaleIdentityAndPendingPause(t *testing.T) {
	current := runtimePart{part: Part{BarrierInstance: "instance", BarrierSessionID: "session", BarrierShardID: "shard"}}
	receipt := runtimedriver.SnapshotBarrierReceipt{
		BarrierID: "hot-current-attempt", State: "released", ProducerInstanceID: "instance", SessionID: "session", ShardID: "shard",
	}
	if !hotBarrierDrained(current, receipt, receipt.BarrierID) {
		t.Fatal("drained current receipt was rejected")
	}
	for _, mutate := range []func(*runtimedriver.SnapshotBarrierReceipt){
		func(r *runtimedriver.SnapshotBarrierReceipt) { r.BarrierID = "hot-old-attempt" },
		func(r *runtimedriver.SnapshotBarrierReceipt) { r.ProducerInstanceID = "old-instance" },
		func(r *runtimedriver.SnapshotBarrierReceipt) { r.SessionID = "old-session" },
		func(r *runtimedriver.SnapshotBarrierReceipt) { r.ShardID = "other-shard" },
		func(r *runtimedriver.SnapshotBarrierReceipt) { r.InFlight = true },
		func(r *runtimedriver.SnapshotBarrierReceipt) { r.Busy = true },
		func(r *runtimedriver.SnapshotBarrierReceipt) { r.Code = "PAUSE_RESTORE_FAILED" },
	} {
		invalid := receipt
		mutate(&invalid)
		if hotBarrierDrained(current, invalid, receipt.BarrierID) {
			t.Fatalf("unverified recovery accepted: %#v", invalid)
		}
	}
}

func TestHotBarrierWaitPreservesPauseRecoveryFailure(t *testing.T) {
	for name, wait := range map[string]func(context.Context, runtimePart, string) error{
		"release": waitHotBarrierReleased,
		"cleanup": waitHotBarrierDrained,
	} {
		t.Run(name, func(t *testing.T) {
			driver := &hotBarrierDriver{harness: &hotBarrierHarness{}, receipt: runtimedriver.SnapshotBarrierReceipt{
				BarrierID: "hot-failed-pause", State: "released", Busy: true,
				Code: "PAUSE_RESTORE_FAILED", Message: "world pause recovery is not confirmed",
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			err := wait(ctx, runtimePart{driver: driver}, driver.receipt.BarrierID)
			var failure *runtimedriver.SnapshotBarrierError
			if !errors.Is(err, ErrBarrierFailed) || !errors.As(err, &failure) || failure.Code != "PAUSE_RESTORE_FAILED" {
				t.Fatalf("recovery failure was reduced to a generic timeout: %v", err)
			}
		})
	}
}
