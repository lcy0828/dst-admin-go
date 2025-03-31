package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"dont/shared"
	"github.com/gorilla/websocket"
	"github.com/go-ini/ini"
	"bytes"
	"io"
	"errors"
)

// 常量
const (
	// 心跳间隔
	HeartbeatInterval = 30 * time.Second
	// 重连间隔
	ReconnectInterval = 5 * time.Second
	// 连接超时
	ConnectionTimeout = 10 * time.Second
	// 密钥更新消息类型
	TypeSecurityKeyUpdate = "security_key_update"
	// 密钥更新提议类型
	TypeSecurityKeyUpdateProposal = "security_key_update_proposal"
	// 密钥更新准备类型
	TypeSecurityKeyUpdateReady = "security_key_update_ready"
)

// Agent 表示一个代理实例
type Agent struct {
	Config         *Config
	ConfigPath     string               // 配置文件路径
	keyPair        *shared.KeyPair
	serverPubKey   [32]byte
	conn           *shared.SecureConnection
	isConnected    bool
	connMutex      sync.Mutex
	reconnecting   bool
	stopChan       chan struct{}
	wg             sync.WaitGroup
	reportInterval time.Duration
	reportMutex    sync.Mutex
	keyManager     *shared.KeyManager  // 添加密钥管理器
	reconnectMutex sync.Mutex
	isReconnecting bool
	OnReconnected  func()              // 重连成功后的回调函数
}

