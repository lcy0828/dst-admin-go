package hostresource

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDetectAtReadsCgroupV2Limits(t *testing.T) {
	root := t.TempDir()
	writeLimitFile(t, root, "cpu.max", "250000 100000\n")
	writeLimitFile(t, root, "cpuset.cpus.effective", "2-3,7\n")
	writeLimitFile(t, root, "memory.max", "4294967296\n")
	writeLimitFile(t, root, "memory.current", "1073741824\n")

	limits := detectAt(root)
	if limits.CPUCount != 2 || !reflect.DeepEqual(limits.CPUSet, []int{2, 3, 7}) {
		t.Fatalf("CPU limits = %#v", limits)
	}
	total, used, available := EffectiveMemory(32<<30, 8<<30, 24<<30, limits)
	if total != 4<<30 || used != 1<<30 || available != 3<<30 {
		t.Fatalf("effective memory = %d/%d/%d", total, used, available)
	}
	if count := EffectiveCPUCount(16, limits); count != 2 {
		t.Fatalf("effective CPU count = %d", count)
	}
}

func TestDetectAtSupportsUnlimitedAndCgroupV1(t *testing.T) {
	root := t.TempDir()
	writeLimitFile(t, root, filepath.Join("cpu", "cpu.cfs_quota_us"), "400000")
	writeLimitFile(t, root, filepath.Join("cpu", "cpu.cfs_period_us"), "100000")
	writeLimitFile(t, root, filepath.Join("cpuset", "cpuset.cpus"), "0-1")
	writeLimitFile(t, root, filepath.Join("memory", "memory.limit_in_bytes"), "9223372036854771712")

	limits := detectAt(root)
	if limits.CPUCount != 2 || limits.MemoryLimitBytes != 0 {
		t.Fatalf("v1 limits = %#v", limits)
	}
	if parsed := parseCPUSet("0-2,4,6-7"); !reflect.DeepEqual(parsed, []int{0, 1, 2, 4, 6, 7}) {
		t.Fatalf("parsed CPU set = %#v", parsed)
	}
}

func writeLimitFile(t *testing.T, root, name, value string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}
