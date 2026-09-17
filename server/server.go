package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"encoding/base64"

	"dont/shared"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var agentIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var agentReleaseIDPattern = regexp.MustCompile(`^[a-f0-9-]{36}$`)
var agentReleaseVersionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z._+-]{0,63}$`)
var agentReleaseSHA256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

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

func allowAgentOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		// Native Agents do not send Origin. Browser clients must be same-origin.
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded == "http" || forwarded == "https" {
		scheme = forwarded
	}
	return strings.EqualFold(parsed.Scheme, scheme) && strings.EqualFold(parsed.Host, r.Host)
}

func agentAuthorizationKey(r *http.Request) (string, bool, error) {
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if authorization != "" {
		parts := strings.Fields(authorization)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
			return "", false, errors.New("invalid Agent authorization header")
		}
		return parts[1], false, nil
	}
	if key := strings.TrimSpace(r.URL.Query().Get("key")); key != "" {
		return key, true, nil
	}
	return "", false, errors.New("missing Agent authorization")
}

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
	keyUpdateSessions *KeyUpdateSessionManager  // 密钥更新会话管理器
	commandResults    map[string]*CommandResult // 存储命令执行结果
	commandMutex      sync.RWMutex              // 命令结果互斥锁
	stopOnce          sync.Once
}

// Config 服务器配置
type Config struct {
	ListenAddr  string // 监听地址
	TLSCert     string // TLS证书文件
	TLSKey      string // TLS密钥文件
	KeyFile     string // 通信密钥文件
	SecurityKey string // 通信安全密钥
}

// KeyUpdateSession 表示一次密钥更新会话
type KeyUpdateSession struct {
	ID              string
	ProposedKey     string
	StartTime       time.Time
	Status          string // 'pending', 'completed', 'failed'
	ReadyAgents     map[string]bool
	TotalAgentCount int
	Timeout         time.Duration
	TimeoutTimer    *time.Timer
	OnComplete      func(success bool, key string)
	Mutex           sync.Mutex
}

// KeyUpdateSessionManager 管理所有密钥更新会话
type KeyUpdateSessionManager struct {
	CurrentSession    *KeyUpdateSession
	CompletedSessions int
	FailedSessions    int
	Mutex             sync.Mutex
}

// CommandResult 命令执行结果
type CommandResult struct {
	AgentID   string                         `json:"agent_id"`   // 执行命令的Agent ID
	CommandID string                         `json:"command_id"` // 命令ID
	Type      string                         `json:"type"`       // 命令类型
	Content   string                         `json:"content"`    // 命令内容
	Output    string                         `json:"output"`     // 命令输出
	ErrorMsg  string                         `json:"error_msg"`  // 错误信息
	ExitCode  int                            `json:"exit_code"`  // 退出码
	Success   bool                           `json:"success"`    // 是否成功
	StartTime int64                          `json:"start_time"` // 开始时间
	EndTime   int64                          `json:"end_time"`   // 结束时间
	Status    string                         `json:"status"`     // 状态：pending, completed, failed
	Progress  *shared.CommandProgressPayload `json:"progress,omitempty"`
	changed   chan struct{}
}

// NewKeyUpdateSessionManager 创建新的密钥更新会话管理器
func NewKeyUpdateSessionManager() *KeyUpdateSessionManager {
	return &KeyUpdateSessionManager{
		CompletedSessions: 0,
		FailedSessions:    0,
	}
}

// NewServer 创建新的服务器实例
func NewServer(config *Config) (*Server, error) {
	if config == nil {
		return nil, errors.New("Agent 网关配置不能为空")
	}
	if strings.TrimSpace(config.KeyFile) == "" {
		config.KeyFile = "./conf/app.conf"
	}
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

	currentKey := keyManager.GetKey()
	if configured := strings.TrimSpace(config.SecurityKey); configured != "" && configured != currentKey {
		if err := keyManager.SetKey(configured); err != nil {
			keyManager.StopWatching()
			return nil, fmt.Errorf("应用 Agent 网关通信密钥: %w", err)
		}
		currentKey = configured
	}
	log.Printf("服务器通信密钥已加载")

	// 同步服务器配置中的密钥
	config.SecurityKey = currentKey

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
			CheckOrigin:     allowAgentOrigin,
		},
		commandResults: make(map[string]*CommandResult),
	}

	// 设置密钥变更回调
	keyManager.SetKeyChangedCallback(func(newKey string) {
		log.Printf("检测到通信密钥变更")
		log.Printf("服务器已应用新的通信密钥")

		// 向所有连接的客户端广播密钥变更通知
		server.agentMutex.RLock()
		agentCount := len(server.agents)
		var agentList []string
		for agentID := range server.agents {
			agentList = append(agentList, agentID)
		}
		server.agentMutex.RUnlock()

		if agentCount > 0 {
			log.Printf("检测到 %d 个已连接的客户端，将通知密钥变更: %v", agentCount, agentList)
			// 创建一个新的会话以推送密钥变更
			go func() {
				session, err := server.createKeyUpdateSession(newKey)
				if err != nil {
					log.Printf("无法创建密钥更新会话: %v", err)
					return
				}

				if session != nil {
					server.broadcastKeyUpdateProposal(session, newKey)
				} else {
					log.Printf("无需创建密钥更新会话，可能没有连接的客户端或会话已经存在")
				}
			}()
		} else {
			log.Printf("没有已连接的客户端，无需广播密钥变更")
		}
	})

	// 启动命令结果清理协程
	go server.cleanupCommandResults()

	return server, nil
}

// Start 启动服务器
func (s *Server) Start() error {
	log.Println("服务器开始启动...")

	// 创建和配置HTTP服务器
	httpServer := &http.Server{
		Addr:         s.Config.ListenAddr,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	mux := http.NewServeMux()
	mux.Handle("/agent", s.Handler())
	httpServer.Handler = mux

	// 创建错误通道
	errChan := make(chan error, 1)

	// 启动HTTP服务
	go func() {
		var err error
		log.Printf("准备启动HTTP服务器，监听地址: %s", s.Config.ListenAddr)

		// 根据配置决定是否使用TLS
		if s.Config.TLSCert != "" && s.Config.TLSKey != "" {
			log.Printf("使用TLS启动服务器，监听: %s", s.Config.ListenAddr)
			err = httpServer.ListenAndServeTLS(s.Config.TLSCert, s.Config.TLSKey)
		} else {
			log.Printf("以非TLS模式启动服务器，监听: %s", s.Config.ListenAddr)
			err = httpServer.ListenAndServe()
		}

		if err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP服务启动失败: %v", err)
			errChan <- err
		}
	}()

	// 启动清理过期连接的goroutine
	go s.cleanupExpiredConnections()

	// 等待停止信号或错误
	select {
	case <-s.stopChan:
		log.Println("收到停止信号，正在关闭HTTP服务...")
		// 关闭HTTP服务
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("HTTP服务关闭错误: %v", err)
		}
		log.Println("HTTP服务已关闭")
		return nil
	case err := <-errChan:
		log.Printf("服务器发生错误: %v", err)
		return err
	}
}

// Stop 停止服务器
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
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
	})
}

// Handler exposes the authenticated Agent WebSocket on the control-plane
// HTTP server without starting a second listener.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.handleAgentConnection)
}

// Maintain runs connection expiry under the owning Application lifecycle.
func (s *Server) Maintain(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopChan:
			return
		case <-ticker.C:
			s.expireConnections(time.Now())
		}
	}
}

// 处理Agent连接
func (s *Server) handleAgentConnection(w http.ResponseWriter, r *http.Request) {
	authKey, legacyQuery, err := agentAuthorizationKey(r)
	if err != nil || s.keyManager == nil || !s.keyManager.ValidateKey(authKey) {
		log.Printf("拒绝 Agent 连接：通信密钥缺失或无效")
		http.Error(w, "Agent 认证失败", http.StatusUnauthorized)
		return
	}
	if legacyQuery {
		w.Header().Set("Deprecation", "true")
		w.Header().Set("Warning", `299 - "URL key authentication is deprecated; use Authorization: Bearer"`)
		log.Printf("Agent 使用已弃用的 URL key 认证，来源: %s", r.RemoteAddr)
	}

	// 升级HTTP连接为WebSocket
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("升级连接失败: %v", err)
		return
	}
	conn.SetReadLimit(shared.MaximumRegistrationMessageBytes)

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

	// 使用AgentUUID作为唯一标识符
	agentID := regPayload.AgentUUID

	// 检查AgentUUID是否为空，如果为空则回退到使用消息中的AgentID
	if agentID == "" {
		log.Printf("警告: Agent未提供UUID，将使用消息中的ID: %s", msg.AgentID)
		agentID = msg.AgentID
	}

	// Agent identity is stable and human-readable; legacy numeric UUIDs remain valid.
	if !agentIdentityPattern.MatchString(agentID) {
		log.Printf("错误: Agent提供的 ID 无效，连接将被拒绝")
		conn.Close()
		return
	}

	// 检查UUID是否已存在，避免重复标识符
	s.agentMutex.Lock()
	if _, exists := s.agents[agentID]; exists {
		log.Printf("检测到重复的Agent UUID: %s，关闭旧连接", agentID)
		existingAgent := s.agents[agentID]
		existingAgent.Mutex.Lock()
		if existingAgent.Connection != nil {
			existingAgent.Connection.Close()
		}
		existingAgent.Mutex.Unlock()
	}

	// 存储Agent连接信息
	agentConn := &AgentConnection{
		AgentID:       agentID,
		Connection:    secureConn,
		PublicKey:     agentPubKey,
		LastHeartbeat: time.Now(),
		Info: map[string]interface{}{
			"hostname":      regPayload.Hostname,
			"os":            regPayload.OS,
			"arch":          regPayload.Arch,
			"agent_version": regPayload.Version,
			"agent_uuid":    agentID, // 确保UUID也存储在信息中
		},
	}
	s.agents[agentID] = agentConn
	s.agentMutex.Unlock()

	log.Printf("Agent已注册: UUID=%s, 主机名=%s, OS=%s, Arch=%s", agentID, regPayload.Hostname, regPayload.OS, regPayload.Arch)

	// 立即请求system_info上报
	go func() {
		// 等待100ms确保注册流程完成
		time.Sleep(100 * time.Millisecond)

		// 通过被动上报请求立即上报系统信息
		requestPayload := map[string]interface{}{
			"report_type": "system_info",
			"params":      map[string]interface{}{},
		}
		requestMsg, err := shared.CreateMessage(shared.TypeReportRequest, "server", requestPayload)
		if err != nil {
			log.Printf("创建上报请求失败: %v", err)
			return
		}

		agentConn.Mutex.Lock()
		if agentConn.Connection != nil {
			if err := agentConn.Connection.SendEncrypted(requestMsg); err != nil {
				log.Printf("发送上报请求失败: %v", err)
			}
		}
		agentConn.Mutex.Unlock()
	}()

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

	case shared.TypeCommandProgress:
		s.handleCommandProgress(agent, msg)

	case shared.TypeCommandAck:
		// 处理命令确认
		s.handleCommandAck(agent, msg)

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

// 处理命令确认消息
func (s *Server) handleCommandAck(agent *AgentConnection, msg *shared.Message) {
	var ackPayload struct {
		CommandID string `json:"command_id"`
		Status    string `json:"status"`
	}

	if err := json.Unmarshal(msg.Payload, &ackPayload); err != nil {
		log.Printf("解析命令确认消息失败: %v", err)
		return
	}

	// 更新命令状态
	s.commandMutex.Lock()
	if result, exists := s.commandResults[ackPayload.CommandID]; exists && result.AgentID == agent.AgentID &&
		result.Status != "completed" && result.Status != "failed" {
		if ackPayload.Status == "rejected" {
			result.Status = "failed"
			result.ErrorMsg = "Agent 拒绝接收命令"
			result.EndTime = time.Now().Unix()
			notifyCommandResultLocked(result)
		} else {
			result.Status = "received"
		}
	}
	s.commandMutex.Unlock()
	if ackPayload.Status == "rejected" {
		log.Printf("Agent命令被拒绝: AgentID=%s CommandID=%s", agent.AgentID, ackPayload.CommandID)
	}
}

func (s *Server) handleCommandProgress(agent *AgentConnection, msg *shared.Message) {
	var progress shared.CommandProgressPayload
	if err := json.Unmarshal(msg.Payload, &progress); err != nil || !validCommandProgress(progress) {
		return
	}
	s.commandMutex.Lock()
	defer s.commandMutex.Unlock()
	result, exists := s.commandResults[progress.CommandID]
	if !exists || result.AgentID != agent.AgentID || result.Status == "completed" || result.Status == "failed" ||
		result.Progress != nil && result.Progress.Sequence >= progress.Sequence {
		return
	}
	value := progress
	result.Progress = &value
	notifyCommandResultLocked(result)
}

func validCommandProgress(progress shared.CommandProgressPayload) bool {
	if len(progress.Items) > 4096 {
		return false
	}
	for _, item := range progress.Items {
		if item.WorkshopID == "" || len(item.WorkshopID) > 32 || len(item.Message) > 1000 ||
			len(item.TargetID) > 256 || len(item.InstallationID) > 256 ||
			(item.Status != "queued" && item.Status != "downloading" && item.Status != "succeeded" && item.Status != "failed") ||
			item.CurrentBytes < 0 || item.TotalBytes < 0 || item.BytesPerSecond < 0 || item.TotalBytes > 0 && item.CurrentBytes > item.TotalBytes {
			return false
		}
	}
	message := strings.TrimSpace(progress.Message)
	return strings.TrimSpace(progress.CommandID) != "" && len(progress.CommandID) <= 128 && progress.Sequence > 0 && progress.Percent >= 0 && progress.Percent <= 100 &&
		message != "" && len(message) <= 1000 && len(progress.Stage) <= 128 && len(progress.WorkshopID) <= 32 &&
		progress.CurrentItem >= 0 && progress.TotalItems >= 0 && (progress.TotalItems == 0 || progress.CurrentItem <= progress.TotalItems) &&
		progress.CurrentBytes >= 0 && progress.TotalBytes >= 0 &&
		progress.BytesPerSecond >= 0 && (progress.TotalBytes == 0 || progress.CurrentBytes <= progress.TotalBytes)
}

// 处理主动上报
func (s *Server) handleActiveReport(agent *AgentConnection, msg *shared.Message) {
	var reportPayload shared.ReportDataPayload
	if err := json.Unmarshal(msg.Payload, &reportPayload); err != nil {
		log.Printf("解析上报负载失败: %v", err)
		return
	}

	// 存储上报数据（这里只是示例，实际应用可能需要存入数据库等）
	agent.Mutex.Lock()
	for k, v := range reportPayload.Data {
		agent.Info[k] = v
	}
	if reportPayload.ReportType == "system_info" {
		agent.Info["_last_system_report_at"] = time.Now().UnixNano()
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

	// 存储上报数据
	agent.Mutex.Lock()
	if reportPayload.ReportType == "dst_runtime_inventory" {
		delete(agent.Info, "inventory")
		delete(agent.Info, "error")
	}
	for k, v := range reportPayload.Data {
		agent.Info[k] = v
	}
	switch reportPayload.ReportType {
	case "dst_runtime_inventory":
		// Keep the receipt and its result together; unrelated reports share Info.
		agent.Info["_last_inventory_report_at"] = time.Now().UnixNano()
		agent.Info["_last_inventory_report"] = reportPayload.Data
	case "system_info":
		agent.Info["_last_system_report_at"] = time.Now().UnixNano()
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

	if !respPayload.Success {
		detail := strings.TrimSpace(respPayload.ErrorMsg)
		if detail == "" {
			detail = strings.TrimSpace(respPayload.Output)
			if len(detail) > 200 {
				detail = detail[:200] + "..."
			}
		}
		log.Printf("Agent命令执行失败: AgentID=%s CommandID=%s ExitCode=%d Error=%s",
			agent.AgentID, respPayload.CommandID, respPayload.ExitCode, detail)
	}

	// 保存命令执行结果
	s.commandMutex.Lock()
	cmdResult, exists := s.commandResults[respPayload.CommandID]
	if exists {
		if cmdResult.AgentID != agent.AgentID {
			s.commandMutex.Unlock()
			return
		}
		// 更新现有结果
		cmdResult.Output = respPayload.Output
		cmdResult.ErrorMsg = respPayload.ErrorMsg
		cmdResult.ExitCode = respPayload.ExitCode
		cmdResult.Success = respPayload.Success
		cmdResult.EndTime = time.Now().Unix()
		cmdResult.Status = "completed"
		if !respPayload.Success {
			cmdResult.Status = "failed"
		}
		notifyCommandResultLocked(cmdResult)
	} else {
		log.Printf("Agent命令响应缺少本地记录，正在补建: AgentID=%s CommandID=%s", agent.AgentID, respPayload.CommandID)
		// 创建新的结果记录
		s.commandResults[respPayload.CommandID] = &CommandResult{
			AgentID:   agent.AgentID,
			CommandID: respPayload.CommandID,
			Output:    respPayload.Output,
			ErrorMsg:  respPayload.ErrorMsg,
			ExitCode:  respPayload.ExitCode,
			Success:   respPayload.Success,
			EndTime:   time.Now().Unix(),
			Status:    "completed",
		}
		if !respPayload.Success {
			s.commandResults[respPayload.CommandID].Status = "failed"
		}
	}

	s.commandMutex.Unlock()
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
			s.expireConnections(time.Now())
		}
	}
}

func (s *Server) expireConnections(now time.Time) {
	expiredAgents := []string{}
	s.agentMutex.RLock()
	for id, agent := range s.agents {
		agent.Mutex.Lock()
		if agent.LastHeartbeat.Add(2 * time.Minute).Before(now) {
			expiredAgents = append(expiredAgents, id)
		}
		agent.Mutex.Unlock()
	}
	s.agentMutex.RUnlock()
	if len(expiredAgents) == 0 {
		return
	}
	s.agentMutex.Lock()
	defer s.agentMutex.Unlock()
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
}

// 向指定Agent发送执行命令请求
func (s *Server) SendCommand(agentID, commandType, content string, timeout int) (string, error) {
	if commandType != "exec" || timeout < 5 || timeout > 300 {
		return "", fmt.Errorf("Agent 只允许白名单领域动作")
	}
	var request struct {
		Program   string   `json:"program"`
		Arguments []string `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(content), &request); err != nil || !allowedAgentExec(request.Program, request.Arguments) {
		return "", fmt.Errorf("Agent 参数数组命令不在白名单中")
	}
	return s.sendCommandPayload(agentID, shared.CommandPayload{Type: commandType, Content: content, Timeout: timeout}, content)
}

