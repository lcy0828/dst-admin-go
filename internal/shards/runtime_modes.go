package shards

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/shared"
)

type RuntimeModePreferences interface {
	RuntimeModePreferences(string) (map[string]rooms.RuntimeModePreference, error)
	RememberRuntimeMode(string, string, shared.RuntimePerformanceMode, string) error
}

type RuntimeModePlans interface {
	RuntimeModePlan(string, string) ([]jobs.RuntimeModeSelection, error)
	SaveRuntimeModePlan(string, string, []jobs.RuntimeModeSelection) error
}

func (o *Operations) ConfigureRuntimeModePersistence(preferences RuntimeModePreferences, plans RuntimeModePlans) error {
	if preferences == nil || plans == nil {
		return errors.New("runtime mode persistence dependencies are required")
	}
	o.modePreferences, o.modePlans = preferences, plans
	return nil
}

type WorldRuntimeMode struct {
	WorldID       string                        `json:"worldId"`
	WorldName     string                        `json:"worldName"`
	PreferredMode shared.RuntimePerformanceMode `json:"preferredMode,omitempty"`
	ActualMode    shared.RuntimePerformanceMode `json:"actualMode,omitempty"`
	EffectiveMode shared.RuntimePerformanceMode `json:"effectiveMode,omitempty"`
	Source        string                        `json:"source"`
	State         RuntimeState                  `json:"state"`
	Reason        string                        `json:"reason,omitempty"`
}

func confirmedRuntimeMode(value shared.RuntimePerformanceMode) (shared.RuntimePerformanceMode, bool) {
	mode, valid := shared.NormalizeRuntimePerformanceMode(value)
	return mode, valid && value != ""
}

func (o *Operations) savedRuntimeModes(roomID string) (map[string]rooms.RuntimeModePreference, error) {
	if o.modePreferences == nil {
		return map[string]rooms.RuntimeModePreference{}, nil
	}
	return o.modePreferences.RuntimeModePreferences(roomID)
}

