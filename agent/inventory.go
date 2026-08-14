package agent

import (
	"context"

	"dont/internal/runtimeinventory"
	"dont/shared"
)

func (a *Agent) collectRuntimeInventory(ctx context.Context, request shared.RuntimeInventoryRequest) (shared.RuntimeInventoryReport, error) {
	return runtimeinventory.Collect(ctx, request)
}

func collectHostResources() (shared.CPUInventory, shared.MemoryInventory) {
	return runtimeinventory.HostResources()
}

func collectDSTProcesses(ctx context.Context) []shared.ShardProcessReport {
	return runtimeinventory.CollectDSTProcesses(ctx)
}

func isDSTServerProcess(name string, arguments []string) bool {
	return runtimeinventory.IsDSTServerProcess(name, arguments)
}

func shardProcessFromArguments(pid int32, name string, arguments []string) shared.ShardProcessReport {
	return runtimeinventory.ShardProcessFromArguments(pid, name, arguments)
}
