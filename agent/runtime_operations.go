package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"dont/internal/consoledispatch"
	"dont/internal/roomops"
	"dont/internal/runtimefiles"
	"dont/internal/shards"
	"dont/shared"
)

type rememberedRuntimeOperation struct {
	OperationID  string                        `json:"operation_id"`
	Action       shared.RuntimeAction          `json:"action"`
	Cluster      string                        `json:"cluster"`
	Shard        string                        `json:"shard"`
	Fingerprint  string                        `json:"fingerprint"`
	Completed    bool                          `json:"completed"`
	ErrorMessage string                        `json:"error_message,omitempty"`
	AcceptedAt   time.Time                     `json:"accepted_at"`
	Result       shared.RuntimeOperationResult `json:"result"`
}

type backgroundConsoleRuntime interface {
	SendBackground(context.Context, string, string, string, string) error
}

type consoleHealthRuntime interface {
	ConsoleHealth(string, string) consoledispatch.Health
}

func (a *Agent) executeRuntimeOperation(commandType string, request *shared.RuntimeOperationRequest, timeout int) (shared.RuntimeOperationResult, error) {
	if request == nil {
		return shared.RuntimeOperationResult{}, errors.New("Runtime 操作负载缺失")
	}
	now := a.now().UTC()
	if err := validateRuntimeOperationRequest(commandType, *request, timeout, now); err != nil {
		return shared.RuntimeOperationResult{}, err
	}
	installation, exists := a.runtimeInstallation(request.InstallationID)
	if !exists {
		return shared.RuntimeOperationResult{}, errors.New("Agent 未登记该 DST 安装")
	}
	if err := validateShardOwnership(installation, request.Cluster, request.Shard); err != nil {
		return shared.RuntimeOperationResult{}, err
	}
	control, err := a.runtimeControl(installation)
	if err != nil {
		return shared.RuntimeOperationResult{}, err
	}
	operationContext, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	if !shared.RuntimeActionMutates(request.Action) {
		return a.observeRuntimeAction(operationContext, control, installation, *request)
	}
	operationContext, release, err := roomops.Acquire(operationContext, request.InstallationID+"\x00"+request.Cluster)
	if err != nil {
		return shared.RuntimeOperationResult{}, err
	}
	defer release()
	if cached, beginErr := a.shardState.beginRuntime(*request, now); cached != nil {
		return *cached, beginErr
	} else if beginErr != nil {
		return shared.RuntimeOperationResult{}, beginErr
	}
	result, operationErr := executeConsoleSend(operationContext, control, *request)
	if finishErr := a.shardState.finishRuntime(*request, result, operationErr); finishErr != nil {
		return shared.RuntimeOperationResult{}, fmt.Errorf("保存 Agent Runtime 操作结果: %w", finishErr)
	}
	return result, operationErr
}