func (o *Operations) resolveRuntimeModePlan(ctx context.Context, action Action, room rooms.Room, worlds []rooms.World, options PlanOptions) ([]jobs.RuntimeModeSelection, error) {
	prefs, err := o.savedRuntimeModes(room.ID)
	if err != nil {
		return nil, fmt.Errorf("读取启动方式失败，未更改运行中的世界: %w", err)
	}
	metadata, _ := ctx.Value(operationAuditContextKey{}).(OperationAuditMetadata)
	var saved []jobs.RuntimeModeSelection
	if o.modePlans != nil && metadata.JobID != "" {
		saved, err = o.modePlans.RuntimeModePlan(metadata.JobID, room.ID)
		if err != nil {
			return nil, fmt.Errorf("读取任务启动方式失败: %w", err)
		}
	}
	if len(saved) > 0 && len(saved) != len(worlds) {
		return nil, fmt.Errorf("任务的世界范围已变化，请重新提交: %w", ErrInvalidRuntimeMode)
	}
	plan := make([]jobs.RuntimeModeSelection, 0, len(worlds))
	for _, world := range worlds {
		value := jobs.RuntimeModeSelection{
			RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, Action: string(action),
			RequestedMode: options.RuntimeMode, RequestedVersion: options.RuntimeVersion,
			Mode: shared.RuntimePerformanceModeGame, Source: "default", PreferenceRevision: prefs[world.ID].Revision,
		}
		var original *jobs.RuntimeModeSelection
		if len(saved) > 0 {
			for i := range saved {
				if saved[i].WorldID == world.ID {
					original = &saved[i]
					break
				}
			}
			if original == nil || original.Action != value.Action || original.RequestedMode != value.RequestedMode || original.RequestedVersion != value.RequestedVersion {
				return nil, fmt.Errorf("任务的启动请求已变化，请重新提交: %w", ErrInvalidRuntimeMode)
			}
			if pref := prefs[world.ID]; pref.Revision != original.PreferenceRevision && pref.Mode != "" && pref.Mode != original.Mode {
				return nil, fmt.Errorf("世界 %s 已有较新的启动方式，请重新提交: %w", world.Name, rooms.ErrRuntimePreferenceConflict)
			}
		}
		if o.placements != nil {
			placement, placementErr := o.placements.AppliedPlacement(room.ID, world.ID)
			if placementErr == nil && strings.TrimSpace(placement.AppliedInstallationID) == "" {
				// Legacy placements omit the installation ID. Resolve the configured
				// default exactly as the Runtime router does; never assume "default"
				// or compare an unresolved ID with an inspected installation.
				resolved, resolveErr := o.placements.ResolveExecution(ctx, room.ID, world.ID)
				placementErr = resolveErr
				if placementErr == nil {
					if resolved.AppliedTargetID != placement.AppliedTargetID || resolved.Revision != placement.Revision {
						placementErr = fmt.Errorf("世界 %s 的运行位置已变化，请重新提交: %w", world.Name, ErrInvalidRuntimeMode)
					} else if strings.TrimSpace(resolved.AppliedInstallationID) == "" {
						placementErr = ErrRuntimeModeUnavailable
					} else {
						placement = resolved
					}
				}
			}
			if placementErr != nil {
				if action == ActionStart {
					if original != nil {
						value = *original
					} else if value.RequestedMode != "" {
						value.Mode, value.Source = value.RequestedMode, "explicit"
					} else if pref, exists := prefs[world.ID]; exists {
						value.Mode, value.Source = pref.Mode, "saved"
					} else {
						value.Source = "unknown"
					}
					value.Error = &jobs.Error{Code: operationErrorCode(action, placementErr), Message: placementErr.Error()}
					plan = append(plan, value)
					continue
				}
				return nil, placementErr
			}
			value.TargetID, value.InstallationID, value.TopologyRevision = placement.AppliedTargetID, placement.AppliedInstallationID, placement.Revision
		}
		if original != nil {
			if !original.PlacementUnresolved() && (original.TargetID != value.TargetID || original.InstallationID != value.InstallationID || original.TopologyRevision != value.TopologyRevision) {
				return nil, fmt.Errorf("任务的运行位置或启动请求已变化，请重新提交: %w", ErrInvalidRuntimeMode)
			}
			if original.Error == nil || original.Source != "unknown" {
				retained := *original
				retained.Error = nil
				retained.TargetID, retained.InstallationID, retained.TopologyRevision = value.TargetID, value.InstallationID, value.TopologyRevision
				if action == ActionStart {
					status, readErr := o.StatusFor(ctx, room.ID, world.ID)
					retained.WasRunning = status.State == RuntimeRunning || status.State == RuntimeStarting
					if readErr != nil {
						retained.Error = &jobs.Error{Code: operationErrorCode(action, readErr), Message: readErr.Error()}
					} else if status.State == RuntimeUnknown {
						retained.Error = &jobs.Error{Code: "RUNTIME_MODE_UNCONFIRMED", Message: "世界运行状态待确认，未执行启动"}
					} else if actual, known := confirmedRuntimeMode(status.RuntimeMode); retained.WasRunning && (!known || actual != retained.Mode) {
						retained.Error = &jobs.Error{Code: "RUNTIME_MODE_CHANGE_REQUIRES_RESTART", Message: "世界已在运行；切换启动方式请使用重启"}
					}
				}
				plan = append(plan, retained)
				continue
			}
		}
		status, statusErr := o.StatusFor(ctx, room.ID, world.ID)
		if statusErr != nil {
			if action == ActionStart {
				if value.RequestedMode != "" {
					value.Mode, value.Source = value.RequestedMode, "explicit"
				} else if pref, exists := prefs[world.ID]; exists {
					value.Mode, value.Source = pref.Mode, "saved"
				} else {
					value.Source = "unknown"
				}
				value.Error = &jobs.Error{Code: operationErrorCode(action, statusErr), Message: statusErr.Error()}
				plan = append(plan, value)
				continue
			}
			return nil, fmt.Errorf("读取 %s 当前运行方式: %w", world.Name, statusErr)
		}
		if status.State == RuntimeUnknown {
			if action == ActionStart {
				value.Source = "unknown"
				if options.RuntimeMode != "" {
					value.Mode = options.RuntimeMode
				}
				value.Error = &jobs.Error{Code: "RUNTIME_MODE_UNCONFIRMED", Message: "世界运行状态待确认，未执行启动"}
				plan = append(plan, value)
				continue
			}
			return nil, fmt.Errorf("世界 %s 运行状态待确认，未执行启动或重启", world.Name)
		}
		value.WasRunning = status.State == RuntimeRunning || status.State == RuntimeStarting
		actual, actualKnown := confirmedRuntimeMode(status.RuntimeMode)
		if options.RuntimeMode != "" {
			value.Mode, value.Source = options.RuntimeMode, "explicit"
			if action == ActionStart && value.WasRunning && (!actualKnown || actual != value.Mode) {
				value.Error = &jobs.Error{Code: "RUNTIME_MODE_CHANGE_REQUIRES_RESTART", Message: fmt.Sprintf("世界 %s 已在运行；切换启动方式请使用重启", world.Name)}
				plan = append(plan, value)
				continue
			}
		} else if value.WasRunning {
			if !actualKnown {
				if action == ActionStart {
					value.Source = "unknown"
					value.Error = &jobs.Error{Code: "RUNTIME_MODE_UNCONFIRMED", Message: "世界已在运行，但实际 Lua 运行方式待确认"}
					plan = append(plan, value)
					continue
				}
				return nil, fmt.Errorf("世界 %s 的实际 Lua 运行方式待确认，请重新检测或升级 Agent: %w", world.Name, ErrInvalidRuntimeMode)
			}
			value.Mode, value.Source = actual, "running"
		} else if pref, exists := prefs[world.ID]; exists {
			value.Mode, value.Source = pref.Mode, "saved"
		}
		plan = append(plan, value)
	}
	if err := o.validateRuntimeModePlan(ctx, room, worlds, plan); err != nil {
		return nil, err
	}
	if o.modePlans != nil && metadata.JobID != "" {
		if err := o.modePlans.SaveRuntimeModePlan(metadata.JobID, room.ID, plan); err != nil {
			return nil, fmt.Errorf("保存任务启动方式失败，未停止游戏: %w", err)
		}
	}
	return plan, nil
}

