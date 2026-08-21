package configuration

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"dont/internal/configpublication"
	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/topology"

	"github.com/google/uuid"
)

const configurationPublicationLeaseTTL = 5 * time.Minute

type PublicationScope string

const (
	PublicationShared PublicationScope = "shared"
	PublicationWorld  PublicationScope = "world"
)

type PublicationRequest struct {
	RoomID  string
	WorldID string
	Scope   PublicationScope
	Files   []string
}

type PublicationResult struct {
	PublicationID  string
	PublishedCount int
	Warnings       []string
}

type Publisher interface {
	Publish(context.Context, PublicationRequest) (PublicationResult, error)
}

type publicationCatalog interface {
	ProvisionBundle(string) (rooms.ProvisionBundle, error)
}

type publicationPlacements interface {
	ResolveRoomExecutions(context.Context, string) ([]topology.ExecutionPlacement, error)
}

type publicationRuntimes interface {
	DriverTarget(context.Context, string, string) (runtimedriver.Driver, runtimedriver.Target, error)
}

type publicationLeases interface {
	Acquire(context.Context, string, string, time.Duration) (operationlease.Lease, error)
	Renew(context.Context, operationlease.Lease, time.Duration) (operationlease.Lease, error)
	Release(operationlease.Lease) error
}

type RemotePublisher struct {
	rooms      publicationCatalog
	placements publicationPlacements
	runtimes   publicationRuntimes
	leases     publicationLeases
}

type publicationTarget struct {
	driver runtimedriver.ConfigurationDriver
	target runtimedriver.Target
}

func NewRemotePublisher(roomCatalog publicationCatalog, placements publicationPlacements, runtimes publicationRuntimes, leases publicationLeases) (*RemotePublisher, error) {
	if roomCatalog == nil || placements == nil || runtimes == nil || leases == nil {
		return nil, errors.New("configuration publication dependencies are required")
	}
	return &RemotePublisher{rooms: roomCatalog, placements: placements, runtimes: runtimes, leases: leases}, nil
}

func (p *RemotePublisher) Publish(ctx context.Context, request PublicationRequest) (PublicationResult, error) {
	archive, scope, err := p.archive(request)
	if err != nil {
		return PublicationResult{}, err
	}
	targets, err := p.targets(ctx, request)
	if err != nil {
		return PublicationResult{}, err
	}
	result := PublicationResult{PublicationID: uuid.NewString()}
	if len(targets) == 0 {
		return result, nil
	}
	lease, err := p.leases.Acquire(ctx, request.RoomID, "configuration.publish:"+result.PublicationID, configurationPublicationLeaseTTL)
	if err != nil {
		return result, err
	}
	defer func() { _ = p.leases.Release(lease) }()
	sum := sha256.Sum256(archive)
	descriptor := runtimedriver.ConfigurationDescriptor{
		PublicationID: result.PublicationID, Scope: scope, Size: int64(len(archive)), SHA256: hex.EncodeToString(sum[:]),
	}
	started := make([]publicationTarget, 0, len(targets))
	for index, target := range targets {
		if err := p.renew(ctx, &lease); err != nil {
			return result, errors.Join(err, p.rollback(ctx, lease, result.PublicationID, scope, started))
		}
		offset, beginErr := target.driver.BeginConfiguration(ctx, target.target, publicationOperation(lease, result.PublicationID, "begin", index, 0), descriptor)
		if beginErr != nil || offset < 0 || offset > int64(len(archive)) {
			return result, errors.Join(beginErr, p.rollback(ctx, lease, result.PublicationID, scope, started))
		}
		started = append(started, target)
		for offset < int64(len(archive)) {
			if err := p.renew(ctx, &lease); err != nil {
				return result, errors.Join(err, p.rollback(ctx, lease, result.PublicationID, scope, started))
			}
			end := offset + configpublication.MaxChunkBytes
			if end > int64(len(archive)) {
				end = int64(len(archive))
			}
			next, writeErr := target.driver.WriteConfiguration(ctx, target.target, publicationOperation(lease, result.PublicationID, "write", index, offset), descriptor, offset, archive[offset:end])
			if writeErr != nil || next != end {
				return result, errors.Join(writeErr, fmt.Errorf("configuration publication offset %d, expected %d", next, end), p.rollback(ctx, lease, result.PublicationID, scope, started))
			}
			offset = next
		}
		if err := p.renew(ctx, &lease); err != nil {
			return result, errors.Join(err, p.rollback(ctx, lease, result.PublicationID, scope, started))
		}
		if err := target.driver.PrepareConfiguration(ctx, target.target, publicationOperation(lease, result.PublicationID, "prepare", index, 0), result.PublicationID, scope); err != nil {
			return result, errors.Join(err, p.rollback(ctx, lease, result.PublicationID, scope, started))
		}
	}
	for index, target := range targets {
		if err := p.renew(ctx, &lease); err != nil {
			return result, errors.Join(err, p.rollback(ctx, lease, result.PublicationID, scope, started))
		}
		if err := target.driver.PublishConfiguration(ctx, target.target, publicationOperation(lease, result.PublicationID, "publish", index, 0), result.PublicationID, scope); err != nil {
			return result, errors.Join(err, p.rollback(ctx, lease, result.PublicationID, scope, started))
		}
		result.PublishedCount++
	}
	for index, target := range targets {
		if err := p.renew(ctx, &lease); err != nil {
			result.Warnings = append(result.Warnings, err.Error())
			break
		}
		if err := target.driver.CompleteConfiguration(ctx, target.target, publicationOperation(lease, result.PublicationID, "complete", index, 0), result.PublicationID, scope); err != nil {
			result.Warnings = append(result.Warnings, err.Error())
		}
	}
	return result, nil
}

