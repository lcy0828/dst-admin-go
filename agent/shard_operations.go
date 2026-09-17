package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/roomops"
	"dont/internal/runtimeperformance"
	"dont/internal/shards"
	"dont/shared"
)

const maximumRememberedOperationsPerRoom = 128

var (
	shardResourceName          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	operationIdentity          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	errOperationOutcomeUnknown = errors.New("该幂等操作此前已被 Agent 接收，但结果因中断无法确认")
)

type shardRuntimeControl interface {
	Status(context.Context, string, string) (shards.RuntimeStatus, error)
	Start(context.Context, string, string) error
	Stop(context.Context, string, string) error
	Send(context.Context, string, string, string) error
}

type shardRuntimeModeControl interface {
	StartWithRuntimeMode(context.Context, string, string, shared.RuntimePerformanceMode) error
}

type shardRuntimeLaunchControl interface {
	StartWithRuntimeOptions(context.Context, string, string, shared.RuntimePerformanceMode, shared.RuntimeLaunchOptions) error
}

type shardRuntimeFactory func(RuntimeInstallation) (shardRuntimeControl, error)

func newTmuxShardRuntime(installation RuntimeInstallation) (shardRuntimeControl, error) {
	return shards.NewTmuxControl(shards.TmuxConfig{
		SaveRoot: installation.SavePath, UGCDirectory: installation.UGCPath,
		ServerPath: installation.ServerPath, ServerMode: installation.ServerMode, ConsoleSocket: installation.ConsoleSocket,
		LegacyConsoleSockets: installation.LegacyConsoleSockets,
		OwnerLabel:           "Agent Runtime " + installation.ID,
	})
}

func newShardRuntimeControl(installation RuntimeInstallation) (shardRuntimeControl, error) {
	if installation.Driver == "container" {
		return newContainerShardRuntime(installation, newExecContainerCLI(installation.ContainerEngine))
	}
	return newTmuxShardRuntime(installation)
}

type rememberedShardOperation struct {
	InstallationID string                        `json:"installation_id"`
	Action         shared.ShardAction            `json:"action"`
	Cluster        string                        `json:"cluster"`
	Shard          string                        `json:"shard"`
	RuntimeMode    shared.RuntimePerformanceMode `json:"runtime_mode,omitempty"`
	LaunchOptions  shared.RuntimeLaunchOptions   `json:"launch_options,omitempty"`
	Completed      bool                          `json:"completed"`
	ErrorMessage   string                        `json:"error_message,omitempty"`
	AcceptedAt     time.Time                     `json:"accepted_at"`
	Result         shared.ShardOperationResult   `json:"result"`
}

type shardRoomState struct {
	FencingToken      uint64                                `json:"fencing_token"`
	LeaseID           string                                `json:"lease_id"`
	Operations        map[string]rememberedShardOperation   `json:"operations"`
	RuntimeOperations map[string]rememberedRuntimeOperation `json:"runtime_operations,omitempty"`
}

type shardOperationState struct {
	mu            sync.Mutex                `json:"-"`
	path          string                    `json:"-"`
	fresh         bool                      `json:"-"`
	Version       int                       `json:"version"`
	Initialized   bool                      `json:"initialized"`
	InitializedAt *time.Time                `json:"initialized_at,omitempty"`
	Rooms         map[string]shardRoomState `json:"rooms"`
}

type consoleHazard struct {
	Cluster string
	Shard   string
}

func loadShardOperationState(path string) (*shardOperationState, error) {
	state := &shardOperationState{path: path, fresh: true, Version: 1, Rooms: make(map[string]shardRoomState)}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("解析 Agent 分片操作状态: %w", err)
	}
	state.path = path
	state.fresh = false
	if state.Version != 1 || state.Rooms == nil {
		return nil, errors.New("Agent 分片操作状态版本无效")
	}
	// Version 1 state written before the ownership sentinel is trusted because
	// its persisted fencing/idempotency history proves this is not a new volume.
	if !state.Initialized {
		state.Initialized = true
	}
	if err := shared.EnsurePrivateFile(path); err != nil {
		return nil, err
	}
	return state, nil
}

func (state *shardOperationState) ownershipInitialized() bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.Initialized
}