func (o *Operations) validateRuntimeModePlan(ctx context.Context, room rooms.Room, worlds []rooms.World, plan []jobs.RuntimeModeSelection) error {
	checked := map[string]RuntimeModeAvailability{}
	checkErrors := map[string]error{}
	for i := range plan {
		value := &plan[i]
		if value.Error != nil {
			continue
		}
		if _, valid := confirmedRuntimeMode(value.Mode); !valid {
			return ErrInvalidRuntimeMode
		}
		if value.Mode == shared.RuntimePerformanceModeGame || value.Action == string(ActionStart) && value.WasRunning {
			continue
		}
		var checkErr error
		if o.placements == nil {
			if _, supportsMode := o.control.(interface {
				StartWithRuntimeMode(context.Context, string, string, shared.RuntimePerformanceMode) error
			}); !supportsMode {
				checkErr = ErrRuntimeModeUnavailable
			} else if validator, ok := o.control.(interface {
				requireRuntimeMode(shared.RuntimePerformanceMode) error
			}); ok {
				checkErr = validator.requireRuntimeMode(value.Mode)
			}
		} else {
			key := value.TargetID + "\x00" + value.InstallationID
			availability, exists := checked[key]
			if !exists {
				availability, checkErr = o.runtimeModeAvailability(ctx, room, []rooms.World{worlds[i]})
				checked[key], checkErrors[key] = availability, checkErr
			} else {
				checkErr = checkErrors[key]
			}
			if checkErr == nil {
				matched := false
				for _, target := range availability.Targets {
					if target.TargetID != value.TargetID || target.InstallationID != value.InstallationID {
						continue
					}
					matched = true
					if !stringRuntimeModeContains(target.SupportedModes, value.Mode) {
						checkErr = &RuntimeModeError{Mode: value.Mode, Availability: availability}
					} else if value.RequestedVersion != "" && target.PackageVersion != value.RequestedVersion {
						checkErr = &RuntimeVersionError{Version: value.RequestedVersion, Availability: availability}
					}
				}
				if !matched {
					checkErr = ErrRuntimeModeUnavailable
				}
			}
		}
		if checkErr != nil {
			checkErr = fmt.Errorf("世界 %s: %w", value.WorldName, checkErr)
			if value.Action != string(ActionStart) {
				return checkErr
			}
			value.Error = &jobs.Error{Code: operationErrorCode(ActionStart, checkErr), Message: checkErr.Error()}
		}
	}
	return nil
}

