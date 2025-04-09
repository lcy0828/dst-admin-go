package gamelog

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edsrzf/mmap-go"
	"github.com/fsnotify/fsnotify"
	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// 配置变量
var (
	dstSavePath string // DST存档目录
)

// 初始化函数，从配置文件读取配置
func init() {
	// 默认配置
	dstSavePath = "./Klei/DoNotStarveTogether"

	// 尝试从配置文件读取
	configFile := "./conf/app.conf"
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		if cfg, err := ini.Load(configFile); err == nil {
			// 读取路径配置
			if cfg.Section("paths").HasKey("DST_SAVE_PATH") {
				dstSavePath = cfg.Section("paths").Key("DST_SAVE_PATH").String()
				log.Printf("从配置文件加载DST存档路径: %s", dstSavePath)
			}
		}
	} else {
		log.Printf("配置文件不存在，使用默认DST存档路径: %s", dstSavePath)
	}
}

// WebSocket升级器
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// 日志监控管理器
type LogWatcher struct {
	watcher       *fsnotify.Watcher
	logFile       string
	mmapData      mmap.MMap
	file          *os.File
	lastPosition  int64
	clients       map[*websocket.Conn]*ClientInfo
	clientsMutex  sync.Mutex
	stopChan      chan struct{}
	isRunning     bool
	runningMutex  sync.Mutex
	archiveName   string
	worldName     string
	ruleManager   *RuleManager
}

// ClientInfo 客户端信息
type ClientInfo struct {
	Conn        *websocket.Conn // WebSocket连接
	UseRules    bool           // 是否使用规则过滤
}

// 创建新的日志监控管理器
func NewLogWatcher(logFilePath string, archiveName, worldName string) (*LogWatcher, error) {
	// 创建fsnotify监控器
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("创建文件监控器失败: %v", err)
	}

	// 确保日志文件所在目录存在
	logDir := filepath.Dir(logFilePath)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("创建日志目录失败: %v", err)
	}

	// 如果日志文件不存在，创建一个空文件
	if _, err := os.Stat(logFilePath); os.IsNotExist(err) {
		emptyFile, err := os.Create(logFilePath)
		if err != nil {
			return nil, fmt.Errorf("创建日志文件失败: %v", err)
		}
		emptyFile.Close()
	}

	// 打开日志文件
	file, err := os.OpenFile(logFilePath, os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("打开日志文件失败: %v", err)
	}

	// 创建内存映射
	mmapData, err := mmap.Map(file, mmap.RDONLY, 0)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("创建内存映射失败: %v", err)
	}

	// 获取文件当前大小作为初始位置
	fileInfo, err := file.Stat()
	if err != nil {
		mmapData.Unmap()
		file.Close()
		return nil, fmt.Errorf("获取文件信息失败: %v", err)
	}

	// 创建日志监控管理器
	lw := &LogWatcher{
		watcher:      watcher,
		logFile:      logFilePath,
		mmapData:     mmapData,
		file:         file,
		lastPosition: fileInfo.Size(),
		clients:      make(map[*websocket.Conn]*ClientInfo),
		stopChan:     make(chan struct{}),
		archiveName:  archiveName,
		worldName:    worldName,
	}

	// 获取或创建规则管理器
	lw.ruleManager = GetOrCreateRuleManager(archiveName, worldName)

	return lw, nil
}

// 开始监控日志文件
func (lw *LogWatcher) Start() error {
	lw.runningMutex.Lock()
	defer lw.runningMutex.Unlock()

	if lw.isRunning {
		return fmt.Errorf("日志监控已经在运行中")
	}

	// 添加监控目录
	logDir := filepath.Dir(lw.logFile)
	if err := lw.watcher.Add(logDir); err != nil {
		return fmt.Errorf("添加目录监控失败: %v", err)
	}

	// 添加文件监控
	if err := lw.watcher.Add(lw.logFile); err != nil {
		return fmt.Errorf("添加文件监控失败: %v", err)
	}

	lw.isRunning = true
	go lw.watchLoop()

	log.Printf("开始监控日志文件: %s", lw.logFile)
	return nil
}

// 停止监控
func (lw *LogWatcher) Stop() {
	lw.runningMutex.Lock()
	defer lw.runningMutex.Unlock()

	if !lw.isRunning {
		return
	}

	close(lw.stopChan)
	lw.watcher.Close()
	lw.mmapData.Unmap()
	lw.file.Close()
	lw.isRunning = false

	log.Printf("停止监控日志文件: %s", lw.logFile)
}

