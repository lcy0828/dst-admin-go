package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"dont/shared"
	"github.com/gorilla/websocket"
)

// 常量
const (
	// 写超时
	WriteTimeout = 10 * time.Second
	// 读超时
	ReadTimeout = 10 * time.Second
	// 心跳间隔
	HeartbeatInterval = 30 * time.Second
	// Ping间隔
	PingInterval = 25 * time.Second
)

// Agent连接信息
type AgentConnection struct {
	AgentID       string
	Connection    *shared.SecureConnection
	PublicKey     [32]byte
	LastHeartbeat time.Time
	Info          map[string]interface{}
	Mutex         sync.Mutex
}

// Server 表示服务器实例
type Server struct {
	Config     *Config
	keyPair    *shared.KeyPair
	keyManager shared.KeyManagerInterface
	agents     map[string]*AgentConnection
	agentMutex sync.RWMutex
	upgrader   websocket.Upgrader
	stopChan   chan struct{}
}

// Config 服务器配置
type Config struct {
	ListenAddr string // 监听地址
	TLSCert    string // TLS证书文件
	TLSKey     string // TLS密钥文件
	KeyFile    string // 通信密钥文件
}

// NewServer 创建新的服务器实例
func NewServer(config *Config) (*Server, error) {
	// 生成密钥对
	keyPair, err := shared.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("生成密钥对失败: %v", err)
	}
	
	// 初始化密钥管理器，使用配置文件而不是独立的密钥文件
	log.Printf("正在初始化密钥管理器，使用配置文件: conf/app.conf")
	keyManager, err := shared.NewKeyManagerWithConfig("conf/app.conf", "server", "SECURITY_KEY")
	if err != nil {
		return nil, fmt.Errorf("初始化密钥管理器失败: %v", err)
	}

	server := &Server{
		Config:     config,
		keyPair:    keyPair,
		keyManager: keyManager,
		agents:     make(map[string]*AgentConnection),
		stopChan:   make(chan struct{}),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true // 允许所有来源的连接
			},
		},
	}
	
	// 设置密钥变更回调
	keyManager.SetKeyChangedCallback(func(newKey string) {
		log.Println("检测到通信密钥变更，已更新服务器密钥")
	})

	return server, nil
}

// Start 启动服务器
func (s *Server) Start() error {
	log.Println("服务器开始启动...")

	// 设置HTTP处理函数
	http.HandleFunc("/agent", s.handleAgentConnection)

	// 启动清理过期连接的goroutine
	go s.cleanupExpiredConnections()

	// 根据配置决定是否使用TLS
	var err error
	if s.Config.TLSCert != "" && s.Config.TLSKey != "" {
		log.Printf("使用TLS启动服务器，监听: %s", s.Config.ListenAddr)
		err = http.ListenAndServeTLS(s.Config.ListenAddr, s.Config.TLSCert, s.Config.TLSKey, nil)
	} else {
		log.Printf("以非TLS模式启动服务器，监听: %s", s.Config.ListenAddr)
		err = http.ListenAndServe(s.Config.ListenAddr, nil)
	}

	if err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("启动HTTP服务失败: %v", err)
	}

	return nil
}

// Stop 停止服务器
func (s *Server) Stop() {
	log.Println("服务器正在停止...")
	close(s.stopChan)

	// 停止密钥文件监控
	if s.keyManager != nil {
		s.keyManager.StopWatching()
	}

	// 关闭所有agent连接
	s.agentMutex.Lock()
	for _, agent := range s.agents {
		agent.Mutex.Lock()
		if agent.Connection != nil {
			agent.Connection.Close()
		}
		agent.Mutex.Unlock()
	}
	s.agents = make(map[string]*AgentConnection)
	s.agentMutex.Unlock()

	log.Println("服务器已停止")
}