// SendShardOperation sends a fixed, structured Shard action. The request does
// not contain paths or shell content; the Agent resolves InstallationID from
// its local trusted registry.
func (s *Server) SendShardOperation(agentID string, request shared.ShardOperationRequest, timeout int) (string, error) {
	if request.ProtocolVersion != shared.ShardOperationProtocolVersion || !shared.IsShardAction(request.Action) ||
		timeout < 5 || timeout > 300 || strings.TrimSpace(request.InstallationID) == "" ||
		strings.TrimSpace(request.Cluster) == "" || strings.TrimSpace(request.Shard) == "" ||
		strings.ContainsAny(request.InstallationID+request.Cluster+request.Shard+request.TopologyRevision, "\x00\r\n") {
		return "", fmt.Errorf("Agent 分片操作请求无效")
	}
	if shared.ShardActionMutates(request.Action) && (request.FencingToken == 0 || request.LeaseExpiresAt == nil || strings.TrimSpace(request.LeaseID) == "") {
		return "", fmt.Errorf("Agent 分片操作缺少租约或 fencing token")
	}
	content, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	requestCopy := request
	return s.sendCommandPayload(agentID, shared.CommandPayload{
		Type: string(request.Action), ShardOperation: &requestCopy, Timeout: timeout,
	}, string(content))
}

