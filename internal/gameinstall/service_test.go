package gameinstall

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/jobs"
	"dont/internal/operationlease"
	"dont/shared"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type fakeTargets struct {
	values   []agents.RuntimeTarget
	mu       sync.Mutex
	requests []shared.RuntimeOperationRequest
	failure  error
}

func (f *fakeTargets) RuntimeTargets() ([]agents.RuntimeTarget, error) { return f.values, nil }
func (f *fakeTargets) ExecuteRuntime(_ context.Context, target string, r shared.RuntimeOperationRequest, _ int) (agents.RuntimeExecutionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	if f.failure != nil {
		return agents.RuntimeExecutionResult{}, f.failure
	}
	return agents.RuntimeExecutionResult{Result: shared.RuntimeOperationResult{Outcome: shared.RuntimeOutcomeConfirmed, GameInstallation: &shared.GameInstallationReport{Installed: true, GameVersion: "747465", ServerPath: "/remote/" + r.InstallationID}}}, nil
}

func TestInstallJobWithoutRoomsKeepsExactRemoteIdentity(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Skipf("SQLite unavailable: %v", err)
	}
	defer db.Close()
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	store := jobs.NewStore(db, "game_install_test_")
	if err = store.Migrate(); err != nil {
		t.Fatal(err)
	}
	js, err := jobs.NewService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	leases := operationlease.NewService(db, "game_install_test_")
	if err = leases.Migrate(); err != nil {
		t.Fatal(err)
	}
	f := &fakeTargets{values: []agents.RuntimeTarget{{ID: "agent:node", Online: true, Capabilities: []string{"runtime.game-install.v1"}, Installations: []agents.RuntimeInstallation{{ID: "alternate"}}}}}
	s := NewService(f, js, leases)
	job, err := s.Submit("agent:node", "alternate", false, shared.GameInstallationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err = js.Get(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status != jobs.StatusSucceeded {
		t.Fatalf("job=%+v", job)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 {
		t.Fatal(f.requests)
	}
	r := f.requests[0]
	if r.InstallationID != "alternate" || r.Action != shared.RuntimeActionGameInstallationInstall || r.LeaseID == "" || r.FencingToken == 0 || r.GameInstallation == nil || r.GameInstallation.Path != "" {
		t.Fatalf("request=%+v", r)
	}
}
func TestCatalogIncludesEmptyUnassignedAndOfflineNodes(t *testing.T) {
	f := &fakeTargets{values: []agents.RuntimeTarget{
		{ID: "agent:new", Online: true, Capabilities: []string{"runtime.game-install.v1"}, Installations: []agents.RuntimeInstallation{{ID: "first"}, {ID: "second"}}},
		{ID: "agent:offline", Online: false, Installations: []agents.RuntimeInstallation{{ID: "default"}}},
		{ID: "agent:empty", Online: true},
	}}
	s := NewService(f, nil, nil)
	rows, err := s.Catalog(context.Background(), "")
	if err != nil || len(rows) != 4 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if len(f.requests) != 2 || f.requests[0].InstallationID == f.requests[1].InstallationID {
		t.Fatal("exact installations were not inspected", f.requests)
	}
	for _, r := range rows {
		if r.TargetID == "agent:offline" && (r.Installed || r.Error == "") {
			t.Fatal("offline node looked uninstalled/healthy")
		}
		if r.TargetID == "agent:empty" && (r.InstallationID != "" || r.Error == "") {
			t.Fatal("empty registry hidden")
		}
	}
}
func TestProbeKeepsNodeAndInstallationAndDoesNotFallback(t *testing.T) {
	f := &fakeTargets{values: []agents.RuntimeTarget{{ID: "agent:node", Online: true, Capabilities: []string{"runtime.game-install.v1"}, Installations: []agents.RuntimeInstallation{{ID: "alternate"}}}}}
	s := NewService(f, nil, nil)
	_, err := s.Probe(context.Background(), "agent:node", "alternate", "/remote/existing")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 1 || f.requests[0].InstallationID != "alternate" || f.requests[0].GameInstallation.Path != "/remote/existing" {
		t.Fatal(f.requests)
	}
	f.failure = errors.New("disconnected")
	if _, err = s.Probe(context.Background(), "agent:node", "alternate", "/remote/existing"); err == nil {
		t.Fatal("remote failure hidden")
	}
	if _, err = s.Probe(context.Background(), "agent:node", "missing", "/remote/existing"); !errors.Is(err, agents.ErrRuntimeInstallationNotRegistered) {
		t.Fatal("missing installation fell back", err)
	}
}
