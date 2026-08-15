package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"dont/internal/shardtransfer"
	"dont/shared"
	"github.com/go-ini/ini"
	"github.com/gorilla/websocket"
)

// 常量
const (
	AgentVersion = "2.4.0"
	// 心跳间隔
	HeartbeatInterval = 30 * time.Second
	// 重连间隔
	ReconnectInterval = 5 * time.Second
	// 连接超时
	ConnectionTimeout  = 10 * time.Second
	CommandOutputLimit = 128 * 1024
	// 密钥更新消息类型
	TypeSecurityKeyUpdate = "security_key_update"
	// 密钥更新提议类型
	TypeSecurityKeyUpdateProposal = "security_key_update_proposal"
	// 密钥更新准备类型
	TypeSecurityKeyUpdateReady = "security_key_update_ready"
)

// 记录启动时间
var startTime = time.Now()

// Agent 表示一个代理实例
type Agent struct {
	Config          *Config
	keyPair         *shared.KeyPair
	serverPubKey    [32]byte
	conn            *shared.SecureConnection
	isConnected     bool
	connMutex       sync.Mutex
	reconnecting    bool
	stopChan        chan struct{}
	wg              sync.WaitGroup
	reportInterval  time.Duration
	reportMutex     sync.Mutex
	keyManager      *shared.KeyManager // 添加密钥管理器
	shardState      *shardOperationState
	shardRuntime    shardRuntimeFactory
	shardRuntimeMu  sync.Mutex
	shardRuntimes   map[string]shardRuntimeControl
	shardTransferMu sync.Mutex
	shardTransfers  map[string]*shardtransfer.Manager
	now             func() time.Time
}

// Config 代理配置
type Config struct {
	ServerURL            string        // 服务器WebSocket URL
	AgentID              string        // 代理唯一标识
	ReportInterval       time.Duration // 主动上报间隔
	SecurityKey          string        // 通信安全密钥
	KeyFile              string        // 密钥存储文件路径
	RuntimeInstallations []RuntimeInstallation
	OperationStateFile   string
}

func normalizedAgentURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.User != nil {
		return "", fmt.Errorf("Agent 服务器 URL 无效")
	}
	query := parsed.Query()
	for _, name := range []string{"key", "token", "security_key", "password", "secret"} {
		query.Del(name)
	}
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	return parsed.String(), nil
}

func displayAgentURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid Agent URL>"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.User = nil
	return parsed.String()
}

func isINIConfigPath(path string) bool {
	extension := strings.ToLower(filepath.Ext(path))
	return extension == ".conf" || extension == ".ini"
}

// NewAgent 创建一个新的代理实例
func NewAgent(config *Config) (*Agent, error) {
	// 生成密钥对
	keyPair, err := shared.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("生成密钥对失败: %v", err)
	}

	// 如果未指定KeyFile，设置默认路径
	if config.KeyFile == "" {
		// 使用默认的配置文件路径
		config.KeyFile = "./conf/app.conf"
		log.Printf("未指定配置文件，使用默认路径: %s", config.KeyFile)

		// 确保conf目录存在
		dir := filepath.Dir(config.KeyFile)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			log.Printf("创建配置目录: %s", dir)
			if err := os.MkdirAll(dir, 0755); err != nil {
				log.Printf("创建配置目录失败: %v，将使用当前目录", err)
				config.KeyFile = "agent_key.json"
			}
		}
	}

	agent := &Agent{
		Config:         config,
		keyPair:        keyPair,
		isConnected:    false,
		reconnecting:   false,
		stopChan:       make(chan struct{}),
		reportInterval: config.ReportInterval,
		now:            time.Now,
	}

	// 从配置文件加载配置
	serverURL, key, err := agent.loadConfig(config.KeyFile)
	if err != nil {
		log.Printf("警告: 从配置文件加载配置失败: %v", err)
	} else {
		// 设置服务器地址和密钥
		if serverURL != "" {
			log.Printf("从配置文件加载服务器地址: %s", displayAgentURL(serverURL))
			config.ServerURL = serverURL
		}

		if key != "" {
			log.Printf("已从配置文件加载通信密钥")
			config.SecurityKey = key
		}
	}
	if len(config.RuntimeInstallations) == 0 {
		installations, runtimeErr := loadRuntimeInstallations(config.KeyFile)
		if runtimeErr != nil {
			log.Printf("警告: 无法加载受信 DST 安装配置: %v", runtimeErr)
		} else {
			config.RuntimeInstallations = installations
		}
	} else {
		installations, runtimeErr := normalizeRuntimeInstallations(config.RuntimeInstallations)
		if runtimeErr != nil {
			return nil, runtimeErr
		}
		config.RuntimeInstallations = installations
	}
	if value := strings.TrimSpace(os.Getenv("DST_ADMIN_AGENT_SERVER_URL")); value != "" {
		config.ServerURL = value
	}
	if value := strings.TrimSpace(os.Getenv("DST_ADMIN_AGENT_SECURITY_KEY")); value != "" {
		config.SecurityKey = value
	}
	if config.OperationStateFile == "" {
		config.OperationStateFile = strings.TrimSpace(os.Getenv("DST_ADMIN_AGENT_STATE_FILE"))
		if config.OperationStateFile == "" {
			config.OperationStateFile = config.KeyFile + ".runtime-state.json"
		}
	}
	state, err := loadShardOperationState(config.OperationStateFile)
	if err != nil {
		return nil, fmt.Errorf("加载分片操作状态失败: %w", err)
	}
	agent.shardState = state
	agent.shardRuntime = newShardRuntimeControl
	agent.shardRuntimes = make(map[string]shardRuntimeControl)
	agent.shardTransfers = make(map[string]*shardtransfer.Manager)

	return agent, nil
}

// Start 启动代理
func (a *Agent) Start() error {
	log.Println("Agent开始启动...")

	// 连接到服务器
	err := a.Connect()
	if err != nil {
		log.Printf("连接服务器失败: %v, 将尝试重连", err)
		go a.reconnect()
	} else {
		// 连接成功，保存当前使用的配置
		if a.Config.SecurityKey != "" {
			err := a.saveConfig(a.Config.KeyFile, a.Config.ServerURL, a.Config.SecurityKey)
			if err != nil {
				log.Printf("警告: 无法保存配置到文件: %v", err)
			} else {
				log.Printf("配置已成功保存到: %s", a.Config.KeyFile)
			}
		}

		// 启动心跳机制
		a.wg.Add(1)
		go a.startHeartbeat()

		// 启动消息处理循环
		a.wg.Add(1)
		go a.handleMessages()

		// 启动主动上报循环
		if a.reportInterval > 0 {
			a.wg.Add(1)
			go a.startActiveReporting()
		}
	}

	log.Printf("Agent已启动，ID: %s, 连接到服务器: %s", a.Config.AgentID, displayAgentURL(a.Config.ServerURL))
	return nil
}

// Stop 停止代理
func (a *Agent) Stop() {
	log.Println("Agent正在停止...")
	close(a.stopChan)

	a.connMutex.Lock()
	if a.conn != nil {
		a.conn.Close()
	}
	a.isConnected = false
	a.connMutex.Unlock()

	a.wg.Wait()
	log.Println("Agent已停止")
}

