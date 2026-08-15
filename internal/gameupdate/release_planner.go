package gameupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"dont/internal/shards"
)

var releaseIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
var releaseVersionPattern = regexp.MustCompile(`^[0-9]{1,64}$`)

type ReleasePlanner struct {
	snapshots   ReleaseSnapshotSource
	runtime     ReleaseRuntime
	latest      LatestChecker
	minimumFree uint64
	now         func() time.Time
}

func NewReleasePlanner(snapshots ReleaseSnapshotSource, runtime ReleaseRuntime, latest LatestChecker, minimumFree uint64) (*ReleasePlanner, error) {
	if snapshots == nil || runtime == nil || latest == nil {
		return nil, ErrReleaseInvalid
	}
	if minimumFree == 0 {
		minimumFree = DefaultUpdateHeadroom
	}
	return &ReleasePlanner{snapshots: snapshots, runtime: runtime, latest: latest, minimumFree: minimumFree, now: time.Now}, nil
}

func NormalizeReleasePolicy(value ReleasePolicyInput) (ReleasePolicy, error) {
	restart := true
	if value.RestartRunning != nil {
		restart = *value.RestartRunning
	}
	if value.LoadConfirmation == "" {
		value.LoadConfirmation = ReleaseLoadConfirmationLogs
	}
	if value.LoadConfirmation != ReleaseLoadConfirmationLogs && value.LoadConfirmation != ReleaseLoadConfirmationNone {
		return ReleasePolicy{}, ErrReleaseInvalid
	}
	if value.TimeoutSeconds == 0 {
		value.TimeoutSeconds = 300
	}
	if value.TimeoutSeconds < 30 || value.TimeoutSeconds > 900 {
		return ReleasePolicy{}, ErrReleaseInvalid
	}
	return ReleasePolicy{
		CleanCache: value.CleanCache, RestartRunning: restart, LoadConfirmation: value.LoadConfirmation,
		TimeoutSeconds: value.TimeoutSeconds,
	}, nil
}

func (p *ReleasePlanner) Preview(ctx context.Context, request ReleasePreviewRequest) (ReleasePlan, error) {
	policy, err := NormalizeReleasePolicy(request.Policy)
	if err != nil {
		return ReleasePlan{}, err
	}
	desired, _, err := p.latest.Check(ctx, "343050", "0")
	if err != nil || !releaseVersionPattern.MatchString(strings.TrimSpace(desired)) {
		return ReleasePlan{}, errors.Join(err, ErrReleaseInvalid)
	}
	desired = strings.TrimSpace(desired)
	if requested := strings.TrimSpace(request.DesiredVersion); requested != "" {
		if !releaseVersionPattern.MatchString(requested) {
			return ReleasePlan{}, ErrReleaseInvalid
		}
		if requested != desired {
			return ReleasePlan{}, ErrDesiredVersionChanged
		}
	}
	snapshot, err := p.snapshots.Snapshot(ctx)
	if err != nil {
		return ReleasePlan{}, err
	}
	plan := ReleasePlan{
		Version: ReleasePlanVersion, DesiredVersion: desired, TopologyRevision: snapshot.TopologyRevision,
		Policy: policy, Blockers: []ReleaseBlocker{}, Ready: true, CreatedAt: p.now().UTC(),
	}
	targets := make(map[string]*ReleaseInstallationPlan)
	roomIDs := make(map[string]bool)
	for _, observed := range snapshot.Shards {
		targetID := strings.TrimSpace(observed.Target.ID)
		installationID := strings.TrimSpace(observed.Target.Config.InstallationID)
		if targetID == "local" && installationID == "" {
			installationID = "default"
		}
		if installationID == "" {
			installationID = "unknown"
		}
		key := releaseInstallationKey(targetID, installationID)
		target := targets[key]
		if target == nil {
			target = &ReleaseInstallationPlan{
				TargetID: targetID, TargetName: observed.Target.Name, InstallationID: installationID,
				Online: observed.Target.Online || targetID == "local", InventoryFresh: observed.InventoryAvailable && !observed.InventoryStale,
				Capabilities: normalizedReleaseStrings(observed.Target.Capabilities), DesiredVersion: desired, RequiredBytes: p.minimumFree,
				Blockers: []ReleaseBlocker{},
			}
			targets[key] = target
		}
		target.Shards = append(target.Shards, ReleaseShardPlan{
			RoomID: observed.Room.ID, RoomName: observed.Room.Name, RoomDirectory: observed.Room.DirectoryName,
			WorldID: observed.World.ID, WorldName: observed.World.Name, WorldDirectory: observed.World.DirectoryName,
			IsMaster: observed.World.IsMaster, TargetID: targetID, InstallationID: installationID,
			TopologyRevision: observed.TopologyRevision,
		})
		roomIDs[observed.Room.ID] = true
		if !observed.InventoryHasShard {
			target.Blockers = append(target.Blockers, releaseShardBlocker("SHARD_INVENTORY_MISSING", "运行清单中未发现该分片", targetID, installationID, observed.Room.ID, observed.World.ID))
		}
	}
	for roomID := range roomIDs {
		plan.AffectedRoomIDs = append(plan.AffectedRoomIDs, roomID)
	}
	sort.Strings(plan.AffectedRoomIDs)
	for _, target := range targets {
		p.finishReleaseTarget(ctx, target)
		plan.Installations = append(plan.Installations, *target)
		plan.Blockers = append(plan.Blockers, target.Blockers...)
		if !target.UpToDate {
			plan.UpdateRequired = true
		}
	}
	sort.Slice(plan.Installations, func(i, j int) bool {
		return releaseInstallationKey(plan.Installations[i].TargetID, plan.Installations[i].InstallationID) < releaseInstallationKey(plan.Installations[j].TargetID, plan.Installations[j].InstallationID)
	})
	sortReleaseBlockers(plan.Blockers)
	plan.Ready = len(plan.Blockers) == 0
	plan.PlanHash, err = calculateReleasePlanHash(plan)
	if err != nil {
		return ReleasePlan{}, err
	}
	return plan, nil
}

