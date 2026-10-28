package jobs

import (
	"testing"

	"dont/shared"
)

func TestRuntimeModePlanPersistsWithoutReplacingOtherJobProgress(t *testing.T) {
	_, store := newTestJobService(t)
	job, _, err := store.Create("room.restart", "room", "", []TargetSpec{{ID: "master", Name: "Master"}})
	if err != nil {
		t.Fatal(err)
	}
	plan := []RuntimeModeSelection{{RoomID: "room", WorldID: "master", Action: "restart", Mode: shared.RuntimePerformanceModeLuaJIT, Source: "running", WasRunning: true}}
	if err = store.SaveRuntimeModePlan(job.ID, "room", plan); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.MarkRunning(job.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.UpdateProgress(job.ID, 25, "stopping"); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get(job.ID)
	if err != nil || loaded.Progress != 25 || len(loaded.RuntimeModes) != 1 || loaded.RuntimeModes[0].Mode != shared.RuntimePerformanceModeLuaJIT {
		t.Fatalf("job=%+v err=%v", loaded, err)
	}
	if err = store.SaveRuntimeModePlan(job.ID, "room", plan); err != nil {
		t.Fatal(err)
	}
	plan[0].Mode = shared.RuntimePerformanceModeGame
	if err = store.SaveRuntimeModePlan(job.ID, "room", plan); err == nil {
		t.Fatal("immutable mode plan changed")
	}
}

func TestRuntimeModeRetryMayFillMissingPlacementButCannotChangeResolvedMode(t *testing.T) {
	_, store := newTestJobService(t)
	job, _, err := store.Create("room.start", "room", "", []TargetSpec{{ID: "master", Name: "Master"}})
	if err != nil {
		t.Fatal(err)
	}
	plan := []RuntimeModeSelection{{RoomID: "room", WorldID: "master", Action: "start", Mode: shared.RuntimePerformanceModeLuaJIT, Source: "saved", Error: &Error{Code: "AGENT_OFFLINE"}}}
	if err := store.SaveRuntimeModePlan(job.ID, "room", plan); err != nil {
		t.Fatal(err)
	}
	plan[0].TargetID, plan[0].InstallationID, plan[0].TopologyRevision = "local", "default", "v1"
	plan[0].Error = nil
	if err := store.SaveRuntimeModePlan(job.ID, "room", plan); err != nil {
		t.Fatal(err)
	}
	plan[0].Error = &Error{Code: "RUNTIME_MODE_UNAVAILABLE"}
	if err := store.SaveRuntimeModePlan(job.ID, "room", plan); err != nil {
		t.Fatal(err)
	}
	plan[0].Mode = shared.RuntimePerformanceModeGame
	if err := store.SaveRuntimeModePlan(job.ID, "room", plan); err == nil {
		t.Fatal("errored resolved mode could be replaced")
	}
	plan[0].Mode = shared.RuntimePerformanceModeLuaJIT
	plan[0].TargetID = "agent:other"
	if err := store.SaveRuntimeModePlan(job.ID, "room", plan); err == nil {
		t.Fatal("resolved placement could be replaced")
	}
}