// 添加客户端连接
func (lw *LogWatcher) AddClient(conn *websocket.Conn, useRules bool) {
	lw.clientsMutex.Lock()
	defer lw.clientsMutex.Unlock()

	lw.clients[conn] = &ClientInfo{
		Conn:     conn,
		UseRules: useRules,
	}
	log.Printf("新的客户端连接，当前连接数: %d, 使用规则过滤: %v", len(lw.clients), useRules)

	// 监听客户端消息
	go func() {
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				lw.RemoveClient(conn)
				break
			}

			// 处理客户端消息
			lw.handleClientMessage(conn, message)
		}
	}()
}

// 移除客户端连接
func (lw *LogWatcher) RemoveClient(conn *websocket.Conn) {
	lw.clientsMutex.Lock()
	defer lw.clientsMutex.Unlock()

	if clientInfo, ok := lw.clients[conn]; ok {
		delete(lw.clients, conn)
		clientInfo.Conn.Close()
		log.Printf("客户端断开连接，当前连接数: %d", len(lw.clients))
	}
}

// 向所有客户端广播消息
func (lw *LogWatcher) BroadcastMessage(message string) {
	lw.clientsMutex.Lock()
	defer lw.clientsMutex.Unlock()

	// 处理每一行日志
	lines := strings.Split(message, "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}

		// 向每个客户端发送消息
		for conn, clientInfo := range lw.clients {
			// 如果客户端使用规则过滤，则应用规则
			if clientInfo.UseRules && lw.ruleManager != nil {
				// 应用规则过滤
				filteredLine, show, color := lw.ruleManager.FilterLogLine(line)
				if !show {
					// 不显示该行
					continue
				}

				// 创建日志消息
				logMsg := LogMessage{
					Type:      "log",
					Content:   filteredLine,
					Highlight: color != "",
					Color:     color,
					Timestamp: time.Now().Unix(),
				}

				// 序列化为JSON
				jsonData, err := json.Marshal(logMsg)
				if err != nil {
					log.Printf("序列化日志消息失败: %v", err)
					continue
				}

				// 发送JSON消息
				err = clientInfo.Conn.WriteMessage(websocket.TextMessage, jsonData)
			} else {
				// 不使用规则过滤，直接发送原始消息
				logMsg := LogMessage{
					Type:      "log",
					Content:   line,
					Highlight: false,
					Color:     "",
					Timestamp: time.Now().Unix(),
				}

				// 序列化为JSON
				jsonData, err := json.Marshal(logMsg)
				if err != nil {
					log.Printf("序列化日志消息失败: %v", err)
					continue
				}

				// 发送JSON消息
				err = clientInfo.Conn.WriteMessage(websocket.TextMessage, jsonData)
			}

			if err != nil {
				log.Printf("发送消息到客户端失败: %v", err)
				clientInfo.Conn.Close()
				delete(lw.clients, conn)
			}
		}
	}
}

