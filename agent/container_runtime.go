package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"dont/internal/consoledispatch"
	"dont/internal/runtimefiles"
	"dont/internal/shards"
	"dont/shared"
	dsttmux "dont/tmux"
)

const maximumContainerCLIOutput = 256 * 1024

var managedContainerID = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

var errManagedContainerNotFound = errors.New("未找到受管分片容器")

type containerCLI interface {
	Available() bool
	Run(context.Context, ...string) ([]byte, error)
}

type execContainerCLI struct{ binary string }

func newExecContainerCLI(name string) *execContainerCLI {
	binary, _ := exec.LookPath(strings.TrimSpace(name))
	return &execContainerCLI{binary: binary}
}

func (c *execContainerCLI) Available() bool { return c != nil && c.binary != "" }

func (c *execContainerCLI) Run(ctx context.Context, arguments ...string) ([]byte, error) {
	if !c.Available() {
		return nil, errors.New("容器 CLI 不可用")
	}
	output, err := exec.CommandContext(ctx, c.binary, arguments...).CombinedOutput()
	if len(output) > maximumContainerCLIOutput {
		output = append(output[:maximumContainerCLIOutput], []byte("\n[输出已截断]")...)
	}
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return output, errors.New(message)
	}
	return output, nil
}

type managedContainer struct {
	ID      string
	Name    string
	State   string
	Cluster string
	Shard   string
}

type containerShardRuntime struct {
	installation RuntimeInstallation
	cli          containerCLI
	dispatcher   *consoledispatch.Dispatcher
	gracePeriod  time.Duration
	pollInterval time.Duration
}

type containerExitState struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	Restarting bool   `json:"Restarting"`
	OOMKilled  bool   `json:"OOMKilled"`
	Dead       bool   `json:"Dead"`
	ExitCode   int    `json:"ExitCode"`
	Error      string `json:"Error"`
	FinishedAt string `json:"FinishedAt"`
}

type ContainerStopFallbackError struct {
	Reason string
	Forced bool
}

func (e *ContainerStopFallbackError) Error() string {
	if e.Forced {
		return "容器未在优雅停止期限内退出，已强制终止；最后一次存档无法确认: " + e.Reason
	}
	return "控制台优雅关服不可用，已通过容器 TERM fallback 停止；最后一次存档无法确认: " + e.Reason
}

type containerInventoryProvider interface {
	ContainerProcesses(context.Context) ([]shared.ShardProcessReport, error)
}

func newContainerShardRuntime(installation RuntimeInstallation, cli containerCLI) (*containerShardRuntime, error) {
	if installation.Driver != "container" || cli == nil || !cli.Available() {
		return nil, errors.New("容器 Runtime 未正确配置或容器 CLI 不可用")
	}
	return &containerShardRuntime{
		installation: installation, cli: cli, dispatcher: consoledispatch.New(),
		gracePeriod: 30 * time.Second, pollInterval: 500 * time.Millisecond,
	}, nil
}

