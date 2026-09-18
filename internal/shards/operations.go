package shards

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/agents"
	"dont/internal/jobs"
	"dont/internal/maintenance"
	"dont/internal/operationlease"
	"dont/internal/operationprogress"
	"dont/internal/roomops"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"

	"github.com/google/uuid"
)

var (
	ErrRoomNotManaged            = errors.New("room must be registered before it can be controlled")
	ErrNoWorlds                  = errors.New("room has no controllable worlds")
	ErrUnknownAction             = errors.New("unknown room action")
	ErrUnsafeName                = errors.New("room or world name cannot be represented safely by tmux")
	ErrCapacityRisk              = errors.New("shard start requires capacity risk confirmation")
	ErrNoRooms                   = errors.New("no rooms selected")
	ErrInvalidBatch              = errors.New("batch room selection is invalid")
	ErrRuntimeModeUnavailable    = errors.New("selected Lua runtime mode is unavailable")
	ErrInvalidRuntimeMode        = errors.New("selected Lua runtime mode is invalid")
	ErrRuntimeVersionUnavailable = errors.New("selected Lua runtime version is unavailable")
	ErrInvalidRuntimeVersion     = errors.New("selected Lua runtime version is invalid")
	controlName                  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
	ActionSave    Action = "save"
	ActionCleanup Action = "cleanup"
)

type RuntimeState string

const (
	RuntimeStopped  RuntimeState = "stopped"
	RuntimeStarting RuntimeState = "starting"
	RuntimeRunning  RuntimeState = "running"
	RuntimeFailed   RuntimeState = "failed"
	RuntimeUnknown  RuntimeState = "unknown"
)

type RuntimeStatus struct {
	State         RuntimeState
	StartupStage  string
	Code          string
	Message       string
	SessionExists bool
	Paused        *bool
}

type Control interface {
	IsRunning(context.Context, string, string) (bool, error)
	Start(context.Context, string, string) error
	Stop(context.Context, string, string) error
}

type CleanupControl interface {
	Cleanup(context.Context, string, string) error
}

type ConsoleControl interface {
	Send(context.Context, string, string, string) error
}

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
}

type CapacityRiskError struct {
	Preview topology.StartCapacityPreview
}

func (e *CapacityRiskError) Error() string { return ErrCapacityRisk.Error() }
func (e *CapacityRiskError) Unwrap() error { return ErrCapacityRisk }

type PlanOptions struct {
	RuntimeMode shared.RuntimePerformanceMode
	Immediate   bool
}

type BatchRoomSelection struct {
	RoomID      string
	WorldIDs    []string
	RuntimeMode shared.RuntimePerformanceMode
}

type RuntimeModeTarget struct {
	TargetID            string                          `json:"targetId"`
	InstallationID      string                          `json:"installationId"`
	TargetName          string                          `json:"targetName"`
	OS                  string                          `json:"os,omitempty"`
	Arch                string                          `json:"arch,omitempty"`
	PackageVersion      string                          `json:"packageVersion,omitempty"`
	GameVersion         string                          `json:"gameVersion,omitempty"`
	CompatibilityStatus shared.RuntimePerformanceStatus `json:"compatibilityStatus,omitempty"`
	SupportedModes      []shared.RuntimePerformanceMode `json:"supportedModes"`
	Issues              []string                        `json:"issues,omitempty"`
	ReasonCode          string                          `json:"reasonCode,omitempty"`
	Reason              string                          `json:"reason,omitempty"`
}

type RuntimePackageOption struct {
	ID                 string   `json:"id"`
	Provider           string   `json:"provider"`
	Version            string   `json:"version,omitempty"`
	Channel            string   `json:"channel"`
	Platforms          []string `json:"platforms"`
	ProcessScopedModes bool     `json:"processScopedModes"`
}

type RuntimeModeAvailability struct {
	RoomID      string                          `json:"roomId"`
	WorldIDs    []string                        `json:"worldIds"`
	DefaultMode shared.RuntimePerformanceMode   `json:"defaultMode"`
	Modes       []shared.RuntimePerformanceMode `json:"modes"`
	Targets     []RuntimeModeTarget             `json:"targets"`
	Packages    []RuntimePackageOption          `json:"packages"`
}

type RuntimeModeError struct {
	Mode         shared.RuntimePerformanceMode
	Availability RuntimeModeAvailability
}

func (e *RuntimeModeError) Error() string {
	return fmt.Sprintf("所选 Lua 运行时 %s 在当前世界运行位置不可用", e.Mode)
}
func (e *RuntimeModeError) Unwrap() error { return ErrRuntimeModeUnavailable }

type RuntimeVersionError struct {
	Version      string
	Availability RuntimeModeAvailability
}

func (e *RuntimeVersionError) Error() string {
	return fmt.Sprintf("所选 LuaJIT 版本 %s 在当前世界运行位置不可用", e.Version)
}
func (e *RuntimeVersionError) Unwrap() error { return ErrRuntimeVersionUnavailable }

type BatchCapacityRiskError struct {
	Preview topology.BatchStartCapacityPreview
}

func (e *BatchCapacityRiskError) Error() string { return ErrCapacityRisk.Error() }
func (e *BatchCapacityRiskError) Unwrap() error { return ErrCapacityRisk }

type executionPlacementResolver interface {
	AppliedPlacement(string, string) (topology.ExecutionPlacement, error)
	ResolveExecution(context.Context, string, string) (topology.ExecutionPlacement, error)
	PreviewStartCapacity(context.Context, string, []string) (topology.StartCapacityPreview, error)
	PreviewBatchStartCapacity(context.Context, []topology.StartCapacitySelection) (topology.BatchStartCapacityPreview, error)
}

type remoteShardExecutor interface {
	ExecuteShard(context.Context, string, shared.ShardOperationRequest, int) (agents.ShardExecutionResult, error)
}

type placedRuntimeExecutor interface {
	Status(context.Context, string, string) (shared.ShardRuntimeStatus, error)
	ExecutePlacedShard(context.Context, string, string, shared.ShardOperationRequest, time.Duration) (shared.ShardOperationResult, error)
}

type operationLeaseService interface {
	Acquire(context.Context, string, string, time.Duration) (operationlease.Lease, error)
	Renew(context.Context, operationlease.Lease, time.Duration) (operationlease.Lease, error)
	Release(operationlease.Lease) error
}

type OperationAuditMetadata struct {
	JobID     string
	RequestID string
	Source    string
}

type OperationAudit struct {
	RoomID           string
	WorldID          string
	TargetID         string
	AgentID          string
	Action           Action
	OperationID      string
	OperationKey     string
	LeaseID          string
	FencingToken     uint64
	TopologyRevision string
	JobID            string
	RequestID        string
	Source           string
}

type OperationObserver interface {
	ObserveOperation(context.Context, OperationAudit) error
}

type OperationNotifier interface {
	BeforeOperation(context.Context, string, string, string, string) error
}

type operationAuditContextKey struct{}

