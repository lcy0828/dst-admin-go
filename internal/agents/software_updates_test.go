package agents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/softwareupdate"
	"dont/shared"
)

type softwareTestTransport struct {
	*MemoryTransport
	stateMu       sync.Mutex
	snapshot      softwareupdate.Snapshot
	requests      []shared.AgentUpgradeRequest
	failReconnect bool
}

type lostSoftwareAcknowledgement struct {
	*softwareTestTransport
	action   string
	rejected bool
}

func (transport *lostSoftwareAcknowledgement) ExecuteUpgrade(ctx context.Context, id string, request shared.AgentUpgradeRequest, timeout int) (AgentUpgradeExecutionResult, error) {
	if request.Action == transport.action && transport.rejected {
		return AgentUpgradeExecutionResult{Result: shared.AgentUpgradeResult{ProtocolVersion: request.ProtocolVersion, ReleaseID: request.ReleaseID}}, errors.New("other Agent work is running")
	}
	result, err := transport.softwareTestTransport.ExecuteUpgrade(ctx, id, request, timeout)
	if err == nil && request.Action == transport.action {
		return AgentUpgradeExecutionResult{}, errors.New("accepted operation; acknowledgement lost")
	}
	return result, err
}

func TestAgentSoftwareLostAcknowledgementObservesWithoutRepeatingCommands(t *testing.T) {
	for _, action := range []string{"update", "apply"} {
		t.Run(action, func(t *testing.T) {
			service, _, jobService, memory := newAgentTestService(t)
			memory.mu.Lock()
			record := memory.snapshots["agent-primary"]
			record.Capabilities = append(record.Capabilities, shared.AgentSoftwareUpdateCapability)
			memory.snapshots[record.ID] = record
			memory.mu.Unlock()
			transport := &lostSoftwareAcknowledgement{softwareTestTransport: &softwareTestTransport{MemoryTransport: memory, snapshot: softwareupdate.Snapshot{Supported: true, Ready: true, Platform: "linux-amd64"}}, action: action}
			transport.snapshot.Current.Version = "v1.2.2"
			service.transport = transport
			job, err := service.UpdateAgentSoftware("agent-primary", AgentSoftwareInput{Version: "v1.2.3", Confirmation: "v1.2.3"})
			if err != nil {
				t.Fatal(err)
			}
			finished := waitSoftwareAcknowledgementJob(t, jobService, job.ID)
			if finished.Status != jobs.StatusSucceeded {
				t.Fatalf("lost acknowledgement reported failure: %+v", finished)
			}
			transport.stateMu.Lock()
			defer transport.stateMu.Unlock()
			counts := map[string]int{}
			for _, request := range transport.requests {
				counts[request.Action]++
			}
			if counts["update"] != 1 || counts["apply"] != 1 {
				t.Fatalf("unsafe duplicate mutation: %v", counts)
			}
		})
	}
}

