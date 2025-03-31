package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"dont/shared"
	"github.com/gorilla/websocket"
)

// 常量
const (
	// 心跳间隔
	HeartbeatInterval = 30 * time.Second
	// 重连间隔
	ReconnectInterval = 5 * time.Second
	// 连接超时
	ConnectionTimeout = 10 * time.Second
)

// Agent 表示一个代理实例
type Agent struct {
	Config         *Config
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
}

// Config 代理配置
type Config struct {
	ServerURL      string        // 服务器WebSocket URL
	AgentID        string        // 代理唯一标识
	ReportInterval time.Duration // 主动上报间隔
	SecurityKey    string        // 通信安全密钥
}

// NewAgent 创建一个新的代理实例
func NewAgent(config *Config) (*Agent, error) {
	// 生成密钥对
	keyPair, err := shared.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("生成密钥对失败: %v", err)
	}

	return &Agent{
		Config:         config,
		keyPair:        keyPair,
		isConnected:    false,
		reconnecting:   false,
		stopChan:       make(chan struct{}),
		reportInterval: config.ReportInterval,
	}, nil
}

// Start 启动代理
func (a *Agent) Start() error {
	log.Println("Agent开始启动...")

	// 连接到服务器
	err := a.connect()
	if err != nil {
		log.Printf("连接服务器失败: %v, 将尝试重连", err)
		go a.reconnect()
		return nil
	}

	// 启动消息处理循环
	a.wg.Add(1)
	go a.handleMessages()

	// 启动主动上报循环
	if a.reportInterval > 0 {
		a.wg.Add(1)
		go a.startActiveReporting()
	}

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

// 连接到服务器
func (a *Agent) connect() error {
	a.connMutex.Lock()
	defer a.connMutex.Unlock()

	if a.isConnected {
		return nil
	}

	// 构建连接URL（添加密钥参数如果有的话）
	connectURL := a.Config.ServerURL
	if a.Config.SecurityKey != "" {
		// 添加查询参数
		if strings.Contains(connectURL, "?") {
			connectURL = connectURL + "&key=" + a.Config.SecurityKey
		} else {
			connectURL = connectURL + "?key=" + a.Config.SecurityKey
		}
	}

	log.Printf("连接到服务器: %s", a.Config.ServerURL)
	dialer := websocket.Dialer{
		HandshakeTimeout: ConnectionTimeout,
	}
	conn, _, err := dialer.Dial(connectURL, nil)
	if err != nil {
		return fmt.Errorf("WebSocket连接失败: %v", err)
	}

	// 创建安全连接
	secureConn := shared.NewSecureConnection(conn, a.keyPair, false)
	a.conn = secureConn

	// 注册到服务器
	err = a.register()
	if err != nil {
		conn.Close()
		return fmt.Errorf("注册失败: %v", err)
	}

	// 设置连接状态
	a.isConnected = true

	// 启动心跳
	go a.conn.StartHeartbeat(a.Config.AgentID, HeartbeatInterval)

	log.Println("成功连接到服务器")
	return nil
}

// 注册到服务器
func (a *Agent) register() error {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}

	// 准备注册信息
	regPayload := shared.RegisterPayload{
		PublicKey: shared.EncodePublicKey(a.keyPair.PublicKey),
		Hostname:  hostname,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}

	// 创建注册消息
	msg, err := shared.CreateMessage(shared.TypeRegister, a.Config.AgentID, regPayload)
	if err != nil {
		return err
	}

	// 由于尚未建立加密通道，先直接发送未加密消息
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	// 发送注册消息
	// 使用GetRawConnection()获取底层连接
	wsConn := a.conn.GetRawConnection()
	err = wsConn.WriteMessage(websocket.TextMessage, msgBytes)
	if err != nil {
		return err
	}

	// 读取服务器响应
	_, respBytes, err := wsConn.ReadMessage()
	if err != nil {
		return err
	}

	var resp shared.Message
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return err
	}

	if resp.Type != shared.TypeRegisterAck {
		return fmt.Errorf("收到意外消息类型: %s", resp.Type)
	}

	// 解析响应负载
	var ackPayload shared.RegisterAckPayload
	if err := json.Unmarshal(resp.Payload, &ackPayload); err != nil {
		return err
	}

	if !ackPayload.Success {
		return fmt.Errorf("注册失败: %s", ackPayload.Message)
	}

	// 解码服务器公钥
	serverPubKey, err := shared.DecodePublicKey(ackPayload.ServerPublicKey)
	if err != nil {
		return err
	}

	// 设置服务器公钥
	a.serverPubKey = serverPubKey
	a.conn.SetRemotePublicKey(serverPubKey)

	log.Println("注册成功，已接收服务器公钥")
	return nil
}

// 重连机制
func (a *Agent) reconnect() {
	a.connMutex.Lock()
	if a.reconnecting {
		a.connMutex.Unlock()
		return
	}
	a.reconnecting = true
	a.connMutex.Unlock()

	defer func() {
		a.connMutex.Lock()
		a.reconnecting = false
		a.connMutex.Unlock()
	}()

	for {
		select {
		case <-a.stopChan:
			return
		case <-time.After(ReconnectInterval):
			log.Println("尝试重新连接服务器...")
			err := a.connect()
			if err != nil {
				log.Printf("重连失败: %v, 将继续尝试", err)
				continue
			}

			// 重连成功，启动消息处理
			a.wg.Add(1)
			go a.handleMessages()

			// 如果需要主动上报，重启上报
			if a.reportInterval > 0 {
				a.reportMutex.Lock()
				a.wg.Add(1)
				go a.startActiveReporting()
				a.reportMutex.Unlock()
			}
			return
		}
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
			a.connMutex.Lock()
			conn := a.conn
			isConnected := a.isConnected
			a.connMutex.Unlock()

			if !isConnected || conn == nil {
				go a.reconnect()
				return
			}

			// 读取消息
			msg, err := conn.ReadEncrypted()
			if err != nil {
				log.Printf("读取消息失败: %v", err)
				a.connMutex.Lock()
				a.isConnected = false
				if a.conn != nil {
					a.conn.Close()
					a.conn = nil
				}
				a.connMutex.Unlock()
				go a.reconnect()
				return
			}

			// 处理消息
			go a.processMessage(msg)
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
	case shared.TypePassiveReport:
		a.handlePassiveReportRequest(msg)
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