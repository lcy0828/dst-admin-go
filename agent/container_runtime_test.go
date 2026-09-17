package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/consoledispatch"
	"dont/internal/runtimecpu"
	"dont/internal/shards"
	"dont/shared"
)

type containerCLICall struct{ arguments []string }

type fakeContainerCLI struct {
	mu        sync.Mutex
	available bool
	responses [][]byte
	errors    []error
	calls     []containerCLICall
	blockExec chan struct{}
}

func (f *fakeContainerCLI) Available() bool { return f != nil && f.available }

func (f *fakeContainerCLI) Run(ctx context.Context, arguments ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, containerCLICall{arguments: append([]string(nil), arguments...)})
	index := len(f.calls) - 1
	var output []byte
	var err error
	if index < len(f.responses) {
		output = f.responses[index]
	}
	if index < len(f.errors) {
		err = f.errors[index]
	}
	block := false
	if f.blockExec != nil && len(arguments) > 0 && arguments[0] == "exec" {
		for _, argument := range arguments {
			if argument == "send-keys" {
				block = true
				break
			}
		}
	}
	f.mu.Unlock()
	if block {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.blockExec:
		}
	}
	return output, err
}

func containerTestInstallation() RuntimeInstallation {
	return RuntimeInstallation{
		ID: "runtime-a", Driver: "container", SavePath: "/srv/klei", ServerPath: "/srv/dst",
		ServerMode: "64", ContainerEngine: "docker", ConsoleSocket: "/run/dst-admin/tmux/tmux.sock", ConsoleSession: "dst",
	}
}