func (s *Server) SendRuntimeOperation(agentID string, request shared.RuntimeOperationRequest, timeout int) (string, error) {
	if request.ProtocolVersion != shared.RuntimeOperationProtocolVersion || !shared.IsRuntimeAction(request.Action) ||
		timeout < 5 || timeout > runtimeOperationTimeoutLimit(request.Action) || strings.TrimSpace(request.InstallationID) == "" ||
		strings.TrimSpace(request.Cluster) == "" || strings.TrimSpace(request.Shard) == "" ||
		strings.ContainsAny(request.InstallationID+request.Cluster+request.Shard+request.TopologyRevision, "\x00\r\n") {
		return "", fmt.Errorf("Agent Runtime 操作请求无效")
	}
	if shared.RuntimeOperationRequiresLease(request) && (request.FencingToken == 0 || request.LeaseExpiresAt == nil || strings.TrimSpace(request.LeaseID) == "") {
		return "", fmt.Errorf("Agent Runtime 操作缺少租约或 fencing token")
	}
	content, err := runtimeOperationAuditContent(request)
	if err != nil {
		return "", err
	}
	requestCopy := request
	return s.sendCommandPayload(agentID, shared.CommandPayload{
		Type: string(request.Action), RuntimeOperation: &requestCopy, Timeout: timeout,
	}, content)
}

