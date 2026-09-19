package shards

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/internal/consoledispatch"
	"dont/internal/runtimeinventory"
	"dont/internal/runtimeperformance"
	"dont/shared"
	dsttmux "dont/tmux"
)

type TmuxConfig struct {
	SaveRoot             string
	UGCDirectory         string
	ServerPath           string
	ServerMode           string
	ConsoleSocket        string
	LegacyConsoleSockets []string
	OwnerLabel           string
	ProcessProbe         func(context.Context) ([]shared.ShardProcessReport, error)
}

type TmuxControl struct {
	config               TmuxConfig
	dispatcher           *consoledispatch.Dispatcher
	owner                *nativeRuntimeOwner
	now                  func() time.Time
	legacySessionProbe   func(*dsttmux.DSTServer) (bool, error)
	legacyStop           func(*dsttmux.DSTServer) error
	legacyProcessSockets func([]int32) []string

	serverMu sync.Mutex
	servers  map[string]*dsttmux.DSTServer

	statusMu       sync.Mutex
	statusCache    map[string]cachedTmuxRuntimeStatus
	statusInFlight map[string]chan struct{}
	statusVersion  map[string]uint64

	processMu       sync.Mutex
	processCache    cachedTmuxProcesses
	processInFlight chan struct{}
	processVersion  uint64
}

type cachedTmuxRuntimeStatus struct {
	value     RuntimeStatus
	err       error
	expiresAt time.Time
}

type cachedTmuxProcesses struct {
	values    []shared.ShardProcessReport
	err       error
	expiresAt time.Time
}

const tmuxRuntimeStatusCacheTTL = 250 * time.Millisecond
const tmuxProcessCacheTTL = 2 * time.Second

func NewTmuxControl(config TmuxConfig) (*TmuxControl, error) {
	config.SaveRoot = filepath.Clean(strings.TrimSpace(config.SaveRoot))
	config.ServerMode = strings.TrimSpace(config.ServerMode)
	config.ConsoleSocket = strings.TrimSpace(config.ConsoleSocket)
	if config.SaveRoot == "" || config.SaveRoot == "." || !filepath.IsAbs(config.SaveRoot) || strings.ContainsAny(config.SaveRoot, "\x00\r\n") {
		return nil, fmt.Errorf("DST save root must be an absolute safe path")
	}
	if config.ServerMode != "32" && config.ServerMode != "64" {
		return nil, fmt.Errorf("DST server architecture must be 32 or 64")
	}
	managedSocketDirectory := config.ConsoleSocket == ""
	if managedSocketDirectory {
		var err error
		config.ConsoleSocket, err = NativeConsoleSocketPath(config.SaveRoot)
		if err != nil {
			return nil, err
		}
	}
	if !filepath.IsAbs(config.ConsoleSocket) || strings.ContainsAny(config.ConsoleSocket, "\x00\r\n") || len([]byte(config.ConsoleSocket)) > maximumPortableUnixSocketPathBytes {
		return nil, fmt.Errorf("tmux console socket must be an absolute safe path")
	}
	legacySockets := make([]string, 0, len(config.LegacyConsoleSockets))
	seenLegacySockets := make(map[string]struct{}, len(config.LegacyConsoleSockets))
	for _, value := range config.LegacyConsoleSockets {
		value = filepath.Clean(strings.TrimSpace(value))
		if value == "" || value == "." || !filepath.IsAbs(value) || strings.ContainsAny(value, "\x00\r\n") || len([]byte(value)) > maximumPortableUnixSocketPathBytes {
			return nil, fmt.Errorf("legacy tmux console socket must be an absolute safe path")
		}
		identity := canonicalSocketPath(value)
		if identity == canonicalSocketPath(config.ConsoleSocket) {
			continue
		}
		if _, exists := seenLegacySockets[identity]; exists {
			continue
		}
		seenLegacySockets[identity] = struct{}{}
		legacySockets = append(legacySockets, value)
	}
	config.LegacyConsoleSockets = legacySockets
	if err := prepareNativeConsoleSocket(config.ConsoleSocket, managedSocketDirectory); err != nil {
		return nil, err
	}
	if config.ProcessProbe == nil {
		config.ProcessProbe = runtimeinventory.ProbeDSTProcesses
	}
	owner, err := acquireNativeRuntimeOwner(config.SaveRoot, config.OwnerLabel)
	if err != nil {
		return nil, err
	}
	return &TmuxControl{
		config: config, dispatcher: consoledispatch.New(), owner: owner, now: time.Now,
		legacySessionProbe:   func(server *dsttmux.DSTServer) (bool, error) { return server.SessionExists() },
		legacyStop:           func(server *dsttmux.DSTServer) error { return server.Stop() },
		legacyProcessSockets: legacyDefaultConsoleSocketPaths,
		servers:              make(map[string]*dsttmux.DSTServer), statusCache: make(map[string]cachedTmuxRuntimeStatus),
		statusInFlight: make(map[string]chan struct{}), statusVersion: make(map[string]uint64),
	}, nil
}

