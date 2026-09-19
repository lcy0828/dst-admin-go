package shared

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"time"
)

// 初始化随机数种子
func init() {
	rand.Seed(time.Now().UnixNano())
}

// MessageType 定义了消息类型
type MessageType string

const (
	DefaultSystemReportInterval = time.Minute
	SystemReportFreshnessWindow = 90 * time.Second

	// 注册和认证消息
	TypeRegister     MessageType = "register"     // Agent注册
	TypeRegisterAck  MessageType = "register_ack" // 服务器确认注册
	TypeAuthenticate MessageType = "authenticate" // 认证

	// 数据上报消息
	TypeActiveReport  MessageType = "active_report"  // 主动上报
	TypePassiveReport MessageType = "passive_report" // 被动上报（响应查询）
	TypeReportAck     MessageType = "report_ack"     // 服务器确认数据上报
	TypeReportRequest MessageType = "report_request" // 服务器请求数据上报

	// 命令控制消息
	TypeCommand         MessageType = "command"      // 服务器下发命令
	TypeCommandAck      MessageType = "command_ack"  // Agent确认收到命令
	TypeCommandResp     MessageType = "command_resp" // Agent回复命令执行结果
	TypeCommandProgress MessageType = "command_progress"

	// 心跳和状态消息
	TypeHeartbeat    MessageType = "heartbeat"     // 心跳
	TypeHeartbeatAck MessageType = "heartbeat_ack" // 心跳确认
)

// Message 是所有消息的基础结构
type Message struct {
	Type      MessageType     `json:"type"`      // 消息类型
	ID        string          `json:"id"`        // 消息ID
	Timestamp int64           `json:"timestamp"` // 时间戳
	AgentID   string          `json:"agent_id"`  // Agent标识
	Payload   json.RawMessage `json:"payload"`   // 消息负载
}

// RegisterPayload 是注册消息的负载
type RegisterPayload struct {
	PublicKey string `json:"public_key"` // Base64编码的公钥
	Hostname  string `json:"hostname"`   // 主机名
	OS        string `json:"os"`         // 操作系统
	Arch      string `json:"arch"`       // 架构
	AgentUUID string `json:"agent_uuid"` // 代理唯一标识符
	Version   string `json:"version,omitempty"`
}

// RegisterAckPayload 是注册确认消息的负载
type RegisterAckPayload struct {
	ServerPublicKey string `json:"server_public_key"` // 服务器公钥
	Success         bool   `json:"success"`           // 注册是否成功
	Message         string `json:"message"`           // 消息或错误
}

// CommandPayload 是命令消息的负载
type CommandPayload struct {
	CommandID        string                   `json:"command_id"` // 命令ID
	Type             string                   `json:"type"`       // 固定领域动作；不接受任意 Shell
	Content          string                   `json:"content,omitempty"`
	ShardOperation   *ShardOperationRequest   `json:"shard_operation,omitempty"`
	RuntimeOperation *RuntimeOperationRequest `json:"runtime_operation,omitempty"`
	AgentUpgrade     *AgentUpgradeRequest     `json:"agent_upgrade,omitempty"`
	Timeout          int                      `json:"timeout"` // 超时时间(秒)
}

// CommandResponsePayload 是命令响应消息的负载
type CommandResponsePayload struct {
	CommandID string `json:"command_id"` // 对应的命令ID
	Success   bool   `json:"success"`    // 执行是否成功
	Output    string `json:"output"`     // 命令输出
	ErrorMsg  string `json:"error_msg"`  // 错误信息(如果有)
	ExitCode  int    `json:"exit_code"`  // 退出码
}

type CommandProgressPayload struct {
	CommandID      string                `json:"command_id"`
	Sequence       uint64                `json:"sequence"`
	Stage          string                `json:"stage,omitempty"`
	Percent        int                   `json:"percent"`
	Message        string                `json:"message"`
	WorkshopID     string                `json:"workshop_id,omitempty"`
	CurrentItem    int                   `json:"current_item,omitempty"`
	TotalItems     int                   `json:"total_items,omitempty"`
	Items          []ModDownloadProgress `json:"items,omitempty"`
	CurrentBytes   int64                 `json:"current_bytes,omitempty"`
	TotalBytes     int64                 `json:"total_bytes,omitempty"`
	BytesPerSecond int64                 `json:"bytes_per_second,omitempty"`
}