// Config 代理配置
type Config struct {
	ServerURL      string        // 服务器WebSocket URL
	ServerAddr     string        // 服务器地址（包含端口）
	AgentID        string        // 代理唯一标识
	AgentName      string        // 代理名称
	AgentInfo      string        // 代理信息
	ConfigPath     string        // 配置文件路径
	ReportInterval time.Duration // 主动上报间隔
	SecurityKey    string        // 通信安全密钥
	KeyFile        string        // 密钥存储文件路径
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
		log.Printf("未指定密钥文件，使用默认路径: %s", config.KeyFile)
		
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
	}

	// 尝试加载持久化的密钥
	if config.SecurityKey == "" {
		key, err := agent.loadSecurityKey(config.KeyFile)
		if err == nil && key != "" {
			log.Printf("已从文件加载密钥: %s", config.KeyFile)
			config.SecurityKey = key
		}
	}

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
		// 连接成功，保存当前使用的密钥
		if a.Config.SecurityKey != "" {
			err := a.saveSecurityKey(a.Config.KeyFile, a.Config.SecurityKey)
			if err != nil {
				log.Printf("警告: 无法保存密钥到文件: %v", err)
			} else {
				log.Printf("密钥已成功保存到: %s", a.Config.KeyFile)
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

	log.Printf("Agent已启动，ID: %s, 连接到服务器: %s", a.Config.AgentID, a.Config.ServerURL)
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
	// 判断是否已连接
	a.connMutex.Lock()
	if a.isConnected {
		a.connMutex.Unlock()
		return errors.New("已经处于连接状态")
	}
	a.connMutex.Unlock()
	
	// 使用共享的连接方法建立连接
	secureConn, err := a.connect()
	if err != nil {
		return err
	}
	
	// 设置连接状态
	a.connMutex.Lock()
	a.conn = secureConn
	a.isConnected = true
	a.connMutex.Unlock()
	
	// 启动消息处理
	a.wg.Add(1)
	go a.handleMessages()
	
	// 如果配置了主动上报，启动上报
	if a.reportInterval > 0 {
		a.reportMutex.Lock()
		a.wg.Add(1)
		go a.startActiveReporting()
		a.reportMutex.Unlock()
	}
	
	// 启动心跳
	a.wg.Add(1)
	go a.startHeartbeat()
	
	log.Printf("连接成功建立，所有后台服务已启动")
	return nil
}

// 重连服务器
func (a *Agent) reconnect() {
	// 使用互斥锁防止并发重连
	a.reconnectMutex.Lock()
	defer a.reconnectMutex.Unlock()
	
	// 检查是否已经正在进行重连
	if a.isReconnecting {
		log.Println("已有重连过程正在进行，跳过此次重连请求")
		return
	}
	
	// 标记正在重连
	a.isReconnecting = true
	defer func() {
		a.isReconnecting = false
	}()
	
	// 关闭现有连接
	a.connMutex.Lock()
	if a.conn != nil {
		log.Println("关闭现有连接以准备重连")
		a.conn.Close()
		a.conn = nil
	}
	a.isConnected = false
	a.connMutex.Unlock()
	
	// 指数退避重试策略
	baseDelay := 5 * time.Second
	maxDelay := 5 * time.Minute
	maxRetries := 10
	
	var retryCount int
	for retryCount < maxRetries {
		// 计算本次重试的延迟时间（指数增长+随机抖动）
		delay := time.Duration(float64(baseDelay) * math.Pow(1.5, float64(retryCount)))
		if delay > maxDelay {
			delay = maxDelay
		}
		// 添加0-1秒的随机抖动
		jitter := time.Duration(rand.Int63n(int64(time.Second)))
		delay += jitter
		
		log.Printf("尝试重连 (%d/%d)，等待 %v 后开始...", retryCount+1, maxRetries, delay)
		time.Sleep(delay)
		
		// 重新加载配置，检查密钥是否已更新
		config, err := LoadConfig(a.ConfigPath)
		if err != nil {
			log.Printf("重连时加载配置失败: %v", err)
		} else if config.SecurityKey != a.Config.SecurityKey {
			log.Printf("检测到新的安全密钥: %v (旧密钥: %v)，使用新密钥重连", 
				config.SecurityKey, a.Config.SecurityKey)
			a.Config.SecurityKey = config.SecurityKey
		}
		
		// 尝试连接
		log.Printf("使用密钥 %s 尝试连接服务器 %s", a.Config.SecurityKey, a.Config.ServerAddr)
		
		// 先创建一个临时连接变量，确保连接成功后再赋值给 a.conn
		tempConn, err := a.connect()
		if err != nil {
			log.Printf("重连失败 (%d/%d): %v", retryCount+1, maxRetries, err)
			retryCount++
			continue
		}
		
		// 连接成功，我们需要确保连接有效
		a.connMutex.Lock()
		a.conn = tempConn
		a.isConnected = true
		a.connMutex.Unlock()
		
		// 验证连接是否真的可用
		if a.testConnection() {
			log.Printf("重连成功！连接已验证可用")
			
			// 成功连接后，开启心跳线程
			go a.startHeartbeat()
			
			// 如果有回调，通知连接已恢复
			if a.OnReconnected != nil {
				go a.OnReconnected()
			}
			
			return
		} else {
			log.Printf("重连似乎成功但连接测试失败，将继续尝试")
			a.connMutex.Lock()
			if a.conn != nil {
				a.conn.Close()
				a.conn = nil
			}
			a.isConnected = false
			a.connMutex.Unlock()
		}
		
		retryCount++
	}
	
	log.Printf("达到最大重试次数 (%d)，重连失败", maxRetries)
}

// connect 建立到服务器的连接（不包含重连逻辑）
func (a *Agent) connect() (*shared.SecureConnection, error) {
	log.Printf("正在连接到服务器 %s, 使用密钥: %s", a.Config.ServerAddr, a.Config.SecurityKey)
	
	// 检查参数
	if a.Config.ServerAddr == "" {
		return nil, errors.New("未设置服务器地址")
	}
	if a.Config.SecurityKey == "" {
		return nil, errors.New("未设置安全密钥")
	}
	
	// 创建URL，添加时间戳和编码后的安全密钥，防止缓存和特殊字符问题
	serverURL := a.Config.ServerAddr
	if !strings.HasPrefix(serverURL, "ws://") && !strings.HasPrefix(serverURL, "wss://") {
		serverURL = "ws://" + serverURL
	}
	
	// URL编码安全密钥
	encodedKey := url.QueryEscape(a.Config.SecurityKey)
	
	// 添加时间戳和密钥参数
	separator := "?"
	if strings.Contains(serverURL, "?") {
		separator = "&"
	}
	serverURL = fmt.Sprintf("%s%skey=%s&ts=%d", serverURL, separator, encodedKey, time.Now().Unix())
	
	log.Printf("连接URL: %s", serverURL)
	
	// 设置连接超时
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	
	// 设置WebSocket自定义的请求头
	header := http.Header{}
	header.Add("X-Agent-ID", a.Config.AgentID)
	
	// 建立WebSocket连接
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		EnableCompression: true,
	}
	
	conn, resp, err := dialer.DialContext(ctx, serverURL, header)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("WebSocket连接失败，HTTP状态: %d, 错误: %v", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("WebSocket连接失败: %v", err)
	}
	
	// 设置自动ping/pong以保持连接活跃
	conn.SetPingHandler(func(data string) error {
		log.Printf("收到服务器ping: %s", data)
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	
	conn.SetPongHandler(func(data string) error {
		log.Printf("收到服务器pong: %s", data)
		return nil
	})
	
	// 生成密钥对
	keyPair, err := shared.GenerateKeyPair()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("生成密钥对失败: %v", err)
	}
	
	// 创建安全连接
	secureConn := shared.NewSecureConnection(conn, keyPair, false)
	
	// 发送公钥
	if err := conn.WriteMessage(websocket.BinaryMessage, keyPair.PublicKey[:]); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送公钥失败: %v", err)
	}
	
	// 接收服务器公钥
	_, serverPubKeyBytes, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("接收服务器公钥失败: %v", err)
	}
	
	// 验证服务器公钥长度
	if len(serverPubKeyBytes) != 32 {
		conn.Close()
		return nil, fmt.Errorf("服务器公钥长度错误: %d", len(serverPubKeyBytes))
	}
	
	// 设置服务器公钥
	var serverPubKey [32]byte
	copy(serverPubKey[:], serverPubKeyBytes)
	secureConn.SetRemotePublicKey(serverPubKey)
	
	// 发送注册消息
	regMsg, err := shared.CreateMessage(shared.TypeRegister, a.Config.AgentID, map[string]string{
		"agent_id":   a.Config.AgentID,
		"agent_name": a.Config.AgentName,
		"agent_info": a.Config.AgentInfo,
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("创建注册消息失败: %v", err)
	}
	
	// 发送注册消息
	if err := secureConn.SendEncrypted(regMsg); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送注册消息失败: %v", err)
	}
	
	// 等待注册确认
	regAckChan := make(chan bool, 1)
	regErrChan := make(chan error, 1)
	
	go func() {
		regResponse, err := secureConn.ReadEncrypted()
		if err != nil {
			regErrChan <- fmt.Errorf("接收注册确认失败: %v", err)
			return
		}
		
		if regResponse.Type != shared.TypeRegisterAck {
			regErrChan <- fmt.Errorf("收到非预期的注册响应类型: %s", regResponse.Type)
			return
		}
		
		regAckChan <- true
	}()
	
	// 等待注册确认或超时
	select {
	case <-regAckChan:
		log.Printf("注册确认成功，连接已建立")
	case err := <-regErrChan:
		conn.Close()
		return nil, err
	case <-time.After(10 * time.Second):
		conn.Close()
		return nil, errors.New("等待注册确认超时")
	}
	
	return secureConn, nil
}