func (c *containerShardRuntime) Status(ctx context.Context, cluster, shard string) (shards.RuntimeStatus, error) {
	instance, err := c.find(ctx, cluster, shard)
	if err != nil {
		return shards.RuntimeStatus{State: shards.RuntimeUnknown}, err
	}
	switch instance.State {
	case "running":
		_, healthErr := c.cli.Run(ctx, "exec", instance.ID, "tmux", "-S", c.installation.ConsoleSocket, "has-session", "-t", "="+c.installation.ConsoleSession)
		if healthErr != nil {
			return shards.RuntimeStatus{State: shards.RuntimeStarting, Code: "CONSOLE_STARTING", Message: "容器已运行，等待 tmux 与 DST 会话就绪", SessionExists: true}, nil
		}
		startedAt, identityErr := c.runtimeStartedAt(ctx, instance.ID)
		if identityErr != nil {
			return shards.RuntimeStatus{State: shards.RuntimeStarting, Code: "INSTANCE_IDENTITY_PENDING", Message: "容器已运行，等待 Runtime 实例身份就绪", SessionExists: true}, nil
		}
		return c.runtimeLogStatus(ctx, cluster, shard, startedAt)
	case "restarting":
		return shards.RuntimeStatus{State: shards.RuntimeStarting, Message: "容器正在启动", SessionExists: true}, nil
	case "created":
		return shards.RuntimeStatus{State: shards.RuntimeStopped, Code: "CONTAINER_CREATED", Message: "容器已创建但尚未启动"}, nil
	case "exited", "dead":
		exit, inspectErr := c.inspectExit(ctx, instance.ID)
		if inspectErr != nil {
			return shards.RuntimeStatus{State: shards.RuntimeUnknown, Code: "CONTAINER_EXIT_INSPECT_FAILED", Message: inspectErr.Error()}, inspectErr
		}
		return containerExitRuntimeStatus(exit), nil
	default:
		return shards.RuntimeStatus{State: shards.RuntimeFailed, Code: "CONTAINER_" + strings.ToUpper(instance.State), Message: "分片容器状态异常: " + instance.State, SessionExists: instance.State != "dead"}, nil
	}
}

func (c *containerShardRuntime) Start(ctx context.Context, cluster, shard string) error {
	key := c.shardKey(cluster, shard)
	instance, err := c.find(ctx, cluster, shard)
	if err != nil {
		_ = c.dispatcher.Pause(context.Background(), key)
		return err
	}
	if instance.State == "running" || instance.State == "restarting" {
		instanceID, identityErr := c.runtimeInstanceID(ctx, instance.ID)
		if identityErr != nil {
			return identityErr
		}
		if err := c.dispatcher.BindInstance(key, instanceID); err != nil {
			return err
		}
		return c.dispatcher.Resume(key)
	}
	if _, err := c.cli.Run(ctx, "start", instance.ID); err != nil {
		_ = c.dispatcher.Pause(context.Background(), key)
		return fmt.Errorf("启动分片容器: %w", err)
	}
	instanceID, err := c.waitForRuntimeInstance(ctx, instance.ID, c.gracePeriod)
	if err != nil {
		return err
	}
	if err := c.dispatcher.BindInstance(key, instanceID); err != nil {
		return err
	}
	return c.dispatcher.Resume(key)
}

func (c *containerShardRuntime) Stop(ctx context.Context, cluster, shard string) error {
	key := c.shardKey(cluster, shard)
	if err := c.dispatcher.Pause(ctx, key); err != nil {
		return err
	}
	instance, err := c.find(ctx, cluster, shard)
	if err != nil {
		_ = c.dispatcher.Resume(key)
		return err
	}
	if instance.State != "running" {
		return nil
	}
	sendErr := c.sendToInstance(ctx, instance.ID, "c_shutdown(true)")
	if sendErr == nil {
		if err := c.waitForExit(ctx, cluster, shard, instance.ID, c.gracePeriod); err == nil {
			return nil
		} else if !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	} else {
		c.dispatcher.MarkInputDirty(key)
	}
	reason := "DST 未在优雅停止期限内退出"
	if sendErr != nil {
		reason = sendErr.Error()
	}
	stopSeconds := int(c.gracePeriod / time.Second)
	if stopSeconds < 1 {
		stopSeconds = 1
	}
	if _, stopErr := c.cli.Run(ctx, "stop", "--time", strconv.Itoa(stopSeconds), instance.ID); stopErr == nil {
		if waitErr := c.waitForExit(ctx, cluster, shard, instance.ID, c.gracePeriod); waitErr == nil {
			return &ContainerStopFallbackError{Reason: reason}
		}
	} else {
		reason = errors.Join(errors.New(reason), stopErr).Error()
	}
	if _, killErr := c.cli.Run(ctx, "kill", "--signal", "KILL", instance.ID); killErr != nil {
		return errors.Join(&ContainerStopFallbackError{Reason: reason, Forced: true}, killErr)
	}
	if waitErr := c.waitForExit(ctx, cluster, shard, instance.ID, c.gracePeriod); waitErr != nil {
		return errors.Join(&ContainerStopFallbackError{Reason: reason, Forced: true}, waitErr)
	}
	return &ContainerStopFallbackError{Reason: reason, Forced: true}
}