func TestContainerRuntimeWritesPrivateOneShotLaunchOptions(t *testing.T) {
	installation := containerTestInstallation()
	installation.SavePath = t.TempDir()
	runtime, err := newContainerShardRuntime(installation, &fakeContainerCLI{available: true})
	if err != nil {
		t.Fatal(err)
	}
	path, err := runtime.writeLaunchOptions("Cluster_1", "Master", shared.RuntimeLaunchOptions{SkipUpdateServerMods: true})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "skip_update_server_mods=1\n" {
		t.Fatalf("launch options=%q err=%v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("launch option mode=%v", info.Mode().Perm())
	}
	if _, err := runtime.writeLaunchOptions("Cluster_1", "Master", shared.RuntimeLaunchOptions{}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "skip_update_server_mods=1\n" {
		t.Fatalf("default launch must also skip downloads on older images: %q err=%v", data, err)
	}
}

func containerListLine(id, state, cluster, shard string) []byte {
	return []byte(`{"ID":"` + id + `","Names":"dst-` + shard + `","State":"` + state + `","Labels":"com.dst-admin.managed=true,com.dst-admin.installation=runtime-a,com.dst-admin.cluster=` + cluster + `,com.dst-admin.shard=` + shard + `"}` + "\n")
}

func containerCPUInspect(id, cluster, shard, cpuset string, nanoCPUs int64, running bool) []byte {
	return []byte(`{"Id":"` + id + `","Config":{"Labels":{"com.dst-admin.managed":"true","com.dst-admin.installation":"runtime-a","com.dst-admin.cluster":"` + cluster + `","com.dst-admin.shard":"` + shard + `"}},"State":{"Running":` + strconv.FormatBool(running) + `},"HostConfig":{"NanoCpus":` + strconv.FormatInt(nanoCPUs, 10) + `,"CpusetCpus":"` + cpuset + `"}}`)
}

func containerExitInspect(exitCode int, oomKilled, dead bool) []byte {
	return []byte(`{"Status":"exited","Running":false,"Restarting":false,"OOMKilled":` + strconv.FormatBool(oomKilled) + `,"Dead":` + strconv.FormatBool(dead) + `,"ExitCode":` + strconv.Itoa(exitCode) + `,"Error":"","FinishedAt":"2026-08-16T00:00:00Z"}`)
}

func containerConsolePane() []byte {
	return []byte("0|dontstarve_dedicated_server_nullrenderer_x64|123\n")
}

func containerStartedAt(value string) []byte {
	return []byte(value + "\n")
}

const containerStartTime = "2026-08-16T02:00:00.123456789Z"

func TestContainerRuntimeUsesTrustedLabelsAndFixedConsoleArguments(t *testing.T) {
	id := strings.Repeat("a", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"),
		containerConsolePane(), nil,
		containerStartedAt(containerStartTime), containerListLine(id, "running", "Cluster_1", "Master"),
		containerStartedAt(containerStartTime), nil,
	}}
	runtime, err := newContainerShardRuntime(containerTestInstallation(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Send(context.Background(), "Cluster_1", "Master", "c_announce('hello')"); err != nil {
		t.Fatal(err)
	}
	wantList := []string{"ps", "-a", "--no-trunc", "--filter", "label=com.dst-admin.managed=true", "--filter", "label=com.dst-admin.installation=runtime-a", "--filter", "label=com.dst-admin.cluster=Cluster_1", "--filter", "label=com.dst-admin.shard=Master", "--format", "{{json .}}"}
	wantProbe := []string{"exec", id, "tmux", "-S", "/run/dst-admin/tmux/tmux.sock", "display-message", "-p", "-t", "=dst:0.0", "#{pane_dead}|#{pane_current_command}|#{pane_pid}"}
	wantClients := []string{"exec", id, "tmux", "-S", "/run/dst-admin/tmux/tmux.sock", "list-clients", "-F", "#{client_session}|#{client_readonly}"}
	wantExec := []string{
		"exec", id, "tmux", "-S", "/run/dst-admin/tmux/tmux.sock",
		"send-keys", "-t", "=dst:0.0", "C-q", "C-u", ";",
		"send-keys", "-t", "=dst:0.0", "-l", "--", "c_announce('hello')", ";",
		"send-keys", "-t", "=dst:0.0", "Enter",
	}
	if !reflect.DeepEqual(cli.calls[0].arguments, wantList) || !reflect.DeepEqual(cli.calls[1].arguments, wantProbe) ||
		!reflect.DeepEqual(cli.calls[2].arguments, wantClients) || !reflect.DeepEqual(cli.calls[4].arguments, wantList) ||
		!reflect.DeepEqual(cli.calls[6].arguments, wantExec) {
		t.Fatalf("calls=%#v", cli.calls)
	}
	for _, argument := range cli.calls[6].arguments {
		if argument == "sh" || argument == "bash" || argument == "-c" {
			t.Fatalf("shell argument found: %#v", cli.calls[2].arguments)
		}
	}
}

func TestContainerRuntimeDetectsWritableExternalClient(t *testing.T) {
	id := strings.Repeat("b", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"),
		containerConsolePane(), []byte("dst|0\n"),
	}}
	runtime, err := newContainerShardRuntime(containerTestInstallation(), cli)
	if err != nil {
		t.Fatal(err)
	}
	health := runtime.ConsoleHealth("Cluster_1", "Master")
	if health.Status != "external_writer" || health.Accepting || !health.ExternalWriter {
		t.Fatalf("health=%#v", health)
	}
}

func TestContainerRuntimeStatusMappingAndStart(t *testing.T) {
	id := strings.Repeat("b", 64)
	for state, expected := range map[string]shards.RuntimeState{
		"created": shards.RuntimeStopped, "exited": shards.RuntimeStopped, "restarting": shards.RuntimeStarting, "dead": shards.RuntimeFailed,
	} {
		t.Run(state, func(t *testing.T) {
			responses := [][]byte{containerListLine(id, state, "Cluster_1", "Caves")}
			if state == "exited" {
				responses = append(responses, containerExitInspect(0, false, false))
			}
			if state == "dead" {
				responses = append(responses, containerExitInspect(1, false, true))
			}
			cli := &fakeContainerCLI{available: true, responses: responses}
			runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
			status, err := runtime.Status(context.Background(), "Cluster_1", "Caves")
			if err != nil || status.State != expected {
				t.Fatalf("status=%#v err=%v", status, err)
			}
			if state == "created" && status.Code != "CONTAINER_CREATED" {
				t.Fatalf("created status=%#v", status)
			}
		})
	}
	cli := &fakeContainerCLI{available: true, responses: [][]byte{containerListLine(id, "exited", "Cluster_1", "Master"), nil, containerStartedAt(containerStartTime)}}
	installation := containerTestInstallation()
	installation.SavePath = t.TempDir()
	if err := os.MkdirAll(filepath.Join(installation.SavePath, "Cluster_1", "Master"), 0o700); err != nil {
		t.Fatal(err)
	}
	runtime, _ := newContainerShardRuntime(installation, cli)
	if err := runtime.Start(context.Background(), "Cluster_1", "Master"); err != nil {
		t.Fatal(err)
	}
	if got := cli.calls[1].arguments; !reflect.DeepEqual(got, []string{"start", id}) {
		t.Fatalf("start arguments=%#v", got)
	}
}

func TestContainerRuntimeReportsDistinctExitCauses(t *testing.T) {
	tests := []struct {
		name   string
		state  containerExitState
		code   string
		status shards.RuntimeState
	}{
		{name: "clean", state: containerExitState{ExitCode: 0}, code: "CONTAINER_EXIT_CLEAN", status: shards.RuntimeStopped},
		{name: "oom", state: containerExitState{ExitCode: 137, OOMKilled: true}, code: "CONTAINER_OOM_KILLED", status: shards.RuntimeFailed},
		{name: "sigkill", state: containerExitState{ExitCode: 137}, code: "CONTAINER_SIGKILL", status: shards.RuntimeFailed},
		{name: "crash", state: containerExitState{ExitCode: 42}, code: "CONTAINER_EXIT_NONZERO", status: shards.RuntimeFailed},
		{name: "dead", state: containerExitState{Dead: true, ExitCode: 1}, code: "CONTAINER_DEAD", status: shards.RuntimeFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := containerExitRuntimeStatus(test.state)
			if status.State != test.status || status.Code != test.code {
				t.Fatalf("status=%#v", status)
			}
		})
	}
}

func TestContainerRuntimeStopWaitsForRealExit(t *testing.T) {
	id := strings.Repeat("4", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"), nil,
		containerListLine(id, "exited", "Cluster_1", "Master"),
	}}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	runtime.gracePeriod, runtime.pollInterval = 20*time.Millisecond, time.Millisecond
	if err := runtime.Stop(context.Background(), "Cluster_1", "Master"); err != nil {
		t.Fatal(err)
	}
	if len(cli.calls) != 3 || cli.calls[1].arguments[0] != "exec" {
		t.Fatalf("calls=%#v", cli.calls)
	}
}