func (state *shardOperationState) initializeOwnership(now time.Time) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.Initialized {
		return nil
	}
	instant := now.UTC()
	state.Initialized, state.InitializedAt, state.fresh = true, &instant, false
	if err := state.persistLocked(); err != nil {
		state.Initialized, state.InitializedAt, state.fresh = false, nil, true
		return err
	}
	return nil
}

func (state *shardOperationState) pendingConsoleHazards() map[string][]consoleHazard {
	state.mu.Lock()
	defer state.mu.Unlock()
	result := make(map[string][]consoleHazard)
	seen := make(map[string]bool)
	for roomKey, room := range state.Rooms {
		installationID, _, ok := strings.Cut(roomKey, "\x00")
		if !ok {
			continue
		}
		for _, operation := range room.RuntimeOperations {
			if operation.Completed || operation.Action != shared.RuntimeActionConsoleSend {
				continue
			}
			identity := installationID + "\x00" + operation.Cluster + "\x00" + operation.Shard
			if seen[identity] {
				continue
			}
			seen[identity] = true
			result[installationID] = append(result[installationID], consoleHazard{Cluster: operation.Cluster, Shard: operation.Shard})
		}
	}
	return result
}

func (state *shardOperationState) begin(request shared.ShardOperationRequest, now time.Time) (*shared.ShardOperationResult, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	roomKey := request.InstallationID + "\x00" + strings.ToLower(request.Cluster)
	originalRoom, roomExisted := state.Rooms[roomKey]
	room := cloneShardRoomState(originalRoom)
	if room.Operations == nil {
		room.Operations = make(map[string]rememberedShardOperation)
	}
	if remembered, exists := room.Operations[request.OperationKey]; exists {
		if remembered.InstallationID != request.InstallationID || remembered.Action != request.Action ||
			remembered.Cluster != request.Cluster || remembered.Shard != request.Shard || remembered.RuntimeMode != request.RuntimeMode ||
			remembered.LaunchOptions != request.LaunchOptions {
			return nil, errors.New("幂等键已被另一项分片操作使用")
		}
		if !remembered.Completed {
			return nil, errOperationOutcomeUnknown
		}
		result := remembered.Result
		result.Idempotent = true
		if remembered.ErrorMessage != "" {
			return &result, errors.New(remembered.ErrorMessage)
		}
		return &result, nil
	}
	if request.FencingToken < room.FencingToken {
		return nil, fmt.Errorf("fencing token 已过期: %d < %d", request.FencingToken, room.FencingToken)
	}
	if request.FencingToken == room.FencingToken && room.FencingToken > 0 && room.LeaseID != request.LeaseID {
		return nil, errors.New("fencing token 已由另一租约占用")
	}
	room.FencingToken = request.FencingToken
	room.LeaseID = request.LeaseID
	room.Operations[request.OperationKey] = rememberedShardOperation{
		InstallationID: request.InstallationID, Action: request.Action, Cluster: request.Cluster,
		Shard: request.Shard, RuntimeMode: request.RuntimeMode, LaunchOptions: request.LaunchOptions, AcceptedAt: now.UTC(),
	}
	state.Rooms[roomKey] = room
	if err := state.persistLocked(); err != nil {
		if roomExisted {
			state.Rooms[roomKey] = originalRoom
		} else {
			delete(state.Rooms, roomKey)
		}
		return nil, err
	}
	return nil, nil
}

func cloneShardRoomState(value shardRoomState) shardRoomState {
	operations := make(map[string]rememberedShardOperation, len(value.Operations))
	for key, operation := range value.Operations {
		operations[key] = operation
	}
	value.Operations = operations
	runtimeOperations := make(map[string]rememberedRuntimeOperation, len(value.RuntimeOperations))
	for key, operation := range value.RuntimeOperations {
		runtimeOperations[key] = operation
	}
	value.RuntimeOperations = runtimeOperations
	return value
}

func (state *shardOperationState) finish(request shared.ShardOperationRequest, result shared.ShardOperationResult, operationErr error) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	roomKey := request.InstallationID + "\x00" + strings.ToLower(request.Cluster)
	room := state.Rooms[roomKey]
	remembered, exists := room.Operations[request.OperationKey]
	if !exists {
		return errors.New("Agent 分片操作状态缺失")
	}
	remembered.Completed = true
	remembered.Result = result
	if operationErr != nil {
		remembered.ErrorMessage = operationErr.Error()
	}
	room.Operations[request.OperationKey] = remembered
	trimRememberedOperations(room.Operations)
	state.Rooms[roomKey] = room
	return state.persistLocked()
}