func waitSoftwareAcknowledgementJob(t *testing.T, service *jobs.Service, id string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err := service.Get(id)
		if err == nil && (job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusFailed) {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("acknowledgement observation did not finish")
	return jobs.Job{}
}

func TestAgentSoftwareExplicitRejectionFailsWithoutRepeatingApply(t *testing.T) {
	service, _, jobService, memory := newAgentTestService(t)
	memory.mu.Lock()
	record := memory.snapshots["agent-primary"]
	record.Capabilities = append(record.Capabilities, shared.AgentSoftwareUpdateCapability)
	memory.snapshots[record.ID] = record
	memory.mu.Unlock()
	transport := &lostSoftwareAcknowledgement{softwareTestTransport: &softwareTestTransport{MemoryTransport: memory, snapshot: softwareupdate.Snapshot{Supported: true, Ready: true, Platform: "linux-amd64"}}, action: "apply", rejected: true}
	transport.snapshot.Current.Version = "v1.2.2"
	service.transport = transport
	job, err := service.UpdateAgentSoftware("agent-primary", AgentSoftwareInput{Version: "v1.2.3", Confirmation: "v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	finished := waitAgentJob(t, jobService, job.ID)
	if finished.Status != jobs.StatusFailed || finished.Targets[0].Error == nil {
		t.Fatalf("explicit rejection was ignored: %+v", finished)
	}
}

func (t *softwareTestTransport) ExecuteUpgrade(ctx context.Context, id string, request shared.AgentUpgradeRequest, _ int) (AgentUpgradeExecutionResult, error) {
	if err := shared.ValidateAgentSoftwareRequest(request); err != nil {
		return AgentUpgradeExecutionResult{}, err
	}
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.requests = append(t.requests, request)
	switch request.Action {
	case "update":
		t.snapshot.Operation = &softwareupdate.Operation{ID: "1234567890abcdef1234567890abcdef", Version: request.Version, Phase: "prepared", Progress: 100}
	case "apply":
		t.snapshot.Operation.Phase = "restarting"
	case "status":
		if t.snapshot.Operation != nil && t.snapshot.Operation.Phase == "restarting" {
			if t.failReconnect {
				t.snapshot.Operation.Phase = "rolled_back"
				t.snapshot.Operation.Error = "new Agent did not reconnect; original restored"
			} else {
				t.snapshot.Current.Version = t.snapshot.Operation.Version
				t.snapshot.Operation.Phase = "succeeded"
			}
		}
	}
	data, err := json.Marshal(t.snapshot)
	return AgentUpgradeExecutionResult{Result: shared.AgentUpgradeResult{ProtocolVersion: shared.AgentUpgradeProtocolVersion, ReleaseID: request.ReleaseID, Software: data}}, err
}

func TestAgentSoftwareJobsApplyVerifiedUpdateAndReportRecovery(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "reconnect_failure"}[fail], func(t *testing.T) {
			service, _, jobService, memory := newAgentTestService(t)
			memory.mu.Lock()
			record := memory.snapshots["agent-primary"]
			record.Capabilities = append(record.Capabilities, shared.AgentSoftwareUpdateCapability)
			memory.snapshots[record.ID] = record
			memory.mu.Unlock()
			transport := &softwareTestTransport{MemoryTransport: memory, failReconnect: fail, snapshot: softwareupdate.Snapshot{Supported: true, Ready: true, Platform: "linux-amd64"}}
			transport.snapshot.Current.Version = "v1.2.2"
			service.transport = transport
			job, err := service.UpdateAgentSoftware("agent-primary", AgentSoftwareInput{Version: "v1.2.3", Confirmation: "v1.2.3", Source: "auto"})
			if err != nil {
				t.Fatal(err)
			}
			finished := waitAgentJob(t, jobService, job.ID)
			if fail {
				if finished.Status != jobs.StatusFailed || finished.Targets[0].Error == nil {
					t.Fatalf("failed reconnect was reported as success: %+v", finished)
				}
			} else if finished.Status != jobs.StatusSucceeded {
				t.Fatalf("update failed: %+v", finished)
			}
			transport.stateMu.Lock()
			defer transport.stateMu.Unlock()
			var updates, applies int
			for _, request := range transport.requests {
				if request.Action == "update" {
					updates++
				}
				if request.Action == "apply" {
					applies++
					if request.OperationID != "1234567890abcdef1234567890abcdef" {
						t.Fatal("wrong prepared operation applied")
					}
				}
			}
			if updates != 1 || applies != 1 {
				t.Fatalf("duplicate install: update %d apply %d", updates, applies)
			}
		})
	}
}

func TestAgentSoftwareCapabilitySupportsDockerAndWindowsWithoutClaimingLegacySupport(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	for _, osName := range []string{"linux", "darwin", "windows"} {
		agent := Agent{OS: osName, Arch: "amd64", Capabilities: []string{shared.AgentSoftwareUpdateCapability}, Details: map[string]interface{}{"deployment_profile": "container"}}
		if status := service.agentUpdateStatus(agent, nil); !status.Supported || status.Mode != AgentUpdateModeSelf {
			t.Fatalf("supervised Agent excluded: %+v", status)
		}
		agent.Capabilities = nil
		if status := service.agentUpdateStatus(agent, nil); status.Supported {
			t.Fatal("old Agent incorrectly advertised online updates")
		}
	}
}

type relayFixtureClient struct {
	payload   []byte
	release   softwareupdate.Release
	downloads int
}

func (c *relayFixtureClient) Latest(context.Context, string, string) (*softwareupdate.Release, error) {
	return &c.release, nil
}
func (c *relayFixtureClient) Checksum(context.Context, softwareupdate.Asset, string) ([]byte, error) {
	hash := sha256.Sum256(c.payload)
	return []byte(hex.EncodeToString(hash[:]) + "  " + c.release.Archive.Name), nil
}
func (c *relayFixtureClient) Download(_ context.Context, _ softwareupdate.Asset, _ string, writer io.Writer) error {
	c.downloads++
	_, err := writer.Write(c.payload)
	return err
}