// ModDownloadProgress is cumulative download evidence, not world-load status.
// Sending all items keeps completed results when transport coalesces updates.
type ModDownloadProgress struct {
	WorkshopID     string `json:"workshopId"`
	TargetID       string `json:"targetId,omitempty"`
	InstallationID string `json:"installationId,omitempty"`
	Status         string `json:"status"`
	Message        string `json:"message,omitempty"`
	CurrentBytes   int64  `json:"currentBytes,omitempty"`
	TotalBytes     int64  `json:"totalBytes,omitempty"`
	BytesPerSecond int64  `json:"bytesPerSecond,omitempty"`
}

// WorldOperationProgress describes observed lifecycle/log milestones. Percent
// is a stage estimate, never a timer or a guarantee of remaining time.
type WorldOperationProgress struct {
	WorldID  string `json:"worldId"`
	Name     string `json:"name"`
	IsMaster bool   `json:"isMaster,omitempty"`
	Stage    string `json:"stage"`
	Percent  int    `json:"percent"`
	Message  string `json:"message,omitempty"`
}

// ReportDataPayload 是数据上报消息的负载
type ReportDataPayload struct {
	ReportID   string                 `json:"report_id"`   // 上报ID
	ReportType string                 `json:"report_type"` // 上报类型
	Data       map[string]interface{} `json:"data"`        // 上报数据
}

const AgentUpgradeProtocolVersion = 1

const AgentUpgradeCommand = "agent.upgrade.v1"