// Connect 连接到服务器
func (a *Agent) Connect() error {
	a.connMutex.Lock()
	defer a.connMutex.Unlock()

	// 如果已经连接，直接返回
	if a.isConnected && a.conn != nil {
		return nil
	}

	// 检查是否设置了通信密钥
	if err := shared.ValidateSecurityKey(a.Config.SecurityKey); err != nil {
		return fmt.Errorf("未提供通信密钥，请设置Config.SecurityKey或在密钥文件中提供")
	}

	connectURL, err := normalizedAgentURL(a.Config.ServerURL)
	if err != nil {
		return err
	}
	a.Config.ServerURL = connectURL
	log.Printf("正在连接到服务器: %s", displayAgentURL(connectURL))

	// 创建WebSocket连接
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = ConnectionTimeout

	// 设置连接超时上下文
	ctx, cancel := context.WithTimeout(context.Background(), ConnectionTimeout)
	defer cancel()

	// 使用上下文创建连接
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+a.Config.SecurityKey)
	c, resp, err := dialer.DialContext(ctx, connectURL, headers)
	if err != nil {
		// 检查HTTP响应以提供更详细的错误信息
		if resp != nil {
			if resp.StatusCode == http.StatusUnauthorized {
				return fmt.Errorf("连接被拒绝，Agent 认证失败 (401 Unauthorized)")
			}
			// 读取错误消息
			errMsg := fmt.Sprintf("HTTP状态码: %d", resp.StatusCode)
			if resp.Body != nil {
				defer resp.Body.Close()
				body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
				if readErr == nil && len(body) > 0 {
					errMsg = string(body)
				}
			}
			return fmt.Errorf("WebSocket 连接失败: %s: %w", errMsg, err)
		}
		return fmt.Errorf("WebSocket 连接失败: %w", err)
	}

	log.Println("WebSocket连接已建立，准备进行身份验证")

	// 设置读取超时
	c.SetReadDeadline(time.Now().Add(ConnectionTimeout))

	// 生成客户端公钥
	publicKey := shared.EncodePublicKey(a.keyPair.PublicKey)

	// 准备主机名
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	// 获取或创建Agent UUID
	agentUUID, err := a.getOrCreateAgentUUID()
	if err != nil {
		log.Printf("获取Agent UUID失败: %v, 将使用临时UUID", err)
		agentUUID = shared.GenerateUUID() // 临时生成一个UUID作为备用
	}

	// 更新Agent ID为UUID
	a.Config.AgentID = agentUUID

	// 构建注册负载
	payload := shared.RegisterPayload{
		Hostname:  hostname,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		PublicKey: publicKey,
		AgentUUID: agentUUID,
		Version:   AgentVersion,
	}

	// 创建并发送注册消息
	regMsg, err := shared.CreateMessage(shared.TypeRegister, a.Config.AgentID, payload)
	if err != nil {
		c.Close()
		return fmt.Errorf("创建注册消息失败: %v", err)
	}

	registrationData, err := json.Marshal(regMsg)
	if err != nil {
		c.Close()
		return fmt.Errorf("序列化注册消息失败: %v", err)
	}

	// 设置写入超时
	c.SetWriteDeadline(time.Now().Add(ConnectionTimeout))

	err = c.WriteMessage(websocket.TextMessage, registrationData)
	if err != nil {
		c.Close()
		return fmt.Errorf("发送注册消息失败: %v", err)
	}

	log.Println("已发送注册信息，等待服务器响应")

	// 等待注册确认
	_, ackData, err := c.ReadMessage()
	if err != nil {
		c.Close()
		return fmt.Errorf("接收注册响应失败: %v", err)
	}

	// 清除超时设置
	c.SetReadDeadline(time.Time{})
	c.SetWriteDeadline(time.Time{})

	// 解析注册确认
	var ackMsg shared.Message
	if err := json.Unmarshal(ackData, &ackMsg); err != nil {
		c.Close()
		return fmt.Errorf("解析注册响应失败: %v", err)
	}

	if ackMsg.Type != shared.TypeRegisterAck {
		c.Close()
		return fmt.Errorf("接收到非预期的消息类型: %s，期望: %s", ackMsg.Type, shared.TypeRegisterAck)
	}

	// 解析确认负载
	var ackPayload shared.RegisterAckPayload
	if err := json.Unmarshal(ackMsg.Payload, &ackPayload); err != nil {
		c.Close()
		return fmt.Errorf("解析确认负载失败: %v", err)
	}

	// 检查注册是否成功
	if !ackPayload.Success {
		c.Close()
		return fmt.Errorf("注册失败: %s", ackPayload.Message)
	}

	// 解码服务器公钥
	serverPubKey, err := shared.DecodePublicKey(ackPayload.ServerPublicKey)
	if err != nil {
		c.Close()
		return fmt.Errorf("解码服务器公钥失败: %v", err)
	}

	// 创建加密通信连接
	secureConn := shared.NewSecureConnection(c, a.keyPair, false)
	secureConn.SetRemotePublicKey(serverPubKey)

	// 保存连接信息
	a.conn = secureConn
	a.isConnected = true
	a.serverPubKey = serverPubKey

	log.Printf("连接已建立，服务器公钥已获取")
	return nil
}

// 重连服务器
func (a *Agent) reconnect() {
	a.connMutex.Lock()
	if a.reconnecting {
		a.connMutex.Unlock()
		return
	}
	a.reconnecting = true

	// 先彻底清理旧连接
	if a.conn != nil {
		log.Printf("清理旧连接")
		a.conn.Close()
		a.conn = nil
	}
	a.isConnected = false
	a.connMutex.Unlock()

	log.Printf("开始使用已配置的通信密钥重连")

	// 使用简单的重连延迟
	baseDelay := 5 * time.Second
	maxDelay := 60 * time.Second
	factor := 1.5
	delay := baseDelay
	maxAttempts := 10

	// 添加总重连时间限制（10分钟）
	startTime := time.Now()
	maxReconnectTime := 10 * time.Minute

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// 检查是否超过了最大重连时间
		if time.Since(startTime) > maxReconnectTime {
			log.Printf("重连时间超过%v，停止重连", maxReconnectTime)
			break
		}

		// 检查是否应该停止重连
		select {
		case <-a.stopChan:
			log.Printf("收到停止信号，终止重连")
			a.connMutex.Lock()
			a.reconnecting = false
			a.connMutex.Unlock()
			return
		default:
			// 继续重连
		}

		log.Printf("尝试重连 #%d，等待 %v...", attempt, delay)

		// 使用带有超时的等待，允许提前退出
		select {
		case <-time.After(delay):
			// 继续执行
		case <-a.stopChan:
			log.Printf("等待期间收到停止信号，终止重连")
			a.connMutex.Lock()
			a.reconnecting = false
			a.connMutex.Unlock()
			return
		}

		// 尝试连接
		log.Printf("执行第 %d 次连接尝试", attempt)
		err := a.Connect()

		// 检查连接是否成功
		a.connMutex.Lock()
		isConnected := a.isConnected && a.conn != nil
		a.connMutex.Unlock()

		if err == nil && isConnected {
			// 快速测试连接
			log.Printf("WebSocket连接已建立，测试连接...")
			connectSuccess := false

			// 进行不超过2次的连接测试
			for testAttempt := 1; testAttempt <= 2; testAttempt++ {
				if a.testConnection() {
					connectSuccess = true
					break
				}

				if testAttempt < 2 {
					log.Printf("连接测试失败，1秒后再次尝试测试")
					time.Sleep(1 * time.Second)
				}
			}

			if connectSuccess {
				log.Printf("重连成功，连接已恢复")

				// 启动所需的服务
				a.wg.Add(1)
				go a.handleMessages()

				if a.reportInterval > 0 {
					a.wg.Add(1)
					go a.startActiveReporting()
				}

				a.wg.Add(1)
				go a.startHeartbeat()

				// 保存配置
				if err := a.saveConfig(a.Config.KeyFile, a.Config.ServerURL, a.Config.SecurityKey); err != nil {
					log.Printf("保存配置失败: %v", err)
				}

				// 重置重连状态
				a.connMutex.Lock()
				a.reconnecting = false
				a.connMutex.Unlock()
				return
			}

			// 连接测试失败，清理连接后继续重试
			log.Printf("连接测试失败，清理连接后继续重试")
			a.connMutex.Lock()
			if a.conn != nil {
				a.conn.Close()
				a.conn = nil
			}
			a.isConnected = false
			a.connMutex.Unlock()
		} else if err != nil {
			log.Printf("连接失败: %v", err)
		}

		// 增加延迟
		delay = time.Duration(float64(delay) * factor)
		if delay > maxDelay {
			delay = maxDelay
		}
	}

	log.Printf("达到最大重试次数或超时，停止重连")
	a.connMutex.Lock()
	a.reconnecting = false
	a.connMutex.Unlock()
}