func WithOperationAudit(ctx context.Context, metadata OperationAuditMetadata) context.Context {
	return context.WithValue(ctx, operationAuditContextKey{}, metadata)
}

type Operations struct {
	rooms        RoomCatalog
	control      Control
	pollInterval time.Duration
	startTimeout time.Duration
	stopTimeout  time.Duration
	placements   executionPlacementResolver
	remote       remoteShardExecutor
	runtime      placedRuntimeExecutor
	leases       operationLeaseService
	leaseTTL     time.Duration
	observer     OperationObserver
	notifier     OperationNotifier
	activeMu     sync.Mutex
	activeSeq    uint64
	activeStarts map[string]map[uint64]context.CancelFunc
	// stopEpoch invalidates start plans submitted before a stop runner begins.
	stopEpoch map[string]uint64
}

func (o *Operations) ConfigureObserver(observer OperationObserver) {
	o.observer = observer
}

func (o *Operations) ConfigureNotifier(notifier OperationNotifier) {
	o.notifier = notifier
}

func NewOperations(roomCatalog RoomCatalog, control Control) *Operations {
	return &Operations{
		rooms: roomCatalog, control: control,
		pollInterval: 500 * time.Millisecond, startTimeout: 5 * time.Minute, stopTimeout: 60 * time.Second,
		leaseTTL:     6 * time.Minute,
		activeStarts: make(map[string]map[uint64]context.CancelFunc),
		stopEpoch:    make(map[string]uint64),
	}
}

func (o *Operations) ConfigureDistributed(placements executionPlacementResolver, remote remoteShardExecutor, leases operationLeaseService) error {
	if placements == nil || remote == nil || leases == nil {
		return errors.New("distributed shard control dependencies are required")
	}
	o.placements, o.remote, o.leases = placements, remote, leases
	return nil
}

// ConfigureRuntime routes local and remote lifecycle operations through one
// Runtime Driver boundary while retaining ConfigureDistributed for legacy tests.
func (o *Operations) ConfigureRuntime(placements executionPlacementResolver, runtime placedRuntimeExecutor, leases operationLeaseService) error {
	if placements == nil || runtime == nil || leases == nil {
		return errors.New("runtime shard control dependencies are required")
	}
	o.placements, o.runtime, o.leases = placements, runtime, leases
	return nil
}

func (o *Operations) Plan(action Action, roomID string, selectedWorldIDs []string) ([]jobs.TargetSpec, jobs.Runner, error) {
	return o.PlanWithOptions(action, roomID, selectedWorldIDs, PlanOptions{})
}

func (o *Operations) PlanWithOptions(action Action, roomID string, selectedWorldIDs []string, options PlanOptions) ([]jobs.TargetSpec, jobs.Runner, error) {
	return o.planWithOptions(action, roomID, selectedWorldIDs, options)
}

func (o *Operations) planWithOptions(action Action, roomID string, selectedWorldIDs []string, options PlanOptions) ([]jobs.TargetSpec, jobs.Runner, error) {
	if action != ActionStart && action != ActionStop && action != ActionRestart && action != ActionSave && action != ActionCleanup {
		return nil, nil, ErrUnknownAction
	}
	runtimeMode, runtimeModeValid := shared.NormalizeRuntimePerformanceMode(options.RuntimeMode)
	if !runtimeModeValid {
		return nil, nil, ErrInvalidRuntimeMode
	}
	if action == ActionStart || action == ActionRestart {
		options.RuntimeMode = runtimeMode
	} else if options.RuntimeMode != "" {
		return nil, nil, ErrInvalidRuntimeMode
	}
	room, worlds, err := o.resolvePlan(roomID, selectedWorldIDs)
	if err != nil {
		return nil, nil, err
	}
	orderWorlds(worlds, action)
	plannedWorldIDs := make([]string, 0, len(worlds))
	targets := make([]jobs.TargetSpec, 0, len(worlds))
	for _, world := range worlds {
		plannedWorldIDs = append(plannedWorldIDs, world.ID)
		targets = append(targets, jobs.TargetSpec{ID: world.ID, Name: world.Name})
	}
	leaseOperationKey := uuid.NewString()
	operationIDs := make(map[string]string, len(worlds))
	for _, world := range worlds {
		operationIDs[world.ID] = uuid.NewString()
	}
	startEpoch := uint64(0)
	if action == ActionStart || action == ActionRestart {
		startEpoch = o.currentStopEpoch(room.ID)
	}
	runner := func(ctx context.Context, report func(jobs.TargetResult)) error {
		if failure := o.validateDependencySelection(ctx, action, room, worlds); failure != nil {
			reportDependencySelectionFailure(worlds, failure, report)
			return nil
		}
		if action == ActionStop && ctx.Err() == nil {
			o.interruptStarts(room.ID)
		}
		if action == ActionStart || action == ActionRestart {
			startContext, cancel := context.WithCancel(ctx)
			unregister, interrupted := o.registerStart(room.ID, startEpoch, cancel)
			defer func() {
				unregister()
				cancel()
			}()
			if interrupted {
				cancel()
			}
			ctx = startContext
		}
		if o.notifier != nil && !options.Immediate && (action == ActionStop || action == ActionRestart) {
			metadata, _ := ctx.Value(operationAuditContextKey{}).(OperationAuditMetadata)
			if notifyErr := o.notifier.BeforeOperation(ctx, room.ID, string(action), metadata.Source, metadata.JobID); notifyErr != nil {
				if errors.Is(notifyErr, context.Canceled) || errors.Is(notifyErr, context.DeadlineExceeded) {
					for _, world := range worlds {
						report(jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusCanceled, Error: &jobs.Error{Code: "JOB_CANCELED", Message: "操作通知倒计时已取消"}})
					}
					return nil
				}
			}
		}
		ctx, release, err := roomops.Acquire(ctx, room.ID)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				message := "任务已取消"
				if action == ActionStart || action == ActionRestart {
					message = "启动已被停止请求取消"
				}
				for _, world := range worlds {
					report(jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusCanceled, Error: &jobs.Error{Code: "JOB_CANCELED", Message: message}})
				}
				return nil
			}
			return err
		}
		defer release()
		var activeLease *operationlease.Lease
		if o.leases != nil {
			lease, leaseErr := o.leases.Acquire(ctx, room.ID, leaseOperationKey, o.leaseTTL)
			if leaseErr != nil {
				code := "ROOM_LEASE_FAILED"
				if errors.Is(leaseErr, operationlease.ErrBusy) {
					code = "ROOM_LEASE_BUSY"
				}
				for _, world := range worlds {
					report(jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: code, Message: leaseErr.Error()}})
				}
				return nil
			}
			activeLease = &lease
			defer func() { _ = o.leases.Release(*activeLease) }()
		}
		currentRoom, currentWorlds, err := o.resolvePlan(room.ID, plannedWorldIDs)
		if err != nil {
			for _, world := range worlds {
				report(jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "ROOM_CHANGED", Message: "等待执行期间房间或分片配置已变化，请重新提交操作"}})
			}
			return nil
		}
		orderWorlds(currentWorlds, action)
		if action == ActionStop || action == ActionRestart {
			if err := maintenance.Check(ctx); err != nil {
				return err
			}
		}
		if action == ActionStart || action == ActionRestart {
			// Game Lua is supported by every DST installation. Only optional
			// runtimes need an additional target capability check here.
			if options.RuntimeMode != shared.RuntimePerformanceModeGame {
				if _, modeErr := o.RequireRuntimeMode(ctx, currentRoom.ID, plannedWorldIDs, options.RuntimeMode); modeErr != nil {
					for _, world := range currentWorlds {
						report(jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "RUNTIME_MODE_UNAVAILABLE", Message: modeErr.Error()}})
					}
					return nil
				}
			}
		}
		if activeLease != nil {
			renewed, renewErr := o.leases.Renew(ctx, *activeLease, o.leaseTTL)
			if renewErr != nil {
				for _, world := range currentWorlds {
					report(jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "ROOM_LEASE_LOST", Message: renewErr.Error()}})
				}
				return nil
			}
			*activeLease = renewed
		}
		emit := func(result worldExecutionResult) {
			report(targetResult(action, result))
		}
		switch action {
		case ActionStart:
			o.executeStartPhase(ctx, currentRoom, currentWorlds, activeLease, operationIDs, options.RuntimeMode, emit)
		case ActionStop:
			o.executeStopPhase(ctx, currentRoom, currentWorlds, activeLease, operationIDs, emit)
		case ActionRestart:
			o.executeRestartPlan(ctx, currentRoom, currentWorlds, activeLease, operationIDs, options.RuntimeMode, report)
		default:
			for _, world := range currentWorlds {
				result := o.executeWorld(ctx, action, currentRoom, world, activeLease, operationIDs[world.ID], "")
				emit(result)
			}
		}
		return nil
	}
	return targets, runner, nil
}

