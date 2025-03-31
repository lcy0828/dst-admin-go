package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"io/ioutil"
	"io"
	"os"
	"path/filepath"
	"bytes"

	"dont/shared"
	"github.com/gorilla/websocket"
	"encoding/base64"
	"github.com/google/uuid"
	"gopkg.in/ini.v1"
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
	Config            *Config
	keyPair           *shared.KeyPair
	keyManager        shared.KeyManagerInterface
	agents            map[string]*AgentConnection
	agentMutex        sync.RWMutex
	upgrader          websocket.Upgrader
	stopChan          chan struct{}
	keyUpdateSessions *KeyUpdateSessionManager // 密钥更新会话管理器
}

// Config 服务器配置
type Config struct {
	ListenAddr string // 监听地址
	TLSCert    string // TLS证书文件
	TLSKey     string // TLS密钥文件
	KeyFile    string // 通信密钥文件
	SecurityKey string // 通信安全密钥
}

// KeyUpdateSession 表示一次密钥更新会话
type KeyUpdateSession struct {
	ID                 string
	ProposedKey        string
	StartTime          time.Time
	Status             string // 'pending', 'completed', 'failed'
	ReadyAgents        map[string]bool
	TotalAgentCount    int
	Timeout            time.Duration
	TimeoutTimer       *time.Timer
	OnComplete         func(success bool)
	Mutex              sync.Mutex
}

// KeyUpdateSessionManager 管理所有密钥更新会话
type KeyUpdateSessionManager struct {
	CurrentSession     *KeyUpdateSession
	CompletedSessions  int
	FailedSessions     int
	Mutex              sync.Mutex
}

// NewKeyUpdateSessionManager 创建新的密钥更新会话管理器
func NewKeyUpdateSessionManager() *KeyUpdateSessionManager {
	return &KeyUpdateSessionManager{
		CompletedSessions: 0,
		FailedSessions: 0,
	}
}