// 测试连接是否可用
func (a *Agent) testConnection() bool {
	a.connMutex.Lock()
	conn := a.conn
	isConnected := a.isConnected
	a.connMutex.Unlock()

	if !isConnected || conn == nil {
		return false
	}

	log.Printf("开始测试连接是否可用...")

	// 设置更短的超时时间，避免长时间等待
	conn.SetTimeout(3 * time.Second)
	defer conn.SetTimeout(0) // 重置超时

	// 创建测试心跳消息
	testMsg, err := shared.CreateMessage(shared.TypeHeartbeat, a.Config.AgentID, nil)
	if err != nil {
		log.Printf("创建测试心跳消息失败: %v", err)
		return false
	}

	// 同步发送心跳消息
	err = conn.SendEncrypted(testMsg)
	if err != nil {
		log.Printf("发送测试心跳失败: %v", err)
		return false
	}

	log.Printf("测试心跳消息已发送，尝试读取响应(3秒超时)...")

	// 直接尝试读取响应，这里利用了SetTimeout设置的超时
	resp, err := conn.ReadEncrypted()
	if err != nil {
		log.Printf("接收测试心跳响应失败: %v", err)
		return false
	}

	// 验证响应类型
	if resp.Type != shared.TypeHeartbeatAck {
		log.Printf("收到非预期的响应类型: %s，期望: %s", resp.Type, shared.TypeHeartbeatAck)
		return false
	}

	log.Printf("连接测试成功，服务器已确认心跳")
	return true
}

// 处理从服务器接收的消息
func (a *Agent) handleMessages() {
	defer a.wg.Done()

	for {
		select {
		case <-a.stopChan:
			return
		default:
			// 获取连接
			a.connMutex.Lock()
			conn := a.conn
			isConnected := a.isConnected
			a.connMutex.Unlock()

			if !isConnected || conn == nil {
				return
			}

			// 读取消息
			msg, err := conn.ReadEncrypted()
			if err != nil {
				log.Printf("读取消息错误: %v", err)
				a.handleDisconnect()
				return
			}

			// 处理消息
			a.processMessage(msg)
		}
	}
}

// 处理接收到的消息
func (a *Agent) processMessage(msg *shared.Message) {
	switch msg.Type {
	case shared.TypeCommand:
		a.handleCommand(msg)

	case shared.TypeHeartbeatAck:
		// 心跳确认，不需要特殊处理

	case shared.TypeReportAck:
		// 处理上报确认
		a.handleReportAck(msg)

	case "security_key_update_proposal":
		// 处理安全密钥更新提议
		a.handleSecurityKeyUpdateProposal(msg)

	case "security_key_update":
		// 处理安全密钥更新
		a.handleSecurityKeyUpdate(msg)

	case shared.TypeReportRequest:
		// 处理服务器请求上报
		a.handleReportRequest(msg)

	case shared.TypePassiveReport:
		// 处理被动上报请求
		a.handlePassiveReportRequest(msg)

	default:
		log.Printf("未知消息类型: %s", msg.Type)
	}
}

// 处理命令消息
func (a *Agent) handleCommand(msg *shared.Message) {
	// 解析命令负载
	var cmdPayload shared.CommandPayload
	if err := json.Unmarshal(msg.Payload, &cmdPayload); err != nil {
		log.Printf("解析命令负载失败: %v", err)
		return
	}

	log.Printf("收到命令，ID: %s, 类型: %s", cmdPayload.CommandID, cmdPayload.Type)

	// 检查命令ID格式
	if !strings.HasPrefix(cmdPayload.CommandID, "CMD") && strings.Contains(cmdPayload.CommandID, "-") {
		// 老版本格式的命令ID，为兼容性考虑继续处理
		log.Printf("警告: 收到旧格式的命令ID: %s", cmdPayload.CommandID)
	}

	// 发送命令确认
	ackMsg, _ := shared.CreateMessage(shared.TypeCommandAck, a.Config.AgentID, map[string]string{
		"command_id": cmdPayload.CommandID,
		"status":     "received",
	})

	// 发送确认
	if err := a.conn.SendEncrypted(ackMsg); err != nil {
		log.Printf("发送命令确认失败: %v", err)
	}

	// 根据命令类型处理命令
	var output string
	var errMsg string
	var exitCode int
	var success bool

	switch cmdPayload.Type {
	case "exec":
		output, errMsg, exitCode = a.executeArgumentCommand(cmdPayload.Content, cmdPayload.Timeout)
		success = exitCode == 0 && errMsg == ""
	default:
		if shared.IsShardAction(shared.ShardAction(cmdPayload.Type)) {
			result, operationErr := a.executeShardOperation(cmdPayload.Type, cmdPayload.ShardOperation, cmdPayload.Timeout)
			encoded, encodeErr := json.Marshal(result)
			if encodeErr != nil {
				errMsg = encodeErr.Error()
			} else {
				output = string(encoded)
			}
			if operationErr != nil {
				errMsg = operationErr.Error()
			}
			success = operationErr == nil && encodeErr == nil
			if !success {
				exitCode = 1
			}
		} else if shared.IsRuntimeAction(shared.RuntimeAction(cmdPayload.Type)) {
			result, operationErr := a.executeRuntimeOperation(cmdPayload.Type, cmdPayload.RuntimeOperation, cmdPayload.Timeout)
			encoded, encodeErr := json.Marshal(result)
			if encodeErr != nil {
				errMsg = encodeErr.Error()
			} else {
				output = string(encoded)
			}
			if operationErr != nil {
				errMsg = operationErr.Error()
			}
			success = operationErr == nil && encodeErr == nil
			if !success {
				exitCode = 1
			}
		} else {
			errMsg = fmt.Sprintf("不支持的命令类型: %s", cmdPayload.Type)
			success = false
			exitCode = 1
		}
	}

	// 创建命令响应
	respPayload := shared.CommandResponsePayload{
		CommandID: cmdPayload.CommandID,
		Success:   success,
		Output:    output,
		ErrorMsg:  errMsg,
		ExitCode:  exitCode,
	}

	respMsg, err := shared.CreateMessage(shared.TypeCommandResp, a.Config.AgentID, respPayload)
	if err != nil {
		log.Printf("创建命令响应失败: %v", err)
		return
	}

	// 发送响应
	if err := a.conn.SendEncrypted(respMsg); err != nil {
		log.Printf("发送命令响应失败: %v", err)
	}
}

