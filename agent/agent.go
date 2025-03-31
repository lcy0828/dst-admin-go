package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
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
	keyManager     *shared.KeyManager  // 添加密钥管理器
}

// Config 代理配置
type Config struct {
	ServerURL      string        // 服务器WebSocket URL
	AgentID        string        // 代理唯一标识
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

// Connect 连接到服务器
func (a *Agent) Connect() error {
	a.connMutex.Lock()
	defer a.connMutex.Unlock()

	// 如果已经连接，直接返回
	if a.isConnected && a.conn != nil {
		return nil
	}

	// 如果正在重连，直接返回
	if a.reconnecting {
		return nil
	}

	// 检查是否设置了通信密钥
	if a.Config.SecurityKey == "" {
		return fmt.Errorf("未提供通信密钥，请设置Config.SecurityKey或在密钥文件中提供")
	}

	log.Printf("正在连接到服务器: %s", a.Config.ServerURL)
	log.Printf("使用通信密钥: %s", a.Config.SecurityKey)

	// 创建WebSocket连接
	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = ConnectionTimeout

	// 构建连接URL（添加密钥参数）
	connectURL := a.Config.ServerURL
	if a.Config.SecurityKey != "" {
		// 对密钥进行URL编码，防止特殊字符（如+、/、=等）在URL中被改变
		encodedKey := url.QueryEscape(a.Config.SecurityKey)
		
		// 添加查询参数
		if strings.Contains(connectURL, "?") {
			connectURL = connectURL + "&key=" + encodedKey
		} else {
			connectURL = connectURL + "?key=" + encodedKey
		}
	}

	c, resp, err := dialer.Dial(connectURL, nil)
	if err != nil {
		// 检查HTTP响应以提供更详细的错误信息
		if resp != nil {
			if resp.StatusCode == http.StatusUnauthorized {
				return fmt.Errorf("连接失败: 密钥无效或未提供，请检查密钥是否正确。使用的密钥: %s", a.Config.SecurityKey)
			}
			// 读取错误消息
			if resp.Body != nil {
				defer resp.Body.Close()
				body, readErr := ioutil.ReadAll(resp.Body)
				if readErr == nil && len(body) > 0 {
					return fmt.Errorf("连接失败 (HTTP %d): %s。使用的密钥: %s", resp.StatusCode, string(body), a.Config.SecurityKey)
				}
			}
		}
		return fmt.Errorf("WebSocket连接失败: %v。使用的密钥: %s", err, a.Config.SecurityKey)
	}

	// 创建安全连接
	conn := shared.NewSecureConnection(c, a.keyPair, false)

	// 准备注册信息
	pubKeyStr := shared.EncodePublicKey(a.keyPair.PublicKey)
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}

	registerPayload := shared.RegisterPayload{
		PublicKey: pubKeyStr,
		Hostname:  hostname,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}

	// 创建注册消息
	msg, err := shared.CreateMessage(shared.TypeRegister, a.Config.AgentID, registerPayload)
	if err != nil {
		c.Close()
		return fmt.Errorf("创建注册消息失败: %v", err)
	}

	// 发送注册消息
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		c.Close()
		return fmt.Errorf("序列化注册消息失败: %v", err)
	}

	if err := c.WriteMessage(websocket.TextMessage, msgBytes); err != nil {
		c.Close()
		return fmt.Errorf("发送注册消息失败: %v", err)
	}

	// 等待注册确认
	_, respBytes, err := c.ReadMessage()
	if err != nil {
		c.Close()
		return fmt.Errorf("读取注册响应失败: %v", err)
	}

	var respMsg shared.Message
	if err := json.Unmarshal(respBytes, &respMsg); err != nil {
		c.Close()
		return fmt.Errorf("解析注册确认消息失败: %v", err)
	}

	// 检查响应类型
	if respMsg.Type != shared.TypeRegisterAck {
		c.Close()
		return fmt.Errorf("收到非预期的响应类型: %s", respMsg.Type)
	}

	// 处理注册确认
	var ackPayload shared.RegisterAckPayload
	if err := json.Unmarshal(respMsg.Payload, &ackPayload); err != nil {
		c.Close()
		return fmt.Errorf("解析注册确认失败: %v", err)
	}

	// 检查注册是否成功
	if !ackPayload.Success {
		c.Close()
		// 如果错误信息包含密钥错误的提示，提供明确的错误信息
		if strings.Contains(strings.ToLower(ackPayload.Message), "security key") || 
		   strings.Contains(strings.ToLower(ackPayload.Message), "密钥") {
			// 删除本地存储的错误密钥
			if err := os.Remove(a.Config.KeyFile); err == nil {
				log.Printf("已删除无效的密钥文件: %s", a.Config.KeyFile)
			}
			return fmt.Errorf("通信密钥无效，请提供正确的密钥: %s", ackPayload.Message)
		}
		return fmt.Errorf("服务器拒绝注册: %s", ackPayload.Message)
	}

	// 解码服务器公钥
	serverPubKey, err := shared.DecodePublicKey(ackPayload.ServerPublicKey)
	if err != nil {
		c.Close()
		return fmt.Errorf("解码服务器公钥失败: %v", err)
	}

	// 设置服务器公钥
	a.serverPubKey = serverPubKey
	conn.SetRemotePublicKey(serverPubKey)

	// 保存连接
	a.conn = conn
	a.isConnected = true

	// 如果连接成功，保存当前使用的密钥
	if a.Config.SecurityKey != "" {
		err := a.saveSecurityKey(a.Config.KeyFile, a.Config.SecurityKey)
		if err != nil {
			log.Printf("警告: 无法保存密钥到文件: %v", err)
		} else {
			log.Printf("密钥已成功保存到: %s", a.Config.KeyFile)
		}
	}

	log.Println("注册成功，已接收服务器公钥")

	// 启动心跳机制
	a.wg.Add(1)
	go a.startHeartbeat()

	// 启动消息处理
	a.wg.Add(1)
	go a.handleMessages()

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
			err := a.Connect()
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
		
		// 加载现有配置
		cfg, err := ini.Load(keyFile)
		if err != nil {
			if os.IsNotExist(err) {
				// 如果文件不存在，创建新的配置文件
				cfg = ini.Empty()
			} else {
				return fmt.Errorf("读取配置文件失败: %v", err)
			}
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
		return
	}
	
	a.connMutex.Lock()
	defer a.connMutex.Unlock()
	
	if a.conn != nil && a.isConnected {
		if err := a.conn.SendEncrypted(ackMsg); err != nil {
			log.Printf("发送密钥更新确认失败: %v", err)
		}
	}
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