// 处理Agent连接
func (s *Server) handleAgentConnection(w http.ResponseWriter, r *http.Request) {
	// 获取查询参数中的密钥
	authKey := r.URL.Query().Get("key")
	
	// 验证密钥（必须提供有效密钥）
	if s.keyManager != nil {
		if authKey == "" {
			log.Printf("拒绝连接：未提供通信密钥")
			http.Error(w, "必须提供通信密钥", http.StatusUnauthorized)
			return
		}
		
		if !s.keyManager.ValidateKey(authKey) {
			log.Printf("拒绝连接：无效的通信密钥")
			http.Error(w, "无效的通信密钥", http.StatusUnauthorized)
			return
		}
	}

	// 升级HTTP连接为WebSocket
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("升级连接失败: %v", err)
		return
	}

	log.Printf("新的WebSocket连接: %s", conn.RemoteAddr())

	// 读取注册消息
	_, msgBytes, err := conn.ReadMessage()
	if err != nil {
		log.Printf("读取注册消息失败: %v", err)
		conn.Close()
		return
	}

	var msg shared.Message
	if err := json.Unmarshal(msgBytes, &msg); err != nil {
		log.Printf("解析注册消息失败: %v", err)
		conn.Close()
		return
	}

	if msg.Type != shared.TypeRegister {
		log.Printf("预期注册消息，但收到: %s", msg.Type)
		conn.Close()
		return
	}

	// 解析注册负载
	var regPayload shared.RegisterPayload
	if err := json.Unmarshal(msg.Payload, &regPayload); err != nil {
		log.Printf("解析注册负载失败: %v", err)
		conn.Close()
		return
	}

	// 解码Agent公钥
	agentPubKey, err := shared.DecodePublicKey(regPayload.PublicKey)
	if err != nil {
		log.Printf("解码Agent公钥失败: %v", err)
		conn.Close()
		return
	}

	// 创建注册确认负载
	ackPayload := shared.RegisterAckPayload{
		ServerPublicKey: shared.EncodePublicKey(s.keyPair.PublicKey),
		Success:         true,
		Message:         "注册成功",
	}

	// 创建确认消息
	ackMsg, err := shared.CreateMessage(shared.TypeRegisterAck, "server", ackPayload)
	if err != nil {
		log.Printf("创建注册确认消息失败: %v", err)
		conn.Close()
		return
	}

	// 发送确认
	ackBytes, err := json.Marshal(ackMsg)
	if err != nil {
		log.Printf("序列化注册确认消息失败: %v", err)
		conn.Close()
		return
	}

	if err := conn.WriteMessage(websocket.TextMessage, ackBytes); err != nil {
		log.Printf("发送注册确认失败: %v", err)
		conn.Close()
		return
	}

	// 创建安全连接
	secureConn := shared.NewSecureConnection(conn, s.keyPair, true)
	secureConn.SetRemotePublicKey(agentPubKey)

	// 检查是否已有相同ID的连接
	s.agentMutex.Lock()
	existingAgent, exists := s.agents[msg.AgentID]
	if exists {
		log.Printf("重复的Agent ID: %s，关闭旧连接", msg.AgentID)
		existingAgent.Mutex.Lock()
		if existingAgent.Connection != nil {
			existingAgent.Connection.Close()
		}
		existingAgent.Mutex.Unlock()
	}

	// 存储Agent连接信息
	agentConn := &AgentConnection{
		AgentID:       msg.AgentID,
		Connection:    secureConn,
		PublicKey:     agentPubKey,
		LastHeartbeat: time.Now(),
		Info: map[string]interface{}{
			"hostname": regPayload.Hostname,
			"os":       regPayload.OS,
			"arch":     regPayload.Arch,
		},
	}
	s.agents[msg.AgentID] = agentConn
	s.agentMutex.Unlock()

	log.Printf("Agent已注册: ID=%s, OS=%s, Arch=%s", msg.AgentID, regPayload.OS, regPayload.Arch)

	// 启动消息处理循环
	go s.handleAgentMessages(agentConn)
}