func (p *RemotePublisher) archive(request PublicationRequest) ([]byte, string, error) {
	if strings.TrimSpace(request.RoomID) == "" || request.Scope != PublicationShared && request.Scope != PublicationWorld || len(request.Files) == 0 {
		return nil, "", ErrInvalidConfiguration
	}
	bundle, err := p.rooms.ProvisionBundle(request.RoomID)
	if err != nil {
		return nil, "", err
	}
	available := make(map[string]rooms.ProvisionFile)
	if request.Scope == PublicationShared {
		for _, file := range bundle.Shared {
			available[file.Name] = file
		}
	} else {
		for _, world := range bundle.Worlds {
			if world.World.ID != request.WorldID {
				continue
			}
			for _, file := range world.Files {
				available[file.Name] = file
			}
		}
	}
	names := append([]string(nil), request.Files...)
	sort.Strings(names)
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for index, name := range names {
		if index > 0 && names[index-1] == name || !publicationFileAllowed(request.Scope, name) {
			_ = writer.Close()
			return nil, "", ErrInvalidConfiguration
		}
		file, exists := available[name]
		if !exists {
			_ = writer.Close()
			return nil, "", fmt.Errorf("configuration publication file %s is missing", name)
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o600)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			_ = writer.Close()
			return nil, "", err
		}
		if _, err := entry.Write(file.Data); err != nil {
			_ = writer.Close()
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return output.Bytes(), string(request.Scope), nil
}

func (p *RemotePublisher) targets(ctx context.Context, request PublicationRequest) ([]publicationTarget, error) {
	executions, err := p.placements.ResolveRoomExecutions(ctx, request.RoomID)
	if err != nil {
		return nil, err
	}
	result := make([]publicationTarget, 0, len(executions))
	seen := make(map[string]bool)
	for _, execution := range executions {
		if request.Scope == PublicationWorld && execution.World.ID != request.WorldID {
			continue
		}
		driver, target, err := p.runtimes.DriverTarget(ctx, request.RoomID, execution.World.ID)
		if err != nil {
			return nil, err
		}
		if target.TargetID == "local" {
			continue
		}
		key := target.TargetID + "\x00" + target.InstallationID + "\x00" + target.Cluster
		if request.Scope == PublicationWorld {
			key += "\x00" + target.Shard
		}
		if seen[key] {
			continue
		}
		publisher, ok := driver.(runtimedriver.ConfigurationDriver)
		if !ok || !runtimedriver.HasCapability(driver, runtimedriver.CapabilityConfigPublish) {
			return nil, runtimedriver.ErrCapabilityMissing
		}
		seen[key] = true
		result = append(result, publicationTarget{driver: publisher, target: target})
	}
	if request.Scope == PublicationWorld && len(result) == 0 {
		for _, execution := range executions {
			if execution.World.ID == request.WorldID && execution.AppliedTargetID == "local" {
				return result, nil
			}
		}
		return nil, rooms.ErrWorldNotFound
	}
	return result, nil
}

func (p *RemotePublisher) renew(ctx context.Context, lease *operationlease.Lease) error {
	value, err := p.leases.Renew(ctx, *lease, configurationPublicationLeaseTTL)
	if err == nil {
		*lease = value
	}
	return err
}

func (p *RemotePublisher) rollback(ctx context.Context, lease operationlease.Lease, publicationID, scope string, targets []publicationTarget) error {
	var result error
	for index := len(targets) - 1; index >= 0; index-- {
		value, renewErr := p.leases.Renew(context.WithoutCancel(ctx), lease, configurationPublicationLeaseTTL)
		if renewErr == nil {
			lease = value
		}
		target := targets[index]
		rollbackErr := target.driver.RollbackConfiguration(context.WithoutCancel(ctx), target.target, publicationOperation(lease, publicationID, "rollback", index, 0), publicationID, scope)
		result = errors.Join(result, renewErr, rollbackErr)
	}
	return result
}

func publicationOperation(lease operationlease.Lease, publicationID, phase string, index int, offset int64) runtimedriver.Operation {
	expires := lease.ExpiresAt.UTC()
	return runtimedriver.Operation{
		ID: publicationID, Key: publicationID + "-" + phase + "-" + strconv.Itoa(index) + "-" + strconv.FormatInt(offset, 10),
		LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, LeaseExpiresAt: &expires,
	}
}

func publicationFileAllowed(scope PublicationScope, name string) bool {
	if scope == PublicationShared {
		switch name {
		case "cluster.ini", "cluster_token.txt", "adminlist.txt", "blocklist.txt", "whitelist.txt":
			return true
		}
		return false
	}
	return name == "server.ini" || name == "leveldataoverride.lua"
}