// Close releases this process' host-local ownership claim. Running tmux/DST
// processes are intentionally preserved so a service upgrade can reacquire it.
func (c *TmuxControl) Close() error {
	if c == nil {
		return nil
	}
	return c.owner.close()
}

func (c *TmuxControl) IsRunning(ctx context.Context, roomName, worldName string) (bool, error) {
	status, err := c.Status(ctx, roomName, worldName)
	return status.State == RuntimeRunning, err
}

func (c *TmuxControl) Status(ctx context.Context, roomName, worldName string) (RuntimeStatus, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeStatus{State: RuntimeUnknown}, err
	}
	server, err := c.server(roomName, worldName)
	if err != nil {
		return RuntimeStatus{State: RuntimeUnknown}, err
	}
	status, err := c.cachedRuntimeStatus(ctx, c.shardKey(roomName, worldName), func() (RuntimeStatus, error) {
		value, loadErr := server.RuntimeStatus()
		converted := RuntimeStatus{
			State: RuntimeState(value.State), StartupStage: value.StartupStage, Code: value.Code, Message: value.Message, SessionExists: value.SessionExists, Paused: value.Paused,
		}
		if loadErr != nil {
			return converted, loadErr
		}
		return c.inspectRuntimeOwnership(ctx, server, roomName, worldName, converted)
	})
	if err != nil {
		return RuntimeStatus{State: RuntimeUnknown}, err
	}
	return status, nil
}

func (c *TmuxControl) Start(ctx context.Context, roomName, worldName string) error {
	return c.StartWithRuntimeMode(ctx, roomName, worldName, shared.RuntimePerformanceModeGame)
}

func (c *TmuxControl) StartWithRuntimeMode(ctx context.Context, roomName, worldName string, runtimeMode shared.RuntimePerformanceMode) error {
	return c.StartWithRuntimeOptions(ctx, roomName, worldName, runtimeMode, shared.RuntimeLaunchOptions{})
}

func (c *TmuxControl) StartWithRuntimeOptions(ctx context.Context, roomName, worldName string, runtimeMode shared.RuntimePerformanceMode, launchOptions shared.RuntimeLaunchOptions) error {
	c.invalidateRuntimeStatus(roomName, worldName)
	defer c.invalidateRuntimeStatus(roomName, worldName)
	defer c.invalidateProcessSnapshot()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.requireRuntimeMode(runtimeMode); err != nil {
		return err
	}
	key := c.shardKey(roomName, worldName)
	server, err := c.server(roomName, worldName)
	if err != nil {
		return err
	}
	c.invalidateProcessSnapshot()
	status, err := c.Status(ctx, roomName, worldName)
	if err != nil {
		return err
	}
	if runtimeOwnershipConflict(status.Code) {
		return fmt.Errorf("%s: %s", status.Code, status.Message)
	}
	if err := server.StartWithRuntimeOptions(runtimeMode, launchOptions); err != nil {
		_ = c.dispatcher.Pause(context.Background(), key)
		return err
	}
	instanceID, err := server.RuntimeInstanceID()
	if err != nil {
		_ = c.dispatcher.Pause(context.Background(), key)
		return fmt.Errorf("读取新启动的 tmux 实例身份: %w", err)
	}
	if err := c.dispatcher.BindInstance(key, instanceID); err != nil {
		return err
	}
	return c.dispatcher.Resume(key)
}

