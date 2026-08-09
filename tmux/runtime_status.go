package tmux

import (
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
	"Server registered via geo DNS",
	"Registering master server in lobby",
	"Shard server ready",
	"Connected to master",
	"Master connection established",
	"[Shard] secondary shard is now ready!",
	"[Shard] secondary shard LUA is now ready!",
	"Sim paused",
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
	content, err := readFileTail(logPath, runtimeLogTailBytes)
	if err != nil {
		return RuntimeStatus{}, fmt.Errorf("读取分片日志: %w", err)
	}
	if createdErr == nil {
		if logStartedAt, ok := runtimeLogStartedAt(string(content)); ok && logStartedAt.Before(createdAt) {
			return status, nil
		}
	}
	return classifyRuntimeLog(string(content), status), nil
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

func (s *DSTServer) SessionExists() (bool, error) {
	command := exec.Command("tmux", "has-session", "-t", "="+s.SessionName)
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("检查 tmux 会话: %w", err)
	}
	return true, nil
}

func (s *DSTServer) sessionCreatedAt() (time.Time, error) {
	output, err := exec.Command("tmux", "display-message", "-p", "-t", s.SessionName, "#{session_created}").Output()
	if err != nil {
		return time.Time{}, err
	}
	timestamp, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(timestamp, 0), nil
}

func (s *DSTServer) runtimeLogPath() string {
	return filepath.Join(s.StorageRoot, s.ConfDir, s.ArchiveName, s.WorldName, "server_log.txt")
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
