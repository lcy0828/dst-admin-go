package configuration

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"dont/internal/configpublication"
	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/runtimefiles"
	"dont/internal/topology"
	"dont/shared"

	"github.com/go-ini/ini"
	"github.com/google/uuid"
)

const configurationPublicationLeaseTTL = 5 * time.Minute

type PublicationScope string

const (
	PublicationShared PublicationScope = "shared"
	PublicationWorld  PublicationScope = "world"
	PublicationMod    PublicationScope = "mod"
)

type PublicationRequest struct {
	RoomID        string
	WorldID       string
	Scope         PublicationScope
	Files         []string
	Payload       []rooms.ProvisionFile
	ExpectedFiles map[string]string
	IncludeLocal  bool
	ShardLinks    []topology.ShardLink
	UseShardLinks bool
}

type PublicationResult struct {
	PublicationID  string
	PublishedCount int
	Warnings       []string
}

type Publisher interface {
	Publish(context.Context, PublicationRequest) (PublicationResult, error)
}

type publicationPlacements interface {
	ResolveRoomExecutions(context.Context, string) ([]topology.ExecutionPlacement, error)
}

type publicationShardLinks interface {
	ResolveAppliedShardLinks(context.Context, string) ([]topology.ShardLink, error)
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
	placements publicationPlacements
	runtimes   publicationRuntimes
	leases     publicationLeases
	mutations  runtimedriver.RuntimeMutationObserver
}

type publicationTarget struct {
	driver  runtimedriver.ConfigurationDriver
	applier runtimedriver.ConfigurationApplier
	reader  runtimedriver.ConfigurationReader
	target  runtimedriver.Target
	archive []byte
	master  bool
}

func NewRemotePublisher(placements publicationPlacements, runtimes publicationRuntimes, leases publicationLeases) (*RemotePublisher, error) {
	if placements == nil || runtimes == nil || leases == nil {
		return nil, errors.New("configuration publication dependencies are required")
	}
	return &RemotePublisher{placements: placements, runtimes: runtimes, leases: leases}, nil
}

func (p *RemotePublisher) ConfigureMutationObserver(observer runtimedriver.RuntimeMutationObserver) error {
	if observer == nil {
		return errors.New("configuration mutation observer is required")
	}
	p.mutations = observer
	return nil
}