func (c *TmuxControl) requireRuntimeMode(runtimeMode shared.RuntimePerformanceMode) error {
	mode, valid := shared.NormalizeRuntimePerformanceMode(runtimeMode)
	if !valid {
		return ErrInvalidRuntimeMode
	}
	if mode == shared.RuntimePerformanceModeGame {
		return nil
	}
	report := runtimeperformance.Inspect(runtimeperformance.Options{
		ServerPath: c.config.ServerPath,
		ServerMode: c.config.ServerMode,
	})
	if report.Status == shared.RuntimePerformanceReady && report.CanEnable {
		for _, supported := range report.SupportedModes {
			if supported == mode {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: 本机未检测到可用于 %s 的 DontStarveLuaJIT2", ErrRuntimeModeUnavailable, mode)
}

func (c *TmuxControl) Stop(ctx context.Context, roomName, worldName string) error {
	c.invalidateRuntimeStatus(roomName, worldName)
	defer c.invalidateRuntimeStatus(roomName, worldName)
	defer c.invalidateProcessSnapshot()
	if err := ctx.Err(); err != nil {
		return err
	}
	key := c.shardKey(roomName, worldName)
	if err := c.dispatcher.Pause(ctx, key); err != nil {
		return err
	}
	server, err := c.server(roomName, worldName)
	if err != nil {
		_ = c.dispatcher.Resume(key)
		return err
	}
	stopServer := server
	managedSession, err := server.SessionExists()
	if err != nil {
		_ = c.dispatcher.Resume(key)
		return err
	}
	if !managedSession {
		legacySessions, probeErr := c.legacySessions(ctx, roomName, worldName)
		if probeErr != nil {
			_ = c.dispatcher.Resume(key)
			return probeErr
		}
		if len(legacySessions) > 0 {
			processes, processErr := c.runtimeProcesses(ctx)
			if processErr != nil {
				_ = c.dispatcher.Resume(key)
				return fmt.Errorf("确认旧版分片进程: %w", processErr)
			}
			matches := c.matchingShardProcesses(processes, roomName, worldName)
			if len(legacySessions) != 1 || len(matches) != 1 {
				_ = c.dispatcher.Resume(key)
				return fmt.Errorf("LEGACY_TMUX_SOCKET_CONFLICT: 旧版 tmux 通道无法唯一对应目标分片（会话 %d 个，进程 %d 个）", len(legacySessions), len(matches))
			}
			stopServer = legacySessions[0]
		}
	}
	if err := c.stopServer(stopServer); err != nil {
		_ = c.dispatcher.Resume(key)
		return err
	}
	return nil
}

func (c *TmuxControl) Cleanup(ctx context.Context, roomName, worldName string) error {
	c.invalidateRuntimeStatus(roomName, worldName)
	defer c.invalidateRuntimeStatus(roomName, worldName)
	defer c.invalidateProcessSnapshot()
	if err := ctx.Err(); err != nil {
		return err
	}
	key := c.shardKey(roomName, worldName)
	if err := c.dispatcher.Pause(ctx, key); err != nil {
		return err
	}
	server, err := c.server(roomName, worldName)
	if err != nil {
		_ = c.dispatcher.Resume(key)
		return err
	}
	if err := server.KillSession(); err != nil {
		_ = c.dispatcher.Resume(key)
		return err
	}
	return nil
}

func (c *TmuxControl) Send(ctx context.Context, roomName, worldName, command string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	server, err := c.server(roomName, worldName)
	if err != nil {
		return err
	}
	key := c.shardKey(roomName, worldName)
	if err := c.guardConsoleTransport(server, key); err != nil {
		return err
	}
	instanceID, err := server.RuntimeInstanceID()
	if err != nil {
		return err
	}
	if err := c.dispatcher.BindInstance(key, instanceID); err != nil {
		return err
	}
	writeAttempted := false
	err = c.dispatcher.Dispatch(ctx, key, consoledispatch.Request{InstanceID: instanceID, Execute: func(sendContext context.Context) error {
		if err := sendContext.Err(); err != nil {
			return err
		}
		current, err := c.server(roomName, worldName)
		if err != nil {
			return err
		}
		currentID, err := current.RuntimeInstanceID()
		if err != nil {
			return err
		}
		if currentID != instanceID {
			return consoledispatch.ErrInstanceChanged
		}
		writeAttempted = true
		return current.SendCommand(command)
	}})
	if err != nil && writeAttempted && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		c.dispatcher.MarkInputDirty(key)
	}
	return err
}

func (c *TmuxControl) SendBackground(ctx context.Context, roomName, worldName, coalesceKey, command string) error {
	server, err := c.server(roomName, worldName)
	if err != nil {
		return err
	}
	key := c.shardKey(roomName, worldName)
	if err := c.guardConsoleTransport(server, key); err != nil {
		return err
	}
	instanceID, err := server.RuntimeInstanceID()
	if err != nil {
		return err
	}
	if err := c.dispatcher.BindInstance(key, instanceID); err != nil {
		return err
	}
	writeAttempted := false
	err = c.dispatcher.Dispatch(ctx, key, consoledispatch.Request{
		Class: consoledispatch.ClassBackground, CoalesceKey: coalesceKey, InstanceID: instanceID,
		Execute: func(sendContext context.Context) error {
			if err := sendContext.Err(); err != nil {
				return err
			}
			current, err := c.server(roomName, worldName)
			if err != nil {
				return err
			}
			currentID, err := current.RuntimeInstanceID()
			if err != nil {
				return err
			}
			if currentID != instanceID {
				return consoledispatch.ErrInstanceChanged
			}
			writeAttempted = true
			return current.SendCommand(command)
		},
	})
	if err != nil && writeAttempted && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		c.dispatcher.MarkInputDirty(key)
	}
	return err
}

func (c *TmuxControl) ConsoleHealth(roomName, worldName string) consoledispatch.Health {
	key := c.shardKey(roomName, worldName)
	current := c.dispatcher.Health(key)
	server, err := c.server(roomName, worldName)
	if err != nil {
		current.Status, current.Accepting = "not_found", false
		return current
	}
	status, externalWriter, probeErr := server.ConsoleTransportHealth()
	if probeErr != nil {
		current.Status, current.Accepting = "socket_unavailable", false
		return current
	}
	if externalWriter && !current.Maintenance {
		c.dispatcher.MarkExternalWriter(key)
		return c.dispatcher.Health(key)
	}
	if status != "ready" {
		current.Status, current.Accepting = status, false
	}
	return current
}

func (c *TmuxControl) guardConsoleTransport(server *dsttmux.DSTServer, key string) error {
	status, externalWriter, err := server.ConsoleTransportHealth()
	if err != nil {
		return err
	}
	if externalWriter {
		c.dispatcher.MarkExternalWriter(key)
		return consoledispatch.ErrExternalWriter
	}
	if status != "ready" {
		return fmt.Errorf("console transport is %s", status)
	}
	return nil
}

type ConsoleAttachSpec struct {
	Command    []string
	InstanceID string
}

func (c *TmuxControl) ConsoleAttach(roomName, worldName string, readOnly bool) (ConsoleAttachSpec, error) {
	server, err := c.server(roomName, worldName)
	if err != nil {
		return ConsoleAttachSpec{}, err
	}
	command, err := server.ConsoleAttachCommand(readOnly)
	if err != nil {
		return ConsoleAttachSpec{}, err
	}
	instanceID, err := server.RuntimeInstanceID()
	if err != nil {
		return ConsoleAttachSpec{}, err
	}
	return ConsoleAttachSpec{Command: command, InstanceID: instanceID}, nil
}

func (c *TmuxControl) BeginConsoleMaintenance(ctx context.Context, roomName, worldName, owner string) (ConsoleAttachSpec, *consoledispatch.MaintenanceLease, error) {
	spec, err := c.ConsoleAttach(roomName, worldName, false)
	if err != nil {
		return ConsoleAttachSpec{}, nil, err
	}
	key := c.shardKey(roomName, worldName)
	if err := c.dispatcher.BindInstance(key, spec.InstanceID); err != nil {
		return ConsoleAttachSpec{}, nil, err
	}
	lease, err := c.dispatcher.BeginMaintenance(ctx, key, owner, spec.InstanceID)
	if err != nil {
		return ConsoleAttachSpec{}, nil, err
	}
	return spec, lease, nil
}

func (c *TmuxControl) EndConsoleMaintenance(roomName, worldName string, lease *consoledispatch.MaintenanceLease) error {
	if lease == nil {
		return consoledispatch.ErrInvalidRequest
	}
	key := c.shardKey(roomName, worldName)
	server, err := c.server(roomName, worldName)
	if err != nil {
		c.dispatcher.MarkInputDirty(key)
		_ = lease.Release()
		return err
	}
	status, externalWriter, probeErr := server.ConsoleTransportHealth()
	if probeErr != nil || status != "ready" {
		c.dispatcher.MarkInputDirty(key)
	} else if externalWriter {
		c.dispatcher.MarkExternalWriter(key)
	}
	return errors.Join(probeErr, lease.Release())
}

func (c *TmuxControl) RecoverConsoleHazard(ctx context.Context, roomName, worldName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	server, err := c.server(roomName, worldName)
	if err != nil {
		return err
	}
	exists, err := server.SessionExists()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	instanceID, err := server.RuntimeInstanceID()
	if err != nil {
		return err
	}
	key := c.shardKey(roomName, worldName)
	if err := c.dispatcher.BindInstance(key, instanceID); err != nil {
		return err
	}
	c.dispatcher.MarkInputDirty(key)
	return nil
}

func (c *TmuxControl) shardKey(roomName, worldName string) string {
	return c.config.SaveRoot + "\x00" + roomName + "\x00" + worldName
}

func (c *TmuxControl) server(roomName, worldName string) (*dsttmux.DSTServer, error) {
	key := c.shardKey(roomName, worldName)
	c.serverMu.Lock()
	defer c.serverMu.Unlock()
	if server := c.servers[key]; server != nil {
		return server, nil
	}
	server, err := c.newServer(roomName, worldName, c.config.ConsoleSocket)
	if err != nil {
		return nil, err
	}
	c.servers[key] = server
	return server, nil
}

func (c *TmuxControl) newServer(roomName, worldName, socket string) (*dsttmux.DSTServer, error) {
	return dsttmux.NewDSTServerWithSocketAndSessionName(
		roomName,
		worldName,
		dsttmux.V2SessionName(roomName, worldName),
		socket,
		c.config.UGCDirectory,
		filepath.Dir(c.config.SaveRoot),
		filepath.Base(c.config.SaveRoot),
		c.config.ServerPath,
		c.config.ServerMode,
	)
}

func (c *TmuxControl) inspectRuntimeOwnership(ctx context.Context, server *dsttmux.DSTServer, roomName, worldName string, managed RuntimeStatus) (RuntimeStatus, error) {
	processes, err := c.runtimeProcesses(ctx)
	if err != nil {
		return RuntimeStatus{State: RuntimeUnknown, Code: "RUNTIME_OWNERSHIP_INSPECTION_FAILED", Message: err.Error(), SessionExists: managed.SessionExists}, err
	}
	matches := c.matchingShardProcesses(processes, roomName, worldName)
	legacySessions, err := c.legacySessions(ctx, roomName, worldName)
	if err != nil {
		return RuntimeStatus{State: RuntimeUnknown, Code: "RUNTIME_OWNERSHIP_INSPECTION_FAILED", Message: err.Error(), SessionExists: managed.SessionExists}, err
	}
	if len(legacySessions) > 0 {
		if managed.SessionExists || len(legacySessions) > 1 || len(matches) > 1 {
			return RuntimeStatus{
				State: RuntimeFailed, Code: "DUPLICATE_DST_PROCESS_CONFLICT", SessionExists: true,
				Message: fmt.Sprintf("同一世界存在多个 Runtime 控制通道或 DST 进程（旧通道 %d 个，进程 %d 个）；已停止写入", len(legacySessions), len(matches)),
			}, nil
		}
		message := "检测到旧版 tmux 通道，但没有唯一匹配的 DST 进程；为避免误操作，Runtime 已停止写入"
		if len(matches) == 1 {
			message = fmt.Sprintf("检测到旧版 Runtime 启动的 DST 进程（PID %s）；可直接停止，停止后再次启动将自动切换到稳定通道", processIDList(matches))
		}
		return RuntimeStatus{
			State: RuntimeFailed, Code: "LEGACY_TMUX_SOCKET_CONFLICT", SessionExists: true,
			Message: message,
		}, nil
	}
	if managed.SessionExists && len(matches) > 1 {
		return RuntimeStatus{
			State: RuntimeFailed, Code: "DUPLICATE_DST_PROCESS_CONFLICT", SessionExists: true,
			Message: fmt.Sprintf("同一世界检测到多个 DST 进程（PID %s）；Runtime 已停止写入，请先保留一个实例", processIDList(matches)),
		}, nil
	}
	if !managed.SessionExists && len(matches) > 0 {
		return RuntimeStatus{
			State: RuntimeFailed, Code: "UNMANAGED_DST_PROCESS_CONFLICT", SessionExists: true,
			Message: fmt.Sprintf("同一世界的 DST 进程不属于当前 Runtime（PID %s）；已阻止重复启动，请先停止该进程后重试", processIDList(matches)),
		}, nil
	}
	if managed.SessionExists {
		// Reuse the ownership probe: this survives Controller/Agent restarts and
		// adds no extra process scan or background work.
		managed.RuntimeMode = "unknown"
		if len(matches) == 1 {
			managed.RuntimeMode = matches[0].RuntimeMode
		}
	}
	return managed, nil
}

func (c *TmuxControl) legacySessions(ctx context.Context, roomName, worldName string) ([]*dsttmux.DSTServer, error) {
	sockets := append([]string(nil), c.config.LegacyConsoleSockets...)
	sockets = append(sockets, "")
	processes, err := c.runtimeProcesses(ctx)
	if err != nil {
		return nil, fmt.Errorf("发现旧版 tmux 通道前确认分片进程: %w", err)
	}
	matches := c.matchingShardProcesses(processes, roomName, worldName)
	if c.legacyProcessSockets != nil {
		pids := make([]int32, 0, len(matches))
		for _, process := range matches {
			pids = append(pids, process.PID)
		}
		sockets = append(sockets, c.legacyProcessSockets(pids)...)
	}
	result := make([]*dsttmux.DSTServer, 0, len(sockets))
	seen := make(map[string]struct{}, len(sockets))
	for _, socket := range sockets {
		if socket != "" && canonicalSocketPath(socket) == canonicalSocketPath(c.config.ConsoleSocket) {
			continue
		}
		identity := canonicalSocketPath(socket)
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		server, err := c.newServer(roomName, worldName, socket)
		if err != nil {
			return nil, err
		}
		exists, err := c.legacySessionProbe(server)
		if err != nil {
			return nil, fmt.Errorf("检查旧版 tmux 通道: %w", err)
		}
		if exists {
			result = append(result, server)
		}
	}
	return result, nil
}

func (c *TmuxControl) stopServer(server *dsttmux.DSTServer) error {
	if server.SocketPath == c.config.ConsoleSocket {
		return server.Stop()
	}
	return c.legacyStop(server)
}

func canonicalSocketPath(value string) string {
	value = filepath.Clean(strings.TrimSpace(value))
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(value)
	}
	return value
}