// 处理来自Agent的消息
func (s *Server) handleAgentMessages(agent *AgentConnection) {
	defer func() {
		// 断开连接时从列表中移除
		s.agentMutex.Lock()
		if a, exists := s.agents[agent.AgentID]; exists && a == agent {
			delete(s.agents, agent.AgentID)
		}
		s.agentMutex.Unlock()

		agent.Mutex.Lock()
		if agent.Connection != nil {
			agent.Connection.Close()
		}
		agent.Mutex.Unlock()

		log.Printf("Agent连接已关闭: %s", agent.AgentID)
	}()

	for {
		select {
		case <-s.stopChan:
			return
		default:
			agent.Mutex.Lock()
			conn := agent.Connection
			agent.Mutex.Unlock()

			if conn == nil {
				return
			}

			// 读取消息
			msg, err := conn.ReadEncrypted()
			if err != nil {
				log.Printf("从Agent读取消息失败: %s, error: %v", agent.AgentID, err)
				return
			}

			// 处理消息
			s.processAgentMessage(agent, msg)
		}
	}
}

// 处理收到的Agent消息
func (s *Server) processAgentMessage(agent *AgentConnection, msg *shared.Message) {
	switch msg.Type {
	case shared.TypeHeartbeat:
		// 更新最后心跳时间
		agent.Mutex.Lock()
		agent.LastHeartbeat = time.Now()
		agent.Mutex.Unlock()

		// 发送心跳确认
		ackMsg, _ := shared.CreateMessage(shared.TypeHeartbeatAck, "server", nil)
		agent.Mutex.Lock()
		if agent.Connection != nil {
			err := agent.Connection.SendEncrypted(ackMsg)
			if err != nil {
				log.Printf("发送心跳确认失败: %v", err)
			}
		}
		agent.Mutex.Unlock()

	case shared.TypeActiveReport:
		s.handleActiveReport(agent, msg)

	case shared.TypePassiveReport:
		s.handlePassiveReport(agent, msg)

	case shared.TypeCommandResp:
		s.handleCommandResponse(agent, msg)

	default:
		log.Printf("收到未知消息类型: %s, 来自: %s", msg.Type, agent.AgentID)
	}
}

// 处理主动上报
func (s *Server) handleActiveReport(agent *AgentConnection, msg *shared.Message) {
	var reportPayload shared.ReportDataPayload
	if err := json.Unmarshal(msg.Payload, &reportPayload); err != nil {
		log.Printf("解析上报负载失败: %v", err)
		return
	}

	log.Printf("收到主动上报，AgentID: %s, ReportType: %s", agent.AgentID, reportPayload.ReportType)

	// 存储上报数据（这里只是示例，实际应用可能需要存入数据库等）
	agent.Mutex.Lock()
	for k, v := range reportPayload.Data {
		agent.Info[k] = v
	}
	agent.Mutex.Unlock()

	// 发送确认
	ackMsg, _ := shared.CreateMessage(shared.TypeReportAck, "server", map[string]string{
		"report_id": reportPayload.ReportID,
		"status":    "received",
	})

	agent.Mutex.Lock()
	if agent.Connection != nil {
		if err := agent.Connection.SendEncrypted(ackMsg); err != nil {
			log.Printf("发送上报确认失败: %v", err)
		}
	}
	agent.Mutex.Unlock()
}

// 处理被动上报
func (s *Server) handlePassiveReport(agent *AgentConnection, msg *shared.Message) {
	var reportPayload shared.ReportDataPayload
	if err := json.Unmarshal(msg.Payload, &reportPayload); err != nil {
		log.Printf("解析上报负载失败: %v", err)
		return
	}

	log.Printf("收到被动上报，AgentID: %s, ReportType: %s", agent.AgentID, reportPayload.ReportType)

	// 存储上报数据
	agent.Mutex.Lock()
	for k, v := range reportPayload.Data {
		agent.Info[k] = v
	}
	agent.Mutex.Unlock()

	// 发送确认
	ackMsg, _ := shared.CreateMessage(shared.TypeReportAck, "server", map[string]string{
		"report_id": reportPayload.ReportID,
		"status":    "received",
	})

	agent.Mutex.Lock()
	if agent.Connection != nil {
		if err := agent.Connection.SendEncrypted(ackMsg); err != nil {
			log.Printf("发送上报确认失败: %v", err)
		}
	}
	agent.Mutex.Unlock()
}