func TestContainerRuntimeStopUsesAuditedTermFallbackWhenConsoleFails(t *testing.T) {
	id := strings.Repeat("5", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"), nil, nil,
		containerListLine(id, "exited", "Cluster_1", "Master"),
	}, errors: []error{nil, errors.New("tmux socket unavailable")}}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	runtime.gracePeriod, runtime.pollInterval = time.Millisecond, time.Millisecond
	err := runtime.Stop(context.Background(), "Cluster_1", "Master")
	var fallback *ContainerStopFallbackError
	if !errors.As(err, &fallback) || fallback.Forced {
		t.Fatalf("error=%v", err)
	}
	wantStop := []string{"stop", "--time", "1", id}
	if !reflect.DeepEqual(cli.calls[2].arguments, wantStop) {
		t.Fatalf("calls=%#v", cli.calls)
	}
}

func TestContainerRuntimeStopEscalatesToKillAndReportsUnsavedRisk(t *testing.T) {
	id := strings.Repeat("6", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"), nil, nil, nil,
		containerListLine(id, "exited", "Cluster_1", "Master"),
	}, errors: []error{nil, errors.New("console unavailable"), errors.New("stop failed")}}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	runtime.gracePeriod, runtime.pollInterval = time.Millisecond, time.Millisecond
	err := runtime.Stop(context.Background(), "Cluster_1", "Master")
	var fallback *ContainerStopFallbackError
	if !errors.As(err, &fallback) || !fallback.Forced {
		t.Fatalf("error=%v", err)
	}
	wantKill := []string{"kill", "--signal", "KILL", id}
	if !reflect.DeepEqual(cli.calls[3].arguments, wantKill) {
		t.Fatalf("calls=%#v", cli.calls)
	}
}

