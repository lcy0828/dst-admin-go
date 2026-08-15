package agent

import (
	"context"
	"errors"
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
	block := f.blockExec != nil && len(arguments) > 0 && arguments[0] == "exec"
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

func containerListLine(id, state, cluster, shard string) []byte {
	return []byte(`{"ID":"` + id + `","Names":"dst-` + shard + `","State":"` + state + `","Labels":"com.dst-admin.managed=true,com.dst-admin.installation=runtime-a,com.dst-admin.cluster=` + cluster + `,com.dst-admin.shard=` + shard + `"}` + "\n")
}

func containerCPUInspect(id, cluster, shard, cpuset string, nanoCPUs int64, running bool) []byte {
	return []byte(`{"Id":"` + id + `","Config":{"Labels":{"com.dst-admin.managed":"true","com.dst-admin.installation":"runtime-a","com.dst-admin.cluster":"` + cluster + `","com.dst-admin.shard":"` + shard + `"}},"State":{"Running":` + strconv.FormatBool(running) + `},"HostConfig":{"NanoCpus":` + strconv.FormatInt(nanoCPUs, 10) + `,"CpusetCpus":"` + cpuset + `"}}`)
}

func TestContainerRuntimeUsesTrustedLabelsAndFixedConsoleArguments(t *testing.T) {
	id := strings.Repeat("a", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"),
		containerListLine(id, "running", "Cluster_1", "Master"), nil,
	}}
	runtime, err := newContainerShardRuntime(containerTestInstallation(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Send(context.Background(), "Cluster_1", "Master", "c_announce('hello')"); err != nil {
		t.Fatal(err)
	}
	wantList := []string{"ps", "-a", "--no-trunc", "--filter", "label=com.dst-admin.managed=true", "--filter", "label=com.dst-admin.installation=runtime-a", "--filter", "label=com.dst-admin.cluster=Cluster_1", "--filter", "label=com.dst-admin.shard=Master", "--format", "{{json .}}"}
	wantExec := []string{"exec", id, "tmux", "-S", "/run/dst-admin/tmux/tmux.sock", "send-keys", "-t", "=dst:0.0", "-l", "--", "c_announce('hello')", ";", "send-keys", "-t", "=dst:0.0", "Enter"}
	if !reflect.DeepEqual(cli.calls[0].arguments, wantList) || !reflect.DeepEqual(cli.calls[1].arguments, wantList) || !reflect.DeepEqual(cli.calls[2].arguments, wantExec) {
		t.Fatalf("calls=%#v", cli.calls)
	}
	for _, argument := range cli.calls[2].arguments {
		if argument == "sh" || argument == "bash" || argument == "-c" {
			t.Fatalf("shell argument found: %#v", cli.calls[2].arguments)
		}
	}
}

func TestContainerRuntimeStatusMappingAndStart(t *testing.T) {
	id := strings.Repeat("b", 64)
	for state, expected := range map[string]shards.RuntimeState{
		"created": shards.RuntimeStopped, "exited": shards.RuntimeStopped, "restarting": shards.RuntimeStarting, "dead": shards.RuntimeFailed,
	} {
		t.Run(state, func(t *testing.T) {
			cli := &fakeContainerCLI{available: true, responses: [][]byte{containerListLine(id, state, "Cluster_1", "Caves")}}
			runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
			status, err := runtime.Status(context.Background(), "Cluster_1", "Caves")
			if err != nil || status.State != expected {
				t.Fatalf("status=%#v err=%v", status, err)
			}
		})
	}
	cli := &fakeContainerCLI{available: true, responses: [][]byte{containerListLine(id, "exited", "Cluster_1", "Master"), nil}}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	if err := runtime.Start(context.Background(), "Cluster_1", "Master"); err != nil {
		t.Fatal(err)
	}
	if got := cli.calls[1].arguments; !reflect.DeepEqual(got, []string{"start", id}) {
		t.Fatalf("start arguments=%#v", got)
	}
}

func TestContainerRuntimeRunningRequiresConsoleAndReportsInventory(t *testing.T) {
	id := strings.Repeat("c", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"), nil,
		containerListLine(id, "running", "Cluster_1", "Master"),
	}}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	status, err := runtime.Status(context.Background(), "Cluster_1", "Master")
	if err != nil || status.State != shards.RuntimeRunning {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	processes, err := runtime.ContainerProcesses(context.Background())
	if err != nil || len(processes) != 1 || processes[0].RuntimeKind != "container" || processes[0].InstanceID != id || processes[0].PID <= 0 {
		t.Fatalf("processes=%#v err=%v", processes, err)
	}
}

func TestContainerRuntimeAppliesAndVerifiesCPUWithTrustedIdentity(t *testing.T) {
	id := strings.Repeat("9", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"), nil,
		containerListLine(id, "running", "Cluster_1", "Master"),
		containerCPUInspect(id, "Cluster_1", "Master", "2-3", 2_000_000_000, true),
	}}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	result, err := runtime.ExecuteCPU(context.Background(), "Cluster_1", "Master", shared.RuntimeActionCPUApply, shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyExclusive, LogicalCPUIds: []int{3, 2}})
	if err != nil || !result.Enforced || result.State != shared.RuntimeCPUStateApplied || !reflect.DeepEqual(result.EffectiveCPUIds, []int{2, 3}) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	wantUpdate := []string{"update", "--cpus", "2", "--cpuset-cpus", "2-3", id}
	wantInspect := []string{"inspect", "--format", "{{json .}}", id}
	if !reflect.DeepEqual(cli.calls[1].arguments, wantUpdate) || !reflect.DeepEqual(cli.calls[3].arguments, wantInspect) {
		t.Fatalf("calls=%#v", cli.calls)
	}
}

func TestContainerRuntimeRejectsCPUResultForChangedInstance(t *testing.T) {
	first, second := strings.Repeat("7", 64), strings.Repeat("8", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(first, "running", "Cluster_1", "Master"), nil,
		containerListLine(second, "running", "Cluster_1", "Master"),
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
		containerListLine(id, "running", "Cluster_1", "Master"), nil,
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
		if calls == 3 {
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
	if len(cli.calls) != 3 {
		t.Fatalf("coalesced calls=%#v", cli.calls)
	}
}

func TestContainerRuntimeRejectsInstanceChangeBeforeConsoleWrite(t *testing.T) {
	oldID := strings.Repeat("a", 64)
	newID := strings.Repeat("b", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(oldID, "running", "Cluster_1", "Master"),
		containerListLine(newID, "running", "Cluster_1", "Master"),
	}}
	runtime, _ := newContainerShardRuntime(containerTestInstallation(), cli)
	err := runtime.Send(context.Background(), "Cluster_1", "Master", "c_save()")
	if !errors.Is(err, consoledispatch.ErrInstanceChanged) {
		t.Fatalf("send after instance change=%v", err)
	}
	if len(cli.calls) != 2 {
		t.Fatalf("unexpected console write: %#v", cli.calls)
	}
}