// A stop must remain available even if mode detection or its database fails.
func (o *Operations) rememberModesBeforeStop(ctx context.Context, room rooms.Room, worlds []rooms.World) map[string]*jobs.Error {
	warnings := make(map[string]*jobs.Error)
	if o.modePreferences == nil {
		return warnings
	}
	prefs, err := o.savedRuntimeModes(room.ID)
	if err != nil {
		for _, world := range worlds {
			warnings[world.ID] = &jobs.Error{Code: "RUNTIME_MODE_PREFERENCE_SAVE_FAILED", Message: "停止操作继续执行，但未能保存原启动方式: " + err.Error()}
		}
		return warnings
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, world := range worlds {
		if _, exists := prefs[world.ID]; exists {
			continue
		}
		status, statusErr := o.StatusFor(probeCtx, room.ID, world.ID)
		mode, known := confirmedRuntimeMode(status.RuntimeMode)
		if statusErr != nil || status.State == RuntimeUnknown || !known && (status.State == RuntimeRunning || status.State == RuntimeStarting) {
			warnings[world.ID] = &jobs.Error{Code: "RUNTIME_MODE_UNCONFIRMED", Message: "未能确认原启动方式，停止操作继续执行；下次启动前可手动选择方式"}
			continue
		}
		if status.State != RuntimeRunning && status.State != RuntimeStarting {
			continue
		}
		if err := o.modePreferences.RememberRuntimeMode(room.ID, world.ID, mode, ""); err != nil && !errors.Is(err, rooms.ErrRuntimePreferenceConflict) {
			warnings[world.ID] = &jobs.Error{Code: "RUNTIME_MODE_PREFERENCE_SAVE_FAILED", Message: "停止操作继续执行，但未能保存原启动方式: " + err.Error()}
		}
	}
	return warnings
}

func (o *Operations) rememberSuccessfulMode(ctx context.Context, roomID string, selection jobs.RuntimeModeSelection, result *worldExecutionResult) {
	if o.modePreferences == nil || result.err != nil || selection.Action == string(ActionStart) && selection.WasRunning {
		return
	}
	if err := ctx.Err(); err != nil {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, err := o.StatusFor(probeCtx, roomID, selection.WorldID)
	actual, known := confirmedRuntimeMode(status.RuntimeMode)
	if err != nil || !known || status.State != RuntimeRunning {
		result.warning = &jobs.Error{Code: "RUNTIME_MODE_UNCONFIRMED", Message: "启动操作已返回，但实际运行方式尚未确认，未修改启动偏好"}
		return
	}
	if actual != selection.Mode {
		result.err = fmt.Errorf("世界 %s 实际使用 %s，与选择的 %s 不符: %w", selection.WorldName, actual, selection.Mode, ErrInvalidRuntimeMode)
		return
	}
	if err := o.modePreferences.RememberRuntimeMode(roomID, selection.WorldID, actual, selection.PreferenceRevision); err != nil {
		// A retry of a successful target may have already saved this same mode.
		if errors.Is(err, rooms.ErrRuntimePreferenceConflict) {
			if values, readErr := o.savedRuntimeModes(roomID); readErr == nil && values[selection.WorldID].Mode == actual {
				return
			}
		}
		result.warning = &jobs.Error{Code: "RUNTIME_MODE_PREFERENCE_SAVE_FAILED", Message: "游戏已运行，但未能保存启动偏好: " + err.Error()}
	}
}

func (o *Operations) decorateRuntimeModeDefaults(ctx context.Context, room rooms.Room, worlds []rooms.World, availability *RuntimeModeAvailability) error {
	prefs, err := o.savedRuntimeModes(room.ID)
	if err != nil {
		return err
	}
	availability.SelectionState = "uniform"
	for _, world := range worlds {
		value := WorldRuntimeMode{WorldID: world.ID, WorldName: world.Name, PreferredMode: prefs[world.ID].Mode, EffectiveMode: shared.RuntimePerformanceModeGame, Source: "default"}
		if value.PreferredMode != "" {
			value.EffectiveMode, value.Source = value.PreferredMode, "saved"
		}
		status, statusErr := o.StatusFor(ctx, room.ID, world.ID)
		value.State = status.State
		if statusErr != nil || status.State == RuntimeUnknown {
			value.EffectiveMode, value.Source, value.Reason = "", "unknown", "当前运行方式待确认"
		} else if status.State == RuntimeRunning || status.State == RuntimeStarting {
			if mode, known := confirmedRuntimeMode(status.RuntimeMode); known {
				value.ActualMode, value.EffectiveMode, value.Source = mode, mode, "running"
			} else {
				value.EffectiveMode, value.Source, value.Reason = "", "unknown", "实际运行方式待确认"
			}
		}
		availability.Worlds = append(availability.Worlds, value)
		if value.EffectiveMode == "" {
			availability.SelectionState = "unknown"
		}
		if len(availability.Worlds) == 1 {
			availability.DefaultMode = value.EffectiveMode
		} else if value.EffectiveMode != availability.Worlds[0].EffectiveMode && availability.SelectionState != "unknown" {
			availability.SelectionState = "mixed"
		}
	}
	if availability.SelectionState != "uniform" {
		availability.DefaultMode = ""
	}
	return nil
}