func (state *shardOperationState) persistLocked() error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return shared.WritePrivateFile(state.path, data)
}

func trimRememberedOperations(values map[string]rememberedShardOperation) {
	if len(values) <= maximumRememberedOperationsPerRoom {
		return
	}
	type operationTime struct {
		key string
		at  time.Time
	}
	completed := make([]operationTime, 0, len(values))
	for key, value := range values {
		if value.Completed {
			completed = append(completed, operationTime{key: key, at: value.Result.ObservedAt})
		}
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].at.Before(completed[j].at) })
	for _, item := range completed {
		if len(values) <= maximumRememberedOperationsPerRoom {
			break
		}
		delete(values, item.key)
	}
}

func (a *Agent) executeShardOperation(commandType string, request *shared.ShardOperationRequest, timeout int) (shared.ShardOperationResult, error) {
	return a.executeShardOperationContext(context.Background(), commandType, request, timeout)
}

func (a *Agent) executeShardOperationContext(ctx context.Context, commandType string, request *shared.ShardOperationRequest, timeout int) (shared.ShardOperationResult, error) {
	if request == nil {
		return shared.ShardOperationResult{}, errors.New("分片操作负载缺失")
	}
	runtimeMode, runtimeModeValid := shared.NormalizeRuntimePerformanceMode(request.RuntimeMode)
	if !runtimeModeValid {
		return shared.ShardOperationResult{}, errors.New("Lua 运行时模式无效")
	}
	if request.Action == shared.ShardActionStart || request.Action == shared.ShardActionRestart {
		request.RuntimeMode = runtimeMode
	} else if runtimeMode == shared.RuntimePerformanceModeGame {
		// Older callers can reuse a normalized start request for a later stop
		// operation. Treat the universal default as unspecified while keeping
		// explicit LuaJIT modes invalid for non-start actions.
		request.RuntimeMode = ""
	}
	now := a.now().UTC()
	if err := validateShardOperationRequest(commandType, *request, timeout, now); err != nil {
		return shared.ShardOperationResult{}, err
	}
	installation, exists := a.runtimeInstallation(request.InstallationID)
	if !exists {
		return shared.ShardOperationResult{}, errors.New("Agent 未登记该 DST 安装")
	}
	if err := validateShardOwnership(installation, request.Cluster, request.Shard); err != nil {
		return shared.ShardOperationResult{}, err
	}
	if err := validateRuntimeModeForInstallation(installation, request.Action, request.RuntimeMode); err != nil {
		return shared.ShardOperationResult{}, err
	}
	runtimeControl, err := a.runtimeControl(installation)
	if err != nil {
		return shared.ShardOperationResult{}, err
	}
	if installation.Driver == "container" && shared.ShardActionMutates(request.Action) {
		ownershipContext, cancelOwnership := context.WithTimeout(context.Background(), 5*time.Second)
		err := a.ensureContainerOwnership(ownershipContext, runtimeControl, request.Cluster, request.Shard)
		cancelOwnership()
		if err != nil {
			return shared.ShardOperationResult{}, err
		}
	}
	operationContext, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	if request.Action == shared.ShardActionStatus {
		return observeShardOperation(operationContext, runtimeControl, *request, now)
	}
	operationContext, release, err := roomops.Acquire(operationContext, request.InstallationID+"\x00"+request.Cluster+"\x00"+request.Shard)
	if err != nil {
		return shared.ShardOperationResult{}, err
	}
	defer release()
	if cached, beginErr := a.shardState.begin(*request, now); cached != nil {
		return *cached, beginErr
	} else if beginErr != nil {
		return shared.ShardOperationResult{}, beginErr
	}
	var afterLaunch func()
	if request.Action == shared.ShardActionStart || request.Action == shared.ShardActionRestart {
		// Release the per-Shard lifecycle lock after process creation so sibling
		// Shards can launch while this request continues observing readiness.
		afterLaunch = release
	}
	result, operationErr := mutateShard(operationContext, runtimeControl, *request, now, afterLaunch)
	if finishErr := a.shardState.finish(*request, result, operationErr); finishErr != nil {
		return shared.ShardOperationResult{}, fmt.Errorf("保存 Agent 分片操作结果: %w", finishErr)
	}
	return result, operationErr
}