func TestContainerRuntimeRunningRequiresConsoleAndReportsInventory(t *testing.T) {
	id := strings.Repeat("c", 64)
	installation := containerTestInstallation()
	installation.SavePath = t.TempDir()
	writeContainerRuntimeLog(t, installation.SavePath, "Cluster_1", "Master", containerStartTime, "[00:00:35]: [DST-ADMIN-RUNTIME READY] version=2.4.0 protocol=2")
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"), nil, containerStartedAt(containerStartTime),
		containerListLine(id, "running", "Cluster_1", "Master"), containerStartedAt(containerStartTime),
	}}
	runtime, _ := newContainerShardRuntime(installation, cli)
	status, err := runtime.Status(context.Background(), "Cluster_1", "Master")
	if err != nil || status.State != shards.RuntimeRunning {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	processes, err := runtime.ContainerProcesses(context.Background())
	if err != nil || len(processes) != 1 || processes[0].RuntimeKind != "container" || processes[0].InstanceID != id+"@"+containerStartTime || processes[0].PID <= 0 {
		t.Fatalf("processes=%#v err=%v", processes, err)
	}
}

func TestContainerRuntimeRemainsStartingUntilCurrentLogIsReady(t *testing.T) {
	id := strings.Repeat("7", 64)
	installation := containerTestInstallation()
	installation.SavePath = t.TempDir()
	writeContainerRuntimeLog(t, installation.SavePath, "Cluster_1", "Master", containerStartTime, "[00:00:01]: Starting Up")
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"), nil, containerStartedAt(containerStartTime),
	}}
	runtime, _ := newContainerShardRuntime(installation, cli)
	status, err := runtime.Status(context.Background(), "Cluster_1", "Master")
	if err != nil || status.State != shards.RuntimeStarting || status.Code != "DST_WORLD_LOADING" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestContainerRuntimeReadsCurrentPauseBeyondTheLogTail(t *testing.T) {
	installation := containerTestInstallation()
	installation.SavePath = t.TempDir()
	writeContainerRuntimeLog(t, installation.SavePath, "Cluster_1", "Master", containerStartTime,
		"[00:01:00]: Sim paused\n"+strings.Repeat("mod output\n", 80000))
	reader, _ := newContainerShardRuntime(installation, &fakeContainerCLI{available: true})
	startedAt, _ := time.Parse(time.RFC3339Nano, containerStartTime)
	for range 2 {
		status, err := reader.runtimeLogStatus(context.Background(), "Cluster_1", "Master", startedAt)
		if err != nil || status.State != shards.RuntimeRunning || status.Paused == nil || !*status.Paused {
			t.Fatalf("paused container=%+v error=%v", status, err)
		}
	}
	status, err := reader.runtimeLogStatus(context.Background(), "Cluster_1", "Master", startedAt.Add(time.Hour))
	if err != nil || status.State != shards.RuntimeStarting || status.Paused != nil {
		t.Fatalf("new container accepted previous process pause=%+v error=%v", status, err)
	}
}