func (c *containerShardRuntime) waitForExit(ctx context.Context, cluster, shard, instanceID string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = time.Second
	}
	interval := c.pollInterval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		current, err := c.find(ctx, cluster, shard)
		if err != nil {
			return err
		}
		if current.ID != instanceID {
			return consoledispatch.ErrInstanceChanged
		}
		if current.State != "running" && current.State != "restarting" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}

func (c *containerShardRuntime) inspectExit(ctx context.Context, id string) (containerExitState, error) {
	if !managedContainerID.MatchString(id) {
		return containerExitState{}, errors.New("容器 ID 不受信")
	}
	output, err := c.cli.Run(ctx, "inspect", "--format", "{{json .State}}", id)
	if err != nil {
		return containerExitState{}, err
	}
	var state containerExitState
	if err := json.Unmarshal(output, &state); err != nil {
		return containerExitState{}, errors.New("容器 Engine 返回了无效退出状态")
	}
	return state, nil
}

func containerExitRuntimeStatus(state containerExitState) shards.RuntimeStatus {
	switch {
	case state.Running || state.Restarting:
		return shards.RuntimeStatus{State: shards.RuntimeStarting, Code: "CONTAINER_RESTARTING", Message: "容器正在恢复", SessionExists: true}
	case state.OOMKilled:
		return shards.RuntimeStatus{State: shards.RuntimeFailed, Code: "CONTAINER_OOM_KILLED", Message: "DST 分片容器被 OOM Killer 终止"}
	case state.Dead:
		return shards.RuntimeStatus{State: shards.RuntimeFailed, Code: "CONTAINER_DEAD", Message: "DST 分片容器进入 dead 状态"}
	case state.ExitCode == 137:
		return shards.RuntimeStatus{State: shards.RuntimeFailed, Code: "CONTAINER_SIGKILL", Message: "DST 分片容器被 SIGKILL 终止，存档可能未完成"}
	case state.ExitCode != 0:
		message := fmt.Sprintf("DST 分片容器异常退出（exit %d）", state.ExitCode)
		if strings.TrimSpace(state.Error) != "" {
			message += ": " + strings.TrimSpace(state.Error)
		}
		return shards.RuntimeStatus{State: shards.RuntimeFailed, Code: "CONTAINER_EXIT_NONZERO", Message: message}
	default:
		return shards.RuntimeStatus{State: shards.RuntimeStopped, Code: "CONTAINER_EXIT_CLEAN", Message: "DST 分片容器已完成优雅停止"}
	}
}

func (c *containerShardRuntime) Send(ctx context.Context, cluster, shard, command string) error {
	instance, err := c.find(ctx, cluster, shard)
	if err != nil {
		return err
	}
	if instance.State != "running" {
		return errors.New("分片容器未运行")
	}
	key := c.shardKey(cluster, shard)
	if err := c.guardContainerConsole(ctx, instance, key); err != nil {
		return err
	}
	instanceID, err := c.runtimeInstanceID(ctx, instance.ID)
	if err != nil {
		return err
	}
	if err := c.dispatcher.BindInstance(key, instanceID); err != nil {
		return err
	}
	writeAttempted := false
	err = c.dispatcher.Dispatch(ctx, key, consoledispatch.Request{InstanceID: instanceID, Execute: func(sendContext context.Context) error {
		current, err := c.find(sendContext, cluster, shard)
		if err != nil {
			return err
		}
		if current.ID != instance.ID {
			return consoledispatch.ErrInstanceChanged
		}
		currentInstanceID, err := c.runtimeInstanceID(sendContext, current.ID)
		if err != nil {
			return err
		}
		if currentInstanceID != instanceID {
			return consoledispatch.ErrInstanceChanged
		}
		if current.State != "running" {
			return errors.New("分片容器未运行")
		}
		writeAttempted = true
		return c.sendToInstance(sendContext, current.ID, command)
	}})
	if err != nil && writeAttempted && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		c.dispatcher.MarkInputDirty(key)
	}
	return err
}