func (a *Agent) executeArgumentCommand(content string, timeout int) (string, string, int) {
	var request struct {
		Program   string   `json:"program"`
		Arguments []string `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(content), &request); err != nil || strings.TrimSpace(request.Program) == "" {
		return "", "参数数组命令格式无效", 1
	}
	if !allowedAgentCommand(request.Program, request.Arguments) {
		return "", "命令不在 Agent 白名单中", 1
	}
	return executeLocalCommand(request.Program, request.Arguments, timeout)
}

func allowedAgentCommand(program string, arguments []string) bool {
	program = strings.TrimSpace(program)
	if program == "df" && len(arguments) == 1 && arguments[0] == "-Pk" {
		return true
	}
	if strings.EqualFold(program, "powershell.exe") && len(arguments) == 4 {
		return arguments[0] == "-NoProfile" &&
			arguments[1] == "-NonInteractive" &&
			arguments[2] == "-Command" &&
			arguments[3] == "Get-PSDrive -PSProvider FileSystem | Select-Object Name,Used,Free"
	}
	return false
}

type cappedOutput struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func newCappedOutput(limit int) *cappedOutput { return &cappedOutput{remaining: limit} }

func (output *cappedOutput) Write(data []byte) (int, error) {
	originalLength := len(data)
	if len(data) > output.remaining {
		data = data[:output.remaining]
		output.truncated = true
	}
	if len(data) > 0 {
		_, _ = output.buffer.Write(data)
		output.remaining -= len(data)
	}
	return originalLength, nil
}

func (output *cappedOutput) String() string {
	if output.truncated {
		return output.buffer.String() + "\n[output truncated]"
	}
	return output.buffer.String()
}

func (output *cappedOutput) Len() int { return output.buffer.Len() }

func executeLocalCommand(program string, arguments []string, timeout int) (string, string, int) {
	if timeout < 5 || timeout > 300 {
		return "", "命令超时必须在 5 到 300 秒之间", 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, arguments...)
	outBuf := newCappedOutput(CommandOutputLimit)
	errBuf := newCappedOutput(CommandOutputLimit)
	cmd.Stdout, cmd.Stderr = outBuf, errBuf
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		exitCode = 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		if errBuf.Len() == 0 {
			_, _ = errBuf.Write([]byte(err.Error()))
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		_, _ = errBuf.Write([]byte("命令执行超时"))
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// 开始主动上报数据
func (a *Agent) startActiveReporting() {
	defer a.wg.Done()

	ticker := time.NewTicker(a.reportInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.stopChan:
			return
		case <-ticker.C:
			a.sendActiveReport()
		}
	}
}

// 发送主动上报数据
func (a *Agent) sendActiveReport() {
	// 收集系统信息
	data := a.collectSystemInfo()

	// 添加IP地址信息
	if ips, err := a.getIPAddresses(); err == nil && len(ips) > 0 {
		data["ip_addresses"] = ips
	}

	// 添加进程信息（限制数量）
	if a.reportInterval > time.Minute { // 如果上报间隔较长，加入更详细的进程信息
		procData := a.collectProcessList()
		if procData != nil {
			for k, v := range procData {
				data[k] = v
			}
		}
	}

	// 确保UUID存在于每次上报的数据中
	agentUUID, err := a.getOrCreateAgentUUID()
	if err == nil {
		data["agent_uuid"] = agentUUID
	}

	reportPayload := shared.ReportDataPayload{
		ReportID:   shared.GenerateUUID(),
		ReportType: "system_info",
		Data:       data,
	}

	// 创建上报消息
	msg, err := shared.CreateMessage(shared.TypeActiveReport, a.Config.AgentID, reportPayload)
	if err != nil {
		log.Printf("创建上报消息失败: %v", err)
		return
	}

	// 发送上报
	a.connMutex.Lock()
	defer a.connMutex.Unlock()

	if !a.isConnected || a.conn == nil {
		log.Println("未连接到服务器，无法发送上报")
		return
	}

	if err := a.conn.SendEncrypted(msg); err != nil {
		log.Printf("发送主动上报失败: %v", err)
	} else {
		log.Printf("已发送系统信息主动上报，包含 %d 项数据", len(data))
	}
}

// 处理被动上报请求
func (a *Agent) handlePassiveReportRequest(msg *shared.Message) {
	// 解析请求负载
	var requestPayload struct {
		ReportType string                 `json:"report_type"`
		Params     map[string]interface{} `json:"params"`
	}
	if err := json.Unmarshal(msg.Payload, &requestPayload); err != nil {
		log.Printf("解析被动上报请求失败: %v", err)
		return
	}

	log.Printf("收到被动上报请求: %s, 参数: %v", requestPayload.ReportType, requestPayload.Params)

	// 收集数据
	var data map[string]interface{}
	switch requestPayload.ReportType {
	case "system_info":
		data = a.collectSystemInfo()

		// 添加IP地址信息
		if ips, err := a.getIPAddresses(); err == nil && len(ips) > 0 {
			data["ip_addresses"] = ips
		}

		// 添加UUID信息
		agentUUID, err := a.getOrCreateAgentUUID()
		if err == nil {
			data["agent_uuid"] = agentUUID
		}

	case "process_list":
		data = a.collectProcessList()

		// 添加UUID信息
		agentUUID, err := a.getOrCreateAgentUUID()
		if err == nil {
			data["agent_uuid"] = agentUUID
		}

	case "dst_runtime_inventory":
		encoded, encodeErr := json.Marshal(requestPayload.Params)
		var request shared.RuntimeInventoryRequest
		if encodeErr != nil {
			data = map[string]interface{}{"error": "DST 运行时清单参数无效"}
			break
		}
		if decodeErr := json.Unmarshal(encoded, &request); decodeErr != nil {
			data = map[string]interface{}{"error": "DST 运行时清单参数无效"}
			break
		}
		inventoryContext, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		inventory, inventoryErr := a.collectRuntimeInventory(inventoryContext, request)
		cancel()
		if inventoryErr != nil {
			data = map[string]interface{}{"error": inventoryErr.Error()}
			break
		}
		data = map[string]interface{}{"inventory": inventory}

	case "custom":
		data = map[string]interface{}{
			"error":   "自定义命令上报已禁用，请使用白名单领域动作",
			"success": false,
		}

	default:
		data = map[string]interface{}{
			"error": fmt.Sprintf("不支持的上报类型: %s", requestPayload.ReportType),
		}
	}

	// 创建上报消息
	reportPayload := shared.ReportDataPayload{
		ReportID:   shared.GenerateUUID(),
		ReportType: requestPayload.ReportType,
		Data:       data,
	}

	// 创建上报消息
	respMsg, err := shared.CreateMessage(shared.TypePassiveReport, a.Config.AgentID, reportPayload)
	if err != nil {
		log.Printf("创建被动上报响应失败: %v", err)
		return
	}

	// 发送响应
	if err := a.conn.SendEncrypted(respMsg); err != nil {
		log.Printf("发送被动上报响应失败: %v", err)
	} else {
		log.Printf("已发送被动上报响应: %s, 包含 %d 项数据", requestPayload.ReportType, len(data))
	}
}

// 收集系统信息
func (a *Agent) collectSystemInfo() map[string]interface{} {
	cpuInfo, memoryInfo := collectHostResources()
	capabilities := []string{
		"system.report", "command.exec", "disk.inspect",
		"runtime.inventory.read", "runtime.processes.read", "runtime.capacity.read",
	}
	if len(a.Config.RuntimeInstallations) > 0 && runtime.GOOS != "windows" {
		capabilities = append(capabilities,
			"shard.control.v1", "runtime.driver.v1", "runtime.console.v1", "runtime.logs.v1", "runtime.artifacts.v1", "runtime.migration.v1",
		)
		for _, installation := range a.Config.RuntimeInstallations {
			if installation.Driver == "container" {
				capabilities = append(capabilities, "runtime.container.v1")
				break
			}
		}
	}
	info := map[string]interface{}{
		"hostname":      "unknown",
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
		"agent_version": AgentVersion,
		"cpu_count":     cpuInfo.LogicalProcessors,
		"cpu":           cpuInfo,
		"capabilities":  capabilities,
		"timestamp":     time.Now().Unix(),
	}

	hostname, err := os.Hostname()
	if err == nil {
		info["hostname"] = hostname
	}

	// 生成持久化的唯一标识符
	agentUUID, err := a.getOrCreateAgentUUID()
	if err == nil {
		info["agent_uuid"] = agentUUID
	}

	// 添加更多系统信息
	info["go_version"] = runtime.Version()
	info["go_root"] = runtime.GOROOT()

	// 宿主机内存和 Agent 自身内存分开上报，避免把 Go 堆误认为整机占用。
	info["memory"] = map[string]interface{}{
		"total": memoryInfo.TotalBytes, "used": memoryInfo.UsedBytes, "available": memoryInfo.AvailableBytes,
	}
	var memStat runtime.MemStats
	runtime.ReadMemStats(&memStat)
	info["agent_memory"] = map[string]interface{}{
		"allocated":       memStat.Alloc,
		"total_allocated": memStat.TotalAlloc,
		"system":          memStat.Sys,
		"heap_allocated":  memStat.HeapAlloc,
		"heap_system":     memStat.HeapSys,
	}

	// 获取当前目录
	currentDir, err := os.Getwd()
	if err == nil {
		info["current_dir"] = currentDir
	}

	// 获取当前用户
	currentUser := "unknown"
	if usr, err := user.Current(); err == nil {
		currentUser = usr.Username
		info["user"] = map[string]string{
			"name": usr.Username,
			"uid":  usr.Uid,
			"gid":  usr.Gid,
			"home": usr.HomeDir,
		}
	} else {
		info["user"] = currentUser
	}

	// 获取运行时间
	uptime := time.Since(startTime).Seconds()
	info["uptime_seconds"] = uptime

	// 获取当前进程PID
	info["pid"] = os.Getpid()

	processContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	dstProcesses := collectDSTProcesses(processContext)
	cancel()
	info["dst_process_count"] = len(dstProcesses)
	info["dst_processes"] = dstProcesses
	info["runtime_observed_at"] = time.Now().UTC()

	// 获取IP地址信息
	if ips, err := a.getIPAddresses(); err == nil {
		info["ip_addresses"] = ips
	}

	return info
}

// 获取或创建代理唯一标识符
func (a *Agent) getOrCreateAgentUUID() (string, error) {
	// 尝试从配置文件读取UUID
	configFile := a.Config.KeyFile

	// 确保配置文件存在
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		// 如果配置文件不存在，先创建包含UUID的配置
		uuid := shared.GenerateUUID()
		log.Printf("生成新的Agent UUID: %s", uuid)

		// 确保目录存在
		dir := filepath.Dir(configFile)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return uuid, fmt.Errorf("创建配置目录失败: %v", err)
		}

		// 创建新的配置文件
		cfg := ini.Empty()
		section, _ := cfg.NewSection("agent")
		section.Key("AGENT_UUID").SetValue(uuid)
		if a.Config.SecurityKey != "" {
			section.Key("SECURITY_KEY").SetValue(a.Config.SecurityKey)
		}
		if a.Config.ServerURL != "" {
			section.Key("SERVER_URL").SetValue(a.Config.ServerURL)
		}

		if err := savePrivateINI(configFile, cfg); err != nil {
			return uuid, fmt.Errorf("保存UUID到配置文件失败: %v", err)
		}
		return uuid, nil
	}

	// 从现有配置文件中读取UUID
	cfg, err := ini.Load(configFile)
	if err != nil {
		// 如果读取配置失败，生成新的UUID并返回，但不保存
		uuid := shared.GenerateUUID()
		log.Printf("读取配置文件失败，生成临时UUID: %s", uuid)
		return uuid, nil
	}
	if err := shared.EnsurePrivateFile(configFile); err != nil {
		return "", err
	}

	// 从[agent]部分读取UUID
	section := cfg.Section("agent")
	if section.HasKey("AGENT_UUID") {
		uuid := section.Key("AGENT_UUID").String()
		if uuid != "" {
			log.Printf("从配置文件加载Agent UUID: %s", uuid)
			return uuid, nil
		}
	}

	// 如果UUID不存在，生成新的并保存
	uuid := shared.GenerateUUID()
	log.Printf("配置文件中未找到UUID，生成新的: %s", uuid)

	// 更新配置文件
	section.Key("AGENT_UUID").SetValue(uuid)
	if err := savePrivateINI(configFile, cfg); err != nil {
		log.Printf("保存UUID到配置文件失败: %v", err)
	}

	return uuid, nil
}

func savePrivateINI(path string, config *ini.File) error {
	var output bytes.Buffer
	if _, err := config.WriteTo(&output); err != nil {
		return fmt.Errorf("序列化配置文件失败: %w", err)
	}
	return shared.WritePrivateFile(path, output.Bytes())
}

// 获取IP地址列表
func (a *Agent) getIPAddresses() ([]string, error) {
	var ips []string

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	for _, iface := range ifaces {
		// 跳过禁用的接口和回环接口
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			// 跳过IPv6地址
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}

			ips = append(ips, ip.String())
		}
	}

	return ips, nil
}

// 收集进程列表
func (a *Agent) collectProcessList() map[string]interface{} {
	processes := []map[string]interface{}{}

	// 记录开始时间
	startTime := time.Now()

	// 根据操作系统获取不同的进程信息
	switch runtime.GOOS {
	case "windows":
		// Windows下获取更详细的进程信息
		output, _, _ := executeLocalCommand("tasklist", []string{"/fo", "csv", "/nh"}, 20)
		lines := strings.Split(strings.TrimSpace(output), "\n")

		for _, line := range lines {
			if line == "" {
				continue
			}

			// CSV格式解析
			parts := strings.Split(line, ",")
			if len(parts) < 2 {
				continue
			}

			// 处理CSV格式 - 去除引号
			name := strings.Trim(parts[0], "\"")
			pid := strings.Trim(parts[1], "\"")

			memUsage := ""
			if len(parts) > 4 {
				memUsage = strings.Trim(parts[4], "\"")
			}

			proc := map[string]interface{}{
				"name": name,
				"pid":  pid,
			}

			if memUsage != "" {
				proc["memory_usage"] = memUsage
			}

			processes = append(processes, proc)
		}

	case "linux", "darwin":
		// Linux/MacOS使用ps命令获取详细进程信息
		arguments := []string{"-eo", "pid,ppid,user,stat,pcpu,pmem,comm", "--sort=-pcpu"}
		if runtime.GOOS == "darwin" {
			arguments = []string{"-Ao", "pid,ppid,user,state,%cpu,%mem,comm"}
		}
		output, _, _ := executeLocalCommand("ps", arguments, 20)
		lines := strings.Split(strings.TrimSpace(output), "\n")

		// 跳过标题行
		if len(lines) > 1 {
			for i := 1; i < len(lines); i++ {
				line := lines[i]
				fields := strings.Fields(line)

				if len(fields) < 7 {
					continue
				}

				proc := map[string]interface{}{
					"pid":       fields[0],
					"ppid":      fields[1],
					"user":      fields[2],
					"state":     fields[3],
					"cpu_usage": fields[4],
					"mem_usage": fields[5],
					"name":      fields[6],
				}

				processes = append(processes, proc)
			}
		}

	default:
		// 其他操作系统使用简单实现
		output, _, _ := executeLocalCommand("ps", []string{"aux"}, 10)
		lines := strings.Split(output, "\n")

		// 跳过标题行
		if len(lines) > 1 {
			for i := 1; i < len(lines); i++ {
				line := lines[i]
				if line == "" {
					continue
				}

				fields := strings.Fields(line)
				if len(fields) < 11 {
					continue
				}

				proc := map[string]interface{}{
					"user":      fields[0],
					"pid":       fields[1],
					"cpu_usage": fields[2],
					"mem_usage": fields[3],
					"vsz":       fields[4],
					"rss":       fields[5],
					"tty":       fields[6],
					"stat":      fields[7],
					"start":     fields[8],
					"time":      fields[9],
					"command":   strings.Join(fields[10:], " "),
				}

				processes = append(processes, proc)
			}
		}
	}

	// 限制进程数量，避免数据过大
	maxProcesses := 50
	if len(processes) > maxProcesses {
		processes = processes[:maxProcesses]
	}

	// 计算收集时间
	elapsedTime := time.Since(startTime).Milliseconds()

	return map[string]interface{}{
		"processes":     processes,
		"count":         len(processes),
		"collection_ms": elapsedTime,
		"timestamp":     time.Now().Unix(),
		"agent_uuid":    a.getAgentUUIDOrEmpty(),
		"process_limit": maxProcesses,
		"os":            runtime.GOOS,
	}
}

// 获取Agent UUID，如果不存在则返回空字符串
func (a *Agent) getAgentUUIDOrEmpty() string {
	uuid, err := a.getOrCreateAgentUUID()
	if err != nil {
		return ""
	}
	return uuid
}

// 加载配置文件，返回服务器地址和密钥
func (a *Agent) loadConfig(configFile string) (string, string, error) {
	// 如果路径中包含conf/app.conf，则从配置文件加载
	if isINIConfigPath(configFile) {
		log.Printf("从配置文件加载配置: %s", configFile)

		// 检查文件是否存在
		if _, err := os.Stat(configFile); os.IsNotExist(err) {
			return "", "", fmt.Errorf("配置文件不存在: %s", configFile)
		}
		if err := shared.EnsurePrivateFile(configFile); err != nil {
			return "", "", err
		}

		// 加载配置
		cfg, err := ini.Load(configFile)
		if err != nil {
			return "", "", fmt.Errorf("读取配置文件失败: %v", err)
		}

		// 从[agent]部分读取配置
		section := cfg.Section("agent")

		// 读取密钥
		var key string
		if section.HasKey("SECURITY_KEY") {
			key = section.Key("SECURITY_KEY").String()
			if key != "" {
				log.Printf("从配置文件成功加载密钥")
			}
		}

		// 读取服务器地址
		var serverURL string
		if section.HasKey("SERVER_URL") {
			serverURL = section.Key("SERVER_URL").String()
			if serverURL != "" {
				log.Printf("从配置文件成功加载服务器地址")
			}
		}

		if key != "" {
			if err := shared.ValidateSecurityKey(key); err != nil {
				return "", "", err
			}
		}
		return serverURL, key, nil
	}

	// 否则使用原来的JSON方式加载
	// 检查文件是否存在
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		return "", "", fmt.Errorf("配置文件不存在: %s", configFile)
	}

	// 读取文件
	if err := shared.EnsurePrivateFile(configFile); err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(configFile)
	if err != nil {
		return "", "", fmt.Errorf("读取配置文件失败: %v", err)
	}

	// 解析JSON
	var config struct {
		Key       string `json:"key"`
		ServerURL string `json:"server_url"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", "", fmt.Errorf("解析配置文件失败: %v", err)
	}

	if config.Key != "" {
		if err := shared.ValidateSecurityKey(config.Key); err != nil {
			return "", "", err
		}
	}
	return config.ServerURL, config.Key, nil
}

