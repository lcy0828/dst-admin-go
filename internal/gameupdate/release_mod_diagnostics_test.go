package gameupdate

import (
	"context"
	"errors"
	"testing"

	"dont/internal/moddiagnostics"
)

type diagnosticObserver struct {
	t          *testing.T
	runtime    *coordinatorRuntime
	previews   int
	captures   int
	captureErr error
}

func (d *diagnosticObserver) Preflight(_ context.Context, scopes []moddiagnostics.Scope) []moddiagnostics.Report {
	d.previews++
	if len(scopes) != 4 {
		d.t.Fatal("scope lost a shard", scopes)
	}
	return []moddiagnostics.Report{{Status: "observed_problem", Findings: []moddiagnostics.Finding{{Code: "DECLARED_NOT_DST", Kind: "observed"}}}}
}
func (d *diagnosticObserver) Capture(_ context.Context, id string, scopes []moddiagnostics.Scope) error {
	d.captures++
	if id == "" || len(scopes) != 4 {
		d.t.Fatal("capture lost update identity", id, scopes)
	}
	if len(d.runtime.mutationEvents()) != 0 {
		d.t.Fatal("baseline captured after a game mutation")
	}
	return d.captureErr
}

func TestModAdvisoriesDoNotChangePlanHashReadinessOrRetryBaseline(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	d := &diagnosticObserver{t: t, runtime: f.runtime}
	f.coordinator.ConfigureModDiagnostics(d)
	plan, err := f.coordinator.Preview(context.Background(), ReleasePreviewRequest{Policy: ReleasePolicyInput{LoadConfirmation: ReleaseLoadConfirmationNone, TimeoutSeconds: 30}})
	if err != nil || !plan.Ready || len(plan.ModDiagnostics) != 1 {
		t.Fatal(plan, err)
	}
	hash, _ := calculateReleasePlanHash(plan)
	plan.ModDiagnostics[0].Status = "insufficient_evidence"
	same, _ := calculateReleasePlanHash(plan)
	if same != hash {
		t.Fatal("advisory data changed the execution hash")
	}
	f.runtime.updateErrors["agent:node-a\x00primary"] = errors.New("Steam unavailable")
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "release-diagnostics", Plan: plan})
	if err == nil || (value.Stage != ReleaseStageFailed && value.Stage != ReleaseStageRecoveryRequired) || d.captures != 1 {
		t.Fatal(value.Stage, err, d.captures)
	}
	delete(f.runtime.updateErrors, "agent:node-a\x00primary")
	value, err = f.coordinator.Retry(context.Background(), value.ID)
	if err != nil || value.Stage != ReleaseStageSucceeded || d.captures != 1 {
		t.Fatal("retry recaptured post-update observations", value.Stage, err, d.captures)
	}
	_, err = f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: value.ID, Plan: plan})
	if err != nil || d.captures != 1 {
		t.Fatal("idempotent publish recaptured baseline", err)
	}
}

func TestAdvisoryPersistenceFailureDoesNotCancelProtectedGameUpdate(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	d := &diagnosticObserver{t: t, runtime: f.runtime, captureErr: errors.New("baseline database unavailable")}
	f.coordinator.ConfigureModDiagnostics(d)
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "release-diagnostics-warning", Plan: f.plan})
	if err != nil || value.Stage != ReleaseStageSucceeded || value.ModDiagnosticsWarning != "BASELINE_SAVE_FAILED" {
		t.Fatal(value, err)
	}
	stored, err := f.coordinator.Get(value.ID)
	if err != nil || stored.ModDiagnosticsWarning != "BASELINE_SAVE_FAILED" {
		t.Fatal(stored, err)
	}
}