type worldExecutionResult struct {
	world   rooms.World
	message string
	err     error
}

type dependencySelectionFailure struct {
	code    string
	message string
}

func (o *Operations) executeWorld(ctx context.Context, action Action, room rooms.Room, world rooms.World, lease *operationlease.Lease, operationID string, runtimeMode shared.RuntimePerformanceMode) worldExecutionResult {
	return o.executeWorldWithProgress(ctx, action, func(ctx context.Context) worldExecutionResult {
		message, err := o.executePlaced(ctx, action, room, world, lease, operationID, runtimeMode)
		return worldExecutionResult{world: world, message: message, err: err}
	}, shared.WorldOperationProgress{WorldID: world.ID, Name: world.Name, IsMaster: isMasterWorld(world)})
}

func (o *Operations) executeConcurrent(ctx context.Context, action Action, room rooms.Room, worlds []rooms.World, lease *operationlease.Lease, operationIDs map[string]string, runtimeMode shared.RuntimePerformanceMode, emit func(worldExecutionResult)) map[string]worldExecutionResult {
	results := make(map[string]worldExecutionResult, len(worlds))
	if len(worlds) == 0 {
		return results
	}
	completed := make(chan worldExecutionResult, len(worlds))
	for _, world := range worlds {
		world := world
		go func() {
			completed <- o.executeWorld(ctx, action, room, world, lease, operationIDs[world.ID], runtimeMode)
		}()
	}
	for range worlds {
		result := <-completed
		results[result.world.ID] = result
		if emit != nil {
			emit(result)
		}
	}
	return results
}

// executeStartPhase submits every selected Shard without waiting for another
// Shard to become ready. Each Shard reports readiness or failure independently.
func (o *Operations) executeStartPhase(ctx context.Context, room rooms.Room, worlds []rooms.World, lease *operationlease.Lease, operationIDs map[string]string, runtimeMode shared.RuntimePerformanceMode, emit func(worldExecutionResult)) map[string]worldExecutionResult {
	return o.executeConcurrent(ctx, ActionStart, room, worlds, lease, operationIDs, runtimeMode, emit)
}

func (o *Operations) executeStopPhase(ctx context.Context, room rooms.Room, worlds []rooms.World, lease *operationlease.Lease, operationIDs map[string]string, emit func(worldExecutionResult)) map[string]worldExecutionResult {
	results := make(map[string]worldExecutionResult, len(worlds))
	master, dependents := splitMaster(worlds)
	for worldID, result := range o.executeConcurrent(ctx, ActionStop, room, dependents, lease, operationIDs, "", emit) {
		results[worldID] = result
	}
	if master != nil {
		result := o.executeWorld(ctx, ActionStop, room, *master, lease, operationIDs[master.ID], "")
		results[master.ID] = result
		if emit != nil {
			emit(result)
		}
	}
	return results
}

func (o *Operations) executeRestartPlan(ctx context.Context, room rooms.Room, worlds []rooms.World, lease *operationlease.Lease, startOperationIDs map[string]string, runtimeMode shared.RuntimePerformanceMode, report func(jobs.TargetResult)) {
	stopOperationIDs := operationIDsFor(worlds)
	stopped := o.executeStopPhase(ctx, room, worlds, lease, stopOperationIDs, func(result worldExecutionResult) {
		if result.err != nil {
			report(targetResult(ActionRestart, result))
		}
	})
	restartable := make([]rooms.World, 0, len(worlds))
	for _, world := range worlds {
		if result := stopped[world.ID]; result.err == nil {
			restartable = append(restartable, world)
		}
	}
	o.executeStartPhase(ctx, room, restartable, lease, startOperationIDs, runtimeMode, func(result worldExecutionResult) {
		if result.err == nil {
			result.message = "分片已重启"
		}
		report(targetResult(ActionRestart, result))
	})
}

func splitMaster(worlds []rooms.World) (*rooms.World, []rooms.World) {
	dependents := make([]rooms.World, 0, len(worlds))
	var master *rooms.World
	for _, world := range worlds {
		if isMasterWorld(world) && master == nil {
			value := world
			master = &value
			continue
		}
		dependents = append(dependents, world)
	}
	return master, dependents
}

func isMasterWorld(world rooms.World) bool {
	return world.IsMaster || world.Role == rooms.WorldRoleMaster
}

func masterCount(worlds []rooms.World) int {
	count := 0
	for _, world := range worlds {
		if isMasterWorld(world) {
			count++
		}
	}
	return count
}