func runtimeOwnershipConflict(code string) bool {
	switch strings.TrimSpace(code) {
	case "LEGACY_TMUX_SOCKET_CONFLICT", "UNMANAGED_DST_PROCESS_CONFLICT", "DUPLICATE_DST_PROCESS_CONFLICT":
		return true
	default:
		return false
	}
}

func (c *TmuxControl) matchingShardProcesses(values []shared.ShardProcessReport, roomName, worldName string) []shared.ShardProcessReport {
	result := make([]shared.ShardProcessReport, 0)
	for _, value := range values {
		if !strings.EqualFold(strings.TrimSpace(value.Cluster), strings.TrimSpace(roomName)) ||
			!strings.EqualFold(strings.TrimSpace(value.Shard), strings.TrimSpace(worldName)) {
			continue
		}
		processSaveRoot := strings.TrimSpace(value.StorageRoot)
		if processSaveRoot == "" {
			continue
		}
		if configDirectory := strings.TrimSpace(value.ConfigDirectory); configDirectory != "" {
			processSaveRoot = filepath.Join(processSaveRoot, configDirectory)
		}
		if sameNativePath(processSaveRoot, c.config.SaveRoot) {
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PID < result[j].PID })
	return result
}

func sameNativePath(first, second string) bool {
	first, second = filepath.Clean(strings.TrimSpace(first)), filepath.Clean(strings.TrimSpace(second))
	if resolved, err := filepath.EvalSymlinks(first); err == nil {
		first = resolved
	}
	if resolved, err := filepath.EvalSymlinks(second); err == nil {
		second = resolved
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.EqualFold(first, second)
	}
	return first == second
}

