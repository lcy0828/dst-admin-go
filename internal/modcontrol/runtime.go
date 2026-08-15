package modcontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dont/internal/moddistribution"
	"dont/internal/modpublication"
	"dont/internal/runtimedriver"
	"dont/shared"

	"github.com/shirou/gopsutil/v3/disk"
)

const localModRuntimeVersion = "1.0.0"

const modRuntimeLeaseRefreshWindow = time.Minute

type ContentSource struct {
	manager      *moddistribution.Manager
	workshopRoot string
	mu           sync.Mutex
}

func NewContentSource(manager *moddistribution.Manager, workshopRoot string) (*ContentSource, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(workshopRoot))
	if manager == nil || err != nil || strings.TrimSpace(workshopRoot) == "" {
		return nil, ErrInvalidRequest
	}
	return &ContentSource{manager: manager, workshopRoot: filepath.Clean(absolute)}, nil
}

func (s *ContentSource) Resolve(ctx context.Context, requirement modpublication.ModRequirement) (modpublication.ContentArtifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	manifest, err := s.manager.Import(ctx, requirement.WorkshopID, filepath.Join(s.workshopRoot, requirement.WorkshopID), moddistribution.Metadata{})
	if err != nil {
		return modpublication.ContentArtifact{}, err
	}
	if requirement.TreeSHA256 != "" && !strings.EqualFold(requirement.TreeSHA256, manifest.TreeSHA256) {
		return modpublication.ContentArtifact{}, modpublication.ErrPlanChanged
	}
	return modpublication.ContentArtifact{
		WorkshopID: requirement.WorkshopID, TreeSHA256: manifest.TreeSHA256,
		ManifestSHA256: manifest.ManifestSHA256, Size: manifest.Size, FileCount: manifest.FileCount,
		SourceRef: "cache://" + requirement.WorkshopID + "/" + manifest.TreeSHA256,
	}, nil
}

type Runtime struct {
	source    *SnapshotSource
	manager   *moddistribution.Manager
	remote    runtimedriver.ModDriver
	cacheRoot string
	tempRoot  string
	mu        sync.Mutex
}

func NewRuntime(source *SnapshotSource, manager *moddistribution.Manager, remote runtimedriver.ModDriver, cacheRoot, tempRoot string) (*Runtime, error) {
	cacheAbsolute, cacheErr := filepath.Abs(strings.TrimSpace(cacheRoot))
	tempAbsolute, tempErr := filepath.Abs(strings.TrimSpace(tempRoot))
	if source == nil || manager == nil || remote == nil || cacheErr != nil || tempErr != nil || strings.TrimSpace(cacheRoot) == "" || strings.TrimSpace(tempRoot) == "" {
		return nil, ErrInvalidRequest
	}
	if err := os.MkdirAll(tempAbsolute, 0o700); err != nil {
		return nil, err
	}
	return &Runtime{source: source, manager: manager, remote: remote, cacheRoot: cacheAbsolute, tempRoot: tempAbsolute}, nil
}

func (r *Runtime) Observe(ctx context.Context, placement modpublication.AppliedPlacement) (modpublication.RuntimeObservation, error) {
	execution, exists, err := r.source.Execution(ctx, placement.TargetID, placement.InstallationID)
	if err != nil || !exists {
		return modpublication.RuntimeObservation{}, errors.Join(err, errors.New("runtime placement is missing from publication snapshot"))
	}
	result := modpublication.RuntimeObservation{
		TargetID: placement.TargetID, NodeID: placement.NodeID, InstallationID: placement.InstallationID,
		Online: true, Capabilities: []string{modpublication.RequiredCapability}, Version: localModRuntimeVersion,
	}
	if placement.TargetID == "local" {
		usage, err := disk.Usage(r.cacheRoot)
		if err != nil {
			return modpublication.RuntimeObservation{}, err
		}
		result.AvailableBytes = int64(usage.Free)
		return result, nil
	}
	result.Online = execution.Target.Online
	result.Capabilities = append([]string(nil), execution.Target.Capabilities...)
	available, version, err := r.remote.ObserveModTarget(ctx, runtimeTarget(placement, execution.Revision))
	if err != nil {
		return modpublication.RuntimeObservation{}, err
	}
	result.AvailableBytes, result.Version = available, version
	return result, nil
}