func (c *containerShardRuntime) SendBackground(ctx context.Context, cluster, shard, coalesceKey, command string) error {
	key := c.shardKey(cluster, shard)
	instanceID := c.dispatcher.Health(key).InstanceID
	instance, err := c.find(ctx, cluster, shard)
	if err != nil {
		return err
	}
	if instance.State != "running" {
		return errors.New("分片容器未运行")
	}
	observedInstanceID, err := c.runtimeInstanceID(ctx, instance.ID)
	if err != nil {
		return err
	}
	if instanceID == "" {
		instanceID = observedInstanceID
		if err := c.dispatcher.BindInstance(key, instanceID); err != nil {
			return err
		}
	} else if instanceID != observedInstanceID {
		return consoledispatch.ErrInstanceChanged
	}
	if err := c.guardContainerConsole(ctx, instance, key); err != nil {
		return err
	}
	writeAttempted := false
	err = c.dispatcher.Dispatch(ctx, key, consoledispatch.Request{
		Class: consoledispatch.ClassBackground, CoalesceKey: coalesceKey, InstanceID: instanceID,
		Execute: func(sendContext context.Context) error {
			current, err := c.find(sendContext, cluster, shard)
			if err != nil {
				return err
			}
			currentInstanceID, identityErr := c.runtimeInstanceID(sendContext, current.ID)
			if identityErr != nil {
				return identityErr
			}
			if current.ID != instance.ID || currentInstanceID != instanceID {
				return consoledispatch.ErrInstanceChanged
			}
			if current.State != "running" {
				return errors.New("分片容器未运行")
			}
			writeAttempted = true
			return c.sendToInstance(sendContext, current.ID, command)
		},
	})
	if err != nil && writeAttempted && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		c.dispatcher.MarkInputDirty(key)
	}
	return err
}

func (c *containerShardRuntime) ConsoleHealth(cluster, shard string) consoledispatch.Health {
	key := c.shardKey(cluster, shard)
	current := c.dispatcher.Health(key)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	instance, err := c.find(ctx, cluster, shard)
	if err != nil || instance.State != "running" {
		current.Status, current.Accepting = "not_found", false
		return current
	}
	status, external := c.probeContainerConsole(ctx, instance)
	if external && !current.Maintenance {
		c.dispatcher.MarkExternalWriter(key)
		return c.dispatcher.Health(key)
	}
	if status != "ready" {
		current.Status, current.Accepting = status, false
	}
	return current
}

func (c *containerShardRuntime) guardContainerConsole(ctx context.Context, instance managedContainer, key string) error {
	status, external := c.probeContainerConsole(ctx, instance)
	if external {
		c.dispatcher.MarkExternalWriter(key)
		return consoledispatch.ErrExternalWriter
	}
	if status != "ready" {
		return fmt.Errorf("container console transport is %s", status)
	}
	return nil
}