func (o *Operations) validateDependencySelection(ctx context.Context, action Action, room rooms.Room, selectedWorlds []rooms.World) *dependencySelectionFailure {
	if action != ActionStart && action != ActionStop && action != ActionRestart {
		return nil
	}
	allWorlds, err := o.rooms.Worlds(room.ID)
	if err != nil {
		return &dependencySelectionFailure{
			code:    "DEPENDENCY_STATE_UNAVAILABLE",
			message: fmt.Sprintf("无法读取房间世界依赖关系，已拒绝执行: %v", err),
		}
	}
	if action == ActionStart || action == ActionRestart {
		switch count := masterCount(allWorlds); {
		case count == 0:
			return &dependencySelectionFailure{code: "ROOM_MASTER_MISSING", message: "房间没有主分片，无法启动"}
		case count > 1:
			return &dependencySelectionFailure{code: "ROOM_MASTER_MULTIPLE", message: "房间存在多个主分片，无法启动"}
		}
	}
	master, _ := splitMaster(allWorlds)
	if master == nil {
		return nil
	}
	selected := make(map[string]bool, len(selectedWorlds))
	selectedMaster := false
	selectedDependent := false
	for _, world := range selectedWorlds {
		selected[world.ID] = true
		if world.ID == master.ID {
			selectedMaster = true
		} else {
			selectedDependent = true
		}
	}

	if (action == ActionStart || action == ActionRestart) && selectedDependent && !selectedMaster {
		status, statusErr := o.StatusFor(ctx, room.ID, master.ID)
		if statusErr != nil || status.State == RuntimeUnknown {
			message := fmt.Sprintf("无法确认主世界“%s”的运行状态，已拒绝单独%s依赖世界", master.Name, dependencyActionLabel(action))
			if statusErr != nil {
				message += ": " + statusErr.Error()
			}
			return &dependencySelectionFailure{code: "DEPENDENCY_STATE_UNAVAILABLE", message: message}
		}
		if status.State != RuntimeRunning && status.State != RuntimeStarting {
			return &dependencySelectionFailure{
				code: "DEPENDENCY_SELECTION_INCOMPLETE",
				message: fmt.Sprintf(
					"主世界“%s”尚未运行；%s依赖世界时必须将主世界加入同一操作",
					master.Name, dependencyActionLabel(action),
				),
			}
		}
	}

	if (action == ActionStop || action == ActionRestart) && selectedMaster {
		missing := make([]string, 0)
		for _, world := range allWorlds {
			if world.ID == master.ID || selected[world.ID] {
				continue
			}
			status, statusErr := o.StatusFor(ctx, room.ID, world.ID)
			if statusErr != nil || status.State == RuntimeUnknown {
				message := fmt.Sprintf("无法确认依赖世界“%s”的运行状态，已拒绝%s主世界", world.Name, dependencyActionLabel(action))
				if statusErr != nil {
					message += ": " + statusErr.Error()
				}
				return &dependencySelectionFailure{code: "DEPENDENCY_STATE_UNAVAILABLE", message: message}
			}
			if status.State == RuntimeRunning || status.State == RuntimeStarting {
				missing = append(missing, world.Name)
			}
		}
		if len(missing) > 0 {
			return &dependencySelectionFailure{
				code: "DEPENDENCY_SELECTION_INCOMPLETE",
				message: fmt.Sprintf(
					"%s主世界前必须将正在运行的依赖世界加入同一操作: %s",
					dependencyActionLabel(action), strings.Join(missing, "、"),
				),
			}
		}
	}
	return nil
}

func dependencyActionLabel(action Action) string {
	switch action {
	case ActionStart:
		return "启动"
	case ActionStop:
		return "停止"
	case ActionRestart:
		return "重启"
	default:
		return "操作"
	}
}

func reportDependencySelectionFailure(worlds []rooms.World, failure *dependencySelectionFailure, report func(jobs.TargetResult)) {
	for _, world := range worlds {
		report(jobs.TargetResult{
			TargetID: world.ID,
			Status:   jobs.StatusFailed,
			Error:    &jobs.Error{Code: failure.code, Message: failure.message},
		})
	}
}

func operationIDsFor(worlds []rooms.World) map[string]string {
	values := make(map[string]string, len(worlds))
	for _, world := range worlds {
		values[world.ID] = uuid.NewString()
	}
	return values
}

func targetResult(action Action, result worldExecutionResult) jobs.TargetResult {
	if result.err == nil {
		return jobs.TargetResult{TargetID: result.world.ID, Status: jobs.StatusSucceeded, Message: result.message}
	}
	if errors.Is(result.err, context.Canceled) {
		return jobs.TargetResult{TargetID: result.world.ID, Status: jobs.StatusCanceled, Error: &jobs.Error{Code: "JOB_CANCELED", Message: "任务已取消"}}
	}
	return jobs.TargetResult{TargetID: result.world.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: operationErrorCode(action, result.err), Message: result.err.Error()}}
}

func (o *Operations) PlanBatch(action Action, selections []BatchRoomSelection) ([]jobs.TargetSpec, jobs.Runner, error) {
	if action != ActionStart && action != ActionStop && action != ActionRestart && action != ActionSave {
		return nil, nil, ErrUnknownAction
	}
	if len(selections) == 0 {
		return nil, nil, ErrNoRooms
	}
	type roomPlan struct {
		roomID  string
		worlds  []string
		targets []jobs.TargetSpec
		runner  jobs.Runner
	}
	seenRooms := make(map[string]bool, len(selections))
	plans := make([]roomPlan, 0, len(selections))
	batchTargets := make([]jobs.TargetSpec, 0)
	for _, selection := range selections {
		roomID := strings.TrimSpace(selection.RoomID)
		if roomID == "" || seenRooms[roomID] {
			return nil, nil, ErrInvalidBatch
		}
		seenRooms[roomID] = true
		room, worlds, err := o.resolvePlan(roomID, selection.WorldIDs)
		if err != nil {
			return nil, nil, err
		}
		worldIDs := make([]string, 0, len(worlds))
		for _, world := range worlds {
			worldIDs = append(worldIDs, world.ID)
		}
		targets, runner, err := o.planWithOptions(action, roomID, worldIDs, PlanOptions{RuntimeMode: selection.RuntimeMode})
		if err != nil {
			return nil, nil, err
		}
		for _, target := range targets {
			batchTargets = append(batchTargets, jobs.TargetSpec{
				ID: batchTargetID(roomID, target.ID), Name: room.Name + " / " + target.Name,
			})
		}
		plans = append(plans, roomPlan{roomID: roomID, worlds: worldIDs, targets: targets, runner: runner})
	}
	runner := func(ctx context.Context, report func(jobs.TargetResult)) error {
		if action == ActionStop && ctx.Err() == nil {
			for _, plan := range plans {
				o.interruptStarts(plan.roomID)
			}
		}
		for _, plan := range plans {
			reported := make(map[string]bool, len(plan.targets))
			roomCtx := ctx
			if operationprogress.Enabled(ctx) {
				roomCtx = operationprogress.WithReporter(ctx, func(update operationprogress.Update) {
					worlds := append([]shared.WorldOperationProgress(nil), update.Worlds...)
					for index := range worlds {
						worlds[index].WorldID = batchTargetID(plan.roomID, worlds[index].WorldID)
					}
					update.Worlds = worlds
					operationprogress.Report(ctx, update)
				})
			}
			err := plan.runner(roomCtx, func(result jobs.TargetResult) {
				reported[result.TargetID] = true
				result.TargetID = batchTargetID(plan.roomID, result.TargetID)
				report(result)
			})
			if err == nil {
				continue
			}
			for _, target := range plan.targets {
				if reported[target.ID] {
					continue
				}
				report(jobs.TargetResult{TargetID: batchTargetID(plan.roomID, target.ID), Status: jobs.StatusFailed, Error: &jobs.Error{
					Code: "ROOM_BATCH_FAILED", Message: err.Error(),
				}})
			}
		}
		return nil
	}
	return batchTargets, runner, nil
}

