package kubernetesruntime

import (
	"context"
	"fmt"
	"time"
)

// Driver constructs only fixed, typed Kubernetes mutations. It is not wired
// to the production runtime router while the capability remains experimental.
type Driver struct {
	provider Provider
	client   Client
	now      func() time.Time
}

func NewDriver(provider Provider, client Client) (*Driver, error) {
	if err := validateProvider(provider); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("%w: client is required", ErrInvalidProvider)
	}
	return &Driver{provider: cloneProvider(provider), client: client, now: time.Now}, nil
}

func (d *Driver) Release() ReleaseStatus { return ReleaseExperimental }

func (d *Driver) Provider() Provider { return cloneProvider(d.provider) }

func (d *Driver) Preflight(request Request, observation Observation) PreflightReport {
	return preflight(d.provider, request, observation, d.now().UTC())
}

func (d *Driver) Plan(ctx context.Context, request Request) (TypedMutation, error) {
	observation, err := d.client.Observe(ctx, request.Ref)
	if err != nil {
		return TypedMutation{}, fmt.Errorf("%w: observe Shard: %v", ErrObservation, err)
	}
	report := d.Preflight(request, observation)
	if !report.Ready {
		return TypedMutation{}, &PreflightError{Report: report}
	}
	return buildMutation(d.provider, request, observation), nil
}

func (d *Driver) Apply(ctx context.Context, request Request) (Observation, error) {
	mutation, err := d.Plan(ctx, request)
	if err != nil {
		return Observation{}, err
	}
	result, err := d.client.Apply(ctx, mutation)
	if err != nil {
		return Observation{}, err
	}
	if err := validateAppliedObservation(d.provider, request.Ref, result); err != nil {
		return Observation{}, err
	}
	return result, nil
}

func validateAppliedObservation(provider Provider, ref ShardRef, observation Observation) error {
	if observation.Ref != ref {
		return fmt.Errorf("%w: applied observation is outside the requested Shard", ErrObservation)
	}
	for name, resource := range map[string]ResourceObservation{
		"StatefulSet":      observation.StatefulSet,
		"Pod":              observation.Pod.ResourceObservation,
		"PVC":              observation.PVC.ResourceObservation,
		"Service":          observation.Service,
		"PublishedService": observation.PublishedService,
		"NetworkPolicy":    observation.NetworkPolicy,
	} {
		if resource.Exists && !hasOwnership(resource, provider.ID, ref.RoomID, ref.WorldID) {
			return fmt.Errorf("%w: applied %s escaped the managed label scope", ErrObservation, name)
		}
	}
	return nil
}

func cloneProvider(provider Provider) Provider {
	result := provider
	result.StorageProfiles = make(map[string]StorageProfile, len(provider.StorageProfiles))
	for id, profile := range provider.StorageProfiles {
		result.StorageProfiles[id] = profile
	}
	result.ComputeProfiles = make(map[string]ComputeProfile, len(provider.ComputeProfiles))
	for id, profile := range provider.ComputeProfiles {
		profile.NodeSelector = cloneMap(profile.NodeSelector)
		result.ComputeProfiles[id] = profile
	}
	return result
}
