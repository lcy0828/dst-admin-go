package runtimedriver

import (
	"context"
	"errors"
	"strings"
	"time"

	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"

	"github.com/google/uuid"
)

type PlacementResolver interface {
	AppliedPlacement(string, string) (topology.ExecutionPlacement, error)
	ResolveExecution(context.Context, string, string) (topology.ExecutionPlacement, error)
}

type LeaseService interface {
	Acquire(context.Context, string, string, time.Duration) (operationlease.Lease, error)
	Release(operationlease.Lease) error
}

type Router struct {
	placements PlacementResolver
	leases     LeaseService
	local      Driver
	remote     Driver
	leaseTTL   time.Duration
}

func NewRouter(placements PlacementResolver, leases LeaseService, local, remote Driver) (*Router, error) {
	if placements == nil || leases == nil || local == nil || remote == nil {
		return nil, errors.New("runtime driver router dependencies are required")
	}
	return &Router{placements: placements, leases: leases, local: local, remote: remote, leaseTTL: 2 * time.Minute}, nil
}

func (r *Router) DriverTarget(ctx context.Context, roomID, worldID string) (Driver, Target, error) {
	applied, err := r.placements.AppliedPlacement(roomID, worldID)
	if err != nil {
		return nil, Target{}, err
	}
	placement := applied
	driver := r.local
	if applied.AppliedTargetID != "local" {
		placement, err = r.placements.ResolveExecution(ctx, roomID, worldID)
		if err != nil {
			return nil, Target{}, err
		}
		driver = r.remote
	}
	return driver, Target{
		TargetID: placement.AppliedTargetID, InstallationID: placement.Target.Config.InstallationID,
		RoomID: roomID, WorldID: worldID, Cluster: placement.Room.DirectoryName, Shard: placement.World.DirectoryName,
		TopologyRevision: placement.Revision,
	}, nil
}

// Send keeps the established console sender contract while resolving the
// current applied Placement from the stable encoded room/world identifiers.
func (r *Router) Send(ctx context.Context, roomName, worldName, command string) error {
	_, err := r.SendID(ctx, rooms.EncodeID(roomName), rooms.EncodeID(worldName), shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeManaged, Command: command})
	return err
}

func (r *Router) SendBackground(ctx context.Context, roomName, worldName, coalesceKey, command string) error {
	_, err := r.SendID(ctx, rooms.EncodeID(roomName), rooms.EncodeID(worldName), shared.RuntimeConsoleRequest{
		Mode: shared.ConsoleModeProbe, CoalesceKey: coalesceKey, Command: command,
	})
	return err
}

func (r *Router) SendID(ctx context.Context, roomID, worldID string, request shared.RuntimeConsoleRequest) (shared.RuntimeOperationResult, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.RuntimeOperationResult{}, err
	}
	operation := Operation{ID: newOperationID()}
	if target.TargetID != "local" {
		lease, leaseErr := r.leases.Acquire(ctx, roomID, "runtime.console.send:"+worldID, r.leaseTTL)
		if leaseErr != nil {
			return shared.RuntimeOperationResult{}, leaseErr
		}
		defer r.leases.Release(lease)
		expires := lease.ExpiresAt.UTC()
		operation.Key, operation.LeaseID, operation.FencingToken, operation.LeaseExpiresAt = operation.ID, lease.LeaseID, lease.FencingToken, &expires
	}
	return driver.SendConsole(ctx, target, operation, request, 30*time.Second)
}

func (r *Router) Status(ctx context.Context, roomID, worldID string) (shared.ShardRuntimeStatus, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.ShardRuntimeStatus{}, err
	}
	return driver.Status(ctx, target)
}

func (r *Router) ConsoleHealth(ctx context.Context, roomID, worldID string) (shared.RuntimeConsoleHealth, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.RuntimeConsoleHealth{}, err
	}
	return driver.ConsoleHealth(ctx, target)
}

func (r *Router) ReadLogs(ctx context.Context, roomID, worldID string, request shared.RuntimeLogRequest) (shared.RuntimeLogChunk, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	return driver.ReadLogs(ctx, target, request)
}

func (r *Router) ReadArtifacts(ctx context.Context, roomID, worldID string, kind shared.ArtifactKind) (shared.RuntimeArtifactBundle, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.RuntimeArtifactBundle{}, err
	}
	return driver.ReadArtifacts(ctx, target, kind)
}

func newOperationID() string { return strings.ToLower(uuid.NewString()) }