// 从文件加载安全密钥
func (a *Agent) loadSecurityKey(keyFile string) (string, error) {
	// 从配置文件加载
	_, key, err := a.loadConfig(keyFile)
	return key, err
}

// 保存安全密钥到文件
func (a *Agent) saveSecurityKey(keyFile, key string) error {
	// 同时保存服务器地址和密钥
	return a.saveConfig(keyFile, a.Config.ServerURL, key)
}

// 保存配置到文件
func (a *Agent) saveConfig(configFile, serverURL, key string) error {
	if key != "" {
		if err := shared.ValidateSecurityKey(key); err != nil {
			return err
		}
	}
	if serverURL != "" {
		normalized, err := normalizedAgentURL(serverURL)
		if err != nil {
			return err
		}
		serverURL = normalized
	}
	// 如果路径中包含conf/app.conf，则保存到配置文件格式
	if isINIConfigPath(configFile) {
		log.Printf("保存配置到配置文件: %s", configFile)

		// 获取当前UUID
		uuid, _ := a.getOrCreateAgentUUID()

		// 检查文件是否存在并检查格式
		_, err := os.Stat(configFile)
		if err != nil {
			if os.IsNotExist(err) {
				// 文件不存在，创建一个新的INI格式配置文件
				cfg := ini.Empty()
				section, _ := cfg.NewSection("agent")
				if key != "" {
					section.Key("SECURITY_KEY").SetValue(key)
				}
				if serverURL != "" {
					section.Key("SERVER_URL").SetValue(serverURL)
				}
				if uuid != "" {
					section.Key("AGENT_UUID").SetValue(uuid)
				}

				if err := savePrivateINI(configFile, cfg); err != nil {
					return fmt.Errorf("创建新配置文件失败: %v", err)
				}
				log.Printf("已创建新的配置文件: %s", configFile)
				return nil
			}
			return fmt.Errorf("检查配置文件状态失败: %v", err)
		}

		// 读取文件前几个字节来判断是JSON还是INI
		f, err := os.Open(configFile)
		if err != nil {
			return fmt.Errorf("无法打开配置文件: %v", err)
		}

		// 读取前100个字节来判断格式
		header := make([]byte, 100)
		_, err = f.Read(header)
		f.Close()
		if err != nil && err != io.EOF {
			return fmt.Errorf("读取配置文件头部失败: %v", err)
		}

		// 判断是否是JSON格式
		isJSON := bytes.HasPrefix(bytes.TrimSpace(header), []byte{'{'})

		if isJSON {
			// 如果是JSON格式，将其转换为INI格式
			log.Printf("检测到JSON格式配置文件，将转换为INI格式")

			// 创建一个新的INI配置
			cfg := ini.Empty()
			section, _ := cfg.NewSection("agent")
			if key != "" {
				section.Key("SECURITY_KEY").SetValue(key)
			}
			if serverURL != "" {
				section.Key("SERVER_URL").SetValue(serverURL)
			}
			if uuid != "" {
				section.Key("AGENT_UUID").SetValue(uuid)
			}

			// 保存配置
			if err := savePrivateINI(configFile, cfg); err != nil {
				return fmt.Errorf("保存INI配置文件失败: %v", err)
			}

			log.Printf("成功将JSON格式转换为INI格式并保存配置")
			return nil
		}

		// 如果是INI格式，正常处理
		log.Printf("正在更新INI格式配置文件中的配置")

		// 加载现有配置
		cfg, err := ini.Load(configFile)
		if err != nil {
			return fmt.Errorf("读取配置文件失败: %v", err)
		}

		// 设置值到[agent]部分
		section, err := cfg.GetSection("agent")
		if err != nil {
			// 如果节不存在，创建新节
			section, err = cfg.NewSection("agent")
			if err != nil {
				return fmt.Errorf("创建配置节失败: %v", err)
			}
		}

		if key != "" {
			section.Key("SECURITY_KEY").SetValue(key)
		}
		if serverURL != "" {
			section.Key("SERVER_URL").SetValue(serverURL)
		}
		if uuid != "" {
			section.Key("AGENT_UUID").SetValue(uuid)
		}

		// 保存配置
		if err := savePrivateINI(configFile, cfg); err != nil {
			return fmt.Errorf("保存配置文件失败: %v", err)
		}

		log.Printf("配置已保存到文件: %s [agent]段", configFile)
		return nil
	}

	// 否则使用原来的JSON方式保存
	config := struct {
		Key       string `json:"key"`
		ServerURL string `json:"server_url"`
	}{
		Key:       key,
		ServerURL: serverURL,
	}

	// 序列化为JSON
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %v", err)
	}

	// 确保目录存在
	dir := filepath.Dir(configFile)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建目录失败: %v", err)
		}
	}

	// 写入文件
	if err := shared.WritePrivateFile(configFile, data); err != nil {
		return fmt.Errorf("写入配置文件失败: %v", err)
	}

	log.Printf("配置已保存到文件: %s", configFile)
	return nil
}