// testConnection 测试连接是否正常
func (a *Agent) testConnection() bool {
	if a.conn == nil {
		log.Println("测试连接失败：连接未初始化")
		return false
	}

	// 首先尝试使用 WebSocket 的原生 ping/pong 机制
	wsConn := a.conn.GetRawConnection()
	if wsConn == nil {
		log.Println("测试连接失败：无法获取 WebSocket 连接")
		return false
	}

	// 设置一个短暂的读取超时
	if err := wsConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		log.Printf("设置读取超时失败: %v", err)
		return false
	}

	// 发送 ping 消息
	if err := wsConn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second)); err != nil {
		log.Printf("发送 ping 失败: %v", err)
		// 重置读取超时
		_ = wsConn.SetReadDeadline(time.Time{})
		return false
	}

	// 重置读取超时
	if err := wsConn.SetReadDeadline(time.Time{}); err != nil {
		log.Printf("重置读取超时失败: %v", err)
		return false
	}

	// 如果 WebSocket 的 ping 成功，再验证加密通道
	// 创建心跳消息
	heartbeat, err := shared.CreateMessage(shared.TypeHeartbeat, a.Config.AgentID, nil)
	if err != nil {
		log.Printf("创建心跳消息失败: %v", err)
		return false
	}

	// 发送心跳
	if err := a.conn.SendEncrypted(heartbeat); err != nil {
		log.Printf("发送心跳失败: %v", err)
		return false
	}

	// 等待响应
	respChan := make(chan bool, 1)
	errChan := make(chan error, 1)

	go func() {
		msg, err := a.conn.ReadEncrypted()
		if err != nil {
			errChan <- err
			return
		}

		if msg.Type == shared.TypeHeartbeatAck {
			respChan <- true
		} else {
			respChan <- false
		}
	}()

	// 使用超时控制
	select {
	case success := <-respChan:
		if success {
			log.Println("连接测试成功：加密通道正常")
			return true
		} else {
			log.Println("连接测试失败：收到非预期的响应类型")
			return false
		}
	case err := <-errChan:
		log.Printf("连接测试失败：读取响应时发生错误: %v", err)
		return false
	case <-time.After(5 * time.Second):
		log.Println("连接测试失败：等待响应超时")
		return false
	}
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
	case shared.TypeHeartbeatAck:
		// 心跳确认，不需要特殊处理
		
	case shared.TypeCommand:
		// 处理命令
		a.handleCommand(msg)
		
	case shared.TypePassiveReport:
		// 处理被动上报请求
		a.handlePassiveReportRequest(msg)
	
	case TypeSecurityKeyUpdate:
		// 处理密钥更新消息
		a.handleSecurityKeyUpdate(msg)
		
	case TypeSecurityKeyUpdateProposal:
		// 处理密钥更新提议
		a.handleSecurityKeyUpdateProposal(msg)
		
	default:
		log.Printf("收到未知消息类型: %s", msg.Type)
	}
}