func (c *containerShardRuntime) probeContainerConsole(ctx context.Context, instance managedContainer) (string, bool) {
	output, err := c.cli.Run(ctx, "exec", instance.ID, "tmux", "-S", c.installation.ConsoleSocket,
		"display-message", "-p", "-t", "="+c.installation.ConsoleSession+":0.0", "#{pane_dead}|#{pane_current_command}|#{pane_pid}")
	if err != nil {
		return classifyContainerConsoleError(err.Error()), false
	}
	fields := strings.Split(strings.TrimSpace(string(output)), "|")
	if len(fields) != 3 {
		return "process_mismatch", false
	}
	if fields[0] == "1" {
		return "pane_dead", false
	}
	pid, parseErr := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
	if parseErr != nil || pid <= 0 || !strings.Contains(strings.ToLower(fields[1]), "dontstarve") {
		return "process_mismatch", false
	}
	clients, err := c.cli.Run(ctx, "exec", instance.ID, "tmux", "-S", c.installation.ConsoleSocket,
		"list-clients", "-F", "#{client_session}|#{client_readonly}")
	if err != nil {
		return classifyContainerConsoleError(err.Error()), false
	}
	for _, line := range strings.Split(string(clients), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "|")
		if len(fields) == 2 && fields[0] == c.installation.ConsoleSession && fields[1] != "1" {
			return "ready", true
		}
	}
	return "ready", false
}

func classifyContainerConsoleError(message string) string {
	message = strings.ToLower(message)
	switch {
	case strings.Contains(message, "permission denied"), strings.Contains(message, "operation not permitted"):
		return "permission_denied"
	case strings.Contains(message, "can't find session"), strings.Contains(message, "no sessions"):
		return "not_found"
	case strings.Contains(message, "pane is dead"):
		return "pane_dead"
	default:
		return "socket_unavailable"
	}
}

func (c *containerShardRuntime) ConsoleAttach(cluster, shard string, readOnly bool) (shards.ConsoleAttachSpec, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	instance, err := c.find(ctx, cluster, shard)
	if err != nil {
		return shards.ConsoleAttachSpec{}, err
	}
	if instance.State != "running" {
		return shards.ConsoleAttachSpec{}, errors.New("分片容器未运行")
	}
	status, external := c.probeContainerConsole(ctx, instance)
	if status != "ready" {
		return shards.ConsoleAttachSpec{}, fmt.Errorf("container console transport is %s", status)
	}
	if external && !readOnly {
		return shards.ConsoleAttachSpec{}, consoledispatch.ErrExternalWriter
	}
	instanceID, err := c.runtimeInstanceID(ctx, instance.ID)
	if err != nil {
		return shards.ConsoleAttachSpec{}, err
	}
	command := []string{c.installation.ContainerEngine, "exec", "-it", instance.ID, "tmux", "-S", c.installation.ConsoleSocket, "attach-session"}
	if readOnly {
		command = append(command, "-r")
	}
	command = append(command, "-t", "="+c.installation.ConsoleSession)
	return shards.ConsoleAttachSpec{Command: command, InstanceID: instanceID}, nil
}

func (c *containerShardRuntime) BeginConsoleMaintenance(ctx context.Context, cluster, shard, owner string) (shards.ConsoleAttachSpec, *consoledispatch.MaintenanceLease, error) {
	spec, err := c.ConsoleAttach(cluster, shard, false)
	if err != nil {
		return shards.ConsoleAttachSpec{}, nil, err
	}
	key := c.shardKey(cluster, shard)
	if err := c.dispatcher.BindInstance(key, spec.InstanceID); err != nil {
		return shards.ConsoleAttachSpec{}, nil, err
	}
	lease, err := c.dispatcher.BeginMaintenance(ctx, key, owner, spec.InstanceID)
	return spec, lease, err
}

func (c *containerShardRuntime) EndConsoleMaintenance(cluster, shard string, lease *consoledispatch.MaintenanceLease) error {
	if lease == nil {
		return consoledispatch.ErrInvalidRequest
	}
	key := c.shardKey(cluster, shard)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	instance, err := c.find(ctx, cluster, shard)
	if err != nil {
		c.dispatcher.MarkInputDirty(key)
	} else if status, external := c.probeContainerConsole(ctx, instance); status != "ready" {
		c.dispatcher.MarkInputDirty(key)
	} else if external {
		c.dispatcher.MarkExternalWriter(key)
	}
	return errors.Join(err, lease.Release())
}

