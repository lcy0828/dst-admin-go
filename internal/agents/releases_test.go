package agents

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/jobs"
)

func TestReleaseStoreDetectsBinaryAndDrivesMemoryUpgrade(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "dst-admin-agent")
	command := exec.Command("go", "build", "-trimpath", "-ldflags", "-X=dont/agent.AgentVersion=9.9.9", "-o", binary, "../../agent/cmd/agent")
	command.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Agent fixture: %v\n%s", err, output)
	}
	file, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewReleaseStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release, err := store.Save("9.9.9", filepath.Base(binary), file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if release.OS != "linux" || release.Arch != "amd64" || release.Size < 1 || len(release.SHA256) != 64 {
		t.Fatalf("release=%#v", release)
	}
	latest, err := store.Latest("linux", "amd64")
	if err != nil || latest == nil || latest.ID != release.ID {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	token, err := store.IssueDownloadToken("agent-primary", release.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(release.ID); !errors.Is(err, ErrReleaseInUse) {
		t.Fatalf("delete active release error=%v", err)
	}
	openedRelease, download, err := store.OpenDownload(release.ID, token)
	if err != nil || openedRelease.ID != release.ID {
		t.Fatalf("download release=%#v err=%v", openedRelease, err)
	}
	_ = download.Close()
	store.RevokeDownloadToken(token)

	service, _, jobService, _ := newAgentTestService(t)
	if err := service.ConfigureReleaseStore(store); err != nil {
		t.Fatal(err)
	}
	agent, err := service.Agent("agent-primary")
	if err != nil || !agent.Update.Supported || !agent.Update.UpdateAvailable || agent.Update.LatestVersion != release.Version {
		t.Fatalf("agent update=%#v err=%v", agent.Update, err)
	}
	job, err := service.UpgradeAgent(agent.ID, AgentUpgradeInput{ReleaseID: release.ID})
	if err != nil {
		t.Fatal(err)
	}
	completed := waitAgentJob(t, jobService, job.ID)
	if completed.Status != jobs.StatusSucceeded {
		t.Fatalf("upgrade job=%#v", completed)
	}
	upgraded, err := service.Agent(agent.ID)
	if err != nil || upgraded.Version != release.Version || upgraded.Update.UpdateAvailable {
		t.Fatalf("upgraded=%#v err=%v", upgraded, err)
	}
	if err := store.Delete(release.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCompareAgentVersions(t *testing.T) {
	for _, value := range []struct {
		left, right string
		expected    int
	}{
		{left: "2.10.0", right: "2.9.9", expected: 1},
		{left: "2.10.0", right: "2.10.0", expected: 0},
		{left: "2.10.0-rc1", right: "2.10.0", expected: -1},
	} {
		actual := compareAgentVersions(value.left, value.right)
		if actual != value.expected {
			t.Fatalf("compare %s %s=%d expected=%d", value.left, value.right, actual, value.expected)
		}
	}
}

func TestWaitForAgentVersionDistinguishesTimeoutAndMismatch(t *testing.T) {
	service, _, _, transport := newAgentTestService(t)

	err := service.waitForAgentVersionWithin(context.Background(), "missing-agent", "9.9.9", 20*time.Millisecond, time.Millisecond)
	if !errors.Is(err, ErrUpgradeReconnectTimeout) || errors.Is(err, ErrUpgradeVersionMismatch) {
		t.Fatalf("missing Agent error=%v", err)
	}

	transport.mu.Lock()
	snapshot := transport.snapshots["agent-primary"]
	snapshot.Version = "9.9.8"
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()
	err = service.waitForAgentVersionWithin(context.Background(), "agent-primary", "9.9.9", 20*time.Millisecond, time.Millisecond)
	if !errors.Is(err, ErrUpgradeVersionMismatch) || errors.Is(err, ErrUpgradeReconnectTimeout) {
		t.Fatalf("mismatched Agent error=%v", err)
	}
}

func TestAgentUpgradeReservationAllowsOnlyOneActiveJobPerAgent(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	if !service.beginAgentUpgrade("agent-primary") {
		t.Fatal("first upgrade reservation was rejected")
	}
	if service.beginAgentUpgrade("agent-primary") {
		t.Fatal("second upgrade reservation was accepted")
	}
	if !service.beginAgentUpgrade("agent-other") {
		t.Fatal("a different Agent should have an independent reservation")
	}
	service.finishAgentUpgrade("agent-primary")
	if !service.beginAgentUpgrade("agent-primary") {
		t.Fatal("completed upgrade reservation was not released")
	}
}