// 处理命令消息
func (a *Agent) handleCommand(msg *shared.Message) {
	var cmdPayload shared.CommandPayload
	if err := json.Unmarshal(msg.Payload, &cmdPayload); err != nil {
		log.Printf("解析命令负载失败: %v", err)
		return
	}

	log.Printf("收到命令: ID=%s, Type=%s", cmdPayload.CommandID, cmdPayload.Type)

	// 创建命令确认消息
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
	case "shell":
		output, errMsg, exitCode = a.executeShellCommand(cmdPayload.Content, cmdPayload.Timeout)
		success = exitCode == 0 && errMsg == ""
	case "script":
		output, errMsg, exitCode = a.executeScript(cmdPayload.Content, cmdPayload.Timeout)
		success = exitCode == 0 && errMsg == ""
	default:
		errMsg = fmt.Sprintf("不支持的命令类型: %s", cmdPayload.Type)
		success = false
		exitCode = 1
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

// 执行shell命令
func (a *Agent) executeShellCommand(command string, timeout int) (string, string, int) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}

	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	// 处理超时
	var err error
	if timeout > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		defer cancel()
		cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		err = cmd.Run()
	} else {
		err = cmd.Run()
	}

	exitCode := 0
	if err != nil {
		// 尝试获取退出码
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	return outBuf.String(), errBuf.String(), exitCode
}

// 执行脚本
func (a *Agent) executeScript(scriptContent string, timeout int) (string, string, int) {
	// 创建临时脚本文件
	tmpFile, err := ioutil.TempFile("", "agent-script-*.sh")
	if err != nil {
		return "", fmt.Sprintf("创建临时脚本文件失败: %v", err), 1
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(scriptContent); err != nil {
		return "", fmt.Sprintf("写入脚本内容失败: %v", err), 1
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Sprintf("关闭脚本文件失败: %v", err), 1
	}

	// 设置执行权限
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmpFile.Name(), 0755); err != nil {
			return "", fmt.Sprintf("设置脚本权限失败: %v", err), 1
		}
	}

	// 执行脚本
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", tmpFile.Name())
	} else {
		cmd = exec.Command("sh", tmpFile.Name())
	}

	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	// 处理超时
	var cmdErr error
	if timeout > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		defer cancel()
		cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		cmdErr = cmd.Run()
	} else {
		cmdErr = cmd.Run()
	}

	exitCode := 0
	if cmdErr != nil {
		// 尝试获取退出码
		if exitErr, ok := cmdErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
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
	}
}

