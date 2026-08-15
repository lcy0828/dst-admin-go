package modpublication

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	defaultActivationTimeout = 300
	minimumActivationTimeout = 30
	maximumActivationTimeout = 900
)

func NormalizeActivationPolicy(value ActivationPolicy) (ActivationPolicy, error) {
	if value.Mode == "" {
		value.Mode = ActivationModeManual
	}
	if value.LoadConfirmation == "" {
		if value.Mode == ActivationModeRestart {
			value.LoadConfirmation = LoadConfirmationLogs
		} else {
			value.LoadConfirmation = LoadConfirmationNone
		}
	}
	if value.TimeoutSeconds == 0 {
		value.TimeoutSeconds = defaultActivationTimeout
	}
	if value.Mode != ActivationModeManual && value.Mode != ActivationModeRestart ||
		value.LoadConfirmation != LoadConfirmationNone && value.LoadConfirmation != LoadConfirmationLogs ||
		value.TimeoutSeconds < minimumActivationTimeout || value.TimeoutSeconds > maximumActivationTimeout ||
		value.Mode == ActivationModeManual && value.LoadConfirmation != LoadConfirmationNone {
		return ActivationPolicy{}, ErrInvalidInput
	}
	return value, nil
}

func initialActivation(policy ActivationPolicy) Activation {
	status := ActivationStatusSkipped
	if policy.Mode == ActivationModeRestart {
		status = ActivationStatusPending
	}
	return Activation{Policy: policy, Status: status, Shards: []ShardActivationResult{}}
}

func validateActivation(value Activation) error {
	policy, err := NormalizeActivationPolicy(value.Policy)
	if err != nil || policy != value.Policy || !validActivationStatus(value.Status) {
		return ErrInvalidInput
	}
	if value.Status == ActivationStatusSkipped && value.Policy.Mode == ActivationModeRestart {
		for _, shard := range value.Shards {
			if shard.Status != ActivationStatusSkipped {
				return ErrInvalidInput
			}
		}
	}
	seen := make(map[string]bool, len(value.Shards))
	for _, shard := range value.Shards {
		if !validID(shard.RoomID) || !validID(shard.WorldID) || !validID(shard.TargetID) ||
			!validID(shard.InstallationID) || !validActivationStatus(shard.Status) || shard.UpdatedAt.IsZero() {
			return ErrInvalidInput
		}
		key := worldKey(shard.RoomID, shard.WorldID)
		if seen[key] {
			return ErrConflict
		}
		seen[key] = true
	}
	return nil
}

func validActivationStatus(value ActivationStatus) bool {
	switch value {
	case ActivationStatusSkipped, ActivationStatusPending, ActivationStatusRestarting,
		ActivationStatusConfirming, ActivationStatusSucceeded, ActivationStatusFailed:
		return true
	default:
		return false
	}
}

func (c *Coordinator) Activate(ctx context.Context, publicationID, activationID string, policy ActivationPolicy) (Publication, error) {
	if !validID(publicationID) || !validID(activationID) {
		return Publication{}, ErrInvalidInput
	}
	policy, err := NormalizeActivationPolicy(policy)
	if err != nil || policy.Mode != ActivationModeRestart {
		return Publication{}, ErrInvalidInput
	}
	publication, err := c.store.Get(publicationID)
	if err != nil {
		return Publication{}, err
	}
	if publication.Status != StatusSucceeded || !publication.CommitDecision ||
		publication.Activation.Status == ActivationStatusRestarting || publication.Activation.Status == ActivationStatusConfirming {
		return publication, ErrActivationState
	}
	fences, err := c.acquireFencesFor(ctx, publication.Plan, activationOperationKey(publicationID, activationID))
	if err != nil {
		return publication, err
	}
	defer c.releaseFences(fences)
	return c.activateCommitted(ctx, publication, fences, policy)
}

func (c *Coordinator) activateCommitted(ctx context.Context, publication Publication, fences []Fence, policy ActivationPolicy) (Publication, error) {
	return c.activateCommittedWithRunningSnapshot(ctx, publication, fences, policy, nil)
}

