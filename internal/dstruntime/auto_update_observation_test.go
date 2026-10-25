package dstruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"dont/shared"
)

type scopedMaintenanceRuntime struct {
	scope  atomic.Value
	ensure func(context.Context, shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error)
}

func (r *scopedMaintenanceRuntime) LuaRuntimeScope(string, string) (string, error) {
	return r.scope.Load().(string), nil
}

func (r *scopedMaintenanceRuntime) EnsureLuaRuntime(ctx context.Context, _, _ string, request shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error) {
	return r.ensure(ctx, request)
}

func TestMaintenanceCoalescesConcurrentChecksWithoutReplacingActiveReport(t *testing.T) {
	_, catalog, _ := newRuntimeTestManager(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	runtime := &scopedMaintenanceRuntime{ensure: func(ctx context.Context, _ shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
			return shared.RuntimeLuaResult{State: "installed", Version: RuntimeVersion, Active: true, Changed: true}, nil
		case <-ctx.Done():
			return shared.RuntimeLuaResult{}, ctx.Err()
		}
	}}
	runtime.scope.Store("node-a/default/boot-a")
	updater := NewAutoUpdater(maintenanceTestCatalog{catalog}, runtime)
	roomID, worldID := catalog.room.ID, catalog.worlds[0].ID
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := updater.EnsureWorld(ctx, roomID, worldID, true); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	before, _ := updater.Report(roomID, worldID)
	value, err := updater.EnsureWorld(ctx, roomID, worldID, false)
	if !errors.Is(err, ErrRuntimeRefreshDeferred) || !value.Deferred || calls.Load() != 1 {
		t.Fatalf("duplicate check was dispatched: %+v %v calls=%d", value, err, calls.Load())
	}
	if after, _ := updater.Report(roomID, worldID); after != before {
		t.Fatalf("duplicate busy response changed active report: before=%+v after=%+v", before, after)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if report, _ := updater.Report(roomID, worldID); report.State != "updated" || report.Version != RuntimeVersion {
		t.Fatalf("active check did not publish its result: %+v", report)
	}
}

func TestMaintenanceLateResponseAndRetryCannotOverwriteNewLocation(t *testing.T) {
	_, catalog, _ := newRuntimeTestManager(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	runtime := &scopedMaintenanceRuntime{ensure: func(ctx context.Context, _ shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
				return shared.RuntimeLuaResult{Deferred: true, State: "outdated", Version: "2.4.11"}, ErrRuntimeRefreshDeferred
			case <-ctx.Done():
				return shared.RuntimeLuaResult{}, ctx.Err()
			}
		}
		return shared.RuntimeLuaResult{State: "installed", Version: RuntimeVersion, Active: true, Changed: true}, nil
	}}
	runtime.scope.Store("node-a/default/boot-a")
	updater := NewAutoUpdater(maintenanceTestCatalog{catalog}, runtime)
	roomID, worldID := catalog.room.ID, catalog.worlds[0].ID
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type outcome struct {
		attempt *maintenanceUpdate
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		_, attempt, err := updater.ensureWorld(ctx, roomID, worldID, false)
		done <- outcome{attempt, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	runtime.scope.Store("node-b/default/boot-b")
	if _, ok := updater.Report(roomID, worldID); ok {
		t.Fatal("old node report survived a placement change")
	}
	if _, err := updater.EnsureWorld(ctx, roomID, worldID, true); err != nil {
		t.Fatal(err)
	}
	before, _ := updater.Report(roomID, worldID)
	close(release)
	old := <-done
	if !errors.Is(old.err, ErrRuntimeRefreshDeferred) {
		t.Fatal(old.err)
	}
	for _, state := range []string{"updating", "failed"} {
		if updater.recordRetry(roomID, worldID, old.attempt, state, "stale retry") {
			t.Fatal("stale scheduler still owns report")
		}
	}
	if after, _ := updater.Report(roomID, worldID); after != before || after.State != "updated" || after.Version != RuntimeVersion {
		t.Fatalf("old response replaced new observation: before=%+v after=%+v", before, after)
	}
}

func TestMaintenanceInstallationObservationStaysWithinLocation(t *testing.T) {
	for _, next := range []string{"node-b/default/boot-a", "node-a/other/boot-a", "node-a/default/boot-b", "node-a/default/boot-a/revision-b"} {
		t.Run(next, func(t *testing.T) {
			_, catalog, _ := newRuntimeTestManager(t)
			runtime := &scopedMaintenanceRuntime{ensure: func(context.Context, shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error) {
				return shared.RuntimeLuaResult{State: "missing"}, nil
			}}
			runtime.scope.Store("node-a/default/boot-a")
			updater := NewAutoUpdater(maintenanceTestCatalog{catalog}, runtime)
			roomID, worldID := catalog.room.ID, catalog.worlds[0].ID
			if _, err := updater.EnsureWorld(context.Background(), roomID, worldID, false); err != nil {
				t.Fatal(err)
			}
			runtime.scope.Store(next)
			if _, ok := updater.Report(roomID, worldID); ok {
				t.Fatal("old location report is still authoritative")
			}
			runtime.ensure = func(context.Context, shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error) {
				return shared.RuntimeLuaResult{Deferred: true}, ErrRuntimeRefreshDeferred
			}
			if _, err := updater.EnsureWorld(context.Background(), roomID, worldID, false); !errors.Is(err, ErrRuntimeRefreshDeferred) {
				t.Fatal(err)
			}
			if report, ok := updater.Report(roomID, worldID); !ok || report.InstallationState != "" || report.Version != "" {
				t.Fatalf("busy check inherited old installation: %+v ok=%t", report, ok)
			}
		})
	}
}

func TestMaintenanceNewAttemptOwnsRetryReport(t *testing.T) {
	_, catalog, _ := newRuntimeTestManager(t)
	runtime := &observationMaintenanceRuntime{ensure: func() (shared.RuntimeLuaResult, error) { return shared.RuntimeLuaResult{}, ErrRuntimeRefreshDeferred }}
	updater := NewAutoUpdater(maintenanceTestCatalog{catalog}, runtime)
	roomID, worldID := catalog.room.ID, catalog.worlds[0].ID
	_, old, _ := updater.ensureWorld(context.Background(), roomID, worldID, false)
	runtime.ensure = func() (shared.RuntimeLuaResult, error) {
		return shared.RuntimeLuaResult{State: "installed", Version: RuntimeVersion, Changed: true}, nil
	}
	if _, err := updater.EnsureWorld(context.Background(), roomID, worldID, true); err != nil {
		t.Fatal(err)
	}
	if updater.recordRetry(roomID, worldID, old, "failed", "old retry exhausted") {
		t.Fatal("old retry overwrote a newer check")
	}
	if report, _ := updater.Report(roomID, worldID); report.State != "updated" {
		t.Fatalf("new report was overwritten: %+v", report)
	}
}
