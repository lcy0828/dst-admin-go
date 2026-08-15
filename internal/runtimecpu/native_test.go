package runtimecpu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"dont/shared"
)

func newNativeTestExecutor(t *testing.T, platform string, identities ...ProcessIdentity) (*Native, string) {
	t.Helper()
	root := t.TempDir()
	for name, value := range map[string]string{
		"cgroup.controllers": "cpu cpuset io", "cgroup.subtree_control": "", "cpuset.mems.effective": "0", "cgroup.procs": "",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	index := 0
	resolver := func(context.Context, string, string, string) (ProcessIdentity, error) {
		if len(identities) == 0 {
			return ProcessIdentity{}, ErrInstanceNotRunning
		}
		if index >= len(identities) {
			return identities[len(identities)-1], nil
		}
		value := identities[index]
		index++
		return value, nil
	}
	executor, err := NewNative(NativeConfig{Platform: platform, ServerRoot: "/srv/dst", CgroupRoot: root, Resolve: resolver, RegularFilesystem: true})
	if err != nil {
		t.Fatal(err)
	}
	return executor, root
}

func TestNativePrepareAndApplyCgroupV2Policy(t *testing.T) {
	identity := ProcessIdentity{PID: 4321, InstanceID: "4321:100", Executable: "/srv/dst/bin/server"}
	executor, _ := newNativeTestExecutor(t, "linux", identity, identity)
	request := shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyExclusive, LogicalCPUIds: []int{3, 2}}
	prepared, err := executor.Prepare(context.Background(), "default", "Cluster_1", "Master", request)
	if err != nil || prepared.State != shared.RuntimeCPUStatePrepared || prepared.Enforced {
		t.Fatalf("prepared=%#v err=%v", prepared, err)
	}
	applied, err := executor.Apply(context.Background(), "default", "Cluster_1", "Master", request)
	if err != nil || !applied.Enforced || applied.InstanceID != identity.InstanceID || applied.QuotaMicros != 200000 || applied.PeriodMicros != 100000 || !reflect.DeepEqual(applied.EffectiveCPUIds, []int{2, 3}) {
		t.Fatalf("applied=%#v err=%v", applied, err)
	}
}

func TestNativeApplyRejectsChangedProcessIdentity(t *testing.T) {
	first := ProcessIdentity{PID: 4321, InstanceID: "4321:100"}
	second := ProcessIdentity{PID: 4322, InstanceID: "4322:200"}
	executor, _ := newNativeTestExecutor(t, "linux", first, second)
	_, err := executor.Apply(context.Background(), "default", "Cluster_1", "Master", shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyShared, LogicalCPUIds: []int{0}})
	if !errors.Is(err, ErrInstanceChanged) {
		t.Fatalf("error=%v", err)
	}
}

func TestNativeCPUEnforcementFailsClosedOnMacOS(t *testing.T) {
	executor, _ := newNativeTestExecutor(t, "darwin")
	for _, policy := range []shared.RuntimeCPUPolicy{shared.RuntimeCPUPolicyShared, shared.RuntimeCPUPolicyExclusive} {
		_, err := executor.Prepare(context.Background(), "default", "Cluster_1", "Master", shared.RuntimeCPURequest{Policy: policy, LogicalCPUIds: []int{0}})
		if !errors.Is(err, ErrUnsupportedPlatform) {
			t.Fatalf("policy=%s error=%v", policy, err)
		}
	}
}

func TestNativeNoneDoesNotMoveUnmanagedProcessToCgroupRoot(t *testing.T) {
	identity := ProcessIdentity{PID: 4321, InstanceID: "4321:100", Executable: "/srv/dst/bin/server"}
	executor, root := newNativeTestExecutor(t, "linux", identity, identity)
	rootProcesses := filepath.Join(root, "cgroup.procs")
	if err := os.WriteFile(rootProcesses, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := executor.Apply(context.Background(), "default", "Cluster_1", "Master", shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyNone})
	if err != nil || result.State != shared.RuntimeCPUStateReleased || !result.Enforced || !result.InstanceRunning {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	content, err := os.ReadFile(rootProcesses)
	if err != nil || string(content) != "existing" {
		t.Fatalf("root cgroup processes=%q err=%v", content, err)
	}
}

func TestNativeNoneRemovesPreparedCgroupAfterShardStops(t *testing.T) {
	executor, _ := newNativeTestExecutor(t, "linux")
	policy := shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyExclusive, LogicalCPUIds: []int{0}}
	if _, err := executor.Prepare(context.Background(), "default", "Cluster_1", "Master", policy); err != nil {
		t.Fatal(err)
	}
	path := executor.cgroupPath("default", "Cluster_1", "Master")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("prepared cgroup missing: %v", err)
	}
	result, err := executor.Apply(context.Background(), "default", "Cluster_1", "Master", shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyNone})
	if err != nil || result.State != shared.RuntimeCPUStateReleased || !result.Enforced || result.InstanceRunning {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("released cgroup still exists: %v", err)
	}
}

func TestNativeObserveStoppedShardWithoutCgroupRetainsPreparedIntent(t *testing.T) {
	executor, _ := newNativeTestExecutor(t, "linux")
	result, err := executor.Observe(context.Background(), "default", "Cluster_1", "Master", shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyShared, LogicalCPUIds: []int{1}})
	if err != nil || result.State != shared.RuntimeCPUStatePrepared || result.Enforced || result.InstanceRunning {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestCPUSetFormattingAndParsing(t *testing.T) {
	formatted := FormatCPUSet([]int{5, 2, 1, 0, 5})
	if formatted != "0-2,5" {
		t.Fatalf("formatted=%q", formatted)
	}
	parsed, err := ParseCPUSet(formatted)
	if err != nil || !reflect.DeepEqual(parsed, []int{0, 1, 2, 5}) {
		t.Fatalf("parsed=%v err=%v", parsed, err)
	}
}