type containerOwnershipProbe interface {
	ManagedRuntimeExists(context.Context, string, string) (bool, error)
}

type consoleHazardRecovery interface {
	RecoverConsoleHazard(context.Context, string, string) error
}

func (a *Agent) ensureContainerOwnership(ctx context.Context, control shardRuntimeControl, cluster, shard string) error {
	if a.shardState.ownershipInitialized() {
		return nil
	}
	probe, ok := control.(containerOwnershipProbe)
	if !ok {
		return errors.New("CONTAINER_OWNERSHIP_UNVERIFIED: 容器 Runtime 无法证明本地所有权")
	}
	exists, err := probe.ManagedRuntimeExists(ctx, cluster, shard)
	if err != nil {
		return fmt.Errorf("CONTAINER_OWNERSHIP_UNVERIFIED: %w", err)
	}
	if exists && !strings.EqualFold(strings.TrimSpace(os.Getenv("DST_ADMIN_ADOPT_EXISTING_CONTAINERS")), "true") {
		return errors.New("CONTAINER_OWNERSHIP_STATE_LOST: Agent 状态卷缺少但已发现受管容器；已拒绝自动关联，请恢复状态卷，或在确认不存在重复实例后临时设置 DST_ADMIN_ADOPT_EXISTING_CONTAINERS=true")
	}
	if err := a.shardState.initializeOwnership(a.now()); err != nil {
		return fmt.Errorf("建立容器 Runtime 所有权哨兵: %w", err)
	}
	return nil
}

func (a *Agent) initializeFreshRuntimeOwnership() {
	if a == nil || a.shardState == nil || a.shardState.ownershipInitialized() {
		return
	}
	hasContainer, allObserved, existing := false, true, false
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, installation := range a.Config.RuntimeInstallations {
		if installation.Driver != "container" {
			continue
		}
		hasContainer = true
		control, err := a.runtimeControl(installation)
		probe, ok := control.(containerOwnershipProbe)
		if err != nil || !ok {
			allObserved = false
			continue
		}
		found, err := probe.ManagedRuntimeExists(ctx, "", "")
		if err != nil {
			allObserved = false
			continue
		}
		existing = existing || found
	}
	adopt := strings.EqualFold(strings.TrimSpace(os.Getenv("DST_ADMIN_ADOPT_EXISTING_CONTAINERS")), "true")
	if !hasContainer || allObserved && (!existing || adopt) {
		_ = a.shardState.initializeOwnership(a.now())
	}
}

func (a *Agent) runtimeControl(installation RuntimeInstallation) (shardRuntimeControl, error) {
	if err := validateRuntimeDeployment(installation); err != nil {
		return nil, err
	}
	a.shardRuntimeMu.Lock()
	defer a.shardRuntimeMu.Unlock()
	if existing := a.shardRuntimes[installation.ID]; existing != nil {
		return existing, nil
	}
	created, err := a.shardRuntime(installation)
	if err != nil {
		return nil, err
	}
	if hazards := a.consoleHazards[installation.ID]; len(hazards) > 0 {
		recovery, ok := created.(consoleHazardRecovery)
		if !ok {
			return nil, errors.New("CONSOLE_RECOVERY_UNAVAILABLE: Agent 无法恢复中断的控制台输入状态")
		}
		for _, hazard := range hazards {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := recovery.RecoverConsoleHazard(ctx, hazard.Cluster, hazard.Shard)
			cancel()
			if err != nil {
				return nil, fmt.Errorf("CONSOLE_RECOVERY_FAILED: %w", err)
			}
		}
		delete(a.consoleHazards, installation.ID)
	}
	a.shardRuntimes[installation.ID] = created
	return created, nil
}