func (c *containerShardRuntime) RecoverConsoleHazard(ctx context.Context, cluster, shard string) error {
	instance, err := c.find(ctx, cluster, shard)
	if err != nil {
		if errors.Is(err, errManagedContainerNotFound) {
			return nil
		}
		return err
	}
	if instance.State != "running" && instance.State != "restarting" {
		return nil
	}
	instanceID, err := c.runtimeInstanceID(ctx, instance.ID)
	if err != nil {
		return err
	}
	key := c.shardKey(cluster, shard)
	if err := c.dispatcher.BindInstance(key, instanceID); err != nil {
		return err
	}
	c.dispatcher.MarkInputDirty(key)
	return nil
}

func (c *containerShardRuntime) ContainerProcesses(ctx context.Context) ([]shared.ShardProcessReport, error) {
	instances, err := c.list(ctx, "", "")
	if err != nil {
		return nil, err
	}
	result := make([]shared.ShardProcessReport, 0, len(instances))
	for _, instance := range instances {
		if instance.State != "running" && instance.State != "restarting" {
			continue
		}
		instanceID, identityErr := c.runtimeInstanceID(ctx, instance.ID)
		if identityErr != nil {
			return nil, identityErr
		}
		result = append(result, shared.ShardProcessReport{
			PID: containerPseudoPID(instance.ID), RuntimeKind: "container", InstanceID: instanceID,
			Executable: "container:" + instance.Name, Cluster: instance.Cluster, Shard: instance.Shard,
		})
	}
	return result, nil
}

func (c *containerShardRuntime) ManagedRuntimeExists(ctx context.Context, cluster, shard string) (bool, error) {
	items, err := c.list(ctx, cluster, shard)
	return len(items) > 0, err
}

func (c *containerShardRuntime) runtimeInstanceID(ctx context.Context, containerID string) (string, error) {
	instant, err := c.runtimeStartedAt(ctx, containerID)
	if err != nil {
		return "", err
	}
	return containerID + "@" + instant.UTC().Format(time.RFC3339Nano), nil
}

func (c *containerShardRuntime) runtimeStartedAt(ctx context.Context, containerID string) (time.Time, error) {
	if !managedContainerID.MatchString(containerID) {
		return time.Time{}, errors.New("容器 ID 不受信")
	}
	output, err := c.cli.Run(ctx, "inspect", "--format", "{{.State.StartedAt}}", containerID)
	if err != nil {
		return time.Time{}, err
	}
	startedAt := strings.TrimSpace(string(output))
	instant, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil || instant.IsZero() || instant.Year() <= 1 {
		return time.Time{}, errors.New("容器 Runtime 启动身份无效")
	}
	return instant.UTC(), nil
}

func (c *containerShardRuntime) runtimeLogStatus(ctx context.Context, cluster, shard string, startedAt time.Time) (shards.RuntimeStatus, error) {
	starting := shards.RuntimeStatus{
		State: shards.RuntimeStarting, Code: "DST_WORLD_LOADING", Message: "等待 DST 完成世界加载和服务注册", SessionExists: true,
	}
	chunk, err := runtimefiles.ReadLogs(ctx, c.installation.SavePath, cluster, shard, shared.RuntimeLogRequest{
		Source: shared.RuntimeLogSourceServer, Cursor: -1, MaxBytes: int(runtimefiles.MaximumLogBytes), Raw: true,
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return starting, nil
		}
		return shards.RuntimeStatus{State: shards.RuntimeUnknown, Code: "RUNTIME_LOG_UNAVAILABLE", Message: err.Error(), SessionExists: true}, err
	}
	// DST logs wall-clock time to whole seconds, while the container identity
	// includes nanoseconds. A two-second tolerance rejects a previous run
	// without treating the current log as stale.
	if chunk.StartedAt.IsZero() || chunk.StartedAt.Before(startedAt.Add(-2*time.Second)) || chunk.UpdatedAt.Before(startedAt) {
		return starting, nil
	}
	classified := dsttmux.ClassifyRuntimeLog(string(chunk.Data))
	if classified.State == dsttmux.RuntimeStarting {
		return starting, nil
	}
	return shards.RuntimeStatus{
		State: shards.RuntimeState(classified.State), Code: classified.Code,
		Message: classified.Message, SessionExists: classified.SessionExists,
	}, nil
}