func runtimeOperationAuditContent(request shared.RuntimeOperationRequest) (string, error) {
	audit := request
	if request.Console != nil {
		value := *request.Console
		if request.Console.CommandDocument != nil {
			document := *request.Console.CommandDocument
			document.Data = nil
			value.CommandDocument = &document
		}
		audit.Console = &value
	}
	if request.Migration != nil {
		value := *request.Migration
		value.Data = nil
		value.FetchLocations = make([]shared.RuntimeMigrationFetchLocation, len(request.Migration.FetchLocations))
		for index, location := range request.Migration.FetchLocations {
			location.DownloadToken = ""
			value.FetchLocations[index] = location
		}
		audit.Migration = &value
	}
	if request.Backup != nil {
		value := *request.Backup
		value.Data = nil
		audit.Backup = &value
	}
	if request.Mod != nil {
		value := *request.Mod
		value.Data = nil
		value.FetchSources = append([]shared.RuntimeModFetchSource(nil), request.Mod.FetchSources...)
		value.FetchLocations = make([]shared.RuntimeModFetchLocation, len(request.Mod.FetchLocations))
		for index, location := range request.Mod.FetchLocations {
			location.DownloadToken = ""
			value.FetchLocations[index] = location
		}
		audit.Mod = &value
	}
	if request.Configuration != nil {
		value := *request.Configuration
		value.Data = nil
		audit.Configuration = &value
	}
	content, err := json.Marshal(audit)
	return string(content), err
}