func validateRuntimeOperationRequest(commandType string, request shared.RuntimeOperationRequest, timeout int, now time.Time) error {
	if request.ProtocolVersion != shared.RuntimeOperationProtocolVersion || !shared.IsRuntimeAction(request.Action) ||
		commandType != string(request.Action) || timeout < 5 || timeout > 300 ||
		!runtimeInstallationID.MatchString(request.InstallationID) || !shardResourceName.MatchString(request.Cluster) ||
		!shardResourceName.MatchString(request.Shard) || !operationIdentity.MatchString(request.OperationID) ||
		len(request.TopologyRevision) < 1 || len(request.TopologyRevision) > 128 || strings.ContainsAny(request.TopologyRevision, "\x00\r\n") {
		return errors.New("Runtime 操作请求无效")
	}
	if shared.RuntimeActionMutates(request.Action) {
		if !operationIdentity.MatchString(request.OperationKey) || !operationIdentity.MatchString(request.LeaseID) ||
			request.FencingToken == 0 || request.LeaseExpiresAt == nil || request.LeaseExpiresAt.Before(now.Add(-30*time.Second)) ||
			request.LeaseExpiresAt.After(now.Add(10*time.Minute)) {
			return errors.New("Runtime 操作租约无效或已过期")
		}
	}
	switch request.Action {
	case shared.RuntimeActionConsoleHealth:
		if request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil {
			return errors.New("控制台健康请求包含无关负载")
		}
	case shared.RuntimeActionConsoleSend:
		if request.Console == nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil {
			return errors.New("控制台请求负载无效")
		}
		console := request.Console
		if console.Mode != shared.ConsoleModeManaged && console.Mode != shared.ConsoleModeProbe && console.Mode != shared.ConsoleModeRaw ||
			strings.TrimSpace(console.Command) == "" || len(console.Command) > 4096 || !utf8.ValidString(console.Command) ||
			strings.ContainsAny(console.Command, "\x00\r\n") || len(console.CoalesceKey) > 80 || strings.ContainsAny(console.CoalesceKey, "\x00\r\n") ||
			console.Mode != shared.ConsoleModeProbe && strings.TrimSpace(console.CoalesceKey) != "" {
			return errors.New("控制台请求内容无效")
		}
	case shared.RuntimeActionReadLogs:
		if request.Logs == nil || request.Console != nil || request.Artifacts != nil || request.Observation != nil ||
			request.Logs.Cursor < -1 || request.Logs.MaxBytes < 1 || request.Logs.MaxBytes > runtimefiles.MaximumLogBytes ||
			request.Logs.MaxLines < 1 || request.Logs.MaxLines > 2000 || len([]rune(request.Logs.Query)) > 256 ||
			len(request.Logs.FileID) > 128 || strings.ContainsAny(request.Logs.FileID, "\x00\r\n") {
			return errors.New("日志读取请求无效")
		}
	case shared.RuntimeActionReadArtifacts:
		if request.Artifacts == nil || request.Console != nil || request.Logs != nil || request.Observation != nil || !runtimefiles.IsArtifactKind(request.Artifacts.Kind) {
			return errors.New("Runtime 制品读取请求无效")
		}
	case shared.RuntimeActionObserveOperation:
		if request.Observation == nil || request.Console != nil || request.Logs != nil || request.Artifacts != nil ||
			!operationIdentity.MatchString(request.Observation.ObservedOperationID) ||
			request.Observation.ObservedOperationKey != "" && !operationIdentity.MatchString(request.Observation.ObservedOperationKey) {
			return errors.New("Runtime 操作观察请求无效")
		}
	}
	return nil
}

func (a *Agent) observeRuntimeAction(ctx context.Context, control shardRuntimeControl, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	result := runtimeResult(request, shared.RuntimeOutcomeObserved, "Runtime 状态已读取")
	switch request.Action {
	case shared.RuntimeActionConsoleHealth:
		status, err := control.Status(ctx, request.Cluster, request.Shard)
		health := shared.RuntimeConsoleHealth{Available: true, Accepting: status.State == shards.RuntimeRunning, Runtime: sharedRuntimeStatus(status)}
		if provider, ok := control.(consoleHealthRuntime); ok {
			current := provider.ConsoleHealth(request.Cluster, request.Shard)
			health.Accepting, health.Busy, health.Pending = current.Accepting, current.Busy, current.Pending
			health.Class, health.CoalesceKey, health.StartedAt = string(current.Class), current.CoalesceKey, current.StartedAt
		}
		result.ConsoleHealth = &health
		return result, err
	case shared.RuntimeActionReadLogs:
		chunk, err := runtimefiles.ReadLogs(ctx, installation.SavePath, request.Cluster, request.Shard, *request.Logs)
		result.Logs = &chunk
		return result, err
	case shared.RuntimeActionReadArtifacts:
		bundle, err := runtimefiles.ReadArtifacts(ctx, installation.SavePath, request.Cluster, request.Shard, request.Artifacts.Kind)
		result.Artifacts = &bundle
		return result, err
	case shared.RuntimeActionObserveOperation:
		evidence, err := a.shardState.observeRuntime(request.InstallationID, request.Cluster, *request.Observation)
		result.Evidence = &evidence
		return result, err
	default:
		return result, errors.New("Runtime 操作不受支持")
	}
}

func executeConsoleSend(ctx context.Context, control shardRuntimeControl, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	console := *request.Console
	var err error
	if console.Mode == shared.ConsoleModeProbe && strings.TrimSpace(console.CoalesceKey) != "" {
		if background, ok := control.(backgroundConsoleRuntime); ok {
			err = background.SendBackground(ctx, request.Cluster, request.Shard, console.CoalesceKey, console.Command)
		} else {
			err = control.Send(ctx, request.Cluster, request.Shard, console.Command)
		}
	} else {
		err = control.Send(ctx, request.Cluster, request.Shard, console.Command)
	}
	result := runtimeResult(request, shared.RuntimeOutcomeSent, "控制台命令已发送；是否执行成功需要对应回执或状态证据")
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
	}
	return result, err
}