// 处理客户端消息
func (lw *LogWatcher) handleClientMessage(conn *websocket.Conn, message []byte) {
	// 尝试解析为规则操作
	var ruleOp RuleOperation
	if err := json.Unmarshal(message, &ruleOp); err != nil {
		log.Printf("解析客户端消息失败: %v", err)
		return
	}

	// 处理规则操作
	var response RuleResponse

	switch ruleOp.Action {
	case "list":
		// 获取规则列表
		response.Success = true
		response.Rules = lw.ruleManager.GetRules()
		response.Message = "获取规则列表成功"

	case "add":
		// 添加新规则
		if ruleOp.Rule == nil {
			response.Success = false
			response.Message = "规则数据不能为空"
			break
		}

		// 生成规则ID
		if ruleOp.Rule.ID == "" {
			ruleOp.Rule.ID = uuid.New().String()
		}

		// 添加规则
		err := lw.ruleManager.AddRule(ruleOp.Rule)
		if err != nil {
			response.Success = false
			response.Message = fmt.Sprintf("添加规则失败: %v", err)
		} else {
			response.Success = true
			response.Message = "添加规则成功"
			response.Rules = lw.ruleManager.GetRules()
		}

	case "update":
		// 更新规则
		if ruleOp.Rule == nil || ruleOp.Rule.ID == "" {
			response.Success = false
			response.Message = "规则数据不完整"
			break
		}

		// 更新规则
		err := lw.ruleManager.UpdateRule(ruleOp.Rule)
		if err != nil {
			response.Success = false
			response.Message = fmt.Sprintf("更新规则失败: %v", err)
		} else {
			response.Success = true
			response.Message = "更新规则成功"
			response.Rules = lw.ruleManager.GetRules()
		}

	case "delete":
		// 删除规则
		if ruleOp.RuleID == "" {
			response.Success = false
			response.Message = "规则ID不能为空"
			break
		}

		// 删除规则
		err := lw.ruleManager.DeleteRule(ruleOp.RuleID)
		if err != nil {
			response.Success = false
			response.Message = fmt.Sprintf("删除规则失败: %v", err)
		} else {
			response.Success = true
			response.Message = "删除规则成功"
			response.Rules = lw.ruleManager.GetRules()
		}

	case "enable":
		// 启用规则
		if ruleOp.RuleID == "" {
			response.Success = false
			response.Message = "规则ID不能为空"
			break
		}

		// 启用规则
		err := lw.ruleManager.EnableRule(ruleOp.RuleID)
		if err != nil {
			response.Success = false
			response.Message = fmt.Sprintf("启用规则失败: %v", err)
		} else {
			response.Success = true
			response.Message = "启用规则成功"
			response.Rules = lw.ruleManager.GetRules()
		}

	case "disable":
		// 禁用规则
		if ruleOp.RuleID == "" {
			response.Success = false
			response.Message = "规则ID不能为空"
			break
		}

		// 禁用规则
		err := lw.ruleManager.DisableRule(ruleOp.RuleID)
		if err != nil {
			response.Success = false
			response.Message = fmt.Sprintf("禁用规则失败: %v", err)
		} else {
			response.Success = true
			response.Message = "禁用规则成功"
			response.Rules = lw.ruleManager.GetRules()
		}

	case "toggle_rules":
		// 切换是否使用规则
		lw.clientsMutex.Lock()
		if clientInfo, ok := lw.clients[conn]; ok {
			clientInfo.UseRules = !clientInfo.UseRules
			response.Success = true
			response.Message = fmt.Sprintf("已%s规则过滤", clientInfo.UseRules ? "启用" : "禁用")
		} else {
			response.Success = false
			response.Message = "客户端信息不存在"
		}
		lw.clientsMutex.Unlock()

	default:
		response.Success = false
		response.Message = fmt.Sprintf("未知操作: %s", ruleOp.Action)
	}

	// 序列化响应
	jsonData, err := json.Marshal(response)
	if err != nil {
		log.Printf("序列化响应失败: %v", err)
		return
	}

	// 发送响应
	err = conn.WriteMessage(websocket.TextMessage, jsonData)
	if err != nil {
		log.Printf("发送响应失败: %v", err)
		lw.RemoveClient(conn)
	}
}

// 监控循环
func (lw *LogWatcher) watchLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-lw.stopChan:
			return
		case event, ok := <-lw.watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Write == fsnotify.Write && event.Name == lw.logFile {
				lw.readNewContent()
			}
		case err, ok := <-lw.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("文件监控错误: %v", err)
		case <-ticker.C:
			// 定期检查文件变化，防止某些系统上fsnotify事件丢失
			lw.readNewContent()
		}
	}
}

// 读取新内容
func (lw *LogWatcher) readNewContent() {
	// 重新打开文件以获取最新状态
	file, err := os.Open(lw.logFile)
	if err != nil {
		log.Printf("重新打开日志文件失败: %v", err)
		return
	}
	defer file.Close()

	// 获取文件当前大小
	fileInfo, err := file.Stat()
	if err != nil {
		log.Printf("获取文件信息失败: %v", err)
		return
	}

	currentSize := fileInfo.Size()
	if currentSize <= lw.lastPosition {
		// 文件没有新内容或被截断
		if currentSize < lw.lastPosition {
			// 文件被截断，重置位置
			lw.lastPosition = 0
			log.Printf("检测到日志文件被截断，重置读取位置")
		}
		return
	}

	// 重新映射文件
	lw.mmapData.Unmap()
	lw.file.Close()

	lw.file, err = os.OpenFile(lw.logFile, os.O_RDWR, 0644)
	if err != nil {
		log.Printf("重新打开日志文件失败: %v", err)
		return
	}

	lw.mmapData, err = mmap.Map(lw.file, mmap.RDONLY, 0)
	if err != nil {
		log.Printf("重新映射文件失败: %v", err)
		lw.file.Close()
		return
	}

	// 读取新内容
	newContent := lw.mmapData[lw.lastPosition:currentSize]
	if len(newContent) > 0 {
		// 广播新内容
		lw.BroadcastMessage(string(newContent))
		lw.lastPosition = currentSize
	}
}

