package runtimedriver

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/shards"
	"dont/shared"
)

type nativeLifecycleControl struct {
	statuses []shards.RuntimeStatus
	index    int
	starts   int
	stops    int
}

func (control *nativeLifecycleControl) Status(context.Context, string, string) (shards.RuntimeStatus, error) {
	if len(control.statuses) == 0 {
		return shards.RuntimeStatus{}, errors.New("missing status")
	}
	index := control.index
	if index >= len(control.statuses) {
		index = len(control.statuses) - 1
	} else {
		control.index++
	}
	return control.statuses[index], nil
}

func (control *nativeLifecycleControl) Start(context.Context, string, string) error {
	control.starts++
	return nil
}

func (control *nativeLifecycleControl) Stop(context.Context, string, string) error {
	control.stops++
	return nil
}

func (*nativeLifecycleControl) Send(context.Context, string, string, string) error { return nil }

func newNativeLifecycleDriver(t *testing.T, control NativeControl) *Native {
	t.Helper()
	driver, err := NewNative(t.TempDir(), control)
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

func nativeLifecycleTarget() Target {
	return Target{TargetID: "local", InstallationID: "local", Cluster: "room", Shard: "Master"}
}

func TestNativeStopWaitsForConfirmedSessionExit(t *testing.T) {
	control := &nativeLifecycleControl{statuses: []shards.RuntimeStatus{
		{State: shards.RuntimeRunning, SessionExists: true},
		{State: shards.RuntimeRunning, SessionExists: true},
		{State: shards.RuntimeStopped, SessionExists: false},
	}}
	driver := newNativeLifecycleDriver(t, control)
	result, err := driver.ExecuteShard(context.Background(), nativeLifecycleTarget(), Operation{ID: "stop-1"}, shared.ShardActionStop, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if control.stops != 1 || result.Status.State != string(shards.RuntimeStopped) || result.Status.SessionExists {
		t.Fatalf("stops=%d result=%#v", control.stops, result)
	}
}

func TestNativeStopFailsWhenSessionDoesNotExit(t *testing.T) {
	control := &nativeLifecycleControl{statuses: []shards.RuntimeStatus{{State: shards.RuntimeRunning, SessionExists: true}}}
	driver := newNativeLifecycleDriver(t, control)
	result, err := driver.ExecuteShard(context.Background(), nativeLifecycleTarget(), Operation{ID: "stop-2"}, shared.ShardActionStop, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	if control.stops != 1 || result.Status.State != string(shards.RuntimeRunning) || !result.Status.SessionExists {
		t.Fatalf("stops=%d result=%#v", control.stops, result)
	}
}

func TestNativeRestartWaitsForExitBeforeStarting(t *testing.T) {
	control := &nativeLifecycleControl{statuses: []shards.RuntimeStatus{
		{State: shards.RuntimeRunning, SessionExists: true},
		{State: shards.RuntimeStopped, SessionExists: false},
		{State: shards.RuntimeRunning, SessionExists: true},
	}}
	driver := newNativeLifecycleDriver(t, control)
	result, err := driver.ExecuteShard(context.Background(), nativeLifecycleTarget(), Operation{ID: "restart-1"}, shared.ShardActionRestart, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if control.stops != 1 || control.starts != 1 || result.Status.State != string(shards.RuntimeRunning) {
		t.Fatalf("starts=%d stops=%d result=%#v", control.starts, control.stops, result)
	}
}
