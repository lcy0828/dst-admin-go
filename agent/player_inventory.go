package agent

import (
	"context"

	"dont/internal/playerinventory"
	"dont/internal/shards"
	"dont/shared"
)

type inventoryControlAdapter struct {
	Native interface {
		Status(context.Context, string, string) (shards.RuntimeStatus, error)
		Send(context.Context, string, string, string) error
	}
}

func (c inventoryControlAdapter) IsRunning(ctx context.Context, cluster, shard string) (bool, error) {
	status, err := c.Native.Status(ctx, cluster, shard)
	return status.State == shards.RuntimeRunning, err
}
func (c inventoryControlAdapter) Send(ctx context.Context, cluster, shard, command string) error {
	return c.Native.Send(ctx, cluster, shard, command)
}

func (a *Agent) readPlayerInventory(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	var adapter playerinventory.Control
	if request.PlayerInventory.Source == "live" {
		control, err := a.runtimeControl(installation)
		if err != nil {
			return shared.RuntimeOperationResult{}, err
		}
		adapter = inventoryControlAdapter{control}
	}
	value, err := playerinventory.Default.Read(ctx, installation.SavePath, request.Cluster, request.Shard, *request.PlayerInventory, adapter)
	if err != nil {
		value.PlayerID = request.PlayerInventory.PlayerID
		value.Code = playerinventory.Code(err)
	}
	result := runtimeResult(request, shared.RuntimeOutcomeObserved, "Player inventory read")
	result.PlayerInventory = &value
	return result, nil
}