func (c *Coordinator) activateCommittedWithRunningSnapshot(ctx context.Context, publication Publication, fences []Fence, policy ActivationPolicy, originalRunning map[string]bool) (Publication, error) {
	if c.activation == nil {
		return c.failActivation(publication, nil, "ACTIVATION_UNAVAILABLE", errors.New("Mod activation runtime is unavailable"))
	}
	policy, err := NormalizeActivationPolicy(policy)
	if err != nil || policy.Mode != ActivationModeRestart {
		return publication, ErrInvalidInput
	}
	requestedAt := c.now().UTC()
	publication.Activation = Activation{
		Policy: policy, Status: ActivationStatusRestarting, RequestedAt: &requestedAt,
		Shards: activationShards(publication.Plan, requestedAt),
	}
	publication.RestartRequired = true
	publication.UpdatedAt = requestedAt
	publication, err = c.store.Save(publication)
	if err != nil {
		return publication, err
	}

	activationContext, cancel := context.WithTimeout(ctx, time.Duration(policy.TimeoutSeconds)*time.Second)
	defer cancel()
	cursors := make(map[string]LogCursor, len(publication.Activation.Shards))
	running := make(map[string]bool, len(publication.Activation.Shards))
	worlds := activationWorlds(publication.Plan)
	for _, world := range worlds {
		key := worldKey(world.RoomID, world.WorldID)
		index := activationShardIndex(publication, world.RoomID, world.WorldID)
		observation, statusErr := c.activation.Status(activationContext, world)
		if statusErr != nil {
			return c.failActivation(publication, &publication.Activation.Shards[index], "SHARD_STATUS_FAILED", statusErr)
		}
		result := &publication.Activation.Shards[index]
		result.RuntimeState = observation.State
		result.WasRunning = observation.SessionExists || observation.State == "running" || observation.State == "starting"
		if originalRunning != nil {
			result.WasRunning = originalRunning[key]
		}
		if !result.WasRunning {
			result.Status, result.UpdatedAt = ActivationStatusSkipped, c.now().UTC()
			continue
		}
		running[key] = true
		if policy.LoadConfirmation == LoadConfirmationLogs {
			cursor, cursorErr := c.activation.CaptureLogCursor(activationContext, world)
			if cursorErr != nil {
				return c.failActivation(publication, result, "LOG_CURSOR_FAILED", cursorErr)
			}
			cursors[key] = cursor
		}
	}
	if len(running) == 0 {
		finished := c.now().UTC()
		publication.Activation.Status, publication.Activation.FinishedAt = ActivationStatusSkipped, &finished
		publication.RestartRequired, publication.UpdatedAt = false, finished
		return c.store.Save(publication)
	}
	publication, err = c.store.Save(publication)
	if err != nil {
		return publication, err
	}

	stopped := make([]WorldPlan, 0, len(running))
	for _, world := range activationStopOrder(worlds) {
		if !running[worldKey(world.RoomID, world.WorldID)] {
			continue
		}
		index := activationShardIndex(publication, world.RoomID, world.WorldID)
		fences, err = c.renewFences(activationContext, fences)
		if err != nil {
			return c.failActivation(publication, &publication.Activation.Shards[index], "ACTIVATION_LEASE_LOST", err)
		}
		operation := c.activationOperation(publication, world, fences, "stop")
		if stopErr := c.activation.Stop(activationContext, world, operation); stopErr != nil {
			for _, stoppedWorld := range activationStartOrder(stopped) {
				_ = c.activation.Start(context.Background(), stoppedWorld, c.activationOperation(publication, stoppedWorld, fences, "restore"))
			}
			return c.failActivation(publication, &publication.Activation.Shards[index], "SHARD_STOP_FAILED", stopErr)
		}
		stopped = append(stopped, world)
	}

	var activationErr error
	started := make(map[string]bool, len(running))
	for _, world := range activationStartOrder(worlds) {
		key := worldKey(world.RoomID, world.WorldID)
		if !running[key] {
			continue
		}
		index := activationShardIndex(publication, world.RoomID, world.WorldID)
		result := &publication.Activation.Shards[index]
		fences, err = c.renewFences(activationContext, fences)
		if err != nil {
			setActivationFailure(result, "ACTIVATION_LEASE_LOST", err, c.now().UTC())
			activationErr = errors.Join(activationErr, err)
			continue
		}
		if startErr := c.activation.Start(activationContext, world, c.activationOperation(publication, world, fences, "start")); startErr != nil {
			setActivationFailure(result, "SHARD_START_FAILED", startErr, c.now().UTC())
			activationErr = errors.Join(activationErr, startErr)
			saved, saveErr := c.store.Save(publication)
			if saveErr != nil {
				return publication, errors.Join(ErrActivationFailed, activationErr, saveErr)
			}
			publication = saved
			continue
		}
		restartedAt := c.now().UTC()
		result.Status, result.RestartedAt, result.UpdatedAt = ActivationStatusConfirming, &restartedAt, restartedAt
		publication.Activation.Status, publication.UpdatedAt = ActivationStatusConfirming, restartedAt
		started[key] = true
		saved, saveErr := c.store.Save(publication)
		if saveErr != nil {
			return publication, errors.Join(ErrActivationFailed, saveErr)
		}
		publication = saved
	}
	for _, world := range activationStartOrder(worlds) {
		key := worldKey(world.RoomID, world.WorldID)
		if !started[key] {
			continue
		}
		index := activationShardIndex(publication, world.RoomID, world.WorldID)
		result := &publication.Activation.Shards[index]
		marker, state, confirmErr := c.confirmShardLoaded(activationContext, world, cursors[key], policy.LoadConfirmation)
		if confirmErr != nil {
			setActivationFailure(result, "SHARD_LOAD_CONFIRMATION_FAILED", confirmErr, c.now().UTC())
			activationErr = errors.Join(activationErr, confirmErr)
		} else {
			confirmedAt := c.now().UTC()
			result.Status, result.RuntimeState, result.LoadMarker = ActivationStatusSucceeded, state, marker
			result.LoadConfirmedAt, result.UpdatedAt = &confirmedAt, confirmedAt
		}
		saved, saveErr := c.store.Save(publication)
		if saveErr != nil {
			return publication, errors.Join(ErrActivationFailed, activationErr, saveErr)
		}
		publication = saved
	}
	if activationErr != nil {
		return c.failActivation(publication, nil, "ACTIVATION_INCOMPLETE", activationErr)
	}
	finished := c.now().UTC()
	publication.Activation.Status, publication.Activation.ErrorCode, publication.Activation.ErrorMessage = ActivationStatusSucceeded, "", ""
	publication.Activation.FinishedAt = &finished
	publication.RestartRequired, publication.UpdatedAt = false, finished
	return c.store.Save(publication)
}

