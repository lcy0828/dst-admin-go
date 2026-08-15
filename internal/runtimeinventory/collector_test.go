package runtimeinventory

import "testing"

func TestHostResourcesReportsConsistentCPUTopology(t *testing.T) {
	cpu, memory := HostResources()
	if cpu.LogicalProcessors < 1 || cpu.PhysicalCores < 0 || cpu.PhysicalCores > cpu.LogicalProcessors {
		t.Fatalf("invalid CPU inventory: %#v", cpu)
	}
	if cpu.TopologyAvailable && len(cpu.Threads) != cpu.LogicalProcessors {
		t.Fatalf("available CPU topology is incomplete: logical=%d threads=%d", cpu.LogicalProcessors, len(cpu.Threads))
	}
	seen := make(map[int]bool, len(cpu.Threads))
	for _, thread := range cpu.Threads {
		if thread.LogicalID < 0 || thread.LogicalID >= cpu.LogicalProcessors || seen[thread.LogicalID] || thread.PackageID == "" || thread.CoreID == "" {
			t.Fatalf("invalid CPU thread inventory: %#v", thread)
		}
		seen[thread.LogicalID] = true
	}
	t.Logf("logical=%d physical=%d topology=%t smt=%t threads=%d memory=%d", cpu.LogicalProcessors, cpu.PhysicalCores, cpu.TopologyAvailable, cpu.SMTDetected, len(cpu.Threads), memory.TotalBytes)
}
