package runtimedriver

import (
	"context"
	"dont/internal/entityart"
	"dont/shared"
	"errors"
)

type EntityArtworkReader interface {
	ReadEntityArtwork(context.Context, Target, shared.RuntimeEntityArtworkRequest) ([]byte, error)
}

func (d *Native) ConfigureEntityArtwork(server, workshop string) {
	d.artworkServer, d.artworkWorkshop = server, workshop
}
func (d *Native) ReadEntityArtwork(ctx context.Context, target Target, input shared.RuntimeEntityArtworkRequest) ([]byte, error) {
	if err := validateTarget(target); err != nil {
		return nil, err
	}
	if d.artworkServer == "" {
		return nil, ErrCapabilityMissing
	}
	return entityart.Default.Read(ctx, d.artworkServer, d.artworkWorkshop, input.Prefab, input.ModID)
}
func (d *Agent) ReadEntityArtwork(ctx context.Context, target Target, input shared.RuntimeEntityArtworkRequest) ([]byte, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionEntityArtwork)
	request.EntityArtwork = &input
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 15)
	if err != nil {
		return nil, err
	}
	value := result.Result.EntityArtwork
	if value == nil || value.Prefab != input.Prefab || value.ModID != input.ModID {
		return nil, errors.New("Agent returned mismatched entity artwork")
	}
	if err := entityart.ValidatePNG(value.Data); err != nil {
		return nil, err
	}
	return value.Data, nil
}
func (r *Router) ReadEntityArtwork(ctx context.Context, room, world string, input shared.RuntimeEntityArtworkRequest) ([]byte, error) {
	if !entityart.Valid(input.Prefab, input.ModID) {
		return nil, entityart.ErrInvalid
	}
	driver, target, err := r.DriverTarget(ctx, room, world)
	if err != nil {
		return nil, err
	}
	reader, ok := driver.(EntityArtworkReader)
	if !ok || !HasTargetCapability(driver, target, CapabilityEntityArtwork) {
		return nil, ErrCapabilityMissing
	}
	return reader.ReadEntityArtwork(ctx, target, input)
}