// 处理密钥更新消息
func (a *Agent) handleSecurityKeyUpdate(msg *shared.Message) {
	// 解析密钥更新消息
	var keyUpdatePayload struct {
		NewKey string `json:"new_key"`
	}

	if err := json.Unmarshal(msg.Payload, &keyUpdatePayload); err != nil {
		log.Printf("解析密钥更新消息失败: %v", err)
		return
	}

	if err := shared.ValidateSecurityKey(keyUpdatePayload.NewKey); err != nil {
		log.Printf("收到无效的密钥更新")
		return
	}

	log.Printf("收到服务器密钥更新")

	// 更新当前使用的密钥
	a.Config.SecurityKey = keyUpdatePayload.NewKey

	// 保存到文件
	if err := a.saveConfig(a.Config.KeyFile, a.Config.ServerURL, keyUpdatePayload.NewKey); err != nil {
		log.Printf("保存更新的配置失败: %v", err)
	} else {
		log.Printf("已成功更新并保存配置")
	}

	// 发送确认消息
	ackPayload := struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}{
		Success: true,
		Message: "密钥已成功更新",
	}

	ackMsg, err := shared.CreateMessage("security_key_update_ack", a.Config.AgentID, ackPayload)
	if err != nil {
		log.Printf("创建密钥更新确认消息失败: %v", err)
	} else {
		a.connMutex.Lock()
		if a.conn != nil && a.isConnected {
			if sendErr := a.conn.SendEncrypted(ackMsg); sendErr != nil {
				log.Printf("发送密钥更新确认失败: %v", sendErr)
			}
		}
		a.connMutex.Unlock()
	}

	// 密钥已更新，等待短暂时间后主动断开并重新连接
	log.Printf("密钥已更新，将在2秒后断开连接并使用新密钥重连")

	// 启动一个goroutine在短时间后重连
	go func() {
		// 等待2秒，确保服务器接收到确认消息
		time.Sleep(2 * time.Second)

		// 主动断开连接
		a.connMutex.Lock()
		if a.conn != nil {
			log.Printf("主动断开与服务器的连接，准备使用新密钥重连")
			a.conn.Close()
			a.conn = nil
		}
		a.isConnected = false
		a.connMutex.Unlock()

		// 延迟一点时间再重连，确保服务端也关闭了连接
		time.Sleep(1 * time.Second)

		// 尝试重新连接
		a.reconnect()
	}()
}