func runtimeOperationTimeoutLimit(action shared.RuntimeAction) int {
	if action == shared.RuntimeActionGameVersionUpdate || action == shared.RuntimeActionMigrationFetch {
		return 1800
	}
	return 300
}

func (s *Server) SendAgentUpgrade(agentID string, request shared.AgentUpgradeRequest, timeout int) (string, error) {
	if request.ProtocolVersion != shared.AgentUpgradeProtocolVersion || timeout < 30 || timeout > 300 ||
		!agentReleaseIDPattern.MatchString(request.ReleaseID) || !agentReleaseVersionPattern.MatchString(request.Version) ||
		(request.OS != "linux" && request.OS != "darwin" && request.OS != "windows") ||
		(request.Arch != "amd64" && request.Arch != "arm64") ||
		request.DownloadPath != "/agent-updates/"+request.ReleaseID || strings.TrimSpace(request.DownloadToken) == "" ||
		len(request.DownloadToken) > 256 || !agentReleaseSHA256Pattern.MatchString(request.SHA256) ||
		request.Size < 1 || request.Size > 128*1024*1024 ||
		strings.ContainsAny(request.DownloadToken, "\x00\r\n") {
		return "", fmt.Errorf("Agent 升级请求无效")
	}
	requestCopy := request
	audit, _ := json.Marshal(map[string]interface{}{
		"release_id": request.ReleaseID, "version": request.Version, "os": request.OS,
		"arch": request.Arch, "sha256": request.SHA256, "size": request.Size,
	})
	return s.sendCommandPayload(agentID, shared.CommandPayload{
		Type: shared.AgentUpgradeCommand, AgentUpgrade: &requestCopy, Timeout: timeout,
	}, string(audit))
}

