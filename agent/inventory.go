package agent

import (
	"context"

	"dont/internal/runtimeinventory"
	"dont/shared"
)

func (a *Agent) collectRuntimeInventory(ctx context.Context, request shared.RuntimeInventoryRequest) (shared.RuntimeInventoryReport, error) {
	report, err := runtimeinventory.Collect(ctx, request)
	if err != nil {
		return report, err
	}
	installation, exists := a.runtimeInstallation(request.InstallationID)
	if !exists || installation.Driver != "container" {
		return report, nil
	}
	control, err := a.runtimeControl(installation)
	if err != nil {
		report.Warnings = append(report.Warnings, "容器 Runtime 不可用: "+err.Error())
		return report, nil
	}
	provider, ok := control.(containerInventoryProvider)
	if !ok {
		return report, nil
	}
	processes, err := provider.ContainerProcesses(ctx)
	if err != nil {
		report.Warnings = append(report.Warnings, "无法读取容器分片状态: "+err.Error())
		return report, nil
	}
	report.Processes = processes
	return report, nil
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