func batchTargetID(roomID, worldID string) string {
	return roomID + ":" + worldID
}

func (o *Operations) PreviewCapacity(ctx context.Context, action Action, roomID string, worldIDs []string) (topology.StartCapacityPreview, error) {
	if action != ActionStart && action != ActionRestart || o.placements == nil {
		return topology.StartCapacityPreview{RoomID: roomID, WorldIDs: append([]string(nil), worldIDs...)}, nil
	}
	return o.placements.PreviewStartCapacity(ctx, roomID, worldIDs)
}

func (o *Operations) RuntimeModes(ctx context.Context, roomID string, worldIDs []string) (RuntimeModeAvailability, error) {
	room, worlds, err := o.resolvePlan(roomID, worldIDs)
	if err != nil {
		return RuntimeModeAvailability{}, err
	}
	availability := RuntimeModeAvailability{
		RoomID: room.ID, DefaultMode: shared.RuntimePerformanceModeGame,
		Modes: []shared.RuntimePerformanceMode{
			shared.RuntimePerformanceModeGame,
			shared.RuntimePerformanceModeLuaJIT,
			shared.RuntimePerformanceModeArenaGC,
		},
		Packages: runtimePackageOptions(),
	}
	for _, world := range worlds {
		availability.WorldIDs = append(availability.WorldIDs, world.ID)
	}
	if o.placements == nil {
		availability.Modes = []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame}
		return availability, nil
	}
	seenTargets := make(map[string]bool)
	for _, world := range worlds {
		placement, resolveErr := o.placements.ResolveExecution(ctx, room.ID, world.ID)
		if resolveErr != nil {
			return RuntimeModeAvailability{}, resolveErr
		}
		endpointKey := placement.AppliedTargetID + "\x00" + placement.AppliedInstallationID
		if seenTargets[endpointKey] {
			continue
		}
		seenTargets[endpointKey] = true
		target := RuntimeModeTarget{
			TargetID: placement.AppliedTargetID, InstallationID: placement.AppliedInstallationID,
			TargetName: placement.Target.Name,
			OS:         placement.Target.OS, Arch: placement.Target.Arch,
			SupportedModes: []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame},
		}
		performance := placement.Target.Performance
		if placement.AppliedTargetID != "local" && !stringSliceContains(placement.Target.Capabilities, "shard.runtime-mode.v1") {
			target.ReasonCode = "agent_runtime_mode_unsupported"
			target.Reason = "Agent 版本不支持启动时选择 Lua 运行时"
		} else if performance == nil {
			target.ReasonCode = "inspection_unavailable"
			target.Reason = "尚未取得该机器的 LuaJIT 检测结果"
		} else {
			target.PackageVersion = performance.PackageVersion
			target.GameVersion = performance.GameVersion
			target.CompatibilityStatus = performance.Status
			target.Issues = append([]string(nil), performance.Issues...)
			if performance.Status == shared.RuntimePerformanceReady && performance.CanEnable {
				target.SupportedModes = normalizedRuntimeModes(performance.SupportedModes)
			} else {
				switch performance.Status {
				case shared.RuntimePerformanceNotInstalled:
					target.ReasonCode = "runtime_not_installed"
				case shared.RuntimePerformanceDetectedUnverified:
					target.ReasonCode = "runtime_unverified"
				case shared.RuntimePerformanceIncompatible:
					target.ReasonCode = "runtime_incompatible"
				default:
					target.ReasonCode = "runtime_unavailable"
				}
				target.Reason = "该机器尚未安装可启用的 DontStarveLuaJIT2"
			}
		}
		if version, valid := normalizeRuntimePackageVersion(target.PackageVersion); valid && version != "game" {
			known := false
			for _, option := range availability.Packages {
				if option.Version == version {
					known = true
					break
				}
			}
			if !known {
				availability.Packages = append(availability.Packages, RuntimePackageOption{ID: "dontstarve-luajit2-" + version, Provider: "dontstarve-luajit2", Version: version, Channel: "installed", Platforms: []string{"linux"}, ProcessScopedModes: false})
			}
		}
		availability.Targets = append(availability.Targets, target)
		availability.Modes = intersectRuntimeModes(availability.Modes, target.SupportedModes)
	}
	return availability, nil
}

func runtimePackageOptions() []RuntimePackageOption {
	return []RuntimePackageOption{
		{
			ID: "game", Provider: "game", Channel: "bundled",
			Platforms: []string{"darwin", "linux", "windows"}, ProcessScopedModes: true,
		},
		{
			ID: "dontstarve-luajit2-v2", Provider: "dontstarve-luajit2", Version: "2.9.2", Channel: "preview",
			Platforms: []string{"darwin", "linux", "windows"}, ProcessScopedModes: false,
		},
		{
			ID: "dontstarve-luajit2-v3", Provider: "dontstarve-luajit2", Version: "3.0.0", Channel: "upstream",
			Platforms: []string{"linux"}, ProcessScopedModes: false,
		},
	}
}

func (o *Operations) RequireRuntimeMode(ctx context.Context, roomID string, worldIDs []string, requested shared.RuntimePerformanceMode) (RuntimeModeAvailability, error) {
	mode, valid := shared.NormalizeRuntimePerformanceMode(requested)
	if !valid {
		return RuntimeModeAvailability{}, ErrInvalidRuntimeMode
	}
	if mode == shared.RuntimePerformanceModeGame {
		return RuntimeModeAvailability{
			RoomID: roomID, WorldIDs: append([]string(nil), worldIDs...),
			DefaultMode: shared.RuntimePerformanceModeGame,
			Modes:       []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame},
		}, nil
	}
	availability, err := o.RuntimeModes(ctx, roomID, worldIDs)
	if err != nil {
		return RuntimeModeAvailability{}, err
	}
	for _, supported := range availability.Modes {
		if supported == mode {
			return availability, nil
		}
	}
	return availability, &RuntimeModeError{Mode: mode, Availability: availability}
}

func (o *Operations) RequireRuntimeSelection(ctx context.Context, roomID string, worldIDs []string, requestedMode shared.RuntimePerformanceMode, requestedVersion string) (RuntimeModeAvailability, error) {
	availability, err := o.RequireRuntimeMode(ctx, roomID, worldIDs, requestedMode)
	if err != nil {
		return availability, err
	}
	mode, _ := shared.NormalizeRuntimePerformanceMode(requestedMode)
	version, valid := normalizeRuntimePackageVersion(requestedVersion)
	if mode == shared.RuntimePerformanceModeGame {
		if requestedVersion != "" && version != "game" {
			return availability, ErrInvalidRuntimeVersion
		}
		return availability, nil
	}
	// Runtime version was added after runtimeMode. Keep older API clients
	// compatible while new clients pin the version they displayed to the user.
	if strings.TrimSpace(requestedVersion) == "" {
		return availability, nil
	}
	if !valid || version == "game" {
		return availability, ErrInvalidRuntimeVersion
	}
	for _, target := range availability.Targets {
		installed, installedValid := normalizeRuntimePackageVersion(target.PackageVersion)
		if !installedValid || installed != version {
			return availability, &RuntimeVersionError{Version: version, Availability: availability}
		}
	}
	return availability, nil
}