// 处理断开连接
func (a *Agent) handleDisconnect() {
	a.connMutex.Lock()
	a.isConnected = false
	if a.conn != nil {
		a.conn.Close()
		a.conn = nil
	}
	a.connMutex.Unlock()

	// 尝试重新连接
	go a.reconnect()
}

// 启动心跳机制
func (a *Agent) startHeartbeat() {
	defer a.wg.Done()

	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.stopChan:
			return
		case <-ticker.C:
			a.sendHeartbeat()
		}
	}
}

// 发送心跳
func (a *Agent) sendHeartbeat() {
	a.connMutex.Lock()
	isConnected := a.isConnected
	conn := a.conn
	a.connMutex.Unlock()

	if !isConnected || conn == nil {
		return
	}

	// 创建心跳消息
	msg, err := shared.CreateMessage(shared.TypeHeartbeat, a.Config.AgentID, nil)
	if err != nil {
		log.Printf("创建心跳消息失败: %v", err)
		return
	}

	// 发送心跳
	if err := conn.SendEncrypted(msg); err != nil {
		log.Printf("发送心跳失败: %v", err)
		a.handleDisconnect()
	}
}

// 处理密钥更新提议
func (a *Agent) handleSecurityKeyUpdateProposal(msg *shared.Message) {
	// 解析密钥更新提议
	var keyUpdateProposalPayload struct {
		NewKey    string `json:"new_key"`
		SessionID string `json:"session_id"`
	}

	if err := json.Unmarshal(msg.Payload, &keyUpdateProposalPayload); err != nil {
		log.Printf("解析密钥更新提议失败: %v", err)
		return
	}

	if err := shared.ValidateSecurityKey(keyUpdateProposalPayload.NewKey); err != nil {
		log.Printf("收到无效的密钥更新提议")
		return
	}

	if keyUpdateProposalPayload.SessionID == "" {
		log.Printf("收到的密钥更新提议缺少会话ID")
		return
	}

	log.Printf("收到服务器密钥更新提议，会话ID: %s", keyUpdateProposalPayload.SessionID)

	// 设置准备更新的等待时间（秒）
	const readyInSeconds = 3

	// 发送准备好更新密钥的消息
	readyPayload := struct {
		AgentID   string `json:"agent_id"`
		NewKey    string `json:"new_key"`
		ReadyIn   int    `json:"ready_in"`
		SessionID string `json:"session_id"`
	}{
		AgentID:   a.Config.AgentID,
		NewKey:    keyUpdateProposalPayload.NewKey,
		ReadyIn:   readyInSeconds,
		SessionID: keyUpdateProposalPayload.SessionID,
	}

	readyMsg, err := shared.CreateMessage(TypeSecurityKeyUpdateReady, a.Config.AgentID, readyPayload)
	if err != nil {
		log.Printf("创建密钥更新准备消息失败: %v", err)
		return
	}

	// 发送准备好的消息
	a.connMutex.Lock()
	if a.conn != nil && a.isConnected {
		if sendErr := a.conn.SendEncrypted(readyMsg); sendErr != nil {
			log.Printf("发送密钥更新准备消息失败: %v", sendErr)
			a.connMutex.Unlock()
			return
		}
	} else {
		a.connMutex.Unlock()
		log.Printf("无法发送密钥更新准备消息：连接不可用")
		return
	}
	a.connMutex.Unlock()

	log.Printf("已通知服务器准备在%d秒后更新密钥", readyInSeconds)

	// 在设定的时间后更新本地密钥并断开连接
	go func(newKey string) {
		log.Printf("将在%d秒后应用新密钥...", readyInSeconds)
		time.Sleep(time.Duration(readyInSeconds) * time.Second)

		// 保存旧密钥以便恢复
		oldKey := a.Config.SecurityKey
		log.Printf("开始应用新的通信密钥")

		// 保存新密钥
		a.Config.SecurityKey = newKey
		if err := a.saveConfig(a.Config.KeyFile, a.Config.ServerURL, newKey); err != nil {
			log.Printf("保存新配置失败: %v, 将恢复使用旧密钥", err)
			a.Config.SecurityKey = oldKey
			return
		}

		// 验证密钥是否正确保存
		_, savedKey, err := a.loadConfig(a.Config.KeyFile)
		if err != nil || savedKey != newKey {
			log.Printf("警告：通信密钥保存后验证失败")
			if savedKey != newKey {
				log.Printf("密钥验证失败，将恢复使用旧密钥")
				a.Config.SecurityKey = oldKey
				return
			}
		} else {
			log.Printf("已成功保存和验证新密钥")
		}

		// 确保不存在正在进行的重连
		a.connMutex.Lock()
		alreadyReconnecting := a.reconnecting
		a.connMutex.Unlock()

		if alreadyReconnecting {
			log.Printf("已有重连过程在进行中，先终止现有重连")
			// 等待一段时间，让现有重连过程可能完成或超时
			time.Sleep(2 * time.Second)
		}

		// 彻底关闭现有连接，确保断开
		a.connMutex.Lock()
		if a.conn != nil {
			log.Printf("断开连接以应用新密钥")
			a.conn.Close()
			a.conn = nil
		}
		a.isConnected = false
		a.reconnecting = false // 重置重连状态，确保可以启动新的重连
		a.connMutex.Unlock()

		// 等待一定时间确保服务器也已更新密钥
		time.Sleep(2 * time.Second)

		// 直接开始新的连接尝试
		log.Printf("开始使用新密钥连接")
		err = a.Connect()
		if err != nil {
			log.Printf("首次连接尝试失败: %v，将启动重连流程", err)
			// 启动后台重连
			go a.reconnect()
		} else {
			// 连接已建立，验证是否真正可用
			a.connMutex.Lock()
			isReallyConnected := a.isConnected && a.conn != nil
			a.connMutex.Unlock()

			if isReallyConnected {
				log.Printf("使用新密钥连接成功")
				// 启动必要的处理程序
				a.wg.Add(1)
				go a.handleMessages()

				if a.reportInterval > 0 {
					a.wg.Add(1)
					go a.startActiveReporting()
				}

				a.wg.Add(1)
				go a.startHeartbeat()
			} else {
				log.Printf("连接看似建立但可能不完整，启动重连流程")
				go a.reconnect()
			}
		}
	}(keyUpdateProposalPayload.NewKey)
}