func validateShardOperationRequest(commandType string, request shared.ShardOperationRequest, timeout int, now time.Time) error {
	_, runtimeModeValid := shared.NormalizeRuntimePerformanceMode(request.RuntimeMode)
	if request.ProtocolVersion != shared.ShardOperationProtocolVersion || !shared.IsShardAction(request.Action) ||
		commandType != string(request.Action) || timeout < 5 || timeout > 300 ||
		!runtimeInstallationID.MatchString(request.InstallationID) || !shardResourceName.MatchString(request.Cluster) ||
		!shardResourceName.MatchString(request.Shard) || !operationIdentity.MatchString(request.OperationID) ||
		len(request.TopologyRevision) < 1 || len(request.TopologyRevision) > 128 || strings.ContainsAny(request.TopologyRevision, "\x00\r\n") || !runtimeModeValid {
		return errors.New("分片操作请求无效")
	}
	if request.Action != shared.ShardActionStart && request.Action != shared.ShardActionRestart &&
		(request.RuntimeMode != "" || request.LaunchOptions.SkipUpdateServerMods) {
		return errors.New("只有启动或重启操作可以指定 Lua 运行时")
	}
	if !shared.ShardActionMutates(request.Action) {
		return nil
	}
	if !operationIdentity.MatchString(request.OperationKey) || !operationIdentity.MatchString(request.LeaseID) ||
		request.FencingToken == 0 || request.LeaseExpiresAt == nil ||
		request.LeaseExpiresAt.Before(now.Add(-30*time.Second)) || request.LeaseExpiresAt.After(now.Add(10*time.Minute)) {
		return errors.New("分片操作租约无效或已过期")
	}
	return nil
}

func validateRuntimeModeForInstallation(installation RuntimeInstallation, action shared.ShardAction, mode shared.RuntimePerformanceMode) error {
	if action != shared.ShardActionStart && action != shared.ShardActionRestart {
		return nil
	}
	normalized, valid := shared.NormalizeRuntimePerformanceMode(mode)
	if !valid {
		return errors.New("Lua 运行时模式无效")
	}
	if normalized == shared.RuntimePerformanceModeGame {
		return nil
	}
	report := runtimeperformance.Inspect(runtimeperformance.Options{
		ServerPath: installation.ServerPath, ServerMode: installation.ServerMode,
		WorkshopContentPath: installation.WorkshopContentPath,
	})
	if report.Status != shared.RuntimePerformanceReady || !report.CanEnable || !slices.Contains(report.SupportedModes, normalized) {
		return fmt.Errorf("RUNTIME_MODE_UNAVAILABLE: 当前 DST 安装不支持 %s", normalized)
	}
	return nil
}

