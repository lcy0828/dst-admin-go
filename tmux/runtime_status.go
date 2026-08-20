package tmux

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	dstinstall "dont/internal/dstserver"
	"dont/internal/rooms"

	"github.com/go-ini/ini"
)

type RuntimeState string

const (
	RuntimeStopped  RuntimeState = "stopped"
	RuntimeStarting RuntimeState = "starting"
	RuntimeRunning  RuntimeState = "running"
	RuntimeFailed   RuntimeState = "failed"
)

type RuntimeStatus struct {
	State         RuntimeState
	Code          string
	Message       string
	SessionExists bool
}

const runtimeLogScanChunkBytes = 64 * 1024
const runtimeLogTailBytes int64 = 256 * 1024

var runtimeFailureSignals = []struct {
	needle   string
	code     string
	message  string
	priority int
}{
	{`E_EXPIRED_TOKEN`, "TOKEN_EXPIRED", "Klei 集群令牌已过期或无效", 2},
	{`No auth token could be found`, "TOKEN_MISSING", "未找到有效的 Klei 集群令牌", 2},
	{`Your Server Will Not Start`, "STARTUP_REJECTED", "DST 拒绝启动，请检查集群令牌和服务器日志", 1},
	{`Address already in use`, "PORT_IN_USE", "服务器端口已被占用", 2},
	{`Failed to bind`, "PORT_BIND_FAILED", "服务器无法绑定配置端口", 2},
	{`Could not bind`, "PORT_BIND_FAILED", "服务器无法绑定配置端口", 2},
	{`Must specify the task set for a level`, "WORLDGEN_TASK_SET_MISSING", "世界配置缺少地图任务集，请重新保存地面或洞穴的世界生成配置", 3},
	{`has no data! If preset`, "WORLDGEN_TASK_SET_INVALID", "世界配置引用的地图任务集不可用，请检查世界预设和相关模组", 3},
	{`Worldgen had an error`, "WORLDGEN_FAILED", "世界生成失败，请检查世界配置和世界生成模组", 1},
	{`Error loading worldgen_main.lua`, "WORLDGEN_FAILED", "世界生成失败，请检查世界配置和世界生成模组", 1},
}

var runtimeReadySignals = []string{
	"[DST-ADMIN-RUNTIME READY]",
	"Server registered via geo DNS",
	"Registering master server in lobby",
	"Shard server ready",
	"Connected to master",
	"Master connection established",
	"[Shard] secondary shard is now ready!",
	"[Shard] secondary shard LUA is now ready!",
	"Sim paused",
	"Serializing world:",
}

// ClassifyRuntimeLog exposes the same readiness and startup-failure semantics
// to non-tmux Runtime drivers without coupling them to a DSTServer instance.
func ClassifyRuntimeLog(content string) RuntimeStatus {
	return classifyRuntimeLog(content, RuntimeStatus{
		State: RuntimeStarting, Message: "等待 DST 完成世界加载和服务注册", SessionExists: true,
	})
}

func (s *DSTServer) RuntimeStatus() (RuntimeStatus, error) {
	exists, err := s.SessionExists()
	if err != nil {
		return RuntimeStatus{}, err
	}
	if !exists {
		return RuntimeStatus{State: RuntimeStopped}, nil
	}

	status := RuntimeStatus{State: RuntimeStarting, Message: "等待 DST 完成世界加载和服务注册", SessionExists: true}
	logPath := s.runtimeLogPath()
	info, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return status, nil
		}
		return RuntimeStatus{}, fmt.Errorf("读取分片日志状态: %w", err)
	}
	createdAt, createdErr := s.sessionCreatedAt()
	if createdErr == nil && !runtimeLogModifiedAfterSession(info.ModTime(), createdAt) {
		return status, nil
	}
	logStartedAt, hasLogStart, err := readRuntimeLogStartedAt(logPath)
	if err != nil {
		return RuntimeStatus{}, fmt.Errorf("读取分片日志: %w", err)
	}
	if createdErr == nil && hasLogStart && logStartedAt.Before(createdAt) {
		return status, nil
	}
	tail, err := readFileTail(logPath, runtimeLogTailBytes)
	if err != nil {
		return RuntimeStatus{}, fmt.Errorf("读取分片日志: %w", err)
	}
	if tailStatus := classifyRuntimeLog(string(tail), status); tailStatus.State != RuntimeStarting {
		return tailStatus, nil
	}
	classified, err := classifyRuntimeLogFile(logPath, status)
	if err != nil {
		return RuntimeStatus{}, fmt.Errorf("读取分片日志: %w", err)
	}
	return classified, nil
}

