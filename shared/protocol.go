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
	// 注册和认证消息
	TypeRegister     MessageType = "register"     // Agent注册
	TypeRegisterAck  MessageType = "register_ack" // 服务器确认注册
	TypeAuthenticate MessageType = "authenticate" // 认证

	// 数据上报消息
	TypeActiveReport  MessageType = "active_report"  // 主动上报
	TypePassiveReport MessageType = "passive_report" // 被动上报（响应查询）
	TypeReportAck     MessageType = "report_ack"     // 服务器确认数据上报

	// 命令控制消息
	TypeCommand     MessageType = "command"      // 服务器下发命令
	TypeCommandAck  MessageType = "command_ack"  // Agent确认收到命令
	TypeCommandResp MessageType = "command_resp" // Agent回复命令执行结果

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
}

// RegisterAckPayload 是注册确认消息的负载
type RegisterAckPayload struct {
	ServerPublicKey string `json:"server_public_key"` // 服务器公钥
	Success         bool   `json:"success"`           // 注册是否成功
	Message         string `json:"message"`           // 消息或错误
}

// CommandPayload 是命令消息的负载
type CommandPayload struct {
	CommandID string `json:"command_id"` // 命令ID
	Type      string `json:"type"`       // 命令类型: "shell", "script", "builtin"
	Content   string `json:"content"`    // 命令内容
	Timeout   int    `json:"timeout"`    // 超时时间(秒)
}

// CommandResponsePayload 是命令响应消息的负载
type CommandResponsePayload struct {
	CommandID string `json:"command_id"` // 对应的命令ID
	Success   bool   `json:"success"`    // 执行是否成功
	Output    string `json:"output"`     // 命令输出
	ErrorMsg  string `json:"error_msg"`  // 错误信息(如果有)
	ExitCode  int    `json:"exit_code"`  // 退出码
}

// ReportDataPayload 是数据上报消息的负载
type ReportDataPayload struct {
	ReportID   string                 `json:"report_id"`   // 上报ID
	ReportType string                 `json:"report_type"` // 上报类型
	Data       map[string]interface{} `json:"data"`        // 上报数据
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