func activationRunningSnapshot(publication Publication) (map[string]bool, bool) {
	worlds := activationWorlds(publication.Plan)
	if len(worlds) == 0 || len(publication.Activation.Shards) != len(worlds) {
		return nil, false
	}
	known := make(map[string]bool, len(worlds))
	captured := false
	for _, shard := range publication.Activation.Shards {
		key := worldKey(shard.RoomID, shard.WorldID)
		if _, exists := known[key]; exists {
			return nil, false
		}
		known[key] = shard.WasRunning
		if shard.WasRunning || shard.Status != ActivationStatusPending {
			captured = true
		}
	}
	for _, world := range worlds {
		if _, exists := known[worldKey(world.RoomID, world.WorldID)]; !exists {
			return nil, false
		}
	}
	return known, captured
}

func (c *Coordinator) confirmShardLoaded(ctx context.Context, world WorldPlan, cursor LogCursor, confirmation LoadConfirmation) (string, string, error) {
	ticker := time.NewTicker(c.activationPollInterval)
	defer ticker.Stop()
	current := cursor
	for {
		status, err := c.activation.Status(ctx, world)
		if err != nil {
			return "", "", err
		}
		if status.State == "failed" || status.State == "stopped" && !status.SessionExists {
			return "", status.State, errors.New("DST shard stopped before load confirmation")
		}
		if status.State == "running" {
			if confirmation == LoadConfirmationNone {
				return "runtime:running", status.State, nil
			}
			logs, logErr := c.activation.ReadLogs(ctx, world, current)
			if logErr != nil {
				return "", status.State, logErr
			}
			current = logs.Cursor
			for _, line := range logs.Lines {
				if marker := activationLoadMarker(world, line); marker != "" {
					return marker, status.State, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", status.State, ctx.Err()
		case <-ticker.C:
		}
	}
}

func activationLoadMarker(world WorldPlan, line string) string {
	lower := strings.ToLower(line)
	if strings.Contains(lower, "[dst-admin-runtime ready]") {
		return "dst-admin-runtime-ready"
	}
	if world.IsMaster || strings.EqualFold(world.WorldDirectory, "Master") {
		if strings.Contains(lower, "[shard] shard server started on port:") {
			return "master-shard-server-started"
		}
	} else if strings.Contains(lower, "[shard] secondary shard lua is now ready!") {
		return "secondary-shard-lua-ready"
	}
	return ""
}

func (c *Coordinator) failActivation(publication Publication, shard *ShardActivationResult, code string, cause error) (Publication, error) {
	now := c.now().UTC()
	if shard != nil {
		setActivationFailure(shard, code, cause, now)
	}
	publication.Activation.Status, publication.Activation.ErrorCode = ActivationStatusFailed, code
	publication.Activation.ErrorMessage, publication.Activation.FinishedAt = cause.Error(), &now
	publication.RestartRequired, publication.UpdatedAt = true, now
	saved, saveErr := c.store.Save(publication)
	return saved, errors.Join(ErrActivationFailed, cause, saveErr)
}

func setActivationFailure(result *ShardActivationResult, code string, cause error, now time.Time) {
	result.Status, result.ErrorCode, result.ErrorMessage = ActivationStatusFailed, code, cause.Error()
	result.UpdatedAt = now
}

func activationShards(plan Plan, now time.Time) []ShardActivationResult {
	result := make([]ShardActivationResult, 0)
	for _, target := range plan.Targets {
		for _, world := range target.Worlds {
			result = append(result, ShardActivationResult{
				RoomID: world.RoomID, WorldID: world.WorldID, TargetID: target.TargetID,
				InstallationID: target.InstallationID, Status: ActivationStatusPending, UpdatedAt: now,
			})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return worldKey(result[i].RoomID, result[i].WorldID) < worldKey(result[j].RoomID, result[j].WorldID)
	})
	return result
}

func activationWorlds(plan Plan) []WorldPlan {
	result := make([]WorldPlan, 0)
	for _, target := range plan.Targets {
		result = append(result, target.Worlds...)
	}
	return result
}

func activationStopOrder(values []WorldPlan) []WorldPlan {
	result := append([]WorldPlan(nil), values...)
	sort.SliceStable(result, func(i, j int) bool {
		leftMaster, rightMaster := result[i].IsMaster || strings.EqualFold(result[i].WorldDirectory, "Master"), result[j].IsMaster || strings.EqualFold(result[j].WorldDirectory, "Master")
		if leftMaster != rightMaster {
			return !leftMaster
		}
		return worldKey(result[i].RoomID, result[i].WorldID) < worldKey(result[j].RoomID, result[j].WorldID)
	})
	return result
}

func activationStartOrder(values []WorldPlan) []WorldPlan {
	result := append([]WorldPlan(nil), values...)
	sort.SliceStable(result, func(i, j int) bool {
		leftMaster, rightMaster := result[i].IsMaster || strings.EqualFold(result[i].WorldDirectory, "Master"), result[j].IsMaster || strings.EqualFold(result[j].WorldDirectory, "Master")
		if leftMaster != rightMaster {
			return leftMaster
		}
		return worldKey(result[i].RoomID, result[i].WorldID) < worldKey(result[j].RoomID, result[j].WorldID)
	})
	return result
}

func activationShardIndex(publication Publication, roomID, worldID string) int {
	for index := range publication.Activation.Shards {
		if publication.Activation.Shards[index].RoomID == roomID && publication.Activation.Shards[index].WorldID == worldID {
			return index
		}
	}
	return -1
}

func activationOperationKey(publicationID, activationID string) string {
	return fmt.Sprintf("mod.activate:%s", hashBytes([]byte(publicationID+"\x00"+activationID)))
}