func validateShardOwnership(installation RuntimeInstallation, cluster, shard string) error {
	root, err := filepath.EvalSymlinks(installation.SavePath)
	if err != nil {
		return fmt.Errorf("读取受信存档根目录: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	clusterPath := filepath.Join(root, cluster)
	shardPath := filepath.Join(clusterPath, shard)
	resolvedCluster, err := filepath.EvalSymlinks(clusterPath)
	if err != nil {
		return fmt.Errorf("目标房间不在 Agent 受信安装中: %w", err)
	}
	if !pathWithin(root, resolvedCluster) {
		return errors.New("目标房间路径越出 Agent 受信存档根目录")
	}
	resolvedShard, err := filepath.EvalSymlinks(shardPath)
	if err != nil {
		return fmt.Errorf("目标世界不在 Agent 受信安装中: %w", err)
	}
	if !pathWithin(resolvedCluster, resolvedShard) {
		return errors.New("目标世界路径越出对应房间目录")
	}
	for _, required := range []string{filepath.Join(clusterPath, "cluster.ini"), filepath.Join(resolvedShard, "server.ini")} {
		info, statErr := os.Stat(required)
		if statErr != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("目标世界缺少受管配置文件: %s", filepath.Base(required))
		}
	}
	serverInfo, err := os.Stat(installation.ServerPath)
	if err != nil || (!serverInfo.IsDir() && !serverInfo.Mode().IsRegular()) {
		return errors.New("受信 DST 服务端路径不可用")
	}
	return nil
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func observeShardOperation(ctx context.Context, control shardRuntimeControl, request shared.ShardOperationRequest, observedAt time.Time) (shared.ShardOperationResult, error) {
	status, err := control.Status(ctx, request.Cluster, request.Shard)
	result := shardResult(request, status, "分片状态已刷新", observedAt)
	return result, err
}

func mutateShard(ctx context.Context, control shardRuntimeControl, request shared.ShardOperationRequest, observedAt time.Time, afterLaunch func()) (shared.ShardOperationResult, error) {
	status, err := control.Status(ctx, request.Cluster, request.Shard)
	if err != nil {
		return shardResult(request, status, "检查分片状态失败", observedAt), err
	}
	message := ""
	switch request.Action {
	case shared.ShardActionStart:
		if status.State != shards.RuntimeRunning && status.State != shards.RuntimeStarting {
			err = startShardWithRuntimeOptions(ctx, control, request.Cluster, request.Shard, request.RuntimeMode, request.LaunchOptions)
		}
		if err == nil {
			if afterLaunch != nil {
				afterLaunch()
			}
			status, err = waitForShardState(ctx, control, request.Cluster, request.Shard, true)
		}
		message = "分片已启动"
	case shared.ShardActionStop:
		if status.SessionExists {
			err = control.Stop(ctx, request.Cluster, request.Shard)
		}
		if err == nil {
			status, err = waitForShardState(ctx, control, request.Cluster, request.Shard, false)
		}
		message = "分片已停止"
	case shared.ShardActionRestart:
		if status.SessionExists {
			err = control.Stop(ctx, request.Cluster, request.Shard)
			if err == nil {
				status, err = waitForShardState(ctx, control, request.Cluster, request.Shard, false)
			}
		}
		if err == nil {
			err = startShardWithRuntimeOptions(ctx, control, request.Cluster, request.Shard, request.RuntimeMode, request.LaunchOptions)
		}
		if err == nil {
			if afterLaunch != nil {
				afterLaunch()
			}
			status, err = waitForShardState(ctx, control, request.Cluster, request.Shard, true)
		}
		message = "分片已重启"
	case shared.ShardActionSave:
		if status.State != shards.RuntimeRunning {
			err = errors.New("分片未运行，无法保存")
		} else {
			err = control.Send(ctx, request.Cluster, request.Shard, "c_save()")
		}
		message = "已请求 DST 保存当前分片"
	}
	if err != nil {
		message = err.Error()
	}
	return shardResult(request, status, message, time.Now().UTC()), err
}

func startShardWithRuntimeOptions(ctx context.Context, control shardRuntimeControl, cluster, shard string, mode shared.RuntimePerformanceMode, options shared.RuntimeLaunchOptions) error {
	normalized, valid := shared.NormalizeRuntimePerformanceMode(mode)
	if !valid {
		return errors.New("Lua 运行时模式无效")
	}
	if runtimeControl, ok := control.(shardRuntimeLaunchControl); ok {
		return runtimeControl.StartWithRuntimeOptions(ctx, cluster, shard, normalized, options)
	}
	if runtimeControl, ok := control.(shardRuntimeModeControl); ok {
		return runtimeControl.StartWithRuntimeMode(ctx, cluster, shard, normalized)
	}
	if normalized != shared.RuntimePerformanceModeGame {
		return errors.New("RUNTIME_MODE_UNAVAILABLE: 当前 Runtime 不支持启动模式选择")
	}
	return control.Start(ctx, cluster, shard)
}

func waitForShardState(ctx context.Context, control shardRuntimeControl, cluster, shard string, running bool) (shards.RuntimeStatus, error) {
	reportStartup := shards.StartupProgressReporter(ctx)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := control.Status(ctx, cluster, shard)
		if err != nil {
			return status, err
		}
		if running {
			reportStartup(status)
		}
		if running && status.State == shards.RuntimeRunning {
			return status, nil
		}
		if running && status.State == shards.RuntimeFailed {
			message := status.Message
			if message == "" {
				message = "DST 分片启动失败"
			}
			return status, errors.New(message)
		}
		if !running && !status.SessionExists && status.State == shards.RuntimeStopped {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-ticker.C:
		}
	}
}

func shardResult(request shared.ShardOperationRequest, status shards.RuntimeStatus, message string, observedAt time.Time) shared.ShardOperationResult {
	return shared.ShardOperationResult{
		ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: request.OperationID,
		OperationKey: request.OperationKey, InstallationID: request.InstallationID, Action: request.Action,
		Cluster: request.Cluster, Shard: request.Shard, RuntimeMode: request.RuntimeMode, LaunchOptions: request.LaunchOptions, FencingToken: request.FencingToken,
		Status:  shared.ShardRuntimeStatus{State: string(status.State), StartupStage: status.StartupStage, Code: status.Code, Message: status.Message, SessionExists: status.SessionExists, Paused: status.Paused},
		Message: message, ObservedAt: observedAt.UTC(),
	}
}