// 全局日志监控管理器映射
var logWatchers = make(map[string]*LogWatcher)
var logWatchersMutex sync.Mutex

// 获取或创建日志监控管理器
func getOrCreateLogWatcher(archiveName, worldName string) (*LogWatcher, error) {
	logWatchersMutex.Lock()
	defer logWatchersMutex.Unlock()

	key := fmt.Sprintf("%s_%s", archiveName, worldName)
	if watcher, ok := logWatchers[key]; ok {
		return watcher, nil
	}

	// 构建日志文件路径
	logPath := filepath.Join(dstSavePath, archiveName, worldName, "server_log.txt")
	log.Printf("创建日志监控器，路径: %s", logPath)

	watcher, err := NewLogWatcher(logPath, archiveName, worldName)
	if err != nil {
		return nil, err
	}

	// 启动监控
	if err := watcher.Start(); err != nil {
		watcher.Stop()
		return nil, err
	}

	logWatchers[key] = watcher
	return watcher, nil
}

// 关闭日志监控管理器
func closeLogWatcher(archiveName, worldName string) {
	logWatchersMutex.Lock()
	defer logWatchersMutex.Unlock()

	key := fmt.Sprintf("%s_%s", archiveName, worldName)
	if watcher, ok := logWatchers[key]; ok {
		watcher.Stop()
		delete(logWatchers, key)
		log.Printf("关闭日志监控器: %s", key)
	}
}

// WebSocket处理函数
func HandleLogWebSocket(c *gin.Context) {
	// 获取参数
	archiveName := c.Query("archive")
	worldName := c.Query("world")

	if archiveName == "" || worldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称(archive)和世界名称(world)",
		})
		return
	}

	// 获取或创建日志监控管理器
	watcher, err := getOrCreateLogWatcher(archiveName, worldName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("创建日志监控失败: %v", err),
		})
		return
	}

	// 升级HTTP连接为WebSocket
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket升级失败: %v", err)
		return
	}

	// 获取是否使用规则过滤
	useRules := c.DefaultQuery("use_rules", "true") == "true"

	// 添加客户端
	watcher.AddClient(conn, useRules)

	// 发送初始连接成功消息
	initMsg := LogMessage{
		Type:      "system",
		Content:   "已连接到日志监控服务",
		Highlight: false,
		Color:     "",
		Timestamp: time.Now().Unix(),
	}

	// 序列化为JSON
	jsonData, err := json.Marshal(initMsg)
	if err != nil {
		log.Printf("序列化初始消息失败: %v", err)
		return
	}

	// 发送初始消息
	conn.WriteMessage(websocket.TextMessage, jsonData)

	// 发送规则列表
	ruleListMsg := RuleResponse{
		Success: true,
		Message: "规则列表",
		Rules:   watcher.ruleManager.GetRules(),
	}

	// 序列化为JSON
	jsonRules, err := json.Marshal(ruleListMsg)
	if err != nil {
		log.Printf("序列化规则列表失败: %v", err)
		return
	}

	// 发送规则列表
	conn.WriteMessage(websocket.TextMessage, jsonRules)
}

// 注册路由
func RegisterRoutes(router *gin.RouterGroup) {
	router.GET("/log/ws", HandleLogWebSocket)

	// 添加规则管理API
	rules := router.Group("/rules")
	{
		// 获取规则列表
		rules.GET("/list", HandleGetRules)

		// 添加规则
		rules.POST("/add", HandleAddRule)

		// 更新规则
		rules.POST("/update", HandleUpdateRule)

		// 删除规则
		rules.POST("/delete", HandleDeleteRule)

		// 启用规则
		rules.POST("/enable", HandleEnableRule)

		// 禁用规则
		rules.POST("/disable", HandleDisableRule)
	}
}