// 处理命令响应
func (s *Server) handleCommandResponse(agent *AgentConnection, msg *shared.Message) {
	var respPayload shared.CommandResponsePayload
	if err := json.Unmarshal(msg.Payload, &respPayload); err != nil {
		log.Printf("解析命令响应负载失败: %v", err)
		return
	}

	status := "成功"
	if !respPayload.Success {
		status = "失败"
	}

	log.Printf("收到命令响应，AgentID: %s, CommandID: %s, 状态: %s, 退出码: %d",
		agent.AgentID, respPayload.CommandID, status, respPayload.ExitCode)
	
	if respPayload.Output != "" {
		log.Printf("命令输出: %s", respPayload.Output)
	}
	
	if respPayload.ErrorMsg != "" {
		log.Printf("命令错误: %s", respPayload.ErrorMsg)
	}
}

// 清理过期连接
func (s *Server) cleanupExpiredConnections() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopChan:
			return
		case <-ticker.C:
			now := time.Now()
			expiredAgents := []string{}

			s.agentMutex.RLock()
			for id, agent := range s.agents {
				agent.Mutex.Lock()
				// 如果超过2分钟没收到心跳，认为连接已断开
				if agent.LastHeartbeat.Add(2 * time.Minute).Before(now) {
					expiredAgents = append(expiredAgents, id)
				}
				agent.Mutex.Unlock()
			}
			s.agentMutex.RUnlock()

			// 删除过期连接
			if len(expiredAgents) > 0 {
				s.agentMutex.Lock()
				for _, id := range expiredAgents {
					if agent, exists := s.agents[id]; exists {
						agent.Mutex.Lock()
						if agent.Connection != nil {
							agent.Connection.Close()
						}
						agent.Mutex.Unlock()
						delete(s.agents, id)
						log.Printf("移除过期Agent连接: %s", id)
					}
				}
				s.agentMutex.Unlock()
			}
		}
	}
}

// 向指定Agent发送执行命令请求
func (s *Server) SendCommand(agentID, commandType, content string, timeout int) (string, error) {
	// 查找Agent
	s.agentMutex.RLock()
	agent, exists := s.agents[agentID]
	s.agentMutex.RUnlock()

	if !exists {
		return "", fmt.Errorf("未找到指定的Agent: %s", agentID)
	}

	// 创建命令ID
	commandID := shared.GenerateUUID()

	// 创建命令负载
	cmdPayload := shared.CommandPayload{
		CommandID: commandID,
		Type:      commandType,
		Content:   content,
		Timeout:   timeout,
	}

	// 创建命令消息
	cmdMsg, err := shared.CreateMessage(shared.TypeCommand, "server", cmdPayload)
	if err != nil {
		return "", fmt.Errorf("创建命令消息失败: %v", err)
	}

	// 发送命令
	agent.Mutex.Lock()
	if agent.Connection == nil {
		agent.Mutex.Unlock()
		return "", fmt.Errorf("Agent连接已关闭: %s", agentID)
	}
	
	err = agent.Connection.SendEncrypted(cmdMsg)
	agent.Mutex.Unlock()
	
	if err != nil {
		return "", fmt.Errorf("发送命令失败: %v", err)
	}

	log.Printf("向Agent发送命令: %s, CommandID: %s, Type: %s", agentID, commandID, commandType)
	return commandID, nil
}

