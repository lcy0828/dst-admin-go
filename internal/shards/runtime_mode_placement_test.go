package shards

import (
	"context"
	"errors"
	"testing"

	"dont/internal/agents"
	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"
)

func TestRuntimeModePlanResolvesLegacyInstallationAndPinsConfiguredDefault(t *testing.T) {
	for _, installation := range []string{"default", "steam-main"} {
		for _, mode := range []shared.RuntimePerformanceMode{"", shared.RuntimePerformanceModeLuaJIT, shared.RuntimePerformanceModeArenaGC} {
			t.Run(installation+"/"+string(mode), func(t *testing.T) {
				o, control, prefs, plans := modeOperations(t)
				for _, world := range []string{"Master", "Caves"} {
					control.running["summer_2026/"+world] = true
					control.modes[world] = shared.RuntimePerformanceModeLuaJIT
				}
				applied := topology.ExecutionPlacement{AppliedTargetID: "local", Revision: "v1"}
				resolved := applied
				resolved.AppliedInstallationID = installation
				resolved.Target = agents.RuntimeTarget{ID: "local", Performance: &shared.RuntimePerformanceReport{
					Status: shared.RuntimePerformanceReady, CanEnable: true, PackageVersion: "3.4.0",
					SupportedModes: []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame, shared.RuntimePerformanceModeLuaJIT, shared.RuntimePerformanceModeArenaGC},
				}}
				resolver := &fakePlacementResolver{applied: applied, resolved: resolved}
				if err := o.ConfigureDistributed(resolver, &fakeRemoteExecutor{}, &fakeLeaseService{}); err != nil {
					t.Fatal(err)
				}
				roomID := rooms.EncodeID("summer_2026")
				if _, err := o.RequireRuntimeMode(context.Background(), roomID, nil, shared.RuntimePerformanceModeLuaJIT); err != nil {
					t.Fatal(err)
				}
				room, worlds, err := o.resolvePlan(roomID, nil)
				if err != nil {
					t.Fatal(err)
				}
				ctx := WithOperationAudit(context.Background(), OperationAuditMetadata{JobID: "job"})
				options := PlanOptions{RuntimeMode: mode}
				for attempt := 0; attempt < 2; attempt++ {
					plan, err := o.resolveRuntimeModePlan(ctx, ActionRestart, room, worlds, options)
					if err != nil || len(plan) != 2 {
						t.Fatalf("attempt=%d plan=%+v error=%v", attempt, plan, err)
					}
					for _, selection := range plan {
						want := mode
						if want == "" {
							want = shared.RuntimePerformanceModeLuaJIT
							if selection.Source != "running" {
								t.Fatalf("inheritance source=%q", selection.Source)
							}
						}
						if selection.Error != nil || selection.Mode != want || selection.InstallationID != installation {
							t.Fatalf("selection=%+v", selection)
						}
					}
				}
				if len(prefs.values) != 0 || len(plans.values) != 2 || len(control.calls) != 0 {
					t.Fatalf("planning mutated preferences or game: prefs=%+v plans=%+v calls=%v", prefs.values, plans.values, control.calls)
				}
				resolver.resolved.AppliedInstallationID = "another-installation"
				if _, err := o.resolveRuntimeModePlan(ctx, ActionRestart, room, worlds, options); !errors.Is(err, ErrInvalidRuntimeMode) {
					t.Fatalf("retry silently followed a changed default: %v", err)
				}
			})
		}
	}
}

func TestRuntimeModeResolutionFailureNeverStopsWorldsOrFallsBack(t *testing.T) {
	for _, test := range []struct {
		name     string
		resolved topology.ExecutionPlacement
		err      error
	}{
		{name: "missing", err: agents.ErrRuntimeNotConfigured},
		{name: "target changed", resolved: topology.ExecutionPlacement{AppliedTargetID: "agent:other", AppliedInstallationID: "default", Revision: "v1"}},
		{name: "revision changed", resolved: topology.ExecutionPlacement{AppliedTargetID: "local", AppliedInstallationID: "default", Revision: "v2"}},
		{name: "installation missing", resolved: topology.ExecutionPlacement{AppliedTargetID: "local", Revision: "v1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			o, control, _, _ := modeOperations(t)
			resolver := &fakePlacementResolver{applied: topology.ExecutionPlacement{AppliedTargetID: "local", Revision: "v1"}, resolved: test.resolved}
			// Keep AppliedPlacement successful while resolving the installation fails.
			o.placements = &failingModePlacementResolver{fakePlacementResolver: resolver, resolveErr: test.err}
			results := runModeAction(t, o, ActionRestart, PlanOptions{RuntimeMode: shared.RuntimePerformanceModeLuaJIT})
			if len(results) != 2 || len(control.calls) != 0 {
				t.Fatalf("results=%+v calls=%v", results, control.calls)
			}
			for _, result := range results {
				if result.Status != jobs.StatusFailed || result.Error == nil {
					t.Fatalf("resolution failure ignored: %+v", result)
				}
			}
		})
	}
}

type failingModePlacementResolver struct {
	*fakePlacementResolver
	resolveErr error
}

func (r *failingModePlacementResolver) ResolveExecution(ctx context.Context, roomID, worldID string) (topology.ExecutionPlacement, error) {
	if r.resolveErr != nil {
		return topology.ExecutionPlacement{}, r.resolveErr
	}
	return r.fakePlacementResolver.ResolveExecution(ctx, roomID, worldID)
}
