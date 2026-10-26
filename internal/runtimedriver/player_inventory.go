package runtimedriver

import (
	"context"

	"dont/internal/playerinventory"
	"dont/internal/shards"
	"dont/shared"
)

type PlayerInventoryReader interface {
	ReadPlayerInventory(context.Context, Target, shared.PlayerInventoryRequest) (shared.PlayerInventory, error)
}
type inventoryControl struct{ NativeControl }

func (c inventoryControl) IsRunning(ctx context.Context, cluster, shard string) (bool, error) {
	status, err := c.Status(ctx, cluster, shard)
	return status.State == shards.RuntimeRunning, err
}

func (d *Native) ReadPlayerInventory(ctx context.Context, target Target, q shared.PlayerInventoryRequest) (shared.PlayerInventory, error) {
	if err := validateTarget(target); err != nil {
		return shared.PlayerInventory{}, err
	}
	return playerinventory.Default.Read(ctx, d.saveRoot, target.Cluster, target.Shard, q, inventoryControl{d.control})
}
func (d *Agent) ReadPlayerInventory(ctx context.Context, target Target, q shared.PlayerInventoryRequest) (shared.PlayerInventory, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionPlayerInventory)
	request.PlayerInventory = &q
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 18)
	if err != nil {
		return shared.PlayerInventory{}, err
	}
	value := result.Result.PlayerInventory
	if value == nil || value.PlayerID != q.PlayerID {
		return shared.PlayerInventory{}, ErrInvalidTarget
	}
	if value.Code != "" {
		return *value, &shared.InventoryError{Code: value.Code, Candidates: value.Candidates}
	}
	return *value, nil
}
func (r *Router) ReadPlayerInventory(ctx context.Context, room, world string, q shared.PlayerInventoryRequest) (shared.PlayerInventory, error) {
	if !playerinventory.ValidRequest(q) {
		return shared.PlayerInventory{}, &shared.InventoryError{Code: "INVALID_PLAYER_ID"}
	}
	driver, target, err := r.DriverTarget(ctx, room, world)
	if err != nil {
		return shared.PlayerInventory{}, err
	}
	reader, ok := driver.(PlayerInventoryReader)
	if !ok || !HasTargetCapability(driver, target, CapabilityPlayerInventory) {
		return shared.PlayerInventory{}, ErrCapabilityMissing
	}
	value, err := reader.ReadPlayerInventory(ctx, target, q)
	if err == nil {
		_, current, resolveErr := r.DriverTarget(ctx, room, world)
		if resolveErr != nil {
			return shared.PlayerInventory{}, resolveErr
		}
		if current.TargetID != target.TargetID || current.InstallationID != target.InstallationID || current.Cluster != target.Cluster || current.Shard != target.Shard || current.TopologyRevision != target.TopologyRevision {
			return shared.PlayerInventory{}, &shared.InventoryError{Code: "PLAYER_LOCATION_UNCERTAIN"}
		}
	}
	value.RoomID, value.WorldID = room, world
	for i := range value.Candidates {
		value.Candidates[i].WorldID = world
	}
	if playerinventory.IsError(err) {
		return value, &shared.InventoryError{Code: playerinventory.Code(err), Candidates: value.Candidates}
	}
	return value, err
}