// NewServer 创建新的服务器实例
func NewServer(config *Config) (*Server, error) {
	// 生成密钥对
	keyPair, err := shared.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("生成密钥对失败: %v", err)
	}
	
	// 初始化密钥管理器，使用配置文件而不是独立的密钥文件
	log.Printf("正在初始化密钥管理器，使用配置文件: %s", config.KeyFile)
	keyManager, err := shared.NewKeyManagerWithConfig(config.KeyFile, "server", "SECURITY_KEY")
	if err != nil {
		return nil, fmt.Errorf("初始化密钥管理器失败: %v", err)
	}

	// 输出当前使用的密钥
	currentKey := keyManager.GetKey()
	log.Printf("当前服务器通信密钥: %s", currentKey)
	
	server := &Server{
		Config:            config,
		keyPair:           keyPair,
		keyManager:        keyManager,
		agents:            make(map[string]*AgentConnection),
		stopChan:          make(chan struct{}),
		keyUpdateSessions: NewKeyUpdateSessionManager(),
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
		log.Printf("检测到通信密钥变更，新密钥: %s", newKey)
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
	
	// 处理Base64编码中的特殊字符
	decodedKey := authKey
	// 检查是否包含%编码字符
	if strings.Contains(authKey, "%") {
		// 替换所有编码的Base64特殊字符
		decodedKey = strings.ReplaceAll(authKey, "%2B", "+")
		decodedKey = strings.ReplaceAll(decodedKey, "%2F", "/")
		decodedKey = strings.ReplaceAll(decodedKey, "%3D", "=")
		
		// 如果还有其他编码字符，尝试URL解码
		if strings.Contains(decodedKey, "%") {
			unescaped, err := url.QueryUnescape(decodedKey)
			if err == nil {
				decodedKey = unescaped
			}
		}
		
		log.Printf("密钥已解码: %s -> %s", authKey, decodedKey)
	}
	
	// 验证密钥（必须提供有效密钥）
	if s.keyManager != nil {
		if decodedKey == "" {
			log.Printf("拒绝连接：未提供通信密钥")
			http.Error(w, "必须提供通信密钥", http.StatusUnauthorized)
			return
		}
		
		if !s.keyManager.ValidateKey(decodedKey) {
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
		
	case "security_key_update_ack":
		// 处理密钥更新确认
		var payload struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			log.Printf("解析密钥更新确认失败: %v", err)
			return
		}
		
		if payload.Success {
			log.Printf("Agent(%s)已确认接收密钥更新: %s", agent.AgentID, payload.Message)
		} else {
			log.Printf("Agent(%s)密钥更新失败: %s", agent.AgentID, payload.Message)
		}
		
	case "security_key_update_ready":
		// 处理Agent已准备好更新密钥的消息
		var payload struct {
			AgentID   string `json:"agent_id"`
			NewKey    string `json:"new_key"`
			ReadyIn   int    `json:"ready_in"`
			SessionID string `json:"session_id"`
		}
		
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			log.Printf("解析密钥更新准备消息失败: %v", err)
			return
		}
		
		// 处理Agent密钥更新准备确认
		s.handleAgentKeyUpdateReady(agent.AgentID, payload)

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

// 创建新的密钥更新会话
func (s *Server) createKeyUpdateSession(proposedKey string) (*KeyUpdateSession, error) {
	s.keyUpdateSessions.Mutex.Lock()
	defer s.keyUpdateSessions.Mutex.Unlock()

	// 检查是否有正在进行的会话
	if s.keyUpdateSessions.CurrentSession != nil && 
	   s.keyUpdateSessions.CurrentSession.Status == "pending" {
		return nil, fmt.Errorf("已有正在进行的密钥更新会话")
	}

	// 计算当前连接的代理数量
	s.agentMutex.RLock()
	totalAgents := len(s.agents)
	s.agentMutex.RUnlock()

	if totalAgents == 0 {
		log.Println("警告: 没有已连接的代理，将直接更新服务器密钥")
		// 直接更新服务器密钥
		oldKey := s.Config.SecurityKey
		s.Config.SecurityKey = proposedKey
		err := s.saveSecurityKey(s.Config.KeyFile, proposedKey)
		if err != nil {
			log.Printf("保存新密钥失败: %v, 恢复使用旧密钥", err)
			s.Config.SecurityKey = oldKey
			return nil, err
		}
		log.Printf("已成功更新服务器密钥: 旧密钥 %s -> 新密钥 %s", oldKey, proposedKey)
		return nil, nil
	}

	log.Printf("创建新的密钥更新会话，当前连接的代理数量: %d", totalAgents)

	// 创建会话
	sessionID := uuid.New().String()
	session := &KeyUpdateSession{
		ID:              sessionID,
		ProposedKey:     proposedKey,
		StartTime:       time.Now(),
		Status:          "pending",
		ReadyAgents:     make(map[string]bool),
		TotalAgentCount: totalAgents,
		Timeout:         time.Minute * 5, // 设置超时时间为5分钟
		OnComplete: func(success bool) {
			if success {
				// 所有代理都已准备好，可以应用新密钥
				log.Printf("所有代理 (%d/%d) 都已准备好应用新密钥 %s", totalAgents, totalAgents, proposedKey)
				
				// 记录旧密钥用于日志
				oldKey := s.Config.SecurityKey
				
				// 更新服务器的密钥
				s.Config.SecurityKey = proposedKey
				err := s.saveSecurityKey(s.Config.KeyFile, proposedKey)
				if err != nil {
					log.Printf("保存新密钥失败: %v, 恢复使用旧密钥", err)
					s.Config.SecurityKey = oldKey
					return
				}
				
				// 验证密钥是否正确保存
				savedKey, err := s.loadSecurityKey(s.Config.KeyFile)
				if err != nil || savedKey != proposedKey {
					log.Printf("警告：密钥可能未正确保存，读取的密钥: %s, 期望的密钥: %s", savedKey, proposedKey)
				} else {
					log.Printf("成功应用新密钥: 旧密钥 %s -> 新密钥 %s", oldKey, savedKey)
				}

				// 更新会话状态
				s.keyUpdateSessions.Mutex.Lock()
				s.keyUpdateSessions.CurrentSession.Status = "completed"
				s.keyUpdateSessions.CompletedSessions++
				s.keyUpdateSessions.CurrentSession = nil
				s.keyUpdateSessions.Mutex.Unlock()
				
				log.Printf("密钥更新会话 %s 已完成", sessionID)
			} else {
				log.Printf("密钥更新会话 %s 失败", sessionID)
				
				// 更新会话状态
				s.keyUpdateSessions.Mutex.Lock()
				s.keyUpdateSessions.CurrentSession.Status = "failed"
				s.keyUpdateSessions.FailedSessions++
				s.keyUpdateSessions.CurrentSession = nil
				s.keyUpdateSessions.Mutex.Unlock()
			}
		},
	}

	// 设置超时定时器
	session.TimeoutTimer = time.AfterFunc(session.Timeout, func() {
		s.keyUpdateSessions.Mutex.Lock()
		defer s.keyUpdateSessions.Mutex.Unlock()
		
		if s.keyUpdateSessions.CurrentSession != nil && 
		   s.keyUpdateSessions.CurrentSession.ID == sessionID && 
		   s.keyUpdateSessions.CurrentSession.Status == "pending" {
			log.Printf("密钥更新会话 %s 超时，当前已准备好的代理: %d/%d", 
				sessionID, 
				len(s.keyUpdateSessions.CurrentSession.ReadyAgents), 
				s.keyUpdateSessions.CurrentSession.TotalAgentCount)
			
			s.keyUpdateSessions.CurrentSession.OnComplete(false)
		}
	})

	// 设置为当前会话
	s.keyUpdateSessions.CurrentSession = session
	
	log.Printf("已创建密钥更新会话 %s，等待 %d 个代理准备就绪", sessionID, totalAgents)
	return session, nil
}

// GenerateNewSecurityKey 生成新的通信安全密钥
func (s *Server) GenerateNewSecurityKey() error {
	if s.keyManager == nil {
		return fmt.Errorf("密钥管理器未初始化")
	}
	
	// 生成随机密钥
	keyBytes := make([]byte, 32)
	_, err := rand.Read(keyBytes)
	if err != nil {
		return fmt.Errorf("生成随机密钥失败: %v", err)
	}
	
	// Base64编码密钥
	newKey := base64.StdEncoding.EncodeToString(keyBytes)
	log.Printf("已成功生成新密钥: %s", newKey)
	
	// 创建密钥更新会话
	session, err := s.createKeyUpdateSession(newKey)
	if err != nil {
		return err
	}
	
	// 如果session为nil，表示没有连接的agent，已直接应用密钥
	if session == nil {
		return nil
	}
	
	// 推送新密钥给所有已连接的Agent
	go s.broadcastKeyUpdateProposal(session, newKey)
	
	return nil
}

// UpdateSecurityKey 更新通信安全密钥
func (s *Server) UpdateSecurityKey(newKey string) error {
	if s.keyManager == nil {
		return fmt.Errorf("密钥管理器未初始化")
	}
	
	// 验证密钥格式
	_, err := base64.StdEncoding.DecodeString(newKey)
	if err != nil {
		return fmt.Errorf("无效的密钥格式，必须是有效的Base64编码字符串: %v", err)
	}
	
	// 创建密钥更新会话
	session, err := s.createKeyUpdateSession(newKey)
	if err != nil {
		return err
	}
	
	// 如果session为nil，表示没有连接的agent，已直接应用密钥
	if session == nil {
		return nil
	}
	
	// 推送新密钥给所有已连接的Agent
	go s.broadcastKeyUpdateProposal(session, newKey)
	
	return nil
}

// broadcastKeyUpdateProposal 向所有已连接的Agent广播密钥更新提议
func (s *Server) broadcastKeyUpdateProposal(session *KeyUpdateSession, newKey string) {
	// 准备消息负载
	payload := struct {
		NewKey    string `json:"new_key"`
		SessionID string `json:"session_id"`
	}{
		NewKey:    session.ProposedKey,
		SessionID: session.ID,
	}
	
	msg, err := shared.CreateMessage("security_key_update_proposal", "server", payload)
	if err != nil {
		log.Printf("创建密钥更新提议消息失败: %v", err)
		session.OnComplete(false)
		return
	}
	
	// 向所有连接的Agent广播
	s.agentMutex.RLock()
	defer s.agentMutex.RUnlock()
	
	successCount := 0
	for _, agent := range s.agents {
		if agent.Connection != nil {
			if err := agent.Connection.SendEncrypted(msg); err != nil {
				log.Printf("向Agent(%s)发送密钥更新提议失败: %v", agent.AgentID, err)
				session.Mutex.Lock()
				session.ReadyAgents[agent.AgentID] = false
				session.Mutex.Unlock()
			} else {
				successCount++
			}
		}
	}
	
	log.Printf("已向%d个Agent推送密钥更新提议，会话ID: %s", successCount, session.ID)
	
	// 如果没有成功发送给任何Agent，取消会话
	if successCount == 0 {
		log.Printf("没有成功发送给任何Agent，取消密钥更新会话")
		session.OnComplete(false)
	}
}

// handleAgentKeyUpdateReady 处理Agent密钥更新准备确认
func (s *Server) handleAgentKeyUpdateReady(agentID string, payload struct {
	AgentID   string `json:"agent_id"`
	NewKey    string `json:"new_key"`
	ReadyIn   int    `json:"ready_in"`
	SessionID string `json:"session_id"`
}) {
	// 检查当前会话
	s.keyUpdateSessions.Mutex.Lock()
	currentSession := s.keyUpdateSessions.CurrentSession
	s.keyUpdateSessions.Mutex.Unlock()
	
	if currentSession == nil || currentSession.ID != payload.SessionID {
		log.Printf("Agent(%s)响应了无效的会话ID: %s", agentID, payload.SessionID)
		return
	}
	
	// 更新Agent状态
	currentSession.Mutex.Lock()
	defer currentSession.Mutex.Unlock()
	
	// 标记该代理为就绪
	currentSession.ReadyAgents[agentID] = true
	
	log.Printf("Agent(%s)已准备好在%d秒后更新密钥，当前进度: %d/%d", 
		agentID, payload.ReadyIn, len(currentSession.ReadyAgents), currentSession.TotalAgentCount)
	
	// 检查是否所有Agent都已准备好
	if len(currentSession.ReadyAgents) == currentSession.TotalAgentCount {
		log.Printf("所有Agent(%d/%d)都已准备好，准备完成密钥更新", 
			len(currentSession.ReadyAgents), currentSession.TotalAgentCount)
		
		// 设置一个延迟，给所有Agent足够的时间进行更新
		go func() {
			// 等待最慢的Agent完成更新
			time.Sleep(time.Duration(payload.ReadyIn+2) * time.Second)
			
			// 完成会话
			select {
			case <-s.stopChan:
				return
			default:
				currentSession.OnComplete(true)
			}
		}()
	}
}

// 保存安全密钥到文件
func (s *Server) saveSecurityKey(keyFile, key string) error {
	// 检查文件是否是INI格式的配置文件
	if strings.Contains(keyFile, "conf/app.conf") || strings.HasSuffix(keyFile, ".conf") || strings.HasSuffix(keyFile, ".ini") {
		log.Printf("保存密钥到配置文件: %s", keyFile)
		
		// 检查文件是否存在
		fileExists := true
		fileInfo, err := os.Stat(keyFile)
		if err != nil {
			if os.IsNotExist(err) {
				fileExists = false
			} else {
				return fmt.Errorf("检查配置文件状态失败: %v", err)
			}
		} else if fileInfo.IsDir() {
			return fmt.Errorf("指定的配置文件路径是一个目录: %s", keyFile)
		}
		
		// 如果文件已存在，检查是否是INI格式
		var isJSON bool
		if fileExists {
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
			isJSON = bytes.HasPrefix(bytes.TrimSpace(header), []byte{'{'})
		}
		
		if !fileExists || isJSON {
			// 文件不存在或是JSON格式，创建/转换为INI格式
			log.Printf("创建/转换为INI格式配置文件")
			
			// 创建一个新的INI配置
			cfg := ini.Empty()
			
			// 添加基本节和密钥
			section, _ := cfg.NewSection("server")
			section.Key("SECURITY_KEY").SetValue(key)
			
			// 保存配置
			if err := cfg.SaveTo(keyFile); err != nil {
				return fmt.Errorf("保存INI配置文件失败: %v", err)
			}
			
			log.Printf("成功创建/转换为INI格式并保存密钥")
			return nil
		}
		
		// 文件存在且是INI格式，更新文件
		log.Printf("更新INI格式配置文件中的密钥")
		
		// 加载现有配置
		cfg, err := ini.Load(keyFile)
		if err != nil {
			return fmt.Errorf("读取配置文件失败: %v", err)
		}
		
		// 更新server段的密钥
		section, err := cfg.GetSection("server")
		if err != nil {
			// 如果server段不存在，创建它
			section, err = cfg.NewSection("server")
			if err != nil {
				return fmt.Errorf("创建配置段失败: %v", err)
			}
		}
		
		// 设置密钥
		section.Key("SECURITY_KEY").SetValue(key)
		
		// 保存配置
		if err := cfg.SaveTo(keyFile); err != nil {
			return fmt.Errorf("保存配置文件失败: %v", err)
		}
		
		log.Printf("密钥已保存到配置文件: %s [server].SECURITY_KEY", keyFile)
		return nil
	}
	
	// 默认使用JSON格式保存
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
func (s *Server) loadSecurityKey(keyFile string) (string, error) {
	// 检查文件是否是INI格式的配置文件
	if strings.Contains(keyFile, "conf/app.conf") || strings.HasSuffix(keyFile, ".conf") || strings.HasSuffix(keyFile, ".ini") {
		log.Printf("从配置文件加载密钥: %s", keyFile)
		
		// 检查文件是否存在
		if _, err := os.Stat(keyFile); os.IsNotExist(err) {
			return "", fmt.Errorf("配置文件不存在: %s", keyFile)
		}
		
		// 尝试读取文件前几个字节来判断是JSON还是INI
		f, err := os.Open(keyFile)
		if err != nil {
			return "", fmt.Errorf("无法打开配置文件: %v", err)
		}
		
		// 读取前100个字节来判断格式
		header := make([]byte, 100)
		_, err = f.Read(header)
		f.Close()
		if err != nil && err != io.EOF {
			return "", fmt.Errorf("读取配置文件头部失败: %v", err)
		}
		
		// 判断是否是JSON格式
		isJSON := bytes.HasPrefix(bytes.TrimSpace(header), []byte{'{'})
		
		if isJSON {
			// 如果是JSON格式，按JSON方式读取
			log.Printf("检测到JSON格式配置文件，将按JSON格式读取")
			
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
		
		// 按INI格式读取
		log.Printf("按INI格式读取配置文件")
		
		// 加载配置
		cfg, err := ini.Load(keyFile)
		if err != nil {
			return "", fmt.Errorf("读取配置文件失败: %v", err)
		}
		
		// 尝试从[server]部分读取密钥
		section := cfg.Section("server")
		if section.HasKey("SECURITY_KEY") {
			keyValue := section.Key("SECURITY_KEY").String()
			if keyValue != "" {
				log.Printf("从配置文件的[server]部分成功加载密钥")
				return keyValue, nil
			}
		}
		
		// 如果在[server]部分找不到，尝试从[agent]部分读取
		// 这是为了兼容性，因为agent可能会把密钥写入[agent]部分
		section = cfg.Section("agent")
		if section.HasKey("SECURITY_KEY") {
			keyValue := section.Key("SECURITY_KEY").String()
			if keyValue != "" {
				log.Printf("从配置文件的[agent]部分成功加载密钥")
				return keyValue, nil
			}
		}
		
		return "", fmt.Errorf("在配置文件中未找到有效的密钥设置")
	}
	
	// 默认按JSON方式读取
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