package dstruntime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"dont/internal/runtimefiles"
	"dont/shared"
)

// ReadCommandReceipt only reads existing artifacts. In particular it must not
// call moduleReady/requireModule: those may send a snapshot refresh to DST.
func (b *Bridge) ReadCommandReceipt(ctx context.Context, roomID, worldID string, request CommandRequest, notBefore time.Time) (CommandReceipt, error) {
	if err := validateCommandRequest(request); err != nil {
		return CommandReceipt{}, err
	}
	room, world, worldPath, err := b.resolveWorld(roomID, worldID)
	if err != nil {
		return CommandReceipt{}, err
	}
	running, err := b.process.IsRunning(ctx, room.DirectoryName, world.DirectoryName)
	if err != nil {
		return CommandReceipt{}, err
	}
	if !running {
		return CommandReceipt{}, ErrRuntimeUnavailable
	}
	health, err := b.manager.Health(room.ID, world.ID)
	if err != nil {
		return CommandReceipt{}, err
	}
	current, err := runtimefiles.ReadWorldIdentity(ctx, b.manager.root, room.DirectoryName, world.DirectoryName, shared.ShardRuntimeStatus{State: "running"})
	if err != nil {
		return CommandReceipt{}, err
	}
	if !healthMatchesCurrentProcess(b.now().UTC(), health, current) {
		return CommandReceipt{}, fmt.Errorf("%w: receipt health belongs to another process", ErrRuntimeUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return CommandReceipt{}, err
	}
	receipt, err := readCommandReceipt(worldPath, current.SessionID, health, request, b.now().UTC(), notBefore)
	if ctx.Err() != nil {
		return CommandReceipt{}, ctx.Err()
	}
	return receipt, err
}

// The distributed adapter uses the applied Placement for all reads, including
// local worlds. An unavailable Agent never falls back to Controller files.
func (b *DistributedBridge) ReadCommandReceipt(ctx context.Context, roomID, worldID string, request CommandRequest, notBefore time.Time) (CommandReceipt, error) {
	if err := validateCommandRequest(request); err != nil {
		return CommandReceipt{}, err
	}
	health, err := b.ObserveHealth(ctx, roomID, worldID)
	if err != nil {
		return CommandReceipt{}, err
	}
	current, err := b.currentProcessHealth(ctx, roomID, worldID, health)
	if err != nil {
		return CommandReceipt{}, err
	}
	if !current {
		return CommandReceipt{}, ErrRuntimeUnavailable
	}
	bundle, err := b.runtime.ReadArtifacts(ctx, roomID, worldID, shared.ArtifactRuntimeCommand)
	if err != nil {
		return CommandReceipt{}, err
	}
	if err := ctx.Err(); err != nil {
		return CommandReceipt{}, err
	}
	artifacts, err := validatedArtifacts(bundle, shared.ArtifactRuntimeCommand, "command-receipt-a.json", "command-receipt-b.json")
	if err != nil {
		return CommandReceipt{}, err
	}
	candidates := make([]CommandReceipt, 0, len(artifacts))
	var invalid error
	for _, artifact := range artifacts {
		receipt, err := decodeCommandReceiptData(artifact.Data, health.SessionID, health.ShardID, request, b.now().UTC(), notBefore)
		if err != nil {
			if !errors.Is(err, ErrRuntimeResultAbsent) {
				invalid = errors.Join(invalid, err)
			}
			continue
		}
		if commandInstanceMatches(health, receipt) {
			candidates = append(candidates, receipt)
		}
	}
	if len(candidates) == 0 {
		if invalid != nil {
			return CommandReceipt{}, invalid
		}
		return CommandReceipt{}, ErrRuntimeResultAbsent
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Sequence > candidates[j].Sequence })
	return candidates[0], nil
}