// 处理被动上报请求
func (a *Agent) handlePassiveReportRequest(msg *shared.Message) {
	var requestPayload struct {
		ReportType string                 `json:"report_type"`
		Params     map[string]interface{} `json:"params"`
	}

	if err := json.Unmarshal(msg.Payload, &requestPayload); err != nil {
		log.Printf("解析被动上报请求失败: %v", err)
		return
	}

	var data map[string]interface{}

	// 根据请求类型收集不同的数据
	switch requestPayload.ReportType {
	case "system_info":
		data = a.collectSystemInfo()
	case "process_list":
		data = a.collectProcessList()
	default:
		log.Printf("不支持的被动上报类型: %s", requestPayload.ReportType)
		return
	}

	// 创建上报负载
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
	}
}

// 收集系统信息
func (a *Agent) collectSystemInfo() map[string]interface{} {
	info := map[string]interface{}{
		"hostname":  "unknown",
		"os":        runtime.GOOS,
		"arch":      runtime.GOARCH,
		"cpu_count": runtime.NumCPU(),
		"timestamp": time.Now().Unix(),
	}

	hostname, err := os.Hostname()
	if err == nil {
		info["hostname"] = hostname
	}

	return info
}

// 收集进程列表
func (a *Agent) collectProcessList() map[string]interface{} {
	processes := []map[string]interface{}{}

	// 这里只是一个简化示例，真实环境需要使用系统特定的API
	var output string

	switch runtime.GOOS {
	case "windows":
		output, _, _ = a.executeShellCommand("tasklist", 10)
	default:
		output, _, _ = a.executeShellCommand("ps aux", 10)
	}

	// 简单处理输出，这里仅作示例
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		if i == 0 { // 跳过标题行
			continue
		}
		if line == "" {
			continue
		}
		
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		proc := map[string]interface{}{
			"pid":  fields[1],
			"name": fields[0],
		}
		processes = append(processes, proc)
	}

	return map[string]interface{}{
		"processes": processes,
		"count":     len(processes),
		"timestamp": time.Now().Unix(),
	}
}

// 保存安全密钥到文件
func (a *Agent) saveSecurityKey(keyFile, key string) error {
	// 如果路径中包含conf/app.conf，则保存到配置文件格式
	if strings.Contains(keyFile, "conf/app.conf") {
		log.Printf("保存密钥到配置文件: %s", keyFile)
		
		// 检查文件是否存在并检查格式
		_, err := os.Stat(keyFile)
		if err != nil {
			if os.IsNotExist(err) {
				// 文件不存在，创建一个新的INI格式配置文件
				cfg := ini.Empty()
				section, _ := cfg.NewSection("agent")
				section.Key("SECURITY_KEY").SetValue(key)
				
				if err := cfg.SaveTo(keyFile); err != nil {
					return fmt.Errorf("创建新配置文件失败: %v", err)
				}
				log.Printf("已创建新的配置文件: %s", keyFile)
				return nil
			}
			return fmt.Errorf("检查配置文件状态失败: %v", err)
		}
		
		// 读取文件前几个字节来判断是JSON还是INI
		f, err := os.Open(keyFile)
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
			section.Key("SECURITY_KEY").SetValue(key)
			
			// 保存配置
			if err := cfg.SaveTo(keyFile); err != nil {
				return fmt.Errorf("保存INI配置文件失败: %v", err)
			}
			
			log.Printf("成功将JSON格式转换为INI格式并保存密钥")
			return nil
		}
		
		// 如果是INI格式，正常处理
		log.Printf("正在更新INI格式配置文件中的密钥")
		
		// 加载现有配置
		cfg, err := ini.Load(keyFile)
		if err != nil {
			return fmt.Errorf("读取配置文件失败: %v", err)
		}
		
		// 设置密钥值到[agent]部分
		section, err := cfg.GetSection("agent")
		if err != nil {
			// 如果节不存在，创建新节
			section, err = cfg.NewSection("agent")
			if err != nil {
				return fmt.Errorf("创建配置节失败: %v", err)
			}
		}
		
		section.Key("SECURITY_KEY").SetValue(key)
		
		// 保存配置
		if err := cfg.SaveTo(keyFile); err != nil {
			return fmt.Errorf("保存配置文件失败: %v", err)
		}
		
		log.Printf("密钥已保存到配置文件: %s [agent].SECURITY_KEY", keyFile)
		return nil
	}
	
	// 否则使用原来的JSON方式保存
	securityKey := struct {
		Key string `json:"key"`
	}{
		Key: key,
	}
	
	// 序列化为JSON
	data, err := json.MarshalIndent(securityKey, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化密钥失败: %v", err)
	}
	
	// 确保目录存在
	dir := filepath.Dir(keyFile)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建目录失败: %v", err)
		}
	}
	
	// 写入文件
	if err := ioutil.WriteFile(keyFile, data, 0600); err != nil {
		return fmt.Errorf("写入密钥文件失败: %v", err)
	}
	
	log.Printf("密钥已保存到文件: %s", keyFile)
	return nil
}

