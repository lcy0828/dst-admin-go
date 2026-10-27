package runtimedriver

import (
	"context"
	"dont/internal/installationmaintenance"
)

type maintenanceBinding struct {
	gate    *installationmaintenance.Gate
	resolve func(Target) (installationmaintenance.Resource, error)
}

func (b maintenanceBinding) acquire(ctx context.Context, target Target) (context.Context, func(), error) {
	return b.acquireMode(ctx, target, false)
}

func (b maintenanceBinding) acquireLifecycle(ctx context.Context, target Target) (context.Context, func(), error) {
	return b.acquireMode(ctx, target, true)
}

func (b maintenanceBinding) acquireMode(ctx context.Context, target Target, shared bool) (context.Context, func(), error) {
	if b.gate == nil {
		return ctx, func() {}, nil
	}
	resource, err := b.resolve(target)
	if err != nil {
		return ctx, nil, err
	}
	if shared {
		return b.gate.AcquireShared(ctx, []installationmaintenance.Resource{resource}, "")
	}
	return b.gate.Acquire(ctx, []installationmaintenance.Resource{resource}, "")
}
func (d *Native) ConfigureMaintenance(gate *installationmaintenance.Gate, resolve func(Target) (installationmaintenance.Resource, error)) {
	d.maintenance = maintenanceBinding{gate, resolve}
}
func (d *Agent) ConfigureMaintenance(gate *installationmaintenance.Gate, resolve func(Target) (installationmaintenance.Resource, error)) {
	d.maintenance = maintenanceBinding{gate, resolve}
}
