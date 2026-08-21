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

	dstinstall "dont/internal/dstserver"
	"dont/internal/shards"
	"dont/shared"
)

var releaseIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
var releaseVersionPattern = regexp.MustCompile(`^[0-9]{1,64}$`)
var releaseAppIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

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
	requested := strings.TrimSpace(request.DesiredVersion)
	if requested != "" && !releaseVersionPattern.MatchString(requested) {
		return ReleasePlan{}, ErrReleaseInvalid
	}
	snapshot, err := p.snapshots.Snapshot(ctx)
	if err != nil {
		return ReleasePlan{}, err
	}
	plan := ReleasePlan{
		Version: ReleasePlanVersion, DesiredVersion: requested, TopologyRevision: snapshot.TopologyRevision,
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
				Capabilities: normalizedReleaseStrings(observed.Target.Capabilities), RequiredBytes: p.minimumFree,
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
		if err := p.finishReleaseTarget(ctx, target, requested); err != nil {
			return ReleasePlan{}, err
		}
		plan.Installations = append(plan.Installations, *target)
		plan.Blockers = append(plan.Blockers, target.Blockers...)
		if !target.UpToDate {
			plan.UpdateRequired = true
		}
	}
	sort.Slice(plan.Installations, func(i, j int) bool {
		return releaseInstallationKey(plan.Installations[i].TargetID, plan.Installations[i].InstallationID) < releaseInstallationKey(plan.Installations[j].TargetID, plan.Installations[j].InstallationID)
	})
	if plan.DesiredVersion == "" {
		plan.DesiredVersion = preferredReleaseVersion(plan.Installations)
	}
	if plan.DesiredVersion == "" {
		desired, _, latestErr := p.latest.Check(ctx, dstinstall.AppIDDedicatedServer, "0")
		if latestErr != nil {
			return ReleasePlan{}, errors.Join(ErrLatestBuildUnavailable, latestErr)
		}
		plan.DesiredVersion = strings.TrimSpace(desired)
	}
	if !releaseVersionPattern.MatchString(plan.DesiredVersion) {
		return ReleasePlan{}, errors.Join(ErrLatestBuildUnavailable, errors.New("Steam did not return a valid public build"))
	}
	sortReleaseBlockers(plan.Blockers)
	plan.Ready = len(plan.Blockers) == 0
	plan.PlanHash, err = calculateReleasePlanHash(plan)
	if err != nil {
		return ReleasePlan{}, err
	}
	return plan, nil
}

func (p *ReleasePlanner) finishReleaseTarget(ctx context.Context, target *ReleaseInstallationPlan, requested string) error {
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
	target.AppID, target.UpdateMethod = dstinstall.AppIDDedicatedServer, dstinstall.UpdateMethodSteamCMD
	observedSuccessfully := false
	if canObserve {
		observation, err := p.runtime.ObserveInstallation(ctx, *target)
		if err != nil {
			add("VERSION_OBSERVE_FAILED", "读取安装版本失败: "+err.Error())
		} else {
			observedSuccessfully = true
			target.Installed, target.CurrentVersion = observation.Installed, strings.TrimSpace(observation.CurrentVersion)
			target.AppID, target.UpdateMethod = normalizeReleaseInstallationMetadata(observation)
			target.AvailableBytes, target.SteamCMDAvailable, target.UpdateSupported = observation.AvailableBytes, observation.SteamCMDAvailable, observation.UpdateSupported
		}
	}
	queryVersion := target.CurrentVersion
	if queryVersion == "" {
		queryVersion = "0"
	}
	desired, upToDate, err := p.latest.Check(ctx, target.AppID, queryVersion)
	if err != nil {
		return errors.Join(ErrLatestBuildUnavailable, err)
	}
	target.DesiredVersion = strings.TrimSpace(desired)
	if !releaseVersionPattern.MatchString(target.DesiredVersion) {
		return errors.Join(ErrLatestBuildUnavailable, errors.New("Steam did not return a valid public build"))
	}
	if requested != "" && target.AppID == dstinstall.AppIDDedicatedServer {
		if requested != target.DesiredVersion {
			return ErrDesiredVersionChanged
		}
		target.DesiredVersion = requested
	}
	target.UpToDate = target.Installed && (upToDate || target.CurrentVersion == target.DesiredVersion)
	if observedSuccessfully {
		if !target.Installed {
			add("INSTALLATION_MISSING", "未发现 DST 专用服务器安装版本")
		}
		if !target.UpToDate {
			switch target.UpdateMethod {
			case dstinstall.UpdateMethodSteamClient:
				add("STEAM_CLIENT_UPDATE_REQUIRED", "该安装由 Steam 客户端管理，请先在 Steam 中完成更新")
			default:
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
	return nil
}

func normalizeReleaseInstallationMetadata(value shared.RuntimeGameVersionResult) (string, string) {
	appID := strings.TrimSpace(value.AppID)
	if !releaseAppIDPattern.MatchString(appID) {
		appID = dstinstall.AppIDDedicatedServer
	}
	method := strings.TrimSpace(value.UpdateMethod)
	if method == "" {
		if appID == dstinstall.AppIDGame {
			method = dstinstall.UpdateMethodSteamClient
		} else {
			method = dstinstall.UpdateMethodSteamCMD
		}
	}
	return appID, method
}

func preferredReleaseVersion(values []ReleaseInstallationPlan) string {
	for _, value := range values {
		if value.AppID == dstinstall.AppIDDedicatedServer && releaseVersionPattern.MatchString(value.DesiredVersion) {
			return value.DesiredVersion
		}
	}
	for _, value := range values {
		if releaseVersionPattern.MatchString(value.DesiredVersion) {
			return value.DesiredVersion
		}
	}
	return ""
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
		AppID                                                    string `json:",omitempty"`
		UpdateMethod                                             string `json:",omitempty"`
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
			DesiredVersion: target.DesiredVersion, AppID: target.AppID, UpdateMethod: target.UpdateMethod,
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
		metadataPresent := target.AppID != "" || target.UpdateMethod != ""
		metadataInvalid := metadataPresent && (!releaseAppIDPattern.MatchString(target.AppID) || target.UpdateMethod != dstinstall.UpdateMethodSteamCMD && target.UpdateMethod != dstinstall.UpdateMethodSteamClient)
		updateBlocked := !target.UpToDate && (!target.SteamCMDAvailable || !target.UpdateSupported || target.AvailableBytes < target.RequiredBytes)
		if seenInstallations[key] || len(target.Shards) == 0 || !releaseVersionPattern.MatchString(target.DesiredVersion) || metadataInvalid || len(target.Blockers) != 0 || !target.Online || !target.InventoryFresh || !target.Installed || updateBlocked {
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