func runtimeResult(request shared.RuntimeOperationRequest, outcome shared.RuntimeOutcome, message string) shared.RuntimeOperationResult {
	return shared.RuntimeOperationResult{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: request.OperationID, OperationKey: request.OperationKey,
		InstallationID: request.InstallationID, Action: request.Action, Cluster: request.Cluster, Shard: request.Shard,
		FencingToken: request.FencingToken, Outcome: outcome, Message: message, ObservedAt: time.Now().UTC(),
	}
}

func sharedRuntimeStatus(status shards.RuntimeStatus) shared.ShardRuntimeStatus {
	return shared.ShardRuntimeStatus{State: string(status.State), Code: status.Code, Message: status.Message, SessionExists: status.SessionExists}
}

func (state *shardOperationState) beginRuntime(request shared.RuntimeOperationRequest, now time.Time) (*shared.RuntimeOperationResult, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	roomKey := request.InstallationID + "\x00" + strings.ToLower(request.Cluster)
	originalRoom, roomExisted := state.Rooms[roomKey]
	room := cloneShardRoomState(originalRoom)
	if room.RuntimeOperations == nil {
		room.RuntimeOperations = make(map[string]rememberedRuntimeOperation)
	}
	fingerprint, err := runtimeOperationFingerprint(request)
	if err != nil {
		return nil, err
	}
	if remembered, exists := room.RuntimeOperations[request.OperationKey]; exists {
		if remembered.Fingerprint != fingerprint {
			return nil, errors.New("幂等键已被另一项 Runtime 操作使用")
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
	room.FencingToken, room.LeaseID = request.FencingToken, request.LeaseID
	room.RuntimeOperations[request.OperationKey] = rememberedRuntimeOperation{
		OperationID: request.OperationID, Action: request.Action, Cluster: request.Cluster, Shard: request.Shard, Fingerprint: fingerprint, AcceptedAt: now.UTC(),
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

func (state *shardOperationState) finishRuntime(request shared.RuntimeOperationRequest, result shared.RuntimeOperationResult, operationErr error) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	roomKey := request.InstallationID + "\x00" + strings.ToLower(request.Cluster)
	room := state.Rooms[roomKey]
	remembered, exists := room.RuntimeOperations[request.OperationKey]
	if !exists {
		return errors.New("Agent Runtime 操作状态缺失")
	}
	remembered.Completed, remembered.Result = true, result
	if operationErr != nil {
		remembered.ErrorMessage = operationErr.Error()
	}
	room.RuntimeOperations[request.OperationKey] = remembered
	state.Rooms[roomKey] = room
	return state.persistLocked()
}

func (state *shardOperationState) observeRuntime(installationID, cluster string, request shared.RuntimeObservationRequest) (shared.RuntimeOperationEvidence, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	room := state.Rooms[installationID+"\x00"+strings.ToLower(cluster)]
	for key, operation := range room.RuntimeOperations {
		if operation.OperationID != request.ObservedOperationID && key != request.ObservedOperationKey {
			continue
		}
		outcome := operation.Result.Outcome
		if !operation.Completed {
			outcome = shared.RuntimeOutcomeUnknown
		}
		return shared.RuntimeOperationEvidence{OperationID: request.ObservedOperationID, Action: string(operation.Action), Completed: operation.Completed, Outcome: outcome, Message: operation.Result.Message, ObservedAt: operation.Result.ObservedAt}, nil
	}
	for key, operation := range room.Operations {
		if operation.Result.OperationID != request.ObservedOperationID && (operation.Result.OperationID != "" || key != request.ObservedOperationKey) {
			continue
		}
		outcome := shared.RuntimeOutcomeConfirmed
		if !operation.Completed {
			outcome = shared.RuntimeOutcomeUnknown
		} else if operation.ErrorMessage != "" {
			outcome = shared.RuntimeOutcomeFailed
		}
		return shared.RuntimeOperationEvidence{OperationID: request.ObservedOperationID, Action: string(operation.Action), Completed: operation.Completed, Outcome: outcome, Message: operation.Result.Message, ObservedAt: operation.Result.ObservedAt}, nil
	}
	return shared.RuntimeOperationEvidence{}, errors.New("未找到需要观察的 Runtime 操作")
}

func runtimeOperationFingerprint(request shared.RuntimeOperationRequest) (string, error) {
	copy := request
	copy.LeaseExpiresAt = nil
	data, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