// 请求Agent进行被动上报
func (s *Server) RequestPassiveReport(agentID, reportType string, params map[string]interface{}) error {
	// 查找Agent
	s.agentMutex.RLock()
	agent, exists := s.agents[agentID]
	s.agentMutex.RUnlock()

	if !exists {
		return fmt.Errorf("未找到指定的Agent: %s", agentID)
	}

	// 创建请求负载
	requestPayload := map[string]interface{}{
		"report_type": reportType,
		"params":      params,
	}

	// 创建请求消息
	requestMsg, err := shared.CreateMessage(shared.TypePassiveReport, "server", requestPayload)
	if err != nil {
		return fmt.Errorf("创建被动上报请求失败: %v", err)
	}

	// 发送请求
	agent.Mutex.Lock()
	if agent.Connection == nil {
		agent.Mutex.Unlock()
		return fmt.Errorf("Agent连接已关闭: %s", agentID)
	}
	
	err = agent.Connection.SendEncrypted(requestMsg)
	agent.Mutex.Unlock()
	
	if err != nil {
		return fmt.Errorf("发送被动上报请求失败: %v", err)
	}

	log.Printf("向Agent请求被动上报: %s, ReportType: %s", agentID, reportType)
	return nil
}

// 获取所有连接的Agent信息
func (s *Server) GetAllAgentInfo() map[string]map[string]interface{} {
	result := make(map[string]map[string]interface{})

	s.agentMutex.RLock()
	defer s.agentMutex.RUnlock()

	for id, agent := range s.agents {
		agent.Mutex.Lock()
		
		// 复制信息以避免并发问题
		info := make(map[string]interface{})
		for k, v := range agent.Info {
			info[k] = v
		}
		
		// 添加连接信息
		info["last_heartbeat"] = agent.LastHeartbeat.Unix()
		info["connected"] = agent.Connection != nil
		
		agent.Mutex.Unlock()
		
		result[id] = info
	}

	return result
}

// GetSecurityKey 获取当前的通信安全密钥
func (s *Server) GetSecurityKey() string {
	if s.keyManager == nil {
		return ""
	}
	return s.keyManager.GetKey()
}

// GenerateNewSecurityKey 生成新的通信安全密钥
func (s *Server) GenerateNewSecurityKey() error {
	if s.keyManager == nil {
		return fmt.Errorf("密钥管理器未初始化")
	}
	err := s.keyManager.GenerateNewKey()
	if err != nil {
		return err
	}
	
	// 获取新生成的密钥，并推送给所有已连接的Agent
	newKey := s.keyManager.GetKey()
	go s.broadcastSecurityKeyUpdate(newKey)
	
	return nil
}

// UpdateSecurityKey 更新通信安全密钥
func (s *Server) UpdateSecurityKey(newKey string) error {
	if s.keyManager == nil {
		return fmt.Errorf("密钥管理器未初始化")
	}
	err := s.keyManager.SetKey(newKey)
	if err != nil {
		return err
	}
	
	// 推送新密钥给所有已连接的Agent
	go s.broadcastSecurityKeyUpdate(newKey)
	
	return nil
}

// broadcastSecurityKeyUpdate 向所有已连接的Agent广播密钥更新
func (s *Server) broadcastSecurityKeyUpdate(newKey string) {
	// 创建密钥更新消息
	payload := struct {
		NewKey string `json:"new_key"`
	}{
		NewKey: newKey,
	}
	
	msg, err := shared.CreateMessage("security_key_update", "server", payload)
	if err != nil {
		log.Printf("创建密钥更新消息失败: %v", err)
		return
	}
	
	// 获取所有Agent
	s.agentMutex.RLock()
	agents := make([]*AgentConnection, 0, len(s.agents))
	for _, agent := range s.agents {
		agents = append(agents, agent)
	}
	s.agentMutex.RUnlock()
	
	// 广播给所有Agent
	successCount := 0
	for _, agent := range agents {
		agent.Mutex.Lock()
		conn := agent.Connection
		agent.Mutex.Unlock()
		
		if conn != nil {
			if err := conn.SendEncrypted(msg); err != nil {
				log.Printf("向Agent(%s)发送密钥更新失败: %v", agent.AgentID, err)
			} else {
				successCount++
			}
		}
	}
	
	log.Printf("已向%d个Agent推送新密钥更新", successCount)
} 