// 处理上报确认消息
func (a *Agent) handleReportAck(msg *shared.Message) {
	var ackPayload struct {
		ReportID string `json:"report_id"`
		Status   string `json:"status"`
	}

	if err := json.Unmarshal(msg.Payload, &ackPayload); err != nil {
		log.Printf("解析上报确认消息失败: %v", err)
		return
	}

	log.Printf("服务器已确认接收上报，报告ID: %s, 状态: %s", ackPayload.ReportID, ackPayload.Status)
}

// 处理服务器请求上报
func (a *Agent) handleReportRequest(msg *shared.Message) {
	// 解析请求负载
	var requestPayload struct {
		ReportType string                 `json:"report_type"`
		Params     map[string]interface{} `json:"params"`
	}
	if err := json.Unmarshal(msg.Payload, &requestPayload); err != nil {
		log.Printf("解析上报请求失败: %v", err)
		return
	}

	log.Printf("收到服务器上报请求: %s", requestPayload.ReportType)

	// 收集数据
	var data map[string]interface{}
	switch requestPayload.ReportType {
	case "system_info":
		data = a.collectSystemInfo()

		// 添加IP地址信息
		if ips, err := a.getIPAddresses(); err == nil && len(ips) > 0 {
			data["ip_addresses"] = ips
		}

		// 添加UUID信息
		agentUUID, err := a.getOrCreateAgentUUID()
		if err == nil {
			data["agent_uuid"] = agentUUID
		}

	case "process_list":
		data = a.collectProcessList()

		// 添加UUID信息
		agentUUID, err := a.getOrCreateAgentUUID()
		if err == nil {
			data["agent_uuid"] = agentUUID
		}

	default:
		log.Printf("不支持的上报请求类型: %s", requestPayload.ReportType)
		return
	}

	// 创建上报响应
	reportPayload := shared.ReportDataPayload{
		ReportID:   shared.GenerateUUID(),
		ReportType: requestPayload.ReportType,
		Data:       data,
	}

	// 创建响应消息
	respMsg, err := shared.CreateMessage(shared.TypePassiveReport, a.Config.AgentID, reportPayload)
	if err != nil {
		log.Printf("创建上报响应失败: %v", err)
		return
	}

	// 发送响应
	if err := a.conn.SendEncrypted(respMsg); err != nil {
		log.Printf("发送上报响应失败: %v", err)
	} else {
		log.Printf("已响应服务器上报请求: %s, 包含 %d 项数据", requestPayload.ReportType, len(data))
	}
}