func processIDList(values []shared.ShardProcessReport) string {
	ids := make([]string, 0, len(values))
	for _, value := range values {
		ids = append(ids, strconv.FormatInt(int64(value.PID), 10))
	}
	return strings.Join(ids, "、")
}

func (c *TmuxControl) runtimeProcesses(ctx context.Context) ([]shared.ShardProcessReport, error) {
	for {
		now := c.now()
		c.processMu.Lock()
		if now.Before(c.processCache.expiresAt) {
			values, err := append([]shared.ShardProcessReport(nil), c.processCache.values...), c.processCache.err
			c.processMu.Unlock()
			return values, err
		}
		if inFlight := c.processInFlight; inFlight != nil {
			c.processMu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-inFlight:
				continue
			}
		}
		inFlight := make(chan struct{})
		c.processInFlight = inFlight
		version := c.processVersion
		c.processMu.Unlock()

		values, err := c.config.ProcessProbe(ctx)
		values = append([]shared.ShardProcessReport(nil), values...)
		c.processMu.Lock()
		current := c.processVersion == version
		if current {
			c.processCache = cachedTmuxProcesses{values: values, err: err, expiresAt: c.now().Add(tmuxProcessCacheTTL)}
		}
		c.processInFlight = nil
		close(inFlight)
		c.processMu.Unlock()
		if !current {
			continue
		}
		return values, err
	}
}