func TestAgentSoftwareRelayIsBoundedSingleUseAndRevocable(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	client := &relayFixtureClient{payload: []byte("official archive")}
	client.release = softwareupdate.Release{Version: "v1.2.3", OnlineUpdate: true, Archive: softwareupdate.Asset{Name: "dst-admin-agent-update-linux-amd64.tar.gz", Size: int64(len(client.payload))}}
	service.softwareClient = client
	request, revoke, err := service.relaySoftwareRequest(context.Background(), Agent{OS: "linux", Arch: "amd64"}, shared.AgentUpgradeRequest{Action: "update", Source: "controller", Version: "v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	defer revoke()
	called := false
	writer := func(size int64) io.Writer {
		called = true
		if size != int64(len(client.payload)) {
			t.Fatal("wrong stream bound")
		}
		return io.Discard
	}
	if _, err := service.AgentSoftwareTransfer(context.Background(), request.ReleaseID, "wrong", writer); !errors.Is(err, ErrDownloadToken) || called {
		t.Fatal("invalid token started stream")
	}
	var received bytes.Buffer
	if _, err := service.AgentSoftwareTransfer(context.Background(), request.ReleaseID, request.DownloadToken, func(int64) io.Writer { return &received }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received.Bytes(), client.payload) || client.downloads != 1 {
		t.Fatal("relay changed archive")
	}
	if _, err := service.AgentSoftwareTransfer(context.Background(), request.ReleaseID, request.DownloadToken, writer); !errors.Is(err, ErrDownloadToken) {
		t.Fatal("single-use relay reused")
	}
	revoke()
	service.upgradeMu.Lock()
	count := len(service.softwareRelays)
	service.upgradeMu.Unlock()
	if count != 0 {
		t.Fatal("finished relay retained")
	}
}

func TestAgentSoftwareRequiresExactConfirmationAndOnlineCapability(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	if _, err := service.UpdateAgentSoftware("agent-primary", AgentSoftwareInput{Version: "v1.2.3", Confirmation: "other"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("confirmation mismatch accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := service.AgentSoftware(ctx, "agent-primary", true, false, "auto"); !errors.Is(err, ErrUpgradeUnsupported) {
		t.Fatal("legacy Agent received new command")
	}
}

type blockedSoftwareCheckClient struct {
	*relayFixtureClient
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (c *blockedSoftwareCheckClient) Latest(ctx context.Context, platform, source string) (*softwareupdate.Release, error) {
	if c.calls.Add(1) == 1 {
		close(c.entered)
	}
	select {
	case <-c.release:
		return c.relayFixtureClient.Latest(ctx, platform, source)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestAgentReleaseCheckCoalescesWithoutBlockingMachineSnapshots(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	client := &blockedSoftwareCheckClient{relayFixtureClient: &relayFixtureClient{release: softwareupdate.Release{Version: "v1.2.4"}}, entered: make(chan struct{}), release: make(chan struct{})}
	service.softwareClient = client
	service.softwareReleaseChecks = map[string]softwareupdate.Check{"linux-amd64": {Latest: &softwareupdate.Release{Version: "v1.2.3"}, CheckedAt: time.Now().Add(-time.Hour)}}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(client.release) }) }
	defer unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	checks := make(chan softwareupdate.Check, 2)
	go func() { checks <- service.checkAgentRelease(ctx, "linux-amd64", false) }()
	select {
	case <-client.entered:
	case <-ctx.Done():
		t.Fatal("release check did not start")
	}
	go func() { checks <- service.checkAgentRelease(ctx, "linux-amd64", false) }()
	snapshot := softwareupdate.Snapshot{Platform: "linux-amd64"}
	snapshot.Current.Version = "v1.2.2"
	read := make(chan softwareupdate.Snapshot, 1)
	go func() { read <- service.withControllerCheck(snapshot) }()
	select {
	case value := <-read:
		if value.Check.Latest == nil || value.Check.Latest.Version != "v1.2.3" || !value.Check.HasUpdate {
			t.Fatal("cached version missing during network check", value)
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary machine snapshot blocked behind network check")
	}
	unblock()
	for i := 0; i < 2; i++ {
		select {
		case value := <-checks:
			if value.Latest == nil || value.Latest.Version != "v1.2.4" {
				t.Fatal("new release not shared", value)
			}
		case <-ctx.Done():
			t.Fatal("coalesced check timed out")
		}
	}
	if client.calls.Load() != 1 || service.withControllerCheck(snapshot).Check.Latest.Version != "v1.2.4" {
		t.Fatal("duplicate release requests or stale machine snapshot", client.calls.Load())
	}
}
