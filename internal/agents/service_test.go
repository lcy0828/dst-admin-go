package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dont/internal/jobs"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func newAgentTestService(t *testing.T) (*Service, *Store, *jobs.Service, *MemoryTransport) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "agent_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobStore := jobs.NewStore(db, "agent_test_")
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	transport := NewMemoryTransport()
	service, err := NewService(store, jobService, transport)
	if err != nil {
		t.Fatal(err)
	}
	return service, store, jobService, transport
}

func TestAgentSyncOfflineProtectionAndForget(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	items, available, err := service.Agents()
	if err != nil || !available || len(items) != 2 {
		t.Fatalf("agents=%#v available=%v err=%v", items, available, err)
	}
	primary, err := service.Agent("agent-primary")
	if err != nil || primary.Status != StatusOnline || primary.Metrics.CPUCount != 8 || primary.Version == "" {
		t.Fatalf("primary=%#v err=%v", primary, err)
	}
	if err := service.Forget(primary.ID); !errors.Is(err, ErrAgentOnline) {
		t.Fatalf("forget online error=%v", err)
	}
	if _, err := service.RunCommand("agent-offline", CommandInput{Action: ActionSystemRefresh, TimeoutSeconds: 30}); !errors.Is(err, ErrAgentOffline) {
		t.Fatalf("offline command error=%v", err)
	}
	if err := service.Forget("agent-offline"); err != nil {
		t.Fatal(err)
	}
	items, _, _ = service.Agents()
	if len(items) != 1 || items[0].ID != "agent-primary" {
		t.Fatalf("forgotten offline snapshots must stay removed, got %#v", items)
	}
}

func TestAgentCommandsPersistSuccessAndFailure(t *testing.T) {
	service, _, jobService, _ := newAgentTestService(t)
	job, err := service.RunCommand("agent-primary", CommandInput{Action: ActionSystemRefresh, TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	completed := waitAgentJob(t, jobService, job.ID)
	if completed.Status != jobs.StatusSucceeded {
		t.Fatalf("success job=%#v", completed)
	}
	job, err = service.RunCommand("agent-primary", CommandInput{Action: ActionDiskInspect, TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	completed = waitAgentJob(t, jobService, job.ID)
	if completed.Status != jobs.StatusFailed || completed.Targets[0].Error == nil {
		t.Fatalf("failure job=%#v", completed)
	}
	commands, err := service.Commands(CommandFilter{AgentID: "agent-primary", Limit: 25})
	if err != nil || commands.Total != 2 || commands.Items[0].Status != CommandFailed || commands.Items[1].Status != CommandSucceeded {
		t.Fatalf("commands=%#v err=%v", commands, err)
	}
	if !strings.Contains(commands.Items[0].Error, "磁盘检查失败") {
		t.Fatalf("failure detail=%#v", commands.Items[0])
	}
	detail, err := service.Command(commands.Items[0].ID)
	if err != nil || detail.ID != commands.Items[0].ID || detail.Status != CommandFailed {
		t.Fatalf("command detail=%#v err=%v", detail, err)
	}
	filtered, err := service.Commands(CommandFilter{Query: "disk", Status: CommandFailed, StartAt: utcAgentTimePointer(time.Now().Add(-time.Hour)), EndAt: utcAgentTimePointer(time.Now().Add(time.Hour)), Limit: 25})
	if err != nil || filtered.Total != 1 || filtered.Items[0].ID != detail.ID {
		t.Fatalf("filtered commands=%#v err=%v", filtered, err)
	}
}

func TestAgentSecurityMasksAndRotatesOnce(t *testing.T) {
	service, _, _, transport := newAgentTestService(t)
	before, _ := transport.CurrentKey()
	status, err := service.Security()
	if err != nil || !status.Available || !status.Configured || strings.Contains(status.MaskedKey, before) || len(status.Fingerprint) != 64 {
		t.Fatalf("security=%#v err=%v", status, err)
	}
	if _, err := service.RotateKey(context.Background(), RotateKeyInput{Confirmation: "yes"}); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("bad confirmation error=%v", err)
	}
	result, err := service.RotateKey(context.Background(), RotateKeyInput{Confirmation: "ROTATE AGENT KEY"})
	if err != nil || result.NewKey == "" || result.NewKey == before || len(result.Fingerprint) != 64 {
		t.Fatalf("rotation=%#v err=%v", result, err)
	}
	after, err := service.Security()
	if err != nil || after.RotatedAt == nil || after.Fingerprint != result.Fingerprint || strings.Contains(after.MaskedKey, result.NewKey) {
		t.Fatalf("after=%#v err=%v", after, err)
	}
}

func TestAgentInterruptedCommandRecovery(t *testing.T) {
	service, store, jobService, transport := newAgentTestService(t)
	now := time.Now().UTC()
	command := Command{ID: "00000000-0000-0000-0000-000000000001", AgentID: "agent-primary", AgentName: "Node", Action: ActionSystemRefresh, Status: CommandRunning, CreatedAt: now}
	if err := store.CreateCommand(command); err != nil {
		t.Fatal(err)
	}
	if err := store.StartCommand(command.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(store, jobService, transport); err != nil {
		t.Fatal(err)
	}
	commands, err := service.Commands(CommandFilter{Limit: 25})
	if err != nil || len(commands.Items) != 1 || commands.Items[0].Status != CommandFailed || !strings.Contains(commands.Items[0].Error, "服务重启") {
		t.Fatalf("commands=%#v err=%v", commands, err)
	}
}

func waitAgentJob(t *testing.T, service *jobs.Service, id string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := service.Get(id)
		if err == nil && (job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusFailed || job.Status == jobs.StatusCanceled) {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("agent job %s did not finish", id)
	return jobs.Job{}
}

func utcAgentTimePointer(value time.Time) *time.Time { utc := value.UTC(); return &utc }