func (c *TmuxControl) invalidateProcessSnapshot() {
	c.processMu.Lock()
	c.processCache = cachedTmuxProcesses{}
	c.processVersion++
	c.processMu.Unlock()
}

func (c *TmuxControl) cachedRuntimeStatus(ctx context.Context, key string, load func() (RuntimeStatus, error)) (RuntimeStatus, error) {
	for {
		now := c.now()
		c.statusMu.Lock()
		if cached, exists := c.statusCache[key]; exists && now.Before(cached.expiresAt) {
			c.statusMu.Unlock()
			return cached.value, cached.err
		}
		if inFlight := c.statusInFlight[key]; inFlight != nil {
			c.statusMu.Unlock()
			select {
			case <-ctx.Done():
				return RuntimeStatus{State: RuntimeUnknown}, ctx.Err()
			case <-inFlight:
				continue
			}
		}
		inFlight := make(chan struct{})
		c.statusInFlight[key] = inFlight
		version := c.statusVersion[key]
		c.statusMu.Unlock()

		value, err := load()
		c.statusMu.Lock()
		if c.statusVersion[key] == version {
			c.statusCache[key] = cachedTmuxRuntimeStatus{value: value, err: err, expiresAt: c.now().Add(tmuxRuntimeStatusCacheTTL)}
		}
		delete(c.statusInFlight, key)
		close(inFlight)
		c.statusMu.Unlock()
		return value, err
	}
}

func (c *TmuxControl) invalidateRuntimeStatus(roomName, worldName string) {
	key := c.shardKey(roomName, worldName)
	c.statusMu.Lock()
	delete(c.statusCache, key)
	c.statusVersion[key]++
	c.statusMu.Unlock()
}
