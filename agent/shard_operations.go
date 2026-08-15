package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/roomops"
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

type shardRuntimeFactory func(RuntimeInstallation) (shardRuntimeControl, error)

func newTmuxShardRuntime(installation RuntimeInstallation) (shardRuntimeControl, error) {
	return shards.NewTmuxControl(shards.TmuxConfig{
		SaveRoot: installation.SavePath, UGCDirectory: installation.UGCPath,
		ServerPath: installation.ServerPath, ServerMode: installation.ServerMode,
	})
}

func newShardRuntimeControl(installation RuntimeInstallation) (shardRuntimeControl, error) {
	if installation.Driver == "container" {
		return newContainerShardRuntime(installation, newExecContainerCLI(installation.ContainerEngine))
	}
	return newTmuxShardRuntime(installation)
}

type rememberedShardOperation struct {
	InstallationID string                      `json:"installation_id"`
	Action         shared.ShardAction          `json:"action"`
	Cluster        string                      `json:"cluster"`
	Shard          string                      `json:"shard"`
	Completed      bool                        `json:"completed"`
	ErrorMessage   string                      `json:"error_message,omitempty"`
	AcceptedAt     time.Time                   `json:"accepted_at"`
	Result         shared.ShardOperationResult `json:"result"`
}

type shardRoomState struct {
	FencingToken      uint64                                `json:"fencing_token"`
	LeaseID           string                                `json:"lease_id"`
	Operations        map[string]rememberedShardOperation   `json:"operations"`
	RuntimeOperations map[string]rememberedRuntimeOperation `json:"runtime_operations,omitempty"`
}

type shardOperationState struct {
	mu      sync.Mutex                `json:"-"`
	path    string                    `json:"-"`
	Version int                       `json:"version"`
	Rooms   map[string]shardRoomState `json:"rooms"`
}

func loadShardOperationState(path string) (*shardOperationState, error) {
	state := &shardOperationState{path: path, Version: 1, Rooms: make(map[string]shardRoomState)}
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
	if state.Version != 1 || state.Rooms == nil {
		return nil, errors.New("Agent 分片操作状态版本无效")
	}
	if err := shared.EnsurePrivateFile(path); err != nil {
		return nil, err
	}
	return state, nil
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
			remembered.Cluster != request.Cluster || remembered.Shard != request.Shard {
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
		Shard: request.Shard, AcceptedAt: now.UTC(),
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
	if request == nil {
		return shared.ShardOperationResult{}, errors.New("分片操作负载缺失")
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
	runtimeControl, err := a.runtimeControl(installation)
	if err != nil {
		return shared.ShardOperationResult{}, err
	}
	operationContext, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	if request.Action == shared.ShardActionStatus {
		return observeShardOperation(operationContext, runtimeControl, *request, now)
	}
	operationContext, release, err := roomops.Acquire(operationContext, request.InstallationID+"\x00"+request.Cluster)
	if err != nil {
		return shared.ShardOperationResult{}, err
	}
	defer release()
	if cached, beginErr := a.shardState.begin(*request, now); cached != nil {
		return *cached, beginErr
	} else if beginErr != nil {
		return shared.ShardOperationResult{}, beginErr
	}
	result, operationErr := mutateShard(operationContext, runtimeControl, *request, now)
	if finishErr := a.shardState.finish(*request, result, operationErr); finishErr != nil {
		return shared.ShardOperationResult{}, fmt.Errorf("保存 Agent 分片操作结果: %w", finishErr)
	}
	return result, operationErr
}

func (a *Agent) runtimeControl(installation RuntimeInstallation) (shardRuntimeControl, error) {
	a.shardRuntimeMu.Lock()
	defer a.shardRuntimeMu.Unlock()
	if existing := a.shardRuntimes[installation.ID]; existing != nil {
		return existing, nil
	}
	created, err := a.shardRuntime(installation)
	if err != nil {
		return nil, err
	}
	a.shardRuntimes[installation.ID] = created
	return created, nil
}

func validateShardOperationRequest(commandType string, request shared.ShardOperationRequest, timeout int, now time.Time) error {
	if request.ProtocolVersion != shared.ShardOperationProtocolVersion || !shared.IsShardAction(request.Action) ||
		commandType != string(request.Action) || timeout < 5 || timeout > 300 ||
		!runtimeInstallationID.MatchString(request.InstallationID) || !shardResourceName.MatchString(request.Cluster) ||
		!shardResourceName.MatchString(request.Shard) || !operationIdentity.MatchString(request.OperationID) ||
		len(request.TopologyRevision) < 1 || len(request.TopologyRevision) > 128 || strings.ContainsAny(request.TopologyRevision, "\x00\r\n") {
		return errors.New("分片操作请求无效")
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

func mutateShard(ctx context.Context, control shardRuntimeControl, request shared.ShardOperationRequest, observedAt time.Time) (shared.ShardOperationResult, error) {
	status, err := control.Status(ctx, request.Cluster, request.Shard)
	if err != nil {
		return shardResult(request, status, "检查分片状态失败", observedAt), err
	}
	message := ""
	switch request.Action {
	case shared.ShardActionStart:
		if status.State != shards.RuntimeRunning && status.State != shards.RuntimeStarting {
			err = control.Start(ctx, request.Cluster, request.Shard)
		}
		if err == nil {
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
			err = control.Start(ctx, request.Cluster, request.Shard)
		}
		if err == nil {
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

func waitForShardState(ctx context.Context, control shardRuntimeControl, cluster, shard string, running bool) (shards.RuntimeStatus, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := control.Status(ctx, cluster, shard)
		if err != nil {
			return status, err
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
		Cluster: request.Cluster, Shard: request.Shard, FencingToken: request.FencingToken,
		Status:  shared.ShardRuntimeStatus{State: string(status.State), Code: status.Code, Message: status.Message, SessionExists: status.SessionExists},
		Message: message, ObservedAt: observedAt.UTC(),
	}
}
