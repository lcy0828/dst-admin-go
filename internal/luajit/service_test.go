package luajit

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/jobs"
	"dont/internal/operationlease"
	"dont/shared"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type nodeOwnedTargets struct {
	target   agents.RuntimeTarget
	requests chan shared.RuntimeOperationRequest
}

func (f *nodeOwnedTargets) RuntimeTargets() ([]agents.RuntimeTarget, error) {
	return []agents.RuntimeTarget{f.target}, nil
}
func (f *nodeOwnedTargets) ExecuteRuntime(_ context.Context, id string, request shared.RuntimeOperationRequest, _ int) (agents.RuntimeExecutionResult, error) {
	f.requests <- request
	r := shared.LuaJITRelease{ID: request.LuaJIT.ReleaseID, Version: "3.0.0"}
	if request.LuaJIT.Release != nil {
		r = *request.LuaJIT.Release
	}
	if request.Action == shared.RuntimeActionLuaJITDownload {
		r.ID = request.LuaJIT.SHA256
	}
	r.SHA256 = r.ID
	return agents.RuntimeExecutionResult{Result: shared.RuntimeOperationResult{Outcome: shared.RuntimeOutcomeConfirmed, LuaJITRelease: &r, LuaJIT: &shared.RuntimePerformanceReport{CanEnable: true, PackageVersion: r.Version}}}, nil
}
func nodeService(t *testing.T) (*Service, *nodeOwnedTargets) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	jobStore := jobs.NewStore(db, "luajit_test_")
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	leases := operationlease.NewService(db, "luajit_test_")
	if err := leases.Migrate(); err != nil {
		t.Fatal(err)
	}
	local, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transfers, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	targets := &nodeOwnedTargets{target: agents.RuntimeTarget{ID: "agent:node", Online: true, OS: "linux", Arch: "amd64", Capabilities: []string{"runtime.luajit.v2"}, Installations: []agents.RuntimeInstallation{{ID: "second", ServerMode: "64"}}}, requests: make(chan shared.RuntimeOperationRequest, 4)}
	return NewService(local, transfers, targets, jobService, leases), targets
}
func finishLuaJITJob(t *testing.T, s *Service, job jobs.Job) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := s.jobs.Get(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == jobs.StatusSucceeded {
			return
		}
		if got.Status == jobs.StatusFailed {
			t.Fatalf("job failed: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not finish")
}
func TestRemoteInstallDoesNotRequireControllerPackage(t *testing.T) {
	s, targets := nodeService(t)
	id := strings.Repeat("a", 64)
	job, err := s.SubmitInstall("agent:node", "second", id, "")
	if err != nil {
		t.Fatal(err)
	}
	finishLuaJITJob(t, s, job)
	r := <-targets.requests
	if r.InstallationID != "second" || r.LuaJIT.ReleaseID != id || r.LuaJIT.Release != nil || r.LuaJIT.DownloadPath != "" || r.LuaJIT.DownloadToken != "" {
		t.Fatalf("default install relayed a controller package: %+v", r.LuaJIT)
	}
	for _, store := range []*Store{s.runtimeStore, s.transfers} {
		files, err := os.ReadDir(store.root)
		if err != nil || len(files) != 0 {
			t.Fatalf("remote install wrote controller storage: %v %v", files, err)
		}
	}
	targets.target.Online = false
	if _, err := s.SubmitInstall("agent:node", "second", id, ""); err == nil {
		t.Fatal("offline node accepted")
	}
	if _, err := s.SubmitInstall("agent:missing", "second", id, ""); err == nil {
		t.Fatal("missing node accepted")
	}
}
func TestDirectDownloadIsDispatchedToSelectedRuntime(t *testing.T) {
	s, targets := nodeService(t)
	digest := strings.Repeat("b", 64)
	job, err := s.ImportURL("agent:node", "second", "https://packages.invalid/runtime.zip", digest)
	if err != nil {
		t.Fatal(err)
	}
	finishLuaJITJob(t, s, job)
	r := <-targets.requests
	if r.Action != shared.RuntimeActionLuaJITDownload || r.InstallationID != "second" || r.LuaJIT.SourceURL != "https://packages.invalid/runtime.zip" || r.LuaJIT.SHA256 != digest || r.LuaJIT.DownloadToken != "" {
		t.Fatalf("unexpected request: %+v", r)
	}
}
func TestControllerTransferRequiresExplicitSource(t *testing.T) {
	s, targets := nodeService(t)
	store, release, _ := fixtureStore(t)
	s.transfers = store
	job, err := s.SubmitInstall("agent:node", "second", release.ID, "controller")
	if err != nil {
		t.Fatal(err)
	}
	finishLuaJITJob(t, s, job)
	r := <-targets.requests
	if r.LuaJIT.Release == nil || r.LuaJIT.ReleaseID != "" || r.LuaJIT.DownloadPath != "/luajit-packages/"+release.ID || r.LuaJIT.DownloadToken == "" {
		t.Fatalf("missing explicit transfer: %+v", r.LuaJIT)
	}
	if _, err := store.Open(release.ID, r.LuaJIT.DownloadToken); !os.IsPermission(err) {
		t.Fatal("completed transfer grant was retained")
	}
}
func TestCatalogReadDoesNotMaterializeBundledPackage(t *testing.T) {
	s, targets := nodeService(t)
	targets.target.ID = "local"
	catalog, err := s.Catalog("local")
	if err != nil || len(catalog.Installations) != 1 || len(catalog.Installations[0].Releases) == 0 || len(catalog.Transfers) != 0 {
		t.Fatalf("catalog: %+v %v", catalog, err)
	}
	files, err := os.ReadDir(s.runtimeStore.root)
	if err != nil || len(files) != 0 {
		t.Fatalf("catalog wrote packages: %v %v", files, err)
	}
}