func (s *Server) sendCommandPayload(agentID string, payload shared.CommandPayload, auditContent string) (string, error) {

	// 查找Agent
	s.agentMutex.RLock()
	agent, exists := s.agents[agentID]
	s.agentMutex.RUnlock()

	if !exists {
		log.Printf("发送命令失败: 未找到指定的Agent: %s", agentID)
		return "", fmt.Errorf("未找到指定的Agent: %s", agentID)
	}

	// 创建命令ID
	commandID := shared.GenerateCommandID() // 使用专用的命令ID生成函数
	// 创建命令负载
	payload.CommandID = commandID

	// 在结果map中记录命令
	s.commandMutex.Lock()
	if s.commandResults == nil {
		s.commandResults = make(map[string]*CommandResult)
	}
	s.commandResults[commandID] = &CommandResult{
		AgentID:   agentID,
		CommandID: commandID,
		Type:      payload.Type,
		Content:   auditContent,
		StartTime: time.Now().Unix(),
		Status:    "pending",
	}
	s.commandMutex.Unlock()

	// 创建命令消息
	cmdMsg, err := shared.CreateMessage(shared.TypeCommand, "server", payload)
	if err != nil {
		// 更新命令状态为失败
		s.commandMutex.Lock()
		s.commandResults[commandID].Status = "failed"
		s.commandResults[commandID].ErrorMsg = fmt.Sprintf("创建命令消息失败: %v", err)
		s.commandResults[commandID].EndTime = time.Now().Unix()
		s.commandMutex.Unlock()

		log.Printf("创建命令消息失败: %v, 命令ID: %s", err, commandID)
		return "", fmt.Errorf("创建命令消息失败: %v", err)
	}

	// 发送命令
	agent.Mutex.Lock()
	if agent.Connection == nil {
		agent.Mutex.Unlock()

		// 更新命令状态为失败
		s.commandMutex.Lock()
		s.commandResults[commandID].Status = "failed"
		s.commandResults[commandID].ErrorMsg = fmt.Sprintf("Agent连接已关闭: %s", agentID)
		s.commandResults[commandID].EndTime = time.Now().Unix()
		s.commandMutex.Unlock()

		log.Printf("发送命令失败: Agent连接已关闭: %s, 命令ID: %s", agentID, commandID)
		return "", fmt.Errorf("Agent连接已关闭: %s", agentID)
	}

	err = agent.Connection.SendEncrypted(cmdMsg)
	agent.Mutex.Unlock()

	if err != nil {
		// 更新命令状态为失败
		s.commandMutex.Lock()
		s.commandResults[commandID].Status = "failed"
		s.commandResults[commandID].ErrorMsg = fmt.Sprintf("发送命令失败: %v", err)
		s.commandResults[commandID].EndTime = time.Now().Unix()
		s.commandMutex.Unlock()

		log.Printf("发送命令失败: %v, 命令ID: %s", err, commandID)
		return "", fmt.Errorf("发送命令失败: %v", err)
	}

	return commandID, nil
}

// SendExec sends a structured program plus argument array. It never asks the
// Agent to parse user-controlled shell syntax.
func (s *Server) SendExec(agentID, program string, arguments []string, timeout int) (string, error) {
	program = strings.TrimSpace(program)
	if !allowedAgentExec(program, arguments) {
		return "", fmt.Errorf("invalid argument command")
	}
	content, err := json.Marshal(struct {
		Program   string   `json:"program"`
		Arguments []string `json:"arguments"`
	}{Program: program, Arguments: arguments})
	if err != nil {
		return "", err
	}
	return s.SendCommand(agentID, "exec", string(content), timeout)
}

// 请求Agent进行被动上报
func (s *Server) RequestPassiveReport(agentID, reportType string, params map[string]interface{}) error {
	if reportType != "system_info" && reportType != "process_list" && reportType != "dst_runtime_inventory" {
		return fmt.Errorf("Agent 上报类型不在白名单中")
	}
	if reportType != "dst_runtime_inventory" && len(params) != 0 {
		return fmt.Errorf("Agent 上报不接受自定义参数")
	}
	if reportType == "dst_runtime_inventory" {
		if len(params) == 0 || len(params) > 5 {
			return fmt.Errorf("DST 运行时清单参数无效")
		}
		encoded, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("DST 运行时清单参数无效")
		}
		var request shared.RuntimeInventoryRequest
		if err := json.Unmarshal(encoded, &request); err != nil || strings.TrimSpace(request.InstallationID) == "" ||
			strings.TrimSpace(request.SavePath) == "" || strings.TrimSpace(request.ServerPath) == "" ||
			len(request.InstallationID) > 128 || len(request.DisplayName) > 100 || len(request.SavePath) > 2048 || len(request.ServerPath) > 2048 ||
			strings.ContainsAny(request.InstallationID+request.DisplayName+request.SavePath+request.ServerPath, "\x00\r\n") {
			return fmt.Errorf("DST 运行时清单参数无效")
		}
	}
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

	return nil
}

