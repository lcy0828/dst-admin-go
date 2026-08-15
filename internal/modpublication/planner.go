package modpublication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Planner struct {
	catalog        Catalog
	placements     Placement
	content        ContentSource
	runtime        Runtime
	minimumVersion string
	now            func() time.Time
}

func NewPlanner(catalog Catalog, placements Placement, content ContentSource, runtime Runtime, minimumVersion string) (*Planner, error) {
	if catalog == nil || placements == nil || content == nil || runtime == nil {
		return nil, ErrInvalidInput
	}
	minimumVersion = strings.TrimSpace(minimumVersion)
	if minimumVersion != "" {
		if _, ok := parseVersion(minimumVersion); !ok {
			return nil, ErrInvalidInput
		}
	}
	return &Planner{catalog: catalog, placements: placements, content: content, runtime: runtime, minimumVersion: minimumVersion, now: time.Now}, nil
}

func (p *Planner) Preview(ctx context.Context, roomID string) (Plan, error) {
	roomID = strings.TrimSpace(roomID)
	if !validID(roomID) {
		return Plan{}, ErrInvalidInput
	}
	worlds, err := p.catalog.ManagedWorlds(ctx)
	if err != nil {
		return Plan{}, err
	}
	snapshot, err := p.placements.AppliedPlacements(ctx)
	if err != nil {
		return Plan{}, err
	}
	if !validID(snapshot.TopologyRevision) {
		return Plan{}, ErrInvalidInput
	}
	worldByKey := make(map[string]ManagedWorld, len(worlds))
	for _, world := range worlds {
		if err := validateWorld(world); err != nil {
			return Plan{}, err
		}
		key := worldKey(world.RoomID, world.WorldID)
		if _, duplicate := worldByKey[key]; duplicate {
			return Plan{}, ErrConflict
		}
		worldByKey[key] = world
	}
	placementByWorld := make(map[string]AppliedPlacement, len(snapshot.Placements))
	requestedInstallations := make(map[string]bool)
	requestedWorlds := 0
	for _, placement := range snapshot.Placements {
		if err := validatePlacement(placement); err != nil {
			return Plan{}, err
		}
		key := worldKey(placement.RoomID, placement.WorldID)
		if _, duplicate := placementByWorld[key]; duplicate {
			return Plan{}, ErrConflict
		}
		placementByWorld[key] = placement
		if placement.RoomID == roomID {
			if _, managed := worldByKey[key]; managed {
				requestedWorlds++
				requestedInstallations[installationKey(placement)] = true
			}
		}
	}
	if requestedWorlds == 0 {
		return Plan{}, ErrNotFound
	}
	targets := make(map[string]*TargetPlan)
	artifacts := make(map[string]ContentArtifact)
	affectedRooms := make(map[string]bool)
	for key, world := range worldByKey {
		placement, exists := placementByWorld[key]
		if !exists || !requestedInstallations[installationKey(placement)] {
			continue
		}
		affectedRooms[world.RoomID] = true
		targetKey := installationKey(placement)
		target := targets[targetKey]
		if target == nil {
			target = &TargetPlan{TargetID: placement.TargetID, NodeID: placement.NodeID, InstallationID: placement.InstallationID, MinimumVersion: p.minimumVersion}
			targets[targetKey] = target
		}
		worldPlan := WorldPlan{
			RoomID: world.RoomID, RoomDirectory: world.RoomDirectory, WorldID: world.WorldID,
			WorldDirectory: world.WorldDirectory, IsMaster: world.IsMaster, ModOverrides: append([]byte(nil), world.ModOverrides...),
		}
		for _, requirement := range world.Mods {
			cacheKey := requirement.WorkshopID + ":" + strings.ToLower(requirement.TreeSHA256)
			artifact, exists := artifacts[cacheKey]
			if !exists {
				artifact, err = p.content.Resolve(ctx, requirement)
				if err != nil {
					return Plan{}, err
				}
				if err := validateArtifact(requirement, artifact); err != nil {
					return Plan{}, err
				}
				artifact.TreeSHA256 = strings.ToLower(artifact.TreeSHA256)
				artifact.ManifestSHA256 = strings.ToLower(artifact.ManifestSHA256)
				artifacts[cacheKey] = artifact
			}
			worldPlan.Mods = append(worldPlan.Mods, artifact)
		}
		sortArtifacts(worldPlan.Mods)
		target.Worlds = append(target.Worlds, worldPlan)
	}
	plan := Plan{
		Version: PlanVersion, RoomID: roomID, TopologyRevision: snapshot.TopologyRevision,
		Ready: true, RestartRequired: len(targets) > 0, CreatedAt: p.now().UTC(),
	}
	for room := range affectedRooms {
		plan.AffectedRoomIDs = append(plan.AffectedRoomIDs, room)
	}
	sort.Strings(plan.AffectedRoomIDs)
	for _, target := range targets {
		if err := p.finishTarget(ctx, target); err != nil {
			return Plan{}, err
		}
		plan.Targets = append(plan.Targets, *target)
		plan.Blockers = append(plan.Blockers, target.Blockers...)
	}
	sort.Slice(plan.Targets, func(i, j int) bool {
		return targetPlanKey(plan.Targets[i]) < targetPlanKey(plan.Targets[j])
	})
	sortBlockers(plan.Blockers)
	plan.Ready = len(plan.Blockers) == 0
	plan.PlanHash, err = calculatePlanHash(plan)
	if err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func (p *Planner) finishTarget(ctx context.Context, target *TargetPlan) error {
	sort.Slice(target.Worlds, func(i, j int) bool {
		return worldKey(target.Worlds[i].RoomID, target.Worlds[i].WorldID) < worldKey(target.Worlds[j].RoomID, target.Worlds[j].WorldID)
	})
	versions := make(map[string]ContentArtifact)
	for _, world := range target.Worlds {
		for _, artifact := range world.Mods {
			if current, exists := versions[artifact.WorkshopID]; exists && current.TreeSHA256 != artifact.TreeSHA256 {
				return fmt.Errorf("%w: target %s installation %s Workshop %s uses %s and %s", ErrVersionConflict, target.TargetID, target.InstallationID, artifact.WorkshopID, current.TreeSHA256, artifact.TreeSHA256)
			}
			versions[artifact.WorkshopID] = artifact
		}
	}
	for _, artifact := range versions {
		target.Mods = append(target.Mods, artifact)
	}
	sortArtifacts(target.Mods)
	placement := AppliedPlacement{TargetID: target.TargetID, NodeID: target.NodeID, InstallationID: target.InstallationID}
	observation, err := p.runtime.Observe(ctx, placement)
	if err != nil {
		target.Blockers = append(target.Blockers, blocker("TARGET_OBSERVE_FAILED", "运行目标状态获取失败: "+err.Error(), *target))
		return nil
	}
	if observation.TargetID != target.TargetID || observation.NodeID != target.NodeID || observation.InstallationID != target.InstallationID || observation.AvailableBytes < 0 {
		return ErrInvalidInput
	}
	target.Online = observation.Online
	target.Capabilities = normalizedStrings(observation.Capabilities)
	target.RuntimeVersion = strings.TrimSpace(observation.Version)
	target.AvailableBytes = observation.AvailableBytes
	if !target.Online {
		target.Blockers = append(target.Blockers, blocker("TARGET_OFFLINE", "运行目标当前离线", *target))
	}
	if !contains(target.Capabilities, RequiredCapability) {
		target.Blockers = append(target.Blockers, blocker("CAPABILITY_MISSING", "运行目标不支持 "+RequiredCapability, *target))
	}
	if p.minimumVersion != "" && versionLess(target.RuntimeVersion, p.minimumVersion) {
		target.Blockers = append(target.Blockers, blocker("RUNTIME_VERSION_BLOCKED", "运行时版本低于 "+p.minimumVersion, *target))
	}
	for _, artifact := range target.Mods {
		if observation.CachedTreeSHA256 != nil && observation.CachedTreeSHA256[artifact.TreeSHA256] {
			continue
		}
		if target.RequiredBytes > (1<<63-1)-artifact.Size {
			return ErrInvalidInput
		}
		target.RequiredBytes += artifact.Size
	}
	if target.RequiredBytes > target.AvailableBytes {
		target.Blockers = append(target.Blockers, blocker("DISK_INSUFFICIENT", "目标磁盘空间不足", *target))
	}
	sortBlockers(target.Blockers)
	return nil
}

func calculatePlanHash(plan Plan) (string, error) {
	type canonicalWorld struct {
		RoomID, RoomDirectory, WorldID, WorldDirectory, ConfigSHA256 string
		Mods                                                         []ContentArtifact
	}
	type canonicalTarget struct {
		TargetID, NodeID, InstallationID string
		Mods                             []ContentArtifact
		Worlds                           []canonicalWorld
	}
	canonical := struct {
		Version                  int
		RoomID, TopologyRevision string
		AffectedRoomIDs          []string
		Targets                  []canonicalTarget
	}{Version: plan.Version, RoomID: plan.RoomID, TopologyRevision: plan.TopologyRevision, AffectedRoomIDs: append([]string(nil), plan.AffectedRoomIDs...)}
	for _, target := range plan.Targets {
		item := canonicalTarget{TargetID: target.TargetID, NodeID: target.NodeID, InstallationID: target.InstallationID, Mods: canonicalArtifacts(target.Mods)}
		for _, world := range target.Worlds {
			item.Worlds = append(item.Worlds, canonicalWorld{
				RoomID: world.RoomID, RoomDirectory: world.RoomDirectory, WorldID: world.WorldID,
				WorldDirectory: world.WorldDirectory, ConfigSHA256: hashBytes(world.ModOverrides), Mods: canonicalArtifacts(world.Mods),
			})
		}
		canonical.Targets = append(canonical.Targets, item)
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return hashBytes(encoded), nil
}

func canonicalArtifacts(values []ContentArtifact) []ContentArtifact {
	result := append([]ContentArtifact(nil), values...)
	sortArtifacts(result)
	return result
}

func validatePlan(plan Plan) error {
	if plan.Version != PlanVersion || !validID(plan.RoomID) || !validID(plan.TopologyRevision) || !validSHA(plan.PlanHash) || len(plan.Targets) == 0 || len(plan.AffectedRoomIDs) == 0 || plan.CreatedAt.IsZero() || !plan.RestartRequired {
		return ErrInvalidInput
	}
	affected := normalizedStrings(plan.AffectedRoomIDs)
	if len(affected) != len(plan.AffectedRoomIDs) || !contains(affected, plan.RoomID) {
		return ErrInvalidInput
	}
	targets := make(map[string]bool, len(plan.Targets))
	worlds := make(map[string]bool)
	rooms := make(map[string]bool)
	aggregatedBlockers := make([]Blocker, 0)
	for _, target := range plan.Targets {
		if !validID(target.TargetID) || !validID(target.NodeID) || !validID(target.InstallationID) || target.AvailableBytes < 0 || target.RequiredBytes < 0 || len(target.Worlds) == 0 {
			return ErrInvalidInput
		}
		key := targetPlanKey(target)
		if targets[key] {
			return ErrConflict
		}
		targets[key] = true
		union := make(map[string]string, len(target.Mods))
		for _, artifact := range target.Mods {
			if err := validateStoredArtifact(artifact); err != nil {
				return err
			}
			if prior, duplicate := union[artifact.WorkshopID]; duplicate && prior != artifact.TreeSHA256 || duplicate {
				return ErrVersionConflict
			}
			union[artifact.WorkshopID] = artifact.TreeSHA256
		}
		desiredUnion := make(map[string]string)
		for _, world := range target.Worlds {
			if !validID(world.RoomID) || !safeComponent(world.RoomDirectory) || !validID(world.WorldID) || !safeComponent(world.WorldDirectory) || len(world.ModOverrides) > 8<<20 || strings.IndexByte(string(world.ModOverrides), 0) >= 0 {
				return ErrInvalidInput
			}
			worldKey := worldKey(world.RoomID, world.WorldID)
			if worlds[worldKey] {
				return ErrConflict
			}
			worlds[worldKey], rooms[world.RoomID] = true, true
			seenMods := make(map[string]bool)
			for _, artifact := range world.Mods {
				if err := validateStoredArtifact(artifact); err != nil || seenMods[artifact.WorkshopID] {
					return errors.Join(ErrInvalidInput, err)
				}
				seenMods[artifact.WorkshopID] = true
				if prior, exists := desiredUnion[artifact.WorkshopID]; exists && prior != artifact.TreeSHA256 {
					return ErrVersionConflict
				}
				desiredUnion[artifact.WorkshopID] = artifact.TreeSHA256
			}
		}
		if len(union) != len(desiredUnion) {
			return ErrInvalidInput
		}
		for workshopID, treeSHA := range desiredUnion {
			if union[workshopID] != treeSHA {
				return ErrInvalidInput
			}
		}
		for _, blocker := range target.Blockers {
			if blocker.TargetID != target.TargetID || blocker.InstallationID != target.InstallationID || !validID(blocker.Code) || strings.TrimSpace(blocker.Message) == "" {
				return ErrInvalidInput
			}
		}
		aggregatedBlockers = append(aggregatedBlockers, target.Blockers...)
	}
	if len(rooms) != len(affected) {
		return ErrInvalidInput
	}
	for _, roomID := range affected {
		if !rooms[roomID] {
			return ErrInvalidInput
		}
	}
	sortBlockers(aggregatedBlockers)
	providedBlockers := append([]Blocker(nil), plan.Blockers...)
	sortBlockers(providedBlockers)
	if !sameBlockers(aggregatedBlockers, providedBlockers) || plan.Ready != (len(providedBlockers) == 0) {
		return ErrInvalidInput
	}
	hash, err := calculatePlanHash(plan)
	if err != nil || hash != strings.ToLower(plan.PlanHash) {
		return errors.Join(ErrPlanChanged, err)
	}
	return nil
}

func validateStoredArtifact(artifact ContentArtifact) error {
	if !validWorkshopID(artifact.WorkshopID) || !validSHA(artifact.TreeSHA256) || !validSHA(artifact.ManifestSHA256) || artifact.Size < 0 || artifact.FileCount < 0 || strings.TrimSpace(artifact.SourceRef) == "" {
		return ErrInvalidInput
	}
	return nil
}

func sameBlockers(first, second []Blocker) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func validateWorld(world ManagedWorld) error {
	if !validID(world.RoomID) || !safeComponent(world.RoomDirectory) || !validID(world.WorldID) || !safeComponent(world.WorldDirectory) || len(world.ModOverrides) > 8<<20 {
		return ErrInvalidInput
	}
	seen := make(map[string]bool)
	for _, mod := range world.Mods {
		if !validWorkshopID(mod.WorkshopID) || mod.TreeSHA256 != "" && !validSHA(mod.TreeSHA256) || seen[mod.WorkshopID] {
			return ErrInvalidInput
		}
		seen[mod.WorkshopID] = true
	}
	return nil
}

func validatePlacement(value AppliedPlacement) error {
	if !validID(value.RoomID) || !validID(value.WorldID) || !validID(value.TargetID) || !validID(value.NodeID) || !validID(value.InstallationID) {
		return ErrInvalidInput
	}
	return nil
}

func validateArtifact(requirement ModRequirement, artifact ContentArtifact) error {
	if artifact.WorkshopID != requirement.WorkshopID || !validWorkshopID(artifact.WorkshopID) || !validSHA(artifact.TreeSHA256) || !validSHA(artifact.ManifestSHA256) || artifact.Size < 0 || artifact.FileCount < 0 || strings.TrimSpace(artifact.SourceRef) == "" {
		return ErrInvalidInput
	}
	if requirement.TreeSHA256 != "" && !strings.EqualFold(requirement.TreeSHA256, artifact.TreeSHA256) {
		return ErrPlanChanged
	}
	artifact.TreeSHA256 = strings.ToLower(artifact.TreeSHA256)
	return nil
}

func sortArtifacts(values []ContentArtifact) {
	sort.Slice(values, func(i, j int) bool {
		if values[i].WorkshopID == values[j].WorkshopID {
			return values[i].TreeSHA256 < values[j].TreeSHA256
		}
		return values[i].WorkshopID < values[j].WorkshopID
	})
}

func blocker(code, message string, target TargetPlan) Blocker {
	return Blocker{Code: code, Message: message, TargetID: target.TargetID, InstallationID: target.InstallationID}
}

func sortBlockers(values []Blocker) {
	sort.Slice(values, func(i, j int) bool {
		first := values[i].TargetID + "\x00" + values[i].InstallationID + "\x00" + values[i].Code
		second := values[j].TargetID + "\x00" + values[j].InstallationID + "\x00" + values[j].Code
		return first < second
	})
}

func installationKey(value AppliedPlacement) string {
	return value.TargetID + "\x00" + value.InstallationID
}
func targetPlanKey(value TargetPlan) string  { return value.TargetID + "\x00" + value.InstallationID }
func worldKey(roomID, worldID string) string { return roomID + "\x00" + worldID }

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validSHA(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validWorkshopID(value string) bool {
	if value == "" || value[0] == '0' || len(value) > 20 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validID(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 128 && !strings.ContainsAny(value, "\x00\r\n")
}

func safeComponent(value string) bool {
	return validID(value) && value != "." && value != ".." && !strings.ContainsAny(value, "/\\")
}

func normalizedStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func contains(values []string, expected string) bool {
	index := sort.SearchStrings(values, expected)
	return index < len(values) && values[index] == expected
}

func parseVersion(value string) ([3]int, bool) {
	var result [3]int
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(value), "v"), ".", 4)
	if len(parts) < 2 {
		return result, false
	}
	for index := 0; index < len(parts) && index < 3; index++ {
		part := parts[index]
		if separator := strings.IndexAny(part, "-+"); separator >= 0 {
			part = part[:separator]
		}
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 0 {
			return result, false
		}
		result[index] = parsed
	}
	return result, true
}

func versionLess(current, minimum string) bool {
	currentValue, currentOK := parseVersion(current)
	minimumValue, minimumOK := parseVersion(minimum)
	if !currentOK || !minimumOK {
		return true
	}
	for index := range currentValue {
		if currentValue[index] != minimumValue[index] {
			return currentValue[index] < minimumValue[index]
		}
	}
	return false
}