func (c *containerShardRuntime) waitForRuntimeInstance(ctx context.Context, containerID string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	interval := c.pollInterval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		instanceID, err := c.runtimeInstanceID(ctx, containerID)
		if err == nil {
			return instanceID, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", errors.New("等待容器 Runtime 实例身份超时")
		case <-ticker.C:
		}
	}
}

func (c *containerShardRuntime) sendToInstance(ctx context.Context, id, command string) error {
	_, err := c.cli.Run(ctx, "exec", id, "tmux", "-S", c.installation.ConsoleSocket,
		"send-keys", "-t", "="+c.installation.ConsoleSession+":0.0", "-l", "--", command,
		";", "send-keys", "-t", "="+c.installation.ConsoleSession+":0.0", "Enter")
	return err
}

func (c *containerShardRuntime) find(ctx context.Context, cluster, shard string) (managedContainer, error) {
	items, err := c.list(ctx, cluster, shard)
	if err != nil {
		return managedContainer{}, err
	}
	if len(items) == 0 {
		return managedContainer{}, errManagedContainerNotFound
	}
	if len(items) != 1 {
		return managedContainer{}, errors.New("发现多个相同 Placement 的受管分片容器")
	}
	return items[0], nil
}

func (c *containerShardRuntime) list(ctx context.Context, cluster, shard string) ([]managedContainer, error) {
	arguments := []string{"ps", "-a", "--no-trunc", "--filter", "label=com.dst-admin.managed=true", "--filter", "label=com.dst-admin.installation=" + c.installation.ID}
	if cluster != "" {
		arguments = append(arguments, "--filter", "label=com.dst-admin.cluster="+cluster)
	}
	if shard != "" {
		arguments = append(arguments, "--filter", "label=com.dst-admin.shard="+shard)
	}
	arguments = append(arguments, "--format", "{{json .}}")
	output, err := c.cli.Run(ctx, arguments...)
	if err != nil {
		return nil, err
	}
	if len(output) > maximumContainerCLIOutput {
		return nil, errors.New("容器 CLI 返回的清单过大")
	}
	items := make([]managedContainer, 0)
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var raw map[string]string
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, errors.New("容器 CLI 返回了无效清单")
		}
		labels := parseContainerLabels(raw["Labels"])
		id := strings.ToLower(strings.TrimSpace(raw["ID"]))
		if !managedContainerID.MatchString(id) || labels["com.dst-admin.managed"] != "true" || labels["com.dst-admin.installation"] != c.installation.ID {
			return nil, errors.New("容器清单包含不受信目标")
		}
		item := managedContainer{ID: id, Name: strings.TrimSpace(raw["Names"]), State: strings.ToLower(strings.TrimSpace(raw["State"])), Cluster: labels["com.dst-admin.cluster"], Shard: labels["com.dst-admin.shard"]}
		if item.Cluster == "" || item.Shard == "" {
			return nil, errors.New("受管容器缺少 Cluster 或 Shard 标签")
		}
		if cluster != "" && item.Cluster != cluster || shard != "" && item.Shard != shard {
			return nil, errors.New("容器清单返回了不匹配请求的 Placement")
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func (c *containerShardRuntime) shardKey(cluster, shard string) string {
	return c.installation.ID + "\x00container\x00" + cluster + "\x00" + shard
}

func parseContainerLabels(value string) map[string]string {
	result := make(map[string]string)
	for _, item := range strings.Split(value, ",") {
		key, raw, ok := strings.Cut(item, "=")
		if ok {
			result[strings.TrimSpace(key)] = strings.TrimSpace(raw)
		}
	}
	return result
}

func containerPseudoPID(id string) int32 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(id))
	return int32(hash.Sum32()%0x7ffffffe) + 1
}