func allowedAgentExec(program string, arguments []string) bool {
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

// 获取所有连接的Agent信息
func (s *Server) GetAllAgentInfo() map[string]map[string]interface{} {
	result := make(map[string]map[string]interface{})

	s.agentMutex.RLock()
	defer s.agentMutex.RUnlock()

	for id, agent := range s.agents {
		result[id] = copyAgentInfo(id, agent)
	}

	return result
}

func (s *Server) GetAgentInfo(agentID string) (map[string]interface{}, bool) {
	s.agentMutex.RLock()
	defer s.agentMutex.RUnlock()
	agent, exists := s.agents[agentID]
	if !exists {
		return nil, false
	}
	return copyAgentInfo(agentID, agent), true
}

func copyAgentInfo(agentID string, agent *AgentConnection) map[string]interface{} {
	agent.Mutex.Lock()
	defer agent.Mutex.Unlock()
	info := make(map[string]interface{}, len(agent.Info)+3)
	for key, value := range agent.Info {
		info[key] = value
	}
	info["last_heartbeat"] = agent.LastHeartbeat.Unix()
	info["connected"] = agent.Connection != nil
	if _, exists := info["agent_uuid"]; !exists {
		info["agent_uuid"] = agentID
	}
	return info
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
		if err := s.keyManager.SetKey(proposedKey); err != nil {
			log.Printf("保存新密钥失败: %v", err)
			return nil, err
		}
		log.Printf("服务器通信密钥已更新")
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
		OnComplete: func(success bool, key string) {
			if success {
				s.keyUpdateSessions.Mutex.Lock()
				if s.keyUpdateSessions.CurrentSession != nil &&
					s.keyUpdateSessions.CurrentSession.ID == sessionID {
					s.keyUpdateSessions.CurrentSession.Status = "completing"
				}
				s.keyUpdateSessions.Mutex.Unlock()

				if err := s.keyManager.SetKey(key); err != nil {
					log.Printf("密钥更新会话应用失败: %v", err)
					success = false
				} else {
					log.Printf("密钥更新会话成功完成")
				}
			}

			if success {
				s.keyUpdateSessions.Mutex.Lock()
				if s.keyUpdateSessions.CurrentSession != nil &&
					s.keyUpdateSessions.CurrentSession.ID == sessionID {
					s.keyUpdateSessions.CurrentSession.Status = "completed"
					s.keyUpdateSessions.CompletedSessions++
					log.Printf("密钥更新会话 %s 已正式完成", sessionID)
				}
				s.keyUpdateSessions.Mutex.Unlock()
			} else {
				log.Printf("密钥更新会话失败或取消")
				s.keyUpdateSessions.Mutex.Lock()
				if s.keyUpdateSessions.CurrentSession != nil &&
					s.keyUpdateSessions.CurrentSession.ID == sessionID {
					s.keyUpdateSessions.CurrentSession.Status = "failed"
					s.keyUpdateSessions.FailedSessions++
				}
				s.keyUpdateSessions.Mutex.Unlock()
			}

			// 清理会话
			s.keyUpdateSessions.Mutex.Lock()
			if s.keyUpdateSessions.CurrentSession != nil &&
				s.keyUpdateSessions.CurrentSession.ID == sessionID {
				s.keyUpdateSessions.CurrentSession = nil
			}
			s.keyUpdateSessions.Mutex.Unlock()
		},
	}

	// 设置超时定时器
	session.TimeoutTimer = time.AfterFunc(session.Timeout, func() {
		log.Printf("会话 %s 定时器触发", sessionID)

		// 获取会话的当前状态
		s.keyUpdateSessions.Mutex.Lock()
		var currentSession *KeyUpdateSession
		var shouldComplete bool

		if s.keyUpdateSessions.CurrentSession != nil &&
			s.keyUpdateSessions.CurrentSession.ID == sessionID {
			currentSession = s.keyUpdateSessions.CurrentSession
			// 只有会话处于pending状态时才需要取消
			shouldComplete = currentSession.Status == "pending"

			// 记录准备好的代理数量
			readyCount := len(currentSession.ReadyAgents)
			totalCount := currentSession.TotalAgentCount
			s.keyUpdateSessions.Mutex.Unlock()

			if shouldComplete {
				if readyCount > 0 && readyCount == totalCount {
					// 所有代理都已经准备好，但可能卡在某个环节，强制完成
					log.Printf("所有代理已准备好但会话超时，强制完成更新: %d/%d",
						readyCount, totalCount)
					currentSession.OnComplete(true, currentSession.ProposedKey)
				} else {
					// 部分代理未准备好，取消会话
					log.Printf("密钥更新会话 %s 超时，当前已准备好的代理: %d/%d",
						sessionID, readyCount, totalCount)
					currentSession.OnComplete(false, "")
				}
			} else {
				log.Printf("会话 %s 已处于非pending状态 (%s)，不进行超时处理",
					sessionID, currentSession.Status)
			}
		} else {
			s.keyUpdateSessions.Mutex.Unlock()
			log.Printf("会话 %s 不再是当前会话，忽略超时", sessionID)
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
	log.Printf("已生成新的通信密钥")

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

	if err := shared.ValidateSecurityKey(newKey); err != nil {
		return err
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
		session.OnComplete(false, "")
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
		session.OnComplete(false, "")
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
	currentSession.ReadyAgents[agentID] = true
	readyCount := len(currentSession.ReadyAgents)
	totalCount := currentSession.TotalAgentCount
	currentSession.Mutex.Unlock()

	log.Printf("Agent(%s)已准备好在%d秒后更新密钥，当前进度: %d/%d",
		agentID, payload.ReadyIn, readyCount, totalCount)

	// 检查是否所有Agent都已准备好
	if readyCount == totalCount {
		log.Printf("所有Agent(%d/%d)都已准备好，准备完成密钥更新",
			readyCount, totalCount)

		// 设置一个定时器，给所有Agent足够的时间进行更新
		gracePeriod := time.Duration(payload.ReadyIn+2) * time.Second
		log.Printf("设置%s的宽限期，等待所有Agent断开连接并应用新密钥", gracePeriod)

		// 使用goroutine避免阻塞当前处理流程
		go func() {
			// 等待Agent完成断开连接
			time.Sleep(gracePeriod)

			// 检查会话是否仍然有效
			s.keyUpdateSessions.Mutex.Lock()
			isValid := s.keyUpdateSessions.CurrentSession != nil &&
				s.keyUpdateSessions.CurrentSession.ID == payload.SessionID &&
				s.keyUpdateSessions.CurrentSession.Status == "pending"
			s.keyUpdateSessions.Mutex.Unlock()

			if !isValid {
				log.Printf("会话 %s 不再有效，取消密钥更新", payload.SessionID)
				return
			}

			log.Printf("宽限期已结束，开始应用新密钥")

			// 完成会话并应用新密钥
			currentSession.OnComplete(true, payload.NewKey)
		}()
	}
}

// 获取命令执行结果
func (s *Server) GetCommandResult(commandID string) (*CommandResult, error) {
	s.commandMutex.RLock()
	defer s.commandMutex.RUnlock()

	// 直接通过命令ID查找结果
	result, exists := s.commandResults[commandID]
	if exists {
		value := commandResultSnapshot(result)
		return &value, nil
	}

	// 如果找不到命令结果，记录详细日志
	log.Printf("未找到命令结果 ID=%s, 当前结果数量: %d", commandID, len(s.commandResults))

	// 打印所有命令ID以便调试
	var commandIDs []string
	for id := range s.commandResults {
		commandIDs = append(commandIDs, id)
	}
	if len(commandIDs) > 0 {
		log.Printf("当前存在的命令ID: %v", commandIDs)
	}

	return nil, fmt.Errorf("未找到命令结果: %s", commandID)
}

// WaitCommandResult returns as soon as a result arrives. Progress callbacks run
// outside commandMutex, so they cannot block Agent message processing.
func (s *Server) WaitCommandResult(ctx context.Context, commandID string, onProgress func(shared.CommandProgressPayload)) (*CommandResult, error) {
	var progressSequence uint64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.commandMutex.Lock()
		result, exists := s.commandResults[commandID]
		if !exists {
			s.commandMutex.Unlock()
			return nil, fmt.Errorf("未找到命令结果: %s", commandID)
		}
		completed := result.Status == "completed" || result.Status == "failed"
		if !completed && result.changed == nil {
			result.changed = make(chan struct{})
		}
		// Capture the result and notification together to avoid missing a reply
		// between checking its status and starting to wait.
		changed := result.changed
		value := commandResultSnapshot(result)
		s.commandMutex.Unlock()

		if onProgress != nil && value.Progress != nil && value.Progress.Sequence > progressSequence {
			progressSequence = value.Progress.Sequence
			onProgress(*value.Progress)
		}
		if completed {
			return &value, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.stopChan:
			return nil, errors.New("Agent 网关已停止")
		case <-changed:
		}
	}
}

// commandMutex must be held. Only active waiters allocate a notification channel.
func notifyCommandResultLocked(result *CommandResult) {
	if result.changed != nil {
		close(result.changed)
		result.changed = nil
	}
}

// 获取命令执行结果列表
func (s *Server) GetCommandResults(agentID string, limit int) []*CommandResult {
	s.commandMutex.RLock()
	defer s.commandMutex.RUnlock()

	var results []*CommandResult

	// 复制结果到临时切片，如果指定了agentID则只返回该agent的结果
	for _, result := range s.commandResults {
		if agentID == "" || result.AgentID == agentID {
			value := commandResultSnapshot(result)
			results = append(results, &value)
		}
	}

	// 按时间倒序排序
	sort.Slice(results, func(i, j int) bool {
		return results[i].StartTime > results[j].StartTime
	})

	// 限制结果数量
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	return results
}

func commandResultSnapshot(result *CommandResult) CommandResult {
	value := *result
	value.changed = nil
	if result.Progress != nil {
		progress := *result.Progress
		value.Progress = &progress
	}
	return value
}

// 清理老旧命令结果
func (s *Server) cleanupCommandResults() {
	ticker := time.NewTicker(24 * time.Hour) // 每天清理一次
	defer ticker.Stop()

	for {
		select {
		case <-s.stopChan:
			return
		case <-ticker.C:
			// 保留最近7天的记录
			cutoffTime := time.Now().Add(-7 * 24 * time.Hour).Unix()

			s.commandMutex.Lock()
			for id, result := range s.commandResults {
				if result.EndTime < cutoffTime {
					notifyCommandResultLocked(result)
					delete(s.commandResults, id)
				}
			}
			s.commandMutex.Unlock()

			log.Printf("已清理老旧的命令执行结果")
		}
	}
}