// 从文件加载安全密钥
func (a *Agent) loadSecurityKey(keyFile string) (string, error) {
	// 如果路径中包含conf/app.conf，则从配置文件加载
	if strings.Contains(keyFile, "conf/app.conf") {
		log.Printf("从配置文件加载密钥: %s", keyFile)
		
		// 检查文件是否存在
		if _, err := os.Stat(keyFile); os.IsNotExist(err) {
			return "", fmt.Errorf("配置文件不存在: %s", keyFile)
		}
		
		// 加载配置
		cfg, err := ini.Load(keyFile)
		if err != nil {
			return "", fmt.Errorf("读取配置文件失败: %v", err)
		}
		
		// 从[agent]部分读取密钥
		section := cfg.Section("agent")
		if !section.HasKey("SECURITY_KEY") {
			return "", fmt.Errorf("配置文件中未找到密钥项 [agent].SECURITY_KEY")
		}
		
		keyValue := section.Key("SECURITY_KEY").String()
		if keyValue == "" {
			return "", fmt.Errorf("配置文件中密钥项值为空 [agent].SECURITY_KEY")
		}
		
		log.Printf("从配置文件成功加载密钥")
		return keyValue, nil
	}
	
	// 否则使用原来的JSON方式加载
	// 检查文件是否存在
	if _, err := os.Stat(keyFile); os.IsNotExist(err) {
		return "", fmt.Errorf("密钥文件不存在: %s", keyFile)
	}
	
	// 读取文件
	data, err := ioutil.ReadFile(keyFile)
	if err != nil {
		return "", fmt.Errorf("读取密钥文件失败: %v", err)
	}
	
	// 解析JSON
	var securityKey struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &securityKey); err != nil {
		return "", fmt.Errorf("解析密钥文件失败: %v", err)
	}
	
	return securityKey.Key, nil
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
	
	if keyUpdatePayload.NewKey == "" {
		log.Printf("收到空的密钥更新")
		return
	}
	
	log.Printf("收到服务器密钥更新")
	
	// 更新当前使用的密钥
	a.Config.SecurityKey = keyUpdatePayload.NewKey
	
	// 保存到文件
	if err := a.saveSecurityKey(a.Config.KeyFile, keyUpdatePayload.NewKey); err != nil {
		log.Printf("保存更新的密钥失败: %v", err)
	} else {
		log.Printf("已成功更新并保存密钥")
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
	
	if keyUpdateProposalPayload.NewKey == "" {
		log.Printf("收到空的密钥更新提议")
		return
	}
	
	if keyUpdateProposalPayload.SessionID == "" {
		log.Printf("收到的密钥更新提议缺少会话ID")
		return
	}
	
	log.Printf("收到服务器密钥更新提议，会话ID: %s, 新密钥: %s", 
		keyUpdateProposalPayload.SessionID, keyUpdateProposalPayload.NewKey)
	
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
		log.Printf("开始应用新密钥，旧密钥: %s, 新密钥: %s", oldKey, newKey)
		
		// 保存新密钥
		a.Config.SecurityKey = newKey
		if err := a.saveSecurityKey(a.Config.KeyFile, newKey); err != nil {
			log.Printf("保存新密钥失败: %v, 将恢复使用旧密钥", err)
			a.Config.SecurityKey = oldKey
			return
		}
		
		// 验证密钥是否正确保存
		savedKey, err := a.loadSecurityKey(a.Config.KeyFile)
		if err != nil || savedKey != newKey {
			log.Printf("警告：密钥可能未正确保存，读取的密钥: %s, 期望的密钥: %s", savedKey, newKey)
			if savedKey != newKey {
				log.Printf("密钥验证失败，将恢复使用旧密钥")
				a.Config.SecurityKey = oldKey
				return
			}
		} else {
			log.Printf("已成功保存和验证新密钥: %s", savedKey)
		}
		
		// 断开当前连接
		a.connMutex.Lock()
		if a.conn != nil {
			log.Printf("断开连接以应用新密钥")
			a.conn.Close()
			a.conn = nil
		}
		a.isConnected = false
		a.connMutex.Unlock()
		
		// 等待一点时间再重连，确保服务端也已更新密钥
		delay := 2 * time.Second
		log.Printf("等待 %v 后尝试使用新密钥重连...", delay)
		time.Sleep(delay)
		
		// 设置重连锁，防止多次重连
		a.connMutex.Lock()
		wasReconnecting := a.reconnecting
		a.connMutex.Unlock()
		
		if !wasReconnecting {
			// 重新连接
			log.Printf("开始使用新密钥 %s 重新连接", a.Config.SecurityKey)
			a.reconnect()
		} else {
			log.Printf("已有重连过程在进行中，跳过此次重连")
		}
	}(keyUpdateProposalPayload.NewKey)
}