func (p *ReleasePlanner) finishReleaseTarget(ctx context.Context, target *ReleaseInstallationPlan) {
	sort.Slice(target.Shards, func(i, j int) bool { return releaseShardKey(target.Shards[i]) < releaseShardKey(target.Shards[j]) })
	add := func(code, message string) {
		target.Blockers = append(target.Blockers, ReleaseBlocker{Code: code, Message: message, TargetID: target.TargetID, InstallationID: target.InstallationID})
	}
	if !releaseIdentityPattern.MatchString(target.TargetID) || !releaseIdentityPattern.MatchString(target.InstallationID) {
		add("INSTALLATION_IDENTITY_INVALID", "运行目标或安装标识无效")
	}
	if !target.Online {
		add("TARGET_OFFLINE", "运行目标当前离线")
	}
	if !target.InventoryFresh {
		add("INVENTORY_STALE", "运行目标清单缺失或已过期")
	}
	if target.TargetID != "local" && !containsReleaseString(target.Capabilities, RequiredUpdateCapability) {
		add("CAPABILITY_MISSING", "运行目标不支持 "+RequiredUpdateCapability)
	}
	canObserve := target.Online && target.InventoryFresh && (target.TargetID == "local" || containsReleaseString(target.Capabilities, RequiredUpdateCapability))
	if canObserve {
		observation, err := p.runtime.ObserveInstallation(ctx, *target)
		if err != nil {
			add("VERSION_OBSERVE_FAILED", "读取安装版本失败: "+err.Error())
		} else {
			target.Installed, target.CurrentVersion = observation.Installed, strings.TrimSpace(observation.CurrentVersion)
			target.AvailableBytes, target.SteamCMDAvailable, target.UpdateSupported = observation.AvailableBytes, observation.SteamCMDAvailable, observation.UpdateSupported
			target.UpToDate = target.Installed && target.CurrentVersion == target.DesiredVersion
			if !target.Installed {
				add("INSTALLATION_MISSING", "未发现 DST 专用服务器安装版本")
			}
			if !target.SteamCMDAvailable {
				add("STEAMCMD_UNAVAILABLE", "运行目标未配置可执行的 SteamCMD")
			}
			if !target.UpdateSupported {
				add("UPDATE_UNSUPPORTED", "该运行目标不支持自动更新 DST")
			}
			if target.AvailableBytes < target.RequiredBytes {
				add("DISK_INSUFFICIENT", "DST 安装所在磁盘可用空间不足")
			}
		}
	}
	for index := range target.Shards {
		shard := &target.Shards[index]
		if !canObserve {
			continue
		}
		status, err := p.runtime.Status(ctx, *shard)
		if err != nil {
			target.Blockers = append(target.Blockers, releaseShardBlocker("SHARD_STATUS_FAILED", "读取分片状态失败: "+err.Error(), target.TargetID, target.InstallationID, shard.RoomID, shard.WorldID))
			continue
		}
		shard.RuntimeState = status.State
		shard.WasRunning = status.State == string(shards.RuntimeRunning) || status.State == string(shards.RuntimeStarting)
		if shard.WasRunning {
			target.RunningShards++
		}
	}
	sortReleaseBlockers(target.Blockers)
}

func calculateReleasePlanHash(plan ReleasePlan) (string, error) {
	type canonicalShard struct {
		RoomID, RoomDirectory, WorldID, WorldDirectory string
		IsMaster                                       bool
		TargetID, InstallationID, TopologyRevision     string
		WasRunning                                     bool
	}
	type canonicalTarget struct {
		TargetID, InstallationID, CurrentVersion, DesiredVersion string
		Shards                                                   []canonicalShard
	}
	payload := struct {
		Version                          int
		DesiredVersion, TopologyRevision string
		Policy                           ReleasePolicy
		AffectedRoomIDs                  []string
		Targets                          []canonicalTarget
	}{
		Version: plan.Version, DesiredVersion: plan.DesiredVersion, TopologyRevision: plan.TopologyRevision,
		Policy: plan.Policy, AffectedRoomIDs: append([]string(nil), plan.AffectedRoomIDs...),
	}
	for _, target := range plan.Installations {
		canonical := canonicalTarget{
			TargetID: target.TargetID, InstallationID: target.InstallationID, CurrentVersion: target.CurrentVersion,
			DesiredVersion: target.DesiredVersion,
		}
		for _, shard := range target.Shards {
			canonical.Shards = append(canonical.Shards, canonicalShard{
				RoomID: shard.RoomID, RoomDirectory: shard.RoomDirectory, WorldID: shard.WorldID,
				WorldDirectory: shard.WorldDirectory, IsMaster: shard.IsMaster, TargetID: shard.TargetID,
				InstallationID: shard.InstallationID, TopologyRevision: shard.TopologyRevision,
				WasRunning: shard.WasRunning,
			})
		}
		payload.Targets = append(payload.Targets, canonical)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validateReleasePlan(plan ReleasePlan) error {
	if plan.Version != ReleasePlanVersion || !releaseVersionPattern.MatchString(plan.DesiredVersion) || len(plan.TopologyRevision) != 64 || len(plan.PlanHash) != 64 ||
		len(plan.AffectedRoomIDs) == 0 || len(plan.Installations) == 0 || plan.CreatedAt.IsZero() || !plan.Ready || len(plan.Blockers) != 0 {
		return ErrReleaseInvalid
	}
	if err := validateStoredReleasePolicy(plan.Policy); err != nil {
		return ErrReleaseInvalid
	}
	seenInstallations := make(map[string]bool)
	seenShards := make(map[string]bool)
	rooms := make(map[string]bool)
	for _, target := range plan.Installations {
		key := releaseInstallationKey(target.TargetID, target.InstallationID)
		if seenInstallations[key] || len(target.Shards) == 0 || target.DesiredVersion != plan.DesiredVersion || len(target.Blockers) != 0 || !target.Online || !target.InventoryFresh || !target.Installed || !target.SteamCMDAvailable || !target.UpdateSupported || target.AvailableBytes < target.RequiredBytes {
			return ErrReleaseInvalid
		}
		seenInstallations[key] = true
		for _, shard := range target.Shards {
			shardKey := releaseShardKey(shard)
			if seenShards[shardKey] || shard.TargetID != target.TargetID || shard.InstallationID != target.InstallationID || !releaseIdentityPattern.MatchString(shard.RoomID) || !releaseIdentityPattern.MatchString(shard.WorldID) || shard.TopologyRevision == "" {
				return ErrReleaseInvalid
			}
			seenShards[shardKey], rooms[shard.RoomID] = true, true
		}
	}
	if len(rooms) != len(plan.AffectedRoomIDs) {
		return ErrReleaseInvalid
	}
	for _, roomID := range plan.AffectedRoomIDs {
		if !rooms[roomID] {
			return ErrReleaseInvalid
		}
	}
	hash, err := calculateReleasePlanHash(plan)
	if err != nil || hash != strings.ToLower(plan.PlanHash) {
		return errors.Join(ErrReleasePlanChanged, err)
	}
	return nil
}

func validateStoredReleasePolicy(value ReleasePolicy) error {
	if value.LoadConfirmation != ReleaseLoadConfirmationLogs && value.LoadConfirmation != ReleaseLoadConfirmationNone || value.TimeoutSeconds < 30 || value.TimeoutSeconds > 900 {
		return ErrReleaseInvalid
	}
	return nil
}

func releaseInstallationKey(targetID, installationID string) string {
	return targetID + "\x00" + installationID
}

func releaseShardKey(value ReleaseShardPlan) string { return value.RoomID + "\x00" + value.WorldID }

func releaseShardBlocker(code, message, targetID, installationID, roomID, worldID string) ReleaseBlocker {
	return ReleaseBlocker{Code: code, Message: message, TargetID: targetID, InstallationID: installationID, RoomID: roomID, WorldID: worldID}
}

func normalizedReleaseStrings(values []string) []string {
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

func containsReleaseString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func sortReleaseBlockers(values []ReleaseBlocker) {
	sort.Slice(values, func(i, j int) bool {
		left := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", values[i].TargetID, values[i].InstallationID, values[i].RoomID, values[i].WorldID, values[i].Code)
		right := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", values[j].TargetID, values[j].InstallationID, values[j].RoomID, values[j].WorldID, values[j].Code)
		return left < right
	})
}