func runtimeLogModifiedAfterSession(logModified, sessionCreated time.Time) bool {
	return logModified.After(sessionCreated)
}

func runtimeLogStartedAt(content string) (time.Time, bool) {
	const marker = "Current time: "
	start := strings.Index(content, marker)
	if start < 0 {
		return time.Time{}, false
	}
	value := content[start+len(marker):]
	if end := strings.IndexByte(value, '\n'); end >= 0 {
		value = value[:end]
	}
	parsed, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.TrimSpace(value), time.Local)
	return parsed, err == nil
}

func classifyRuntimeLog(content string, fallback RuntimeStatus) RuntimeStatus {
	latestReady := -1
	for _, signal := range runtimeReadySignals {
		if index := strings.LastIndex(content, signal); index > latestReady {
			latestReady = index
		}
	}
	latestFailure := -1
	failurePriority := -1
	var failure RuntimeStatus
	for _, signal := range runtimeFailureSignals {
		if index := strings.LastIndex(content, signal.needle); index > latestReady && (signal.priority > failurePriority || signal.priority == failurePriority && index > latestFailure) {
			latestFailure = index
			failurePriority = signal.priority
			failure = RuntimeStatus{
				State: RuntimeFailed, Code: signal.code, Message: signal.message, SessionExists: true,
			}
		}
	}
	if latestFailure >= 0 {
		return failure
	}
	if latestReady >= 0 {
		message := ""
		if strings.Contains(content, "Steam Workshop functionality will be disabled") {
			message = "服务已运行，但 Steam Workshop 当前不可用"
		}
		return RuntimeStatus{State: RuntimeRunning, Message: message, SessionExists: true}
	}
	return fallback
}

func classifyRuntimeLogFile(path string, fallback RuntimeStatus) (RuntimeStatus, error) {
	file, err := os.Open(path)
	if err != nil {
		return RuntimeStatus{}, err
	}
	defer file.Close()

	latestReady := int64(-1)
	failurePositions := make([]int64, len(runtimeFailureSignals))
	for index := range failurePositions {
		failurePositions[index] = -1
	}
	workshopUnavailable := false
	overlapSize := runtimeLogSignalOverlap()
	overlap := make([]byte, 0, overlapSize)
	chunk := make([]byte, runtimeLogScanChunkBytes)
	var consumed int64
	for {
		read, readErr := file.Read(chunk)
		if read > 0 {
			window := make([]byte, 0, len(overlap)+read)
			window = append(window, overlap...)
			window = append(window, chunk[:read]...)
			base := consumed - int64(len(overlap))
			for _, signal := range runtimeReadySignals {
				if index := bytes.LastIndex(window, []byte(signal)); index >= 0 {
					position := base + int64(index)
					if position > latestReady {
						latestReady = position
					}
				}
			}
			for index, signal := range runtimeFailureSignals {
				if found := bytes.LastIndex(window, []byte(signal.needle)); found >= 0 {
					position := base + int64(found)
					if position > failurePositions[index] {
						failurePositions[index] = position
					}
				}
			}
			if bytes.Contains(window, []byte("Steam Workshop functionality will be disabled")) {
				workshopUnavailable = true
			}
			consumed += int64(read)
			keep := overlapSize
			if keep > len(window) {
				keep = len(window)
			}
			overlap = append(overlap[:0], window[len(window)-keep:]...)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return RuntimeStatus{}, readErr
		}
	}

	latestFailure := int64(-1)
	failurePriority := -1
	var failure RuntimeStatus
	for index, signal := range runtimeFailureSignals {
		position := failurePositions[index]
		if position > latestReady && (signal.priority > failurePriority || signal.priority == failurePriority && position > latestFailure) {
			latestFailure = position
			failurePriority = signal.priority
			failure = RuntimeStatus{State: RuntimeFailed, Code: signal.code, Message: signal.message, SessionExists: true}
		}
	}
	if latestFailure >= 0 {
		return failure, nil
	}
	if latestReady >= 0 {
		message := ""
		if workshopUnavailable {
			message = "服务已运行，但 Steam Workshop 当前不可用"
		}
		return RuntimeStatus{State: RuntimeRunning, Message: message, SessionExists: true}, nil
	}
	return fallback, nil
}