// AgentUpgradeRequest contains an authenticated, short-lived controller
// download path and immutable package metadata. It never carries shell input.
type AgentUpgradeRequest struct {
	ProtocolVersion int    `json:"protocol_version"`
	ReleaseID       string `json:"release_id"`
	Version         string `json:"version"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	DownloadPath    string `json:"download_path"`
	DownloadToken   string `json:"download_token"`
	SHA256          string `json:"sha256"`
	Size            int64  `json:"size"`
}

type AgentUpgradeResult struct {
	ProtocolVersion int       `json:"protocol_version"`
	ReleaseID       string    `json:"release_id"`
	PreviousVersion string    `json:"previous_version"`
	Version         string    `json:"version"`
	RestartRequired bool      `json:"restart_required"`
	ObservedAt      time.Time `json:"observed_at"`
}

const RuntimeInventoryProtocolVersion = 1

const ShardOperationProtocolVersion = 1

type ShardAction string

const (
	ShardActionStatus  ShardAction = "shard.status"
	ShardActionStart   ShardAction = "shard.start"
	ShardActionStop    ShardAction = "shard.stop"
	ShardActionRestart ShardAction = "shard.restart"
	ShardActionSave    ShardAction = "shard.save"
)

// ShardOperationRequest contains identifiers only. Runtime paths are resolved
// from the Agent's local trusted installation registry.
type ShardOperationRequest struct {
	ProtocolVersion  int                    `json:"protocol_version"`
	OperationID      string                 `json:"operation_id"`
	OperationKey     string                 `json:"operation_key"`
	InstallationID   string                 `json:"installation_id"`
	Action           ShardAction            `json:"action"`
	RuntimeMode      RuntimePerformanceMode `json:"runtime_mode,omitempty"`
	LaunchOptions    RuntimeLaunchOptions   `json:"launch_options,omitempty"`
	Cluster          string                 `json:"cluster"`
	Shard            string                 `json:"shard"`
	TopologyRevision string                 `json:"topology_revision"`
	LeaseID          string                 `json:"lease_id,omitempty"`
	FencingToken     uint64                 `json:"fencing_token,omitempty"`
	LeaseExpiresAt   *time.Time             `json:"lease_expires_at,omitempty"`
}

type ShardRuntimeStatus struct {
	RuntimeMode   RuntimePerformanceMode `json:"runtime_mode,omitempty"`
	State         string                 `json:"state"`
	StartupStage  string                 `json:"startup_stage,omitempty"`
	Code          string                 `json:"code,omitempty"`
	Message       string                 `json:"message,omitempty"`
	SessionExists bool                   `json:"session_exists"`
	Paused        *bool                  `json:"paused,omitempty"`
}

type ShardOperationResult struct {
	ProtocolVersion int                    `json:"protocol_version"`
	OperationID     string                 `json:"operation_id"`
	OperationKey    string                 `json:"operation_key"`
	InstallationID  string                 `json:"installation_id"`
	Action          ShardAction            `json:"action"`
	RuntimeMode     RuntimePerformanceMode `json:"runtime_mode,omitempty"`
	LaunchOptions   RuntimeLaunchOptions   `json:"launch_options,omitempty"`
	Cluster         string                 `json:"cluster"`
	Shard           string                 `json:"shard"`
	FencingToken    uint64                 `json:"fencing_token,omitempty"`
	Status          ShardRuntimeStatus     `json:"status"`
	Message         string                 `json:"message,omitempty"`
	Idempotent      bool                   `json:"idempotent"`
	ObservedAt      time.Time              `json:"observed_at"`
}

// RuntimeLaunchOptions retain the wire contract with older executors. Current
// executors always skip Workshop updates during ordinary world start.
type RuntimeLaunchOptions struct {
	SkipUpdateServerMods bool `json:"skip_update_server_mods,omitempty"`
}

func IsShardAction(value ShardAction) bool {
	switch value {
	case ShardActionStatus, ShardActionStart, ShardActionStop, ShardActionRestart, ShardActionSave:
		return true
	default:
		return false
	}
}

func ShardActionMutates(value ShardAction) bool {
	return value != ShardActionStatus && IsShardAction(value)
}

// RuntimeInventoryRequest contains controller-managed paths interpreted by
// the Agent. Inventory collection is read-only and rejects relative paths.
type RuntimeInventoryRequest struct {
	InstallationID string `json:"installation_id"`
	DisplayName    string `json:"display_name"`
	SavePath       string `json:"save_path"`
	ServerPath     string `json:"server_path"`
	ServerMode     string `json:"server_mode"`
}

type CPUInventory struct {
	LogicalProcessors     int                  `json:"logical_processors"`
	PhysicalCores         int                  `json:"physical_cores"`
	PhysicalCoreSource    string               `json:"physical_core_source"`
	PhysicalCoreEstimated bool                 `json:"physical_core_estimated"`
	TopologyAvailable     bool                 `json:"topology_available"`
	SMTDetected           bool                 `json:"smt_detected"`
	Threads               []CPUThreadInventory `json:"threads,omitempty"`
}

// CPUThreadInventory maps one schedulable logical CPU to its physical core.
// PackageID and CoreID are opaque platform identifiers and must be interpreted
// together; CoreID alone is not necessarily unique across CPU packages.
type CPUThreadInventory struct {
	LogicalID int    `json:"logical_id"`
	PackageID string `json:"package_id"`
	CoreID    string `json:"core_id"`
}

type MemoryInventory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

type RuntimeInstallationReport struct {
	ID           string `json:"id"`
	DisplayName  string `json:"display_name"`
	SavePath     string `json:"save_path"`
	ServerPath   string `json:"server_path"`
	ServerMode   string `json:"server_mode"`
	SavePathOK   bool   `json:"save_path_ok"`
	ServerPathOK bool   `json:"server_path_ok"`
}

type RuntimePerformanceStatus string

const (
	RuntimePerformanceNotInstalled       RuntimePerformanceStatus = "not_installed"
	RuntimePerformanceDetectedUnverified RuntimePerformanceStatus = "detected_unverified"
	RuntimePerformanceIncompatible       RuntimePerformanceStatus = "incompatible"
	RuntimePerformanceReady              RuntimePerformanceStatus = "ready"
)

type RuntimePerformanceMode string

const (
	RuntimePerformanceModeGame    RuntimePerformanceMode = "game"
	RuntimePerformanceModeLuaJIT  RuntimePerformanceMode = "luajit"
	RuntimePerformanceModeJITOff  RuntimePerformanceMode = "luajit-jit-off"
	RuntimePerformanceModeJITOn   RuntimePerformanceMode = "luajit-jit-on"
	RuntimePerformanceModeArenaGC RuntimePerformanceMode = "arena-gc"
)

// RuntimePerformanceReport describes native VM optimization availability for
// one trusted DST installation. It is observational: reporting readiness never
// installs an injector or changes the selected runtime mode.
type RuntimePerformanceReport struct {
	Provider            string                   `json:"provider"`
	Status              RuntimePerformanceStatus `json:"status"`
	CanEnable           bool                     `json:"canEnable"`
	PackageVersion      string                   `json:"packageVersion,omitempty"`
	GameVersion         string                   `json:"gameVersion,omitempty"`
	SignatureVersion    string                   `json:"signatureVersion,omitempty"`
	BinarySHA256        string                   `json:"binarySha256,omitempty"`
	AutomaticSignatures bool                     `json:"automaticSignatures,omitempty"`
	SupportedModes      []RuntimePerformanceMode `json:"supportedModes"`
	Issues              []string                 `json:"issues"`
}

func NormalizeRuntimePerformanceMode(value RuntimePerformanceMode) (RuntimePerformanceMode, bool) {
	if value == "" {
		return RuntimePerformanceModeGame, true
	}
	switch value {
	case RuntimePerformanceModeGame, RuntimePerformanceModeLuaJIT, RuntimePerformanceModeArenaGC:
		return value, true
	default:
		return "", false
	}
}

type RoomInventoryReport struct {
	Directory     string                 `json:"directory"`
	Name          string                 `json:"name"`
	ConfigPath    string                 `json:"config_path"`
	BindIP        string                 `json:"bind_ip,omitempty"`
	MasterIP      string                 `json:"master_ip,omitempty"`
	MasterPort    int                    `json:"master_port,omitempty"`
	ClusterKeySet bool                   `json:"cluster_key_set"`
	Shards        []ShardInventoryReport `json:"shards"`
}

type ShardInventoryReport struct {
	Directory          string `json:"directory"`
	Name               string `json:"name"`
	ID                 int    `json:"id,omitempty"`
	Role               string `json:"role"`
	Type               string `json:"type,omitempty"`
	ConfigPath         string `json:"config_path"`
	ServerPort         int    `json:"server_port,omitempty"`
	MasterServerPort   int    `json:"master_server_port,omitempty"`
	AuthenticationPort int    `json:"authentication_port,omitempty"`
}

type ShardProcessReport struct {
	RuntimeMode     RuntimePerformanceMode `json:"runtime_mode,omitempty"`
	PID             int32                  `json:"pid"`
	RuntimeKind     string                 `json:"runtime_kind,omitempty"`
	InstanceID      string                 `json:"instance_id,omitempty"`
	Executable      string                 `json:"executable"`
	Cluster         string                 `json:"cluster"`
	Shard           string                 `json:"shard"`
	StorageRoot     string                 `json:"storage_root,omitempty"`
	ConfigDirectory string                 `json:"config_directory,omitempty"`
	StartedAt       *time.Time             `json:"started_at,omitempty"`
	CPUPercent      float64                `json:"cpu_percent,omitempty"`
	RSSBytes        uint64                 `json:"rss_bytes,omitempty"`
}

type RuntimeInventoryReport struct {
	ProtocolVersion int                       `json:"protocol_version"`
	ObservedAt      time.Time                 `json:"observed_at"`
	CPU             CPUInventory              `json:"cpu"`
	Memory          MemoryInventory           `json:"memory"`
	Installation    RuntimeInstallationReport `json:"installation"`
	Rooms           []RoomInventoryReport     `json:"rooms"`
	Processes       []ShardProcessReport      `json:"processes"`
	Warnings        []string                  `json:"warnings"`
}

// CreateMessage 创建新消息
func CreateMessage(msgType MessageType, agentID string, payload interface{}) (*Message, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return &Message{
		Type:      msgType,
		ID:        GenerateUUID(),
		Timestamp: time.Now().Unix(),
		AgentID:   agentID,
		Payload:   payloadBytes,
	}, nil
}

// GenerateUUID 生成唯一标识符
func GenerateUUID() string {
	// 生成更可靠的纯数字UUID，结合时间戳、随机数和主机特征
	// 格式: 时间戳+随机数+主机名散列
	timestamp := time.Now().UnixNano()

	// 生成8位随机数字
	rand.Seed(time.Now().UnixNano())
	randomNum := rand.Intn(100000000)

	// 获取主机名作为额外标识
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	// 计算主机名的简单哈希值（最后4位）
	hostHash := int64(0)
	for _, c := range hostname {
		hostHash = (hostHash*31 + int64(c)) % 10000
	}

	// 组合成最终纯数字UUID
	return fmt.Sprintf("%d%08d%04d", timestamp, randomNum, hostHash)
}

// GenerateCommandID 生成命令唯一标识符
func GenerateCommandID() string {
	// 生成命令ID，使用与UUID不同的格式以区分
	// 格式: CMD-时间戳-随机数
	timestamp := time.Now().UnixNano() / 1000000 // 毫秒时间戳

	// 生成6位随机数字
	rand.Seed(time.Now().UnixNano())
	randomNum := rand.Intn(1000000)

	// 组合成命令ID
	return fmt.Sprintf("CMD%d%06d", timestamp, randomNum)
}