func writeContainerRuntimeLog(t *testing.T, root, cluster, shard, startedAt, line string) {
	t.Helper()
	instant, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, cluster, shard)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	content := "[00:00:00]: Current time: " + instant.In(time.Local).Format("Mon Jan 2 15:04:05 2006") + "\n" + line + "\n"
	path := filepath.Join(directory, "server_log.txt")
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	modified := instant.Add(time.Minute)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func TestContainerRuntimeAppliesAndVerifiesCPUWithTrustedIdentity(t *testing.T) {
	id := strings.Repeat("9", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"), containerStartedAt(containerStartTime), nil,
		containerListLine(id, "running", "Cluster_1", "Master"), containerStartedAt(containerStartTime),
		containerCPUInspect(id, "Cluster_1", "Master", "2-3", 2_000_000_000, true),
	}}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	result, err := runtime.ExecuteCPU(context.Background(), "Cluster_1", "Master", shared.RuntimeActionCPUApply, shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyExclusive, LogicalCPUIds: []int{3, 2}})
	if err != nil || !result.Enforced || result.State != shared.RuntimeCPUStateApplied || !reflect.DeepEqual(result.EffectiveCPUIds, []int{2, 3}) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	wantUpdate := []string{"update", "--cpus", "2", "--cpuset-cpus", "2-3", id}
	wantInspect := []string{"inspect", "--format", "{{json .}}", id}
	if !reflect.DeepEqual(cli.calls[2].arguments, wantUpdate) || !reflect.DeepEqual(cli.calls[5].arguments, wantInspect) {
		t.Fatalf("calls=%#v", cli.calls)
	}
}

func TestContainerRuntimeReleasesCPUForStoppedContainer(t *testing.T) {
	id := strings.Repeat("8", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "exited", "Cluster_1", "Master"), nil,
		containerListLine(id, "exited", "Cluster_1", "Master"),
		containerCPUInspect(id, "Cluster_1", "Master", "", 0, false),
	}}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	result, err := runtime.ExecuteCPU(context.Background(), "Cluster_1", "Master", shared.RuntimeActionCPUApply, shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyNone})
	if err != nil || result.State != shared.RuntimeCPUStateReleased || !result.Enforced || result.InstanceRunning {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	wantUpdate := []string{"update", "--cpus", "0", "--cpuset-cpus", "", id}
	if !reflect.DeepEqual(cli.calls[1].arguments, wantUpdate) {
		t.Fatalf("calls=%#v", cli.calls)
	}
}

func TestContainerRuntimeRejectsCPUResultForChangedInstance(t *testing.T) {
	first, second := strings.Repeat("7", 64), strings.Repeat("8", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(first, "running", "Cluster_1", "Master"), containerStartedAt(containerStartTime), nil,
		containerListLine(second, "running", "Cluster_1", "Master"),
	}}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	_, err := runtime.ExecuteCPU(context.Background(), "Cluster_1", "Master", shared.RuntimeActionCPUApply, shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyShared, LogicalCPUIds: []int{0}})
	if !errors.Is(err, runtimecpu.ErrInstanceChanged) {
		t.Fatalf("error=%v", err)
	}
}

func TestContainerRuntimeRejectsCPUResultAfterSameContainerRestart(t *testing.T) {
	id := strings.Repeat("3", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"), containerStartedAt(containerStartTime), nil,
		containerListLine(id, "running", "Cluster_1", "Master"), containerStartedAt("2026-08-16T02:01:00Z"),
	}}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	_, err := runtime.ExecuteCPU(context.Background(), "Cluster_1", "Master", shared.RuntimeActionCPUApply, shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyShared, LogicalCPUIds: []int{0}})
	if !errors.Is(err, runtimecpu.ErrInstanceChanged) {
		t.Fatalf("error=%v", err)
	}
}

func TestContainerRuntimeRejectsUntrustedDuplicateAndMismatchedInventory(t *testing.T) {
	valid := strings.Repeat("d", 64)
	tests := []struct {
		name   string
		output []byte
	}{
		{name: "malformed id", output: containerListLine("container-name", "running", "Cluster_1", "Master")},
		{name: "duplicate", output: append(containerListLine(valid, "running", "Cluster_1", "Master"), containerListLine(strings.Repeat("e", 64), "running", "Cluster_1", "Master")...)},
		{name: "mismatched placement", output: containerListLine(valid, "running", "Other", "Master")},
		{name: "oversized", output: make([]byte, maximumContainerCLIOutput+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cli := &fakeContainerCLI{available: true, responses: [][]byte{test.output}}
			runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
			if _, err := runtime.find(context.Background(), "Cluster_1", "Master"); err == nil {
				t.Fatal("expected inventory rejection")
			}
		})
	}
}