func runtimeLogSignalOverlap() int {
	longest := len("Steam Workshop functionality will be disabled")
	for _, signal := range runtimeReadySignals {
		if len(signal) > longest {
			longest = len(signal)
		}
	}
	for _, signal := range runtimeFailureSignals {
		if len(signal.needle) > longest {
			longest = len(signal.needle)
		}
	}
	return longest - 1
}

func (s *DSTServer) SessionExists() (bool, error) {
	command := exec.Command("tmux", s.tmuxArguments("has-session", "-t", "="+s.SessionName)...)
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("检查 tmux 会话: %w", err)
	}
	return true, nil
}

func (s *DSTServer) DefaultSocketSessionExists() (bool, error) {
	if strings.TrimSpace(s.SocketPath) == "" {
		return false, nil
	}
	command := exec.Command("tmux", "has-session", "-t", "="+s.SessionName)
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("check legacy tmux session: %w", err)
	}
	return true, nil
}

func (s *DSTServer) sessionCreatedAt() (time.Time, error) {
	output, err := exec.Command("tmux", s.tmuxArguments("display-message", "-p", "-t", s.SessionName, "#{session_created}")...).Output()
	if err != nil {
		return time.Time{}, err
	}
	timestamp, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(timestamp, 0), nil
}

func (s *DSTServer) tmuxArguments(arguments ...string) []string {
	if strings.TrimSpace(s.SocketPath) == "" {
		return arguments
	}
	return append([]string{"-S", s.SocketPath}, arguments...)
}

// ConsoleTransportHealth reports transport failures separately from the DST
// lifecycle state. It never writes to the pane.
func (s *DSTServer) ConsoleTransportHealth() (string, bool, error) {
	configPath := filepath.Join(s.StorageRoot, s.ConfDir, s.ArchiveName, "cluster.ini")
	if config, err := ini.Load(configPath); err == nil {
		value := strings.TrimSpace(config.Section("MISC").Key("console_enabled").String())
		if strings.EqualFold(value, "false") {
			return "disabled", false, nil
		}
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return "not_found", false, nil
	}
	if s.SocketPath != "" {
		if _, err := os.Stat(s.SocketPath); err != nil {
			if os.IsPermission(err) {
				return "permission_denied", false, nil
			}
			if os.IsNotExist(err) {
				return "socket_unavailable", false, nil
			}
			return "socket_unavailable", false, err
		}
	}
	output, err := exec.Command("tmux", s.tmuxArguments(
		"display-message", "-p", "-t", "="+s.SessionName+":0.0",
		"#{pane_dead}|#{pane_current_command}|#{pane_pid}",
	)...).CombinedOutput()
	if err != nil {
		return classifyConsoleCommandError(string(output), s.SocketPath != ""), false, nil
	}
	fields := strings.Split(strings.TrimSpace(string(output)), "|")
	if len(fields) != 3 {
		return "process_mismatch", false, nil
	}
	if fields[0] == "1" {
		return "pane_dead", false, nil
	}
	command := strings.ToLower(strings.TrimSpace(fields[1]))
	pid, parseErr := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
	if parseErr != nil || pid <= 0 || !strings.Contains(command, "dontstarve") {
		return "process_mismatch", false, nil
	}
	clients, err := s.tmux.ListClients()
	if err != nil {
		return "socket_unavailable", false, nil
	}
	for _, client := range clients {
		if client != nil && client.Session == s.SessionName && !client.Readonly {
			return "ready", true, nil
		}
	}
	return "ready", false, nil
}

func classifyConsoleCommandError(output string, privateSocket bool) string {
	message := strings.ToLower(output)
	switch {
	case strings.Contains(message, "permission denied") || strings.Contains(message, "operation not permitted"):
		return "permission_denied"
	case strings.Contains(message, "can't find session") || strings.Contains(message, "no sessions"):
		return "not_found"
	case privateSocket || strings.Contains(message, "no server running") || strings.Contains(message, "connection refused"):
		return "socket_unavailable"
	default:
		return "not_found"
	}
}