var runtimePackageVersionPattern = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$`)

func normalizeRuntimePackageVersion(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	if strings.EqualFold(value, "game") {
		return "game", true
	}
	value = strings.TrimPrefix(strings.TrimPrefix(value, "v"), "V")
	if runtimePackageVersionPattern.MatchString(value) {
		return value, true
	}
	return "", false
}

func normalizedRuntimeModes(values []shared.RuntimePerformanceMode) []shared.RuntimePerformanceMode {
	result := make([]shared.RuntimePerformanceMode, 0, 4)
	for _, candidate := range []shared.RuntimePerformanceMode{
		shared.RuntimePerformanceModeGame,
		shared.RuntimePerformanceModeLuaJIT,
		shared.RuntimePerformanceModeArenaGC,
	} {
		for _, value := range values {
			if value == candidate {
				result = append(result, candidate)
				break
			}
		}
	}
	if !stringRuntimeModeContains(result, shared.RuntimePerformanceModeGame) {
		result = append([]shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame}, result...)
	}
	return result
}

func intersectRuntimeModes(left, right []shared.RuntimePerformanceMode) []shared.RuntimePerformanceMode {
	result := make([]shared.RuntimePerformanceMode, 0, len(left))
	for _, value := range left {
		if stringRuntimeModeContains(right, value) {
			result = append(result, value)
		}
	}
	return result
}

func stringRuntimeModeContains(values []shared.RuntimePerformanceMode, expected shared.RuntimePerformanceMode) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func stringSliceContains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func (o *Operations) PreviewBatchCapacity(ctx context.Context, action Action, selections []BatchRoomSelection) (topology.BatchStartCapacityPreview, error) {
	inputs := make([]topology.StartCapacitySelection, 0, len(selections))
	for _, selection := range selections {
		inputs = append(inputs, topology.StartCapacitySelection{RoomID: selection.RoomID, WorldIDs: append([]string(nil), selection.WorldIDs...)})
	}
	if action != ActionStart && action != ActionRestart || o.placements == nil {
		return topology.BatchStartCapacityPreview{Rooms: inputs}, nil
	}
	return o.placements.PreviewBatchStartCapacity(ctx, inputs)
}

func (o *Operations) RequireCapacityConfirmation(ctx context.Context, action Action, roomID string, worldIDs []string, allow bool) error {
	preview, err := o.PreviewCapacity(ctx, action, roomID, worldIDs)
	if err != nil {
		return err
	}
	if preview.RequiresRiskConfirmation && !allow {
		return &CapacityRiskError{Preview: preview}
	}
	return nil
}

func (o *Operations) RequireBatchCapacityConfirmation(ctx context.Context, action Action, selections []BatchRoomSelection, allow bool) error {
	preview, err := o.PreviewBatchCapacity(ctx, action, selections)
	if err != nil {
		return err
	}
	if preview.RequiresRiskConfirmation && !allow {
		return &BatchCapacityRiskError{Preview: preview}
	}
	return nil
}

func (o *Operations) currentStopEpoch(roomID string) uint64 {
	o.activeMu.Lock()
	defer o.activeMu.Unlock()
	return o.stopEpoch[roomID]
}

func (o *Operations) registerStart(roomID string, expectedEpoch uint64, cancel context.CancelFunc) (func(), bool) {
	o.activeMu.Lock()
	if o.stopEpoch[roomID] != expectedEpoch {
		o.activeMu.Unlock()
		return func() {}, true
	}
	o.activeSeq++
	id := o.activeSeq
	if o.activeStarts[roomID] == nil {
		o.activeStarts[roomID] = make(map[uint64]context.CancelFunc)
	}
	o.activeStarts[roomID][id] = cancel
	o.activeMu.Unlock()
	return func() {
		o.activeMu.Lock()
		delete(o.activeStarts[roomID], id)
		if len(o.activeStarts[roomID]) == 0 {
			delete(o.activeStarts, roomID)
		}
		o.activeMu.Unlock()
	}, false
}

func (o *Operations) interruptStarts(roomID string) {
	o.activeMu.Lock()
	o.stopEpoch[roomID]++
	cancels := make([]context.CancelFunc, 0, len(o.activeStarts[roomID]))
	for _, cancel := range o.activeStarts[roomID] {
		cancels = append(cancels, cancel)
	}
	o.activeMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (o *Operations) resolvePlan(roomID string, selectedWorldIDs []string) (rooms.Room, []rooms.World, error) {
	room, err := o.rooms.Room(roomID)
	if err != nil {
		return rooms.Room{}, nil, err
	}
	if !room.Managed {
		return rooms.Room{}, nil, ErrRoomNotManaged
	}
	if !controlName.MatchString(room.DirectoryName) {
		return rooms.Room{}, nil, ErrUnsafeName
	}
	worlds, err := o.rooms.Worlds(roomID)
	if err != nil {
		return rooms.Room{}, nil, err
	}
	worlds, err = selectWorlds(worlds, selectedWorldIDs)
	if err != nil {
		return rooms.Room{}, nil, err
	}
	if len(worlds) == 0 {
		return rooms.Room{}, nil, ErrNoWorlds
	}
	for _, world := range worlds {
		if !controlName.MatchString(world.DirectoryName) {
			return rooms.Room{}, nil, fmt.Errorf("%w: %s", ErrUnsafeName, world.DirectoryName)
		}
	}
	return room, worlds, nil
}

func (o *Operations) IsRunning(ctx context.Context, roomName, worldName string) (bool, error) {
	status, err := o.Status(ctx, roomName, worldName)
	return status.State == RuntimeRunning, err
}

func (o *Operations) Status(ctx context.Context, roomName, worldName string) (RuntimeStatus, error) {
	if control, ok := o.control.(interface {
		Status(context.Context, string, string) (RuntimeStatus, error)
	}); ok {
		return control.Status(ctx, roomName, worldName)
	}
	running, err := o.control.IsRunning(ctx, roomName, worldName)
	if err != nil {
		return RuntimeStatus{State: RuntimeUnknown}, err
	}
	if running {
		return RuntimeStatus{State: RuntimeRunning, SessionExists: true}, nil
	}
	return RuntimeStatus{State: RuntimeStopped}, nil
}

func (o *Operations) StatusFor(ctx context.Context, roomID, worldID string) (RuntimeStatus, error) {
	room, worlds, err := o.resolvePlan(roomID, []string{worldID})
	if err != nil {
		return RuntimeStatus{State: RuntimeUnknown}, err
	}
	world := worlds[0]
	if o.placements == nil {
		return o.Status(ctx, room.DirectoryName, world.DirectoryName)
	}
	if o.runtime != nil {
		status, err := o.runtime.Status(ctx, room.ID, world.ID)
		if err != nil {
			return RuntimeStatus{State: RuntimeUnknown}, err
		}
		return runtimeStatusFromShared(status), nil
	}
	applied, err := o.placements.AppliedPlacement(room.ID, world.ID)
	if err != nil {
		return RuntimeStatus{State: RuntimeUnknown}, err
	}
	if applied.AppliedTargetID == "local" {
		return o.Status(ctx, room.DirectoryName, world.DirectoryName)
	}
	resolved, err := o.placements.ResolveExecution(ctx, room.ID, world.ID)
	if err != nil {
		return RuntimeStatus{State: RuntimeUnknown}, err
	}
	request := shared.ShardOperationRequest{
		ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: uuid.NewString(),
		Action: shared.ShardActionStatus, Cluster: room.DirectoryName, Shard: world.DirectoryName,
		TopologyRevision: resolved.Revision,
	}
	result, err := o.remote.ExecuteShard(ctx, resolved.AppliedTargetID, request, 30)
	if err != nil {
		return RuntimeStatus{State: RuntimeUnknown}, err
	}
	return runtimeStatusFromShared(result.Result.Status), nil
}

func (o *Operations) executePlaced(ctx context.Context, action Action, room rooms.Room, world rooms.World, lease *operationlease.Lease, operationID string, runtimeMode shared.RuntimePerformanceMode) (string, error) {
	if o.placements == nil {
		return o.execute(ctx, action, room.DirectoryName, world.DirectoryName, runtimeMode)
	}
	applied, err := o.placements.AppliedPlacement(room.ID, world.ID)
	if err != nil {
		return "", err
	}
	o.observeOperation(ctx, action, room.ID, world.ID, applied, lease, operationID)
	launchOptions := shared.RuntimeLaunchOptions{}
	if o.runtime != nil && action != ActionCleanup {
		shardAction, timeoutSeconds, err := remoteAction(action, o.startTimeout, o.stopTimeout)
		if err != nil {
			return "", err
		}
		request := shared.ShardOperationRequest{
			ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: operationID, OperationKey: operationID,
			Action: shardAction, Cluster: room.DirectoryName, Shard: world.DirectoryName, TopologyRevision: applied.Revision,
			RuntimeMode: runtimeMode, LaunchOptions: launchOptions,
		}
		if lease != nil {
			expiresAt := lease.ExpiresAt.UTC()
			request.LeaseID, request.FencingToken, request.LeaseExpiresAt = lease.LeaseID, lease.FencingToken, &expiresAt
		}
		result, err := o.runtime.ExecutePlacedShard(ctx, room.ID, world.ID, request, time.Duration(timeoutSeconds)*time.Second)
		if err != nil {
			return "", err
		}
		reportStartupResult(ctx, action, result.Status)
		message := strings.TrimSpace(result.Message)
		if message == "" {
			message = "分片操作已完成"
		}
		return message, nil
	}
	if applied.AppliedTargetID == "local" {
		message, err := o.execute(ctx, action, room.DirectoryName, world.DirectoryName, runtimeMode)
		if err == nil {
			reportStartupResult(ctx, action, shared.ShardRuntimeStatus{State: "running"})
		}
		return message, err
	}
	if action == ActionCleanup {
		return "", errors.New("远程节点不开放强制清理；请先诊断 Agent 和分片状态")
	}
	if lease == nil {
		return "", errors.New("远程分片操作缺少控制面房间租约")
	}
	resolved, err := o.placements.ResolveExecution(ctx, room.ID, world.ID)
	if err != nil {
		return "", err
	}
	shardAction, timeout, err := remoteAction(action, o.startTimeout, o.stopTimeout)
	if err != nil {
		return "", err
	}
	expiresAt := lease.ExpiresAt.UTC()
	request := shared.ShardOperationRequest{
		ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: operationID, OperationKey: operationID,
		Action: shardAction, Cluster: room.DirectoryName, Shard: world.DirectoryName, TopologyRevision: resolved.Revision,
		RuntimeMode: runtimeMode, LaunchOptions: launchOptions,
		LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, LeaseExpiresAt: &expiresAt,
	}
	result, err := o.remote.ExecuteShard(ctx, resolved.AppliedTargetID, request, timeout)
	if err != nil {
		return "", err
	}
	reportStartupResult(ctx, action, result.Result.Status)
	message := strings.TrimSpace(result.Result.Message)
	if message == "" {
		message = "远程分片操作已完成"
	}
	return message, nil
}

func (o *Operations) observeOperation(ctx context.Context, action Action, roomID, worldID string, placement topology.ExecutionPlacement, lease *operationlease.Lease, operationID string) {
	if o.observer == nil {
		return
	}
	metadata, _ := ctx.Value(operationAuditContextKey{}).(OperationAuditMetadata)
	audit := OperationAudit{
		RoomID: roomID, WorldID: worldID, TargetID: placement.AppliedTargetID,
		Action: action, OperationID: operationID, OperationKey: operationID, TopologyRevision: placement.Revision,
		JobID: metadata.JobID, RequestID: metadata.RequestID, Source: metadata.Source,
	}
	if strings.HasPrefix(placement.AppliedTargetID, "agent:") {
		audit.AgentID = strings.TrimPrefix(placement.AppliedTargetID, "agent:")
	}
	if lease != nil {
		audit.LeaseID, audit.FencingToken = lease.LeaseID, lease.FencingToken
	}
	_ = o.observer.ObserveOperation(ctx, audit)
}

func remoteAction(action Action, startTimeout, stopTimeout time.Duration) (shared.ShardAction, int, error) {
	timeout := 30
	switch action {
	case ActionStart:
		timeout = int(startTimeout.Seconds())
		return shared.ShardActionStart, boundedRemoteTimeout(timeout), nil
	case ActionStop:
		timeout = int(stopTimeout.Seconds())
		return shared.ShardActionStop, boundedRemoteTimeout(timeout), nil
	case ActionRestart:
		timeout = int((startTimeout + stopTimeout).Seconds())
		return shared.ShardActionRestart, boundedRemoteTimeout(timeout), nil
	case ActionSave:
		return shared.ShardActionSave, 30, nil
	default:
		return "", 0, ErrUnknownAction
	}
}

func boundedRemoteTimeout(value int) int {
	if value < 5 {
		return 5
	}
	if value > 300 {
		return 300
	}
	return value
}

func runtimeStatusFromShared(status shared.ShardRuntimeStatus) RuntimeStatus {
	state := RuntimeState(status.State)
	if state != RuntimeStopped && state != RuntimeStarting && state != RuntimeRunning && state != RuntimeFailed {
		state = RuntimeUnknown
	}
	return RuntimeStatus{State: state, StartupStage: status.StartupStage, Code: status.Code, Message: status.Message, SessionExists: status.SessionExists, Paused: status.Paused}
}

func operationErrorCode(action Action, err error) string {
	var executionError *topology.ExecutionError
	if errors.As(err, &executionError) && executionError.Code != "" {
		return executionError.Code
	}
	switch {
	case errors.Is(err, topology.ErrResourceConflict):
		return "RESOURCE_PREFLIGHT_FAILED"
	case errors.Is(err, agents.ErrAgentOffline):
		return "AGENT_OFFLINE"
	case errors.Is(err, agents.ErrUnsupportedAction):
		return "AGENT_CAPABILITY_MISSING"
	case errors.Is(err, operationlease.ErrLeaseLost):
		return "ROOM_LEASE_LOST"
	default:
		return strings.ToUpper(string(action)) + "_FAILED"
	}
}

func (o *Operations) execute(ctx context.Context, action Action, roomName, worldName string, runtimeMode shared.RuntimePerformanceMode) (string, error) {
	status, err := o.Status(ctx, roomName, worldName)
	if err != nil {
		return "", fmt.Errorf("检查分片状态: %w", err)
	}
	switch action {
	case ActionStart:
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if status.State == RuntimeRunning {
			return "分片已在运行", nil
		}
		if status.State != RuntimeStarting {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if err := startControlWithRuntimeMode(ctx, o.control, roomName, worldName, runtimeMode); err != nil {
				return "", err
			}
		}
		if err := o.waitFor(ctx, roomName, worldName, true, o.startTimeout); err != nil {
			return "", err
		}
		return "分片已启动", nil
	case ActionStop:
		if !status.SessionExists && status.State == RuntimeStopped {
			return "分片已停止", nil
		}
		if err := o.control.Stop(ctx, roomName, worldName); err != nil {
			return "", err
		}
		if err := o.waitFor(ctx, roomName, worldName, false, o.stopTimeout); err != nil {
			return "", err
		}
		return "分片已停止", nil
	case ActionCleanup:
		if !status.SessionExists {
			return "没有需要清理的残留会话", nil
		}
		if status.State == RuntimeRunning {
			return "", errors.New("分片仍在运行，请先执行停止操作")
		}
		if err := o.cleanup(ctx, roomName, worldName); err != nil {
			return "", err
		}
		if err := o.waitFor(ctx, roomName, worldName, false, o.stopTimeout); err != nil {
			return "", err
		}
		return "失败或残留会话已清理", nil
	case ActionRestart:
		if status.SessionExists || status.State != RuntimeStopped {
			if err := o.control.Stop(ctx, roomName, worldName); err != nil {
				return "", err
			}
			if err := o.waitFor(ctx, roomName, worldName, false, o.stopTimeout); err != nil {
				return "", err
			}
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := startControlWithRuntimeMode(ctx, o.control, roomName, worldName, runtimeMode); err != nil {
			return "", err
		}
		if err := o.waitFor(ctx, roomName, worldName, true, o.startTimeout); err != nil {
			return "", err
		}
		return "分片已重启", nil
	case ActionSave:
		if status.State != RuntimeRunning {
			return "", errors.New("分片未运行，无法保存")
		}
		control, ok := o.control.(ConsoleControl)
		if !ok {
			return "", errors.New("当前运行控制器不支持保存分片")
		}
		if err := control.Send(ctx, roomName, worldName, "c_save()"); err != nil {
			return "", err
		}
		return "已请求 DST 保存当前分片", nil
	default:
		return "", ErrUnknownAction
	}
}

func startControlWithRuntimeMode(ctx context.Context, control Control, roomName, worldName string, mode shared.RuntimePerformanceMode) error {
	normalized, valid := shared.NormalizeRuntimePerformanceMode(mode)
	if !valid {
		return ErrInvalidRuntimeMode
	}
	if runtimeControl, ok := control.(interface {
		StartWithRuntimeMode(context.Context, string, string, shared.RuntimePerformanceMode) error
	}); ok {
		return runtimeControl.StartWithRuntimeMode(ctx, roomName, worldName, normalized)
	}
	if normalized != shared.RuntimePerformanceModeGame {
		return ErrRuntimeModeUnavailable
	}
	return control.Start(ctx, roomName, worldName)
}

func (o *Operations) waitFor(ctx context.Context, roomName, worldName string, expected bool, timeout time.Duration) error {
	reportStartup := StartupProgressReporter(ctx)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(o.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			state := "启动"
			if !expected {
				state = "停止"
			}
			message := fmt.Sprintf("等待分片%s超时", state)
			if expected {
				return errors.New(o.cleanupFailedStart(ctx, roomName, worldName, message))
			}
			return errors.New(message)
		case <-ticker.C:
			status, err := o.Status(ctx, roomName, worldName)
			if err != nil {
				return err
			}
			if expected {
				reportStartup(status)
			}
			if expected && status.State == RuntimeFailed {
				if status.Message == "" {
					status.Message = "DST 启动失败，请检查分片日志"
				}
				return errors.New(o.cleanupFailedStart(ctx, roomName, worldName, status.Message))
			}
			if expected && status.State == RuntimeRunning {
				return nil
			}
			if !expected && status.State == RuntimeStopped && !status.SessionExists {
				return nil
			}
		}
	}
}

func (o *Operations) cleanupFailedStart(ctx context.Context, roomName, worldName, message string) string {
	if err := o.cleanup(ctx, roomName, worldName); err != nil {
		return fmt.Sprintf("%s；清理失败启动会话时出错: %v", message, err)
	}
	return message
}

func (o *Operations) cleanup(ctx context.Context, roomName, worldName string) error {
	control, ok := o.control.(CleanupControl)
	if !ok {
		return errors.New("当前运行控制器不支持强制清理会话")
	}
	return control.Cleanup(ctx, roomName, worldName)
}

func selectWorlds(worlds []rooms.World, selected []string) ([]rooms.World, error) {
	if len(selected) == 0 {
		return worlds, nil
	}
	wanted := make(map[string]bool, len(selected))
	for _, id := range selected {
		if wanted[id] {
			return nil, fmt.Errorf("duplicate world id: %s", id)
		}
		wanted[id] = true
	}
	result := make([]rooms.World, 0, len(wanted))
	for _, world := range worlds {
		if wanted[world.ID] {
			result = append(result, world)
			delete(wanted, world.ID)
		}
	}
	if len(wanted) > 0 {
		return nil, rooms.ErrWorldNotFound
	}
	return result, nil
}

func orderWorlds(worlds []rooms.World, action Action) {
	sort.SliceStable(worlds, func(i, j int) bool {
		leftMaster := worlds[i].Role == rooms.WorldRoleMaster
		rightMaster := worlds[j].Role == rooms.WorldRoleMaster
		if leftMaster == rightMaster {
			return strings.ToLower(worlds[i].Name) < strings.ToLower(worlds[j].Name)
		}
		if action == ActionStop || action == ActionCleanup {
			return !leftMaster
		}
		return leftMaster
	})
}