func TestContainerRuntimeCoalescesBackgroundConsoleProbes(t *testing.T) {
	id := strings.Repeat("f", 64)
	release := make(chan struct{})
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"),
		containerStartedAt(containerStartTime), containerConsolePane(), nil,
		containerListLine(id, "running", "Cluster_1", "Master"), containerStartedAt(containerStartTime), nil,
		containerListLine(id, "running", "Cluster_1", "Master"), containerStartedAt(containerStartTime), containerConsolePane(), nil,
	}, blockExec: release}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	errorsSeen := make(chan error, 2)
	go func() {
		errorsSeen <- runtime.SendBackground(context.Background(), "Cluster_1", "Master", "players", "dst_admin_players()")
	}()
	for deadline := time.Now().Add(time.Second); ; {
		cli.mu.Lock()
		calls := len(cli.calls)
		cli.mu.Unlock()
		if calls == 7 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first background probe did not reach console")
		}
		time.Sleep(time.Millisecond)
	}
	go func() {
		errorsSeen <- runtime.SendBackground(context.Background(), "Cluster_1", "Master", "players", "ignored_duplicate")
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	for range 2 {
		if err := <-errorsSeen; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	cli.mu.Lock()
	defer cli.mu.Unlock()
	// The joined caller still performs read-only identity/health checks but
	// does not write a second payload to the pane.
	if len(cli.calls) != 11 {
		t.Fatalf("coalesced calls=%#v", cli.calls)
	}
	writes := 0
	for _, call := range cli.calls {
		if len(call.arguments) > 5 && call.arguments[0] == "exec" && call.arguments[2] == "tmux" && call.arguments[5] == "send-keys" {
			writes++
		}
	}
	if writes != 1 {
		t.Fatalf("console writes=%d calls=%#v", writes, cli.calls)
	}
}

func TestContainerRuntimeRejectsInstanceChangeBeforeConsoleWrite(t *testing.T) {
	oldID := strings.Repeat("a", 64)
	newID := strings.Repeat("b", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(oldID, "running", "Cluster_1", "Master"),
		containerConsolePane(), nil,
		containerStartedAt(containerStartTime), containerListLine(newID, "running", "Cluster_1", "Master"),
		containerStartedAt("2026-08-16T02:01:00Z"),
	}}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	err := runtime.Send(context.Background(), "Cluster_1", "Master", "c_save()")
	if !errors.Is(err, consoledispatch.ErrInstanceChanged) {
		t.Fatalf("send after instance change=%v", err)
	}
	if len(cli.calls) != 5 {
		t.Fatalf("unexpected console write: %#v", cli.calls)
	}
}

func TestContainerRuntimeRejectsSameContainerAfterRuntimeRestart(t *testing.T) {
	id := strings.Repeat("2", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"), containerConsolePane(), nil,
		containerStartedAt(containerStartTime), containerListLine(id, "running", "Cluster_1", "Master"),
		containerStartedAt("2026-08-16T02:01:00Z"),
	}}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	err := runtime.Send(context.Background(), "Cluster_1", "Master", "c_save()")
	if !errors.Is(err, consoledispatch.ErrInstanceChanged) {
		t.Fatalf("send after runtime restart=%v", err)
	}
	for _, call := range cli.calls {
		for _, argument := range call.arguments {
			if argument == "send-keys" {
				t.Fatalf("command crossed runtime restart: %#v", cli.calls)
			}
		}
	}
}

func TestContainerRuntimeBuildsFixedReadOnlyAttachCommand(t *testing.T) {
	id := strings.Repeat("1", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"), containerConsolePane(), nil, containerStartedAt(containerStartTime),
	}}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	spec, err := runtime.ConsoleAttach("Cluster_1", "Master", true)
	want := []string{"docker", "exec", "-it", id, "tmux", "-S", "/run/dst-admin/tmux/tmux.sock", "attach-session", "-r", "-t", "=dst"}
	if err != nil || !reflect.DeepEqual(spec.Command, want) || spec.InstanceID != id+"@"+containerStartTime {
		t.Fatalf("spec=%#v err=%v", spec, err)
	}
	if err := validateAttachCommand(spec.Command); err != nil {
		t.Fatal(err)
	}
}