func (s *DSTServer) ConsoleAttachCommand(readOnly bool) ([]string, error) {
	status, externalWriter, err := s.ConsoleTransportHealth()
	if err != nil {
		return nil, err
	}
	if status != "ready" {
		return nil, fmt.Errorf("console transport is %s", status)
	}
	if externalWriter && !readOnly {
		return nil, fmt.Errorf("console already has an unmanaged writer")
	}
	arguments := []string{"tmux"}
	if s.SocketPath != "" {
		arguments = append(arguments, "-S", s.SocketPath)
	}
	arguments = append(arguments, "attach-session")
	if readOnly {
		arguments = append(arguments, "-r")
	}
	arguments = append(arguments, "-t", "="+s.SessionName)
	return arguments, nil
}

// RuntimeInstanceID identifies one concrete tmux session lifetime. A session
// recreated with the same name receives a different identity.
func (s *DSTServer) RuntimeInstanceID() (string, error) {
	output, err := exec.Command("tmux", s.tmuxArguments(
		"display-message", "-p", "-t", "="+s.SessionName,
		"#{session_created}\t#{session_id}\t#{pid}",
	)...).Output()
	if err != nil {
		return "", err
	}
	fields := strings.Split(strings.TrimSpace(string(output)), "\t")
	if len(fields) != 3 {
		return "", errors.New("tmux runtime instance identity is invalid")
	}
	created, createdErr := strconv.ParseInt(fields[0], 10, 64)
	pid, pidErr := strconv.ParseInt(fields[2], 10, 64)
	if createdErr != nil || created <= 0 || pidErr != nil || pid <= 0 || !strings.HasPrefix(fields[1], "$") || len(fields[1]) > 32 {
		return "", errors.New("tmux runtime instance identity is invalid")
	}
	return fmt.Sprintf("%s@%d/%s/%d", s.SessionName, created, fields[1], pid), nil
}

func (s *DSTServer) runtimeLogPath() string {
	return filepath.Join(s.StorageRoot, s.ConfDir, s.ArchiveName, s.WorldName, "server_log.txt")
}

func readRuntimeLogStartedAt(path string) (time.Time, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}, false, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return time.Time{}, false, err
	}
	startedAt, ok := runtimeLogStartedAt(string(content))
	return startedAt, ok, nil
}

func readFileTail(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		if _, err := file.Seek(info.Size()-limit, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(io.LimitReader(file, limit))
}

func (s *DSTServer) validateClusterAuth() error {
	roomPath := filepath.Join(s.StorageRoot, s.ConfDir, s.ArchiveName)
	clusterPath := filepath.Join(roomPath, "cluster.ini")
	if config, err := ini.Load(clusterPath); err == nil && config.Section("NETWORK").Key("offline_cluster").MustBool(false) {
		return nil
	}
	tokenPath := filepath.Join(roomPath, "cluster_token.txt")
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("在线服务器缺少 Klei 集群令牌")
		}
		return fmt.Errorf("读取 Klei 集群令牌: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if err := rooms.ValidateClusterToken(token, false); err != nil {
		return err
	}
	return nil
}

func buildStartCommand(layout dstinstall.Layout, executable string, arguments []string) string {
	environment := startEnvironment(layout)
	parts := make([]string, 0, len(environment)+len(arguments)+2)
	if len(environment) > 0 {
		parts = append(parts, "env")
		keys := make([]string, 0, len(environment))
		for key := range environment {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			parts = append(parts, key+"="+shellArg(environment[key]))
		}
	}
	parts = append(parts, shellArg(executable))
	for _, argument := range arguments {
		parts = append(parts, shellArg(argument))
	}
	return strings.Join(parts, " ")
}

func startEnvironment(layout dstinstall.Layout) map[string]string {
	if layout.Kind != dstinstall.LayoutMac {
		return nil
	}
	libraryDirectory := dstinstall.SteamClientLibraryDirectory(layout)
	if libraryDirectory == "" {
		return nil
	}
	return map[string]string{
		"DYLD_FALLBACK_LIBRARY_PATH": joinSearchPath(libraryDirectory, os.Getenv("DYLD_FALLBACK_LIBRARY_PATH")),
		"DYLD_LIBRARY_PATH":          joinSearchPath(libraryDirectory, os.Getenv("DYLD_LIBRARY_PATH")),
		"SteamAppId":                 dstinstall.AppIDGame,
		"SteamGameId":                dstinstall.AppIDGame,
	}
}

func joinSearchPath(primary, existing string) string {
	if strings.TrimSpace(existing) == "" {
		return primary
	}
	return primary + string(os.PathListSeparator) + existing
}