func (r *Runtime) EnsureCache(ctx context.Context, target modpublication.TargetPlan, operation modpublication.RuntimeOperation) error {
	if target.TargetID == "local" {
		for _, artifact := range target.Mods {
			if _, err := r.manager.Verify(ctx, artifact.WorkshopID, artifact.TreeSHA256); err != nil {
				return err
			}
		}
		return nil
	}
	driverTarget := targetRuntimeTarget(target, operation.TopologyRevision)
	session, err := newRuntimeOperationSession(target, operation)
	if err != nil {
		return err
	}
	for _, artifact := range target.Mods {
		if manifest, inspectErr := r.remote.InspectModCache(ctx, driverTarget, artifact.WorkshopID, artifact.TreeSHA256); inspectErr == nil && cacheManifestMatchesArtifact(manifest, artifact) {
			continue
		}
		if err := r.uploadBundle(ctx, driverTarget, session, artifact); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) Prepare(ctx context.Context, target modpublication.TargetPlan, operation modpublication.RuntimeOperation) error {
	input := distributionInput(target, operation.PublicationID)
	if target.TargetID == "local" {
		plan, err := r.manager.BuildPlan(ctx, input)
		if err != nil {
			return err
		}
		_, err = r.manager.Prepare(ctx, plan)
		return err
	}
	wire := runtimePlanInput(target, operation.PublicationID)
	encoded, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	driverTarget := targetRuntimeTarget(target, operation.TopologyRevision)
	session, err := newRuntimeOperationSession(target, operation)
	if err != nil {
		return err
	}
	descriptor := transferDescriptor("plan", operation.PublicationID, target, encoded)
	if err := uploadBytes(ctx, encoded, descriptor,
		func(offset int64) (int64, error) {
			step, err := session.step(ctx, "plan:begin")
			if err != nil {
				return 0, err
			}
			return r.remote.BeginModReleasePlan(ctx, driverTarget, step, descriptor)
		},
		func(offset int64, data []byte) (int64, error) {
			step, err := session.step(ctx, fmt.Sprintf("plan:write:%d", offset))
			if err != nil {
				return offset, err
			}
			return r.remote.WriteModReleasePlan(ctx, driverTarget, step, descriptor, offset, data)
		},
	); err != nil {
		return err
	}
	step, err := session.step(ctx, "plan:commit")
	if err != nil {
		return err
	}
	if _, err := r.remote.CommitModReleasePlan(ctx, driverTarget, step, descriptor); err != nil {
		return err
	}
	step, err = session.step(ctx, "release:prepare")
	if err != nil {
		return err
	}
	_, err = r.remote.PrepareModRelease(ctx, driverTarget, step, operation.PublicationID)
	return err
}

func (r *Runtime) Publish(ctx context.Context, target modpublication.TargetPlan, operation modpublication.RuntimeOperation) error {
	if target.TargetID == "local" {
		_, err := r.manager.Publish(ctx, operation.PublicationID)
		return err
	}
	session, err := newRuntimeOperationSession(target, operation)
	if err != nil {
		return err
	}
	step, err := session.step(ctx, "release:publish")
	if err != nil {
		return err
	}
	_, err = r.remote.PublishModRelease(ctx, targetRuntimeTarget(target, operation.TopologyRevision), step, operation.PublicationID)
	return err
}

func (r *Runtime) Rollback(ctx context.Context, target modpublication.TargetPlan, operation modpublication.RuntimeOperation) error {
	if target.TargetID == "local" {
		return r.manager.Rollback(ctx, operation.PublicationID)
	}
	session, err := newRuntimeOperationSession(target, operation)
	if err != nil {
		return err
	}
	step, err := session.step(ctx, "release:rollback")
	if err != nil {
		return err
	}
	_, err = r.remote.RollbackModRelease(ctx, targetRuntimeTarget(target, operation.TopologyRevision), step, operation.PublicationID)
	return err
}

func (r *Runtime) Complete(ctx context.Context, target modpublication.TargetPlan, operation modpublication.RuntimeOperation) error {
	if target.TargetID == "local" {
		return r.manager.Complete(ctx, operation.PublicationID)
	}
	session, err := newRuntimeOperationSession(target, operation)
	if err != nil {
		return err
	}
	step, err := session.step(ctx, "release:complete")
	if err != nil {
		return err
	}
	_, err = r.remote.CompleteModRelease(ctx, targetRuntimeTarget(target, operation.TopologyRevision), step, operation.PublicationID)
	return err
}

func (r *Runtime) uploadBundle(ctx context.Context, target runtimedriver.Target, session *runtimeOperationSession, artifact modpublication.ContentArtifact) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	file, err := os.CreateTemp(r.tempRoot, ".mod-bundle-*.tar")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	hash := sha256.New()
	if err := r.manager.WriteBundle(ctx, artifact.WorkshopID, artifact.TreeSHA256, io.MultiWriter(file, hash)); err != nil {
		file.Close()
		return err
	}
	info, err := file.Stat()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	descriptor := runtimedriver.ModUploadDescriptor{
		UploadID:   "cache-" + shaHex([]byte(artifact.WorkshopID+"\x00"+artifact.TreeSHA256)),
		WorkshopID: artifact.WorkshopID, ExpectedTreeSHA256: artifact.TreeSHA256,
		Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)),
		Metadata: shared.RuntimeModMetadata{PublishedFileSize: artifact.Size},
	}
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	defer input.Close()
	step, err := session.step(ctx, "cache:"+artifact.WorkshopID+":begin")
	if err != nil {
		return err
	}
	offset, err := r.remote.BeginModUpload(ctx, target, step, descriptor)
	if err != nil {
		return err
	}
	if offset < 0 || offset > descriptor.Size {
		return errors.New("remote Mod upload returned an invalid resume offset")
	}
	if _, err := input.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	buffer := make([]byte, shared.MaxChunkBytes)
	for offset < descriptor.Size {
		count, readErr := input.Read(buffer)
		if count > 0 {
			step, stepErr := session.step(ctx, fmt.Sprintf("cache:%s:write:%d", artifact.WorkshopID, offset))
			if stepErr != nil {
				return stepErr
			}
			next, writeErr := r.remote.WriteModUpload(ctx, target, step, descriptor, offset, buffer[:count])
			if writeErr != nil {
				return writeErr
			}
			if next != offset+int64(count) {
				return errors.New("remote Mod upload returned an inconsistent offset")
			}
			offset = next
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if count == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	step, err = session.step(ctx, "cache:"+artifact.WorkshopID+":commit")
	if err != nil {
		return err
	}
	manifest, err := r.remote.CommitModUpload(ctx, target, step, descriptor)
	if err != nil {
		return err
	}
	if !cacheManifestMatchesArtifact(manifest, artifact) {
		return errors.New("remote Mod cache manifest does not match the controller artifact")
	}
	return nil
}

func cacheManifestMatchesArtifact(manifest shared.RuntimeModCacheManifest, artifact modpublication.ContentArtifact) bool {
	return manifest.WorkshopID == artifact.WorkshopID &&
		strings.EqualFold(manifest.TreeSHA256, artifact.TreeSHA256) &&
		manifest.Size == artifact.Size && manifest.FileCount == artifact.FileCount
}

func distributionInput(target modpublication.TargetPlan, operationID string) moddistribution.PlanInput {
	input := moddistribution.PlanInput{OperationID: operationID, NodeID: target.NodeID}
	for _, world := range target.Worlds {
		shard := moddistribution.ShardRelease{
			InstallationID: target.InstallationID, RoomID: world.RoomID, RoomDirectory: world.RoomDirectory,
			WorldID: world.WorldID, WorldDirectory: world.WorldDirectory, ModOverrides: append([]byte(nil), world.ModOverrides...),
		}
		for _, artifact := range world.Mods {
			shard.Mods = append(shard.Mods, moddistribution.ModVersion{WorkshopID: artifact.WorkshopID, TreeSHA256: artifact.TreeSHA256})
		}
		input.Shards = append(input.Shards, shard)
	}
	return input
}

func runtimePlanInput(target modpublication.TargetPlan, operationID string) shared.RuntimeModPlanInput {
	input := distributionInput(target, operationID)
	wire := shared.RuntimeModPlanInput{OperationID: operationID, NodeID: target.NodeID}
	for _, shard := range input.Shards {
		item := shared.RuntimeModShardRelease{
			InstallationID: shard.InstallationID, RoomID: shard.RoomID, RoomDirectory: shard.RoomDirectory,
			WorldID: shard.WorldID, WorldDirectory: shard.WorldDirectory, ModOverrides: append([]byte(nil), shard.ModOverrides...),
		}
		for _, mod := range shard.Mods {
			item.Mods = append(item.Mods, shared.RuntimeModVersion{WorkshopID: mod.WorkshopID, TreeSHA256: mod.TreeSHA256})
		}
		wire.Shards = append(wire.Shards, item)
	}
	return wire
}

func runtimeOperation(target modpublication.TargetPlan, value modpublication.RuntimeOperation) (runtimedriver.Operation, error) {
	resource := installationResource(target.TargetID, target.InstallationID)
	for _, fence := range value.Fences {
		if fence.RoomID != resource {
			continue
		}
		expires := fence.ExpiresAt
		digest := shaHex([]byte(value.IdempotencyKey))
		return runtimedriver.Operation{
			ID: "modop-" + digest, Key: "modkey-" + digest, LeaseID: fence.LeaseID,
			FencingToken: fence.FencingToken, LeaseExpiresAt: &expires,
		}, nil
	}
	return runtimedriver.Operation{}, errors.New("installation publication fence is missing")
}

func runtimeStepOperation(base runtimedriver.Operation, step string) runtimedriver.Operation {
	digest := shaHex([]byte(base.ID + "\x00" + base.Key + "\x00" + step))
	base.ID = "modop-" + digest
	base.Key = "modkey-" + digest
	return base
}

type runtimeOperationSession struct {
	target    modpublication.TargetPlan
	operation modpublication.RuntimeOperation
}

func newRuntimeOperationSession(target modpublication.TargetPlan, operation modpublication.RuntimeOperation) (*runtimeOperationSession, error) {
	session := &runtimeOperationSession{target: target, operation: operation}
	if _, err := runtimeOperation(target, operation); err != nil {
		return nil, err
	}
	return session, nil
}

func (s *runtimeOperationSession) step(ctx context.Context, name string) (runtimedriver.Operation, error) {
	if s.operation.RenewFences != nil && installationFenceExpiresSoon(s.target, s.operation.Fences, time.Now().UTC().Add(modRuntimeLeaseRefreshWindow)) {
		fences, err := s.operation.RenewFences(ctx, append([]modpublication.Fence(nil), s.operation.Fences...))
		if err != nil {
			return runtimedriver.Operation{}, err
		}
		s.operation.Fences = append([]modpublication.Fence(nil), fences...)
	}
	base, err := runtimeOperation(s.target, s.operation)
	if err != nil {
		return runtimedriver.Operation{}, err
	}
	return runtimeStepOperation(base, name), nil
}

func installationFenceExpiresSoon(target modpublication.TargetPlan, fences []modpublication.Fence, deadline time.Time) bool {
	resource := installationResource(target.TargetID, target.InstallationID)
	for _, fence := range fences {
		if fence.RoomID == resource {
			return fence.ExpiresAt.Before(deadline)
		}
	}
	return true
}

func installationResource(targetID, installationID string) string {
	return "@mod-installation/" + shaHex([]byte(targetID+"\x00"+installationID))
}

func runtimeTarget(placement modpublication.AppliedPlacement, revision string) runtimedriver.Target {
	return runtimedriver.Target{
		TargetID: placement.TargetID, InstallationID: placement.InstallationID,
		RoomID: placement.RoomID, WorldID: placement.WorldID, Cluster: "Mods", Shard: "Installation", TopologyRevision: revision,
	}
}

func targetRuntimeTarget(target modpublication.TargetPlan, revision string) runtimedriver.Target {
	return runtimedriver.Target{TargetID: target.TargetID, InstallationID: target.InstallationID, Cluster: "Mods", Shard: "Installation", TopologyRevision: revision}
}

func transferDescriptor(prefix, operationID string, target modpublication.TargetPlan, data []byte) runtimedriver.ModUploadDescriptor {
	digest := sha256.Sum256(data)
	return runtimedriver.ModUploadDescriptor{
		UploadID:    prefix + "-" + shaHex([]byte(operationID+"\x00"+target.TargetID+"\x00"+target.InstallationID)),
		OperationID: operationID, Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
	}
}

func uploadBytes(ctx context.Context, data []byte, descriptor runtimedriver.ModUploadDescriptor, begin func(int64) (int64, error), write func(int64, []byte) (int64, error)) error {
	if descriptor.Size != int64(len(data)) || !strings.EqualFold(descriptor.SHA256, shaHex(data)) {
		return errors.New("remote plan upload descriptor does not match its payload")
	}
	offset, err := begin(0)
	if err != nil {
		return err
	}
	if offset < 0 || offset > int64(len(data)) {
		return errors.New("remote plan upload returned an invalid resume offset")
	}
	for offset < int64(len(data)) {
		end := offset + shared.MaxChunkBytes
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		next, err := write(offset, data[offset:end])
		if err != nil {
			return err
		}
		if next != end {
			return fmt.Errorf("remote plan upload returned offset %d, expected %d", next, end)
		}
		offset = next
	}
	return ctx.Err()
}