func (p *RemotePublisher) Publish(ctx context.Context, request PublicationRequest) (PublicationResult, error) {
	baseArchive, scope, err := p.archive(request)
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
	links := []topology.ShardLink{}
	if request.Scope == PublicationShared && containsPublicationFile(request.Files, "cluster.ini") {
		if request.UseShardLinks {
			links = append([]topology.ShardLink(nil), request.ShardLinks...)
		} else if resolver, ok := p.placements.(publicationShardLinks); ok {
			links, err = resolver.ResolveAppliedShardLinks(ctx, request.RoomID)
			if err != nil {
				return result, err
			}
		}
	}
	defer func() {
		targetIDs := make([]string, 0, len(targets))
		for _, target := range targets {
			targetIDs = append(targetIDs, target.target.TargetID)
		}
		runtimedriver.NotifyRuntimeTargetsChanged(p.mutations, targetIDs...)
	}()
	lease, err := p.leases.Acquire(ctx, request.RoomID, "configuration.publish:"+result.PublicationID, configurationPublicationLeaseTTL)
	if err != nil {
		return result, err
	}
	defer func() { _ = p.leases.Release(lease) }()
	started := make([]publicationTarget, 0, len(targets))
	crossMachine := publicationCrossesMachines(targets)
	for index, target := range targets {
		var currentCluster []byte
		if crossMachine && request.Scope == PublicationShared && containsPublicationFile(request.Files, "cluster.ini") {
			current, readErr := target.reader.ReadConfiguration(ctx, target.target, scope)
			if readErr != nil {
				return result, errors.Join(readErr, p.rollback(ctx, lease, result.PublicationID, scope, started))
			}
			if validateErr := runtimefiles.ValidateConfiguration(scope, current); validateErr != nil {
				return result, errors.Join(validateErr, p.rollback(ctx, lease, result.PublicationID, scope, started))
			}
			currentCluster, readErr = publishedConfigurationFile(current, "cluster.ini")
			if readErr != nil {
				return result, errors.Join(readErr, p.rollback(ctx, lease, result.PublicationID, scope, started))
			}
		}
		archive, renderErr := renderPublicationArchiveForTarget(baseArchive, target.target, links, target.master, crossMachine, currentCluster)
		if renderErr != nil {
			return result, errors.Join(renderErr, p.rollback(ctx, lease, result.PublicationID, scope, started))
		}
		sum := sha256.Sum256(archive)
		descriptor := runtimedriver.ConfigurationDescriptor{
			PublicationID: result.PublicationID, Scope: scope, Size: int64(len(archive)), SHA256: hex.EncodeToString(sum[:]),
		}
		if err := p.renew(ctx, &lease); err != nil {
			return result, errors.Join(err, p.rollback(ctx, lease, result.PublicationID, scope, started))
		}
		if len(targets) == 1 && target.applier != nil && len(request.ExpectedFiles) > 0 && len(archive) <= configpublication.MaxChunkBytes {
			startedAt := time.Now()
			warnings, applyErr := target.applier.ApplyConfiguration(ctx, target.target,
				publicationOperation(lease, result.PublicationID, "apply", index, 0), descriptor, archive, request.ExpectedFiles)
			log.Printf("[ConfigurationPublish] room_id=%s world_id=%s target_id=%s mode=inline duration_ms=%d failed=%t", request.RoomID, request.WorldID, target.target.TargetID, time.Since(startedAt).Milliseconds(), applyErr != nil)
			if errors.Is(applyErr, configpublication.ErrRevisionConflict) {
				return result, ErrRevisionConflict
			}
			if applyErr != nil {
				return result, applyErr
			}
			result.PublishedCount = 1
			result.Warnings = append(result.Warnings, warnings...)
			return result, nil
		}
		offset, beginErr := target.driver.BeginConfiguration(ctx, target.target, publicationOperation(lease, result.PublicationID, "begin", index, 0), descriptor)
		if beginErr != nil || offset < 0 || offset > int64(len(archive)) {
			return result, errors.Join(beginErr, p.rollback(ctx, lease, result.PublicationID, scope, started))
		}
		target.archive = archive
		targets[index] = target
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
	for _, target := range targets {
		if err := verifyPublishedConfiguration(ctx, target, scope); err != nil {
			return result, errors.Join(err, p.rollback(ctx, lease, result.PublicationID, scope, started))
		}
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

func (p *RemotePublisher) PublishModOverrides(ctx context.Context, roomID string, updates []runtimedriver.ModOverridesUpdate) (int, error) {
	if len(updates) == 0 {
		return 0, nil
	}
	publicationID := uuid.NewString()
	lease, err := p.leases.Acquire(ctx, roomID, "mod.configuration:"+publicationID, configurationPublicationLeaseTTL)
	if err != nil {
		return 0, err
	}
	defer func() { _ = p.leases.Release(lease) }()
	writers := make([]runtimedriver.ModOverridesWriter, len(updates))
	for index, update := range updates {
		if update.Target.RoomID != roomID || update.ExpectedSHA256 == "" {
			return 0, ErrInvalidConfiguration
		}
		var driver runtimedriver.Driver
		if placements, ok := p.placements.(interface {
			AppliedPlacement(string, string) (topology.ExecutionPlacement, error)
		}); ok {
			current, err := placements.AppliedPlacement(roomID, update.Target.WorldID)
			if err != nil {
				return 0, err
			}
			currentTarget := runtimedriver.Target{
				TargetID: current.AppliedTargetID, InstallationID: strings.TrimSpace(current.AppliedInstallationID),
				TopologyRevision: current.Revision,
			}
			if currentTarget.InstallationID == "" {
				// Legacy placements use the Runtime's default installation, just as reads do.
				driver, currentTarget, err = p.runtimes.DriverTarget(ctx, roomID, update.Target.WorldID)
				if err != nil {
					return 0, err
				}
			}
			if currentTarget.TopologyRevision != update.Target.TopologyRevision || currentTarget.TargetID != update.Target.TargetID || currentTarget.InstallationID != update.Target.InstallationID {
				return 0, fmt.Errorf("Mod 配置的运行位置已改变，请刷新后重试: %w", runtimedriver.ErrTopologyChanged)
			}
		}
		if driver == nil {
			if runtimes, ok := p.runtimes.(interface {
				TrustedTarget(runtimedriver.Target) (runtimedriver.Driver, error)
			}); ok {
				driver, err = runtimes.TrustedTarget(update.Target)
			} else {
				driver, _, err = p.runtimes.DriverTarget(ctx, roomID, update.Target.WorldID)
			}
			if err != nil {
				return 0, err
			}
		}
		writer, ok := driver.(runtimedriver.ModOverridesWriter)
		if !ok {
			return 0, errors.New("运行节点不支持直接保存 Mod 配置，请升级 Agent")
		}
		writers[index] = writer
	}
	published := 0
	for index, update := range updates {
		started := time.Now()
		err := writers[index].WriteModOverrides(ctx, update.Target, publicationOperation(lease, publicationID, "mod-write", index, 0), update.ExpectedSHA256, update.Content)
		log.Printf("[ModConfigWrite] room_id=%s world_id=%s target_id=%s installation_id=%s bytes=%d duration_ms=%d error=%v", roomID, update.Target.WorldID, update.Target.TargetID, update.Target.InstallationID, len(update.Content), time.Since(started).Milliseconds(), err)
		if err != nil {
			return published, fmt.Errorf("已保存 %d 个世界，写入 %s 的 modoverrides.lua 失败: %w", published, update.Target.Shard, err)
		}
		published++
		runtimedriver.NotifyRuntimeTargetsChanged(p.mutations, update.Target.TargetID)
	}
	return published, nil
}

func containsPublicationFile(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func renderPublicationArchiveForTarget(
	source []byte,
	target runtimedriver.Target,
	links []topology.ShardLink,
	master bool,
	crossMachine bool,
	currentCluster []byte,
) ([]byte, error) {
	if !crossMachine {
		return source, nil
	}
	var selected *topology.ShardLink
	for index := range links {
		link := &links[index]
		if link.SourceTargetID == target.TargetID && link.SourceInstallationID == target.InstallationID {
			selected = link
			break
		}
	}
	reader, err := zip.NewReader(bytes.NewReader(source), int64(len(source)))
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	foundCluster := false
	for _, file := range reader.File {
		entry, err := file.Open()
		if err != nil {
			_ = writer.Close()
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(entry, 4*1024*1024+1))
		closeErr := entry.Close()
		if readErr != nil || closeErr != nil || len(data) > 4*1024*1024 {
			_ = writer.Close()
			return nil, errors.Join(readErr, closeErr, ErrInvalidConfiguration)
		}
		if file.Name == "cluster.ini" {
			foundCluster = true
			config, loadErr := composeTargetRoomConfiguration(data, currentCluster)
			if loadErr != nil {
				_ = writer.Close()
				return nil, loadErr
			}
			section := config.Section("SHARD")
			switch {
			case selected != nil:
				section.Key("bind_ip").SetValue("0.0.0.0")
				section.Key("master_ip").SetValue(selected.Address)
				section.Key("master_port").SetValue(strconv.Itoa(selected.Port))
			case master:
				section.Key("bind_ip").SetValue("0.0.0.0")
			default:
				if len(currentCluster) == 0 {
					_ = writer.Close()
					return nil, fmt.Errorf("configuration publication target %s/%s has no applied Shard link or Runtime configuration", target.TargetID, target.InstallationID)
				}
				current, currentErr := ini.Load(currentCluster)
				if currentErr != nil {
					_ = writer.Close()
					return nil, currentErr
				}
				preserveShardTopology(section, current.Section("SHARD"))
			}
			var rendered bytes.Buffer
			if _, writeErr := config.WriteTo(&rendered); writeErr != nil {
				_ = writer.Close()
				return nil, writeErr
			}
			data = rendered.Bytes()
		}
		header := file.FileHeader
		header.SetMode(0o600)
		destination, createErr := writer.CreateHeader(&header)
		if createErr != nil {
			_ = writer.Close()
			return nil, createErr
		}
		if _, writeErr := destination.Write(data); writeErr != nil {
			_ = writer.Close()
			return nil, writeErr
		}
	}
	if !foundCluster {
		_ = writer.Close()
		return nil, ErrInvalidConfiguration
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func composeTargetRoomConfiguration(source, current []byte) (*ini.File, error) {
	sourceConfig, err := ini.Load(source)
	if err != nil {
		return nil, err
	}
	if len(current) == 0 {
		return sourceConfig, nil
	}
	targetConfig, err := ini.Load(current)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(roomKnownKeys))
	for _, section := range sourceConfig.Sections() {
		for _, key := range section.Keys() {
			identity := section.Name() + "\x00" + key.Name()
			if !roomKnownKeys[identity] {
				continue
			}
			seen[identity] = true
			targetConfig.Section(section.Name()).Key(key.Name()).SetValue(key.String())
		}
	}
	for identity := range roomKnownKeys {
		if seen[identity] {
			continue
		}
		sectionName, keyName, found := strings.Cut(identity, "\x00")
		if !found {
			continue
		}
		section, sectionErr := targetConfig.GetSection(sectionName)
		if sectionErr == nil {
			section.DeleteKey(keyName)
		}
	}
	return targetConfig, nil
}

func preserveShardTopology(destination, source *ini.Section) {
	for _, name := range []string{"bind_ip", "master_ip", "master_port"} {
		if source.HasKey(name) {
			destination.Key(name).SetValue(source.Key(name).String())
		} else {
			destination.DeleteKey(name)
		}
	}
}

func publishedConfigurationFile(result shared.RuntimeConfigurationResult, name string) ([]byte, error) {
	for _, file := range result.Files {
		if file.Name == name && file.Exists {
			return append([]byte(nil), file.Data...), nil
		}
	}
	return nil, fmt.Errorf("Runtime configuration file %s is missing", name)
}

func publicationCrossesMachines(targets []publicationTarget) bool {
	values := make(map[string]bool, len(targets))
	for _, target := range targets {
		values[target.target.TargetID] = true
	}
	return len(values) > 1
}

func (p *RemotePublisher) archive(request PublicationRequest) ([]byte, string, error) {
	if strings.TrimSpace(request.RoomID) == "" || request.Scope != PublicationShared && request.Scope != PublicationWorld && request.Scope != PublicationMod || len(request.Files) == 0 || len(request.Payload) == 0 {
		return nil, "", ErrInvalidConfiguration
	}
	available := make(map[string]rooms.ProvisionFile)
	for _, file := range request.Payload {
		if _, exists := available[file.Name]; exists || !publicationFileAllowed(request.Scope, file.Name) || file.Mode.Perm() == 0 || len(file.Data) > 4*1024*1024 {
			return nil, "", ErrInvalidConfiguration
		}
		available[file.Name] = rooms.ProvisionFile{Name: file.Name, Data: append([]byte(nil), file.Data...), Mode: file.Mode.Perm()}
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
	seen := make(map[string]int)
	for _, execution := range executions {
		if (request.Scope == PublicationWorld || request.Scope == PublicationMod) && execution.World.ID != request.WorldID {
			continue
		}
		driver, target, err := p.runtimes.DriverTarget(ctx, request.RoomID, execution.World.ID)
		if err != nil {
			return nil, err
		}
		if target.TargetID == "local" && !request.IncludeLocal {
			continue
		}
		key := target.TargetID + "\x00" + target.InstallationID + "\x00" + target.Cluster
		if request.Scope == PublicationWorld || request.Scope == PublicationMod {
			key += "\x00" + target.Shard
		}
		if existing, ok := seen[key]; ok {
			if execution.World.Role == rooms.WorldRoleMaster || execution.World.IsMaster {
				result[existing].master = true
			}
			continue
		}
		publisher, publishOK := driver.(runtimedriver.ConfigurationDriver)
		reader, readOK := driver.(runtimedriver.ConfigurationReader)
		if !publishOK || !readOK || !runtimedriver.HasTargetCapability(driver, target, runtimedriver.CapabilityConfigPublish) ||
			!runtimedriver.HasTargetCapability(driver, target, runtimedriver.CapabilityConfigRead) {
			return nil, runtimedriver.ErrCapabilityMissing
		}
		seen[key] = len(result)
		var applier runtimedriver.ConfigurationApplier
		if runtimedriver.HasTargetCapability(driver, target, runtimedriver.CapabilityConfigApply) {
			applier, _ = driver.(runtimedriver.ConfigurationApplier)
		}
		result = append(result, publicationTarget{
			driver: publisher, applier: applier, reader: reader, target: target,
			master: execution.World.Role == rooms.WorldRoleMaster || execution.World.IsMaster,
		})
	}
	if (request.Scope == PublicationWorld || request.Scope == PublicationMod) && len(result) == 0 {
		for _, execution := range executions {
			if execution.World.ID == request.WorldID && execution.AppliedTargetID == "local" {
				return result, nil
			}
		}
		return nil, rooms.ErrWorldNotFound
	}
	return result, nil
}

func verifyPublishedConfiguration(ctx context.Context, target publicationTarget, scope string) error {
	expected, err := publicationArchiveFiles(target.archive)
	if err != nil {
		return err
	}
	nonSecret := make(map[string][]byte, len(expected))
	for name, data := range expected {
		if name != "cluster_token.txt" {
			nonSecret[name] = data
		}
	}
	if len(nonSecret) > 0 {
		observed, readErr := target.reader.ReadConfiguration(ctx, target.target, scope)
		if readErr != nil {
			return fmt.Errorf("发布后回读 %s/%s: %w", target.target.TargetID, target.target.InstallationID, readErr)
		}
		if err := runtimefiles.ValidateConfiguration(scope, observed); err != nil {
			return err
		}
		actual := make(map[string][]byte, len(observed.Files))
		for _, file := range observed.Files {
			if file.Exists {
				actual[file.Name] = file.Data
			}
		}
		for name, data := range nonSecret {
			if !bytes.Equal(actual[name], data) {
				return fmt.Errorf("发布后回读 %s/%s 的 %s 内容不一致", target.target.TargetID, target.target.InstallationID, name)
			}
		}
	}
	if data, ok := expected["cluster_token.txt"]; ok {
		observed, readErr := target.reader.ReadConfiguration(ctx, target.target, runtimefiles.ConfigurationScopeTokenStatus)
		if readErr != nil {
			return fmt.Errorf("发布后回读 %s/%s 的 Cluster Token: %w", target.target.TargetID, target.target.InstallationID, readErr)
		}
		if err := runtimefiles.ValidateConfiguration(runtimefiles.ConfigurationScopeTokenStatus, observed); err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(strings.TrimSpace(string(data))))
		if observed.TokenStatus == nil || !observed.TokenStatus.Exists || !strings.EqualFold(observed.TokenStatus.SHA256, hex.EncodeToString(digest[:])) {
			return fmt.Errorf("发布后回读 %s/%s 的 Cluster Token 内容不一致", target.target.TargetID, target.target.InstallationID)
		}
	}
	return nil
}

func publicationArchiveFiles(data []byte) (map[string][]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	result := make(map[string][]byte, len(reader.File))
	for _, file := range reader.File {
		entry, openErr := file.Open()
		if openErr != nil {
			return nil, openErr
		}
		value, readErr := io.ReadAll(io.LimitReader(entry, 4*1024*1024+1))
		closeErr := entry.Close()
		if readErr != nil || closeErr != nil || len(value) > 4*1024*1024 {
			return nil, errors.Join(readErr, closeErr, ErrInvalidConfiguration)
		}
		result[file.Name] = value
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
	if scope == PublicationWorld {
		return name == "server.ini" || name == "leveldataoverride.lua"
	}
	return scope == PublicationMod && name == "modoverrides.lua"
}