// LoadConfig 从文件加载配置
func LoadConfig(configPath string) (*Config, error) {
	if configPath == "" {
		return nil, fmt.Errorf("配置文件路径为空")
	}
	
	// 检查文件是否存在
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("配置文件不存在: %s", configPath)
	}
	
	// 尝试读取配置
	cfg, err := ini.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %v", err)
	}
	
	// 读取 [agent] 部分
	agentSection := cfg.Section("agent")
	
	config := &Config{
		ConfigPath: configPath,
	}
	
	// 读取各配置项
	if agentSection.HasKey("SERVER_ADDR") {
		config.ServerAddr = agentSection.Key("SERVER_ADDR").String()
	}
	
	if agentSection.HasKey("SERVER_URL") {
		config.ServerURL = agentSection.Key("SERVER_URL").String()
	} else if config.ServerAddr != "" {
		// 如果没有显式设置 ServerURL 但有 ServerAddr，构造 URL
		config.ServerURL = "ws://" + config.ServerAddr + "/ws"
	}
	
	if agentSection.HasKey("AGENT_ID") {
		config.AgentID = agentSection.Key("AGENT_ID").String()
	}
	
	if agentSection.HasKey("AGENT_NAME") {
		config.AgentName = agentSection.Key("AGENT_NAME").String()
	}
	
	if agentSection.HasKey("AGENT_INFO") {
		config.AgentInfo = agentSection.Key("AGENT_INFO").String()
	}
	
	if agentSection.HasKey("SECURITY_KEY") {
		config.SecurityKey = agentSection.Key("SECURITY_KEY").String()
	}
	
	if agentSection.HasKey("KEY_FILE") {
		config.KeyFile = agentSection.Key("KEY_FILE").String()
	} else {
		// 默认使用配置文件自身作为密钥文件
		config.KeyFile = configPath
	}
	
	if agentSection.HasKey("REPORT_INTERVAL") {
		intervalStr := agentSection.Key("REPORT_INTERVAL").String()
		if intervalStr != "" {
			interval, err := time.ParseDuration(intervalStr)
			if err == nil {
				config.ReportInterval = interval
			}
		}
	}
	
	return config, nil
} 