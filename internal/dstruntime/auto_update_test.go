package dstruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/shared"
)

type maintenanceTestCatalog struct{ runtimeTestCatalog }

func (c maintenanceTestCatalog) List() ([]rooms.Room, error) { return []rooms.Room{c.room}, nil }

type maintenanceTestRuntime struct {
	mu      sync.Mutex
	busy    bool
	busyErr error
	calls   chan string
}

type observationMaintenanceRuntime struct {
	ensure func() (shared.RuntimeLuaResult, error)
}

func (r *observationMaintenanceRuntime) EnsureLuaRuntime(context.Context, string, string, shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error) {
	return r.ensure()
}

func TestMaintenanceReportsRetainInstallationDuringProgressAndErrors(t *testing.T) {
	for _, state := range []InstallState{InstallStateMissing, InstallStateInstalled, InstallStateOutdated, InstallStateInvalid} {
		for _, problem := range []struct {
			name, reportState string
			err               error
		}{
			{"room busy", "updating", operationlease.ErrBusy},
			{"runtime deferred", "updating", ErrRuntimeRefreshDeferred},
			{"node disconnected", "failed", errors.New("Agent disconnected")},
		} {
			t.Run(string(state)+"/"+problem.name, func(t *testing.T) {
				_, catalog, _ := newRuntimeTestManager(t)
				version := "2.4.11"
				if state == InstallStateMissing {
					version = ""
				}
				runtime := &observationMaintenanceRuntime{ensure: func() (shared.RuntimeLuaResult, error) {
					return shared.RuntimeLuaResult{State: string(state), Version: version}, nil
				}}
				updater := NewAutoUpdater(maintenanceTestCatalog{catalog}, runtime)
				roomID, worldID := catalog.room.ID, catalog.worlds[0].ID
				if _, err := updater.EnsureWorld(context.Background(), roomID, worldID, false); err != nil {
					t.Fatal(err)
				}
				runtime.ensure = func() (shared.RuntimeLuaResult, error) {
					report, _ := updater.Report(roomID, worldID)
					if report.State != "updating" || report.InstallationState != string(state) || report.Version != version {
						t.Fatalf("progress erased confirmed observation: %+v", report)
					}
					return shared.RuntimeLuaResult{}, problem.err
				}
				if _, err := updater.EnsureWorld(context.Background(), roomID, worldID, false); !errors.Is(err, problem.err) {
					t.Fatal(err)
				}
				report, _ := updater.Report(roomID, worldID)
				if report.State != problem.reportState || report.InstallationState != string(state) || report.Version != version {
					t.Fatalf("failed observation erased confirmed state: %+v", report)
				}
				// An actual observation of removal must clear the prior version.
				runtime.ensure = func() (shared.RuntimeLuaResult, error) {
					return shared.RuntimeLuaResult{State: string(InstallStateMissing)}, nil
				}
				if _, err := updater.EnsureWorld(context.Background(), roomID, worldID, false); err != nil {
					t.Fatal(err)
				}
				report, _ = updater.Report(roomID, worldID)
				if report.State != "disabled" || report.Version != "" || report.InstallationState != string(InstallStateMissing) {
					t.Fatalf("confirmed removal did not clear installation: %+v", report)
				}
			})
		}
	}
}

func (r *maintenanceTestRuntime) EnsureLuaRuntime(_ context.Context, _, worldID string, request shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls <- worldID
	if r.busy && worldID == rooms.EncodeID("Master") {
		r.busy = false
		if r.busyErr != nil {
			return shared.RuntimeLuaResult{Deferred: true}, r.busyErr
		}
		return shared.RuntimeLuaResult{}, operationlease.ErrBusy
	}
	return shared.RuntimeLuaResult{Version: request.Version, State: "installed", Active: true, Message: "updated"}, nil
}

func TestAutoUpdateCoalescesSignalsRetriesBusyWorldAndStaysIdle(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"operation lease busy", nil},
		{"room maintenance deferred", ErrRuntimeRefreshDeferred},
	} {
		t.Run(test.name, func(t *testing.T) { testAutoUpdateRetriesBusyWorld(t, test.err) })
	}
}

func testAutoUpdateRetriesBusyWorld(t *testing.T, busyErr error) {
	_, catalog, _ := newRuntimeTestManager(t)
	runtime := &maintenanceTestRuntime{busy: true, busyErr: busyErr, calls: make(chan string, 16)}
	updater := NewAutoUpdater(maintenanceTestCatalog{catalog}, runtime)
	for i := 0; i < 100; i++ {
		updater.Wake()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); updater.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	next := func(timeout time.Duration) string {
		t.Helper()
		select {
		case world := <-runtime.calls:
			return world
		case <-time.After(timeout):
			t.Fatal("maintenance did not run")
			return ""
		}
	}
	first, second := next(time.Second), next(time.Second)
	if first != rooms.EncodeID("Master") || second != rooms.EncodeID("Caves") {
		t.Fatalf("busy world blocked other worlds: %s,%s", first, second)
	}
	if world := next(2 * time.Second); world != rooms.EncodeID("Master") {
		t.Fatalf("retried world=%s", world)
	}
	select {
	case extra := <-runtime.calls:
		t.Fatalf("coalesced events or idle loop repeated %s", extra)
	case <-time.After(60 * time.Millisecond):
	}
	updater.Wake()
	next(time.Second)
	next(time.Second)
}
