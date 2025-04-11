package gamelog

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dont/service/logparser"

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
	watcher        *fsnotify.Watcher
	logFile        string
	file           *os.File
	lastPosition   int64
	lastModTime    time.Time // 上次文件修改时间
	clients        map[*websocket.Conn]*ClientInfo
	clientsMutex   sync.Mutex
	stopChan       chan struct{}
	isRunning      bool
	runningMutex   sync.Mutex
	archiveName    string
	worldName      string
	ruleManager    *RuleManager
	logParser      *logparser.LogParser // 日志解析器
	enableDBStore  bool                 // 是否启用数据库存储
	parserMutex    sync.RWMutex         // 解析器读写锁
	lastActivity   time.Time            // 最后活动时间
	processedLines int64                // 已处理的行数
}

// ClientInfo 客户端信息
type ClientInfo struct {
	Conn         *websocket.Conn // WebSocket连接
	UseRules     bool            // 是否使用规则过滤
	EnableParser bool            // 是否启用日志解析
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
	fileInfo, err := os.Stat(logFilePath)
	if os.IsNotExist(err) {
		emptyFile, err := os.Create(logFilePath)
		if err != nil {
			return nil, fmt.Errorf("创建日志文件失败: %v", err)
		}
		emptyFile.Close()
		// 重新获取文件信息
		fileInfo, err = os.Stat(logFilePath)
		if err != nil {
			return nil, fmt.Errorf("获取文件信息失败: %v", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("获取文件信息失败: %v", err)
	}

	// 创建日志监控管理器
	lw := &LogWatcher{
		watcher:        watcher,
		logFile:        logFilePath,
		lastPosition:   fileInfo.Size(),
		lastModTime:    fileInfo.ModTime(),
		clients:        make(map[*websocket.Conn]*ClientInfo),
		stopChan:       make(chan struct{}),
		archiveName:    archiveName,
		worldName:      worldName,
		lastActivity:   time.Now(),
		processedLines: 0,
	}

	// 获取或创建规则管理器
	lw.ruleManager = GetOrCreateRuleManager(archiveName, worldName)

	// 创建日志解析器
	parser, err := logparser.NewLogParser(archiveName, worldName)
	if err != nil {
		return nil, fmt.Errorf("创建日志解析器失败: %v", err)
	}
	lw.logParser = parser

	// 默认启用数据库存储
	lw.enableDBStore = true

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
	if lw.file != nil {
		lw.file.Close()
		lw.file = nil
	}
	lw.isRunning = false

	log.Printf("停止监控日志文件: %s", lw.logFile)
}

// SetLogParser 设置日志解析器
func (lw *LogWatcher) SetLogParser(parser *logparser.LogParser) {
	lw.parserMutex.Lock()
	defer lw.parserMutex.Unlock()

	lw.logParser = parser
	log.Printf("已设置日志解析器，存档: %s, 世界: %s", lw.archiveName, lw.worldName)
}

// EnableDBStore 启用或禁用数据库存储
func (lw *LogWatcher) EnableDBStore(enable bool) {
	lw.parserMutex.Lock()
	defer lw.parserMutex.Unlock()

	lw.enableDBStore = enable
	statusText := "禁用"
	if enable {
		statusText = "启用"
	}
	log.Printf("已%s数据库存储，存档: %s, 世界: %s",
		statusText, lw.archiveName, lw.worldName)
}

// GetArchiveName 获取存档名称
func (lw *LogWatcher) GetArchiveName() string {
	return lw.archiveName
}

// GetWorldName 获取世界名称
func (lw *LogWatcher) GetWorldName() string {
	return lw.worldName
}

// GetServerType 获取服务器类型
func (lw *LogWatcher) GetServerType() string {
	lw.parserMutex.RLock()
	defer lw.parserMutex.RUnlock()

	if lw.logParser != nil {
		return lw.logParser.GetServerType()
	}

	// 如果日志解析器不可用，尝试从世界名称推断
	if strings.Contains(strings.ToLower(lw.worldName), "cave") {
		return "cave"
	}
	return "forest"
}

// GetStartTime 获取监控器启动时间
func (lw *LogWatcher) GetStartTime() time.Time {
	lw.parserMutex.RLock()
	defer lw.parserMutex.RUnlock()

	if lw.logParser != nil && lw.logParser.GetRealStartTime().Unix() > 0 {
		return lw.logParser.GetRealStartTime()
	}

	// 如果日志解析器没有有效的启动时间，返回当前时间
	return time.Now()
}

// GetLogFile 获取日志文件路径
func (lw *LogWatcher) GetLogFile() string {
	return lw.logFile
}

// GetProcessedLines 获取已处理行数
func (lw *LogWatcher) GetProcessedLines() int64 {
	return lw.processedLines
}

// GetLastActivity 获取最后活动时间
func (lw *LogWatcher) GetLastActivity() time.Time {
	return lw.lastActivity
}

// IsRunning 检查监控器是否正在运行
func (lw *LogWatcher) IsRunning() bool {
	lw.runningMutex.Lock()
	defer lw.runningMutex.Unlock()
	return lw.isRunning
}

// GetClientCount 获取当前连接的客户端数量
func (lw *LogWatcher) GetClientCount() int {
	lw.clientsMutex.Lock()
	defer lw.clientsMutex.Unlock()
	return len(lw.clients)
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
				jsonData, jsonErr := json.Marshal(logMsg)
				if jsonErr != nil {
					log.Printf("序列化日志消息失败: %v", jsonErr)
					continue
				}

				// 发送JSON消息
				sendErr := clientInfo.Conn.WriteMessage(websocket.TextMessage, jsonData)
				if sendErr != nil {
					log.Printf("发送消息到客户端失败: %v", sendErr)
					clientInfo.Conn.Close()
					delete(lw.clients, conn)
				}
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
			if clientInfo.UseRules {
				response.Message = "已启用规则过滤"
			} else {
				response.Message = "已禁用规则过滤"
			}
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
	log.Printf("[LogWatcher] 开始监控循环: 存档=%s, 世界=%s, 文件=%s",
		lw.archiveName, lw.worldName, lw.logFile)

	// 使用更短的间隔进行检查，确保实时性
	ticker := time.NewTicker(100 * time.Millisecond)

	// 首次读取文件内容
	log.Printf("[LogWatcher] 首次读取文件内容: %s", lw.logFile)
	lw.readNewContent()
	defer ticker.Stop()

	for {
		select {
		case <-lw.stopChan:
			log.Printf("[LogWatcher] 监控循环收到停止信号: 存档=%s, 世界=%s",
				lw.archiveName, lw.worldName)
			return
		case event, ok := <-lw.watcher.Events:
			if !ok {
				log.Printf("[LogWatcher] 监控器事件通道关闭: 存档=%s, 世界=%s",
					lw.archiveName, lw.worldName)
				return
			}
			if event.Op&fsnotify.Write == fsnotify.Write && event.Name == lw.logFile {
				log.Printf("[LogWatcher] 检测到文件写入事件: %s", event.Name)
				lw.readNewContent()
			}
		case err, ok := <-lw.watcher.Errors:
			if !ok {
				log.Printf("[LogWatcher] 监控器错误通道关闭: 存档=%s, 世界=%s",
					lw.archiveName, lw.worldName)
				return
			}
			log.Printf("[LogWatcher] 文件监控错误: %v, 存档=%s, 世界=%s",
				err, lw.archiveName, lw.worldName)
		case <-ticker.C:
			// 定期检查文件变化，防止某些系统上fsnotify事件丢失
			lw.readNewContent()
		}
	}
}

// ReadNewContent 公开方法，手动触发读取新内容
func (lw *LogWatcher) ReadNewContent() {
	log.Printf("[LogWatcher] 手动触发读取新内容: 存档=%s, 世界=%s", lw.archiveName, lw.worldName)
	lw.readNewContent()
}

// 读取新内容
func (lw *LogWatcher) readNewContent() {
	log.Printf("[LogWatcher] 开始读取日志文件新内容: %s", lw.logFile)

	// 获取文件信息
	fileInfo, err := os.Stat(lw.logFile)
	if err != nil {
		log.Printf("[LogWatcher] 获取文件信息失败: %v", err)
		return
	}

	// 获取文件当前大小和修改时间
	currentSize := fileInfo.Size()
	currentModTime := fileInfo.ModTime()

	// 从位置管理器获取上次读取位置
	positionManager := GetPositionManager()
	position := positionManager.GetPosition(lw.logFile)
	if position != nil {
		lw.lastPosition = position.LastPosition
		lw.lastModTime = position.LastModTime
	}

	// 检查文件是否被修改
	if currentSize == lw.lastPosition && currentModTime.Equal(lw.lastModTime) {
		// 文件没有变化，跳过处理
		return
	}

	log.Printf("[LogWatcher] 文件大小: %d 字节, 上次读取位置: %d, 上次修改时间: %s, 当前修改时间: %s",
		currentSize, lw.lastPosition, lw.lastModTime.Format("2006-01-02 15:04:05"), currentModTime.Format("2006-01-02 15:04:05"))

	// 检测服务器重启的情况
	isServerRestart := currentSize < lw.lastPosition && !currentModTime.Equal(lw.lastModTime)

	if currentSize <= lw.lastPosition {
		// 文件没有新内容或被截断
		if isServerRestart {
			// 服务器重启，备份旧日志文件
			if lw.lastPosition > 0 {
				// 创建备份目录
				backupDir := filepath.Join(dstSavePath, "logs_backup", lw.archiveName, lw.worldName)
				if err := os.MkdirAll(backupDir, 0755); err != nil {
					log.Printf("[LogWatcher] 创建日志备份目录失败: %v", err)
				} else {
					// 生成备份文件名（使用时间戳）
					timestamp := time.Now().Format("20060102_150405")
					backupFileName := fmt.Sprintf("server_log_%s.txt", timestamp)
					backupFilePath := filepath.Join(backupDir, backupFileName)

					// 复制日志文件
					if err := copyLogFile(lw.logFile, backupFilePath); err != nil {
						log.Printf("[LogWatcher] 备份日志文件失败: %v", err)
					} else {
						log.Printf("[LogWatcher] 成功备份日志文件到: %s", backupFilePath)
					}
				}
			}

			// 服务器重启，重置位置
			lw.lastPosition = 0
			log.Printf("[LogWatcher] 检测到服务器重启，日志文件被重置。文件大小从 %d 变为 %d。重置读取位置", lw.lastPosition, currentSize)

			// 重置解析器状态
			if lw.logParser != nil {
				// 添加一条服务器重启的日志
				restartMsg := fmt.Sprintf("服务器已重启，日志文件被重置。文件大小从 %d 字节变为 %d 字节", lw.lastPosition, currentSize)
				lw.logParser.SaveLogToDatabase("system", restartMsg, time.Now()) // 使用字符串“system”代替models.LogTypeSystem常量，因为我们不想引入models包

				// 创建新的解析器，使用相同的存档名称和世界名称
				newParser, err := logparser.NewLogParser(lw.archiveName, lw.worldName)
				if err != nil {
					log.Printf("重置解析器状态失败: %v", err)
				} else {
					lw.logParser = newParser
				}
			}
		}
		return
	}

	// 使用增量读取方式读取新内容

	// 确保lastPosition不超过文件大小
	if lw.lastPosition > currentSize {
		log.Printf("[LogWatcher] 上次读取位置超过文件大小，重置为0")
		lw.lastPosition = 0
	}

	// 打开文件
	file, err := os.Open(lw.logFile)
	if err != nil {
		log.Printf("[LogWatcher] 打开文件失败: %v", err)
		return
	}
	defer file.Close()

	// 设置读取位置
	if _, err := file.Seek(lw.lastPosition, 0); err != nil {
		log.Printf("[LogWatcher] 设置文件读取位置失败: %v", err)
		return
	}

	// 读取新内容
	newContent := make([]byte, currentSize-lw.lastPosition)
	n, err := file.Read(newContent)
	if err != nil {
		log.Printf("[LogWatcher] 读取文件内容失败: %v", err)
		return
	}

	// 如果没有读取到内容，跳过处理
	if n == 0 {
		log.Printf("[LogWatcher] 没有读取到新内容")
		return
	}

	// 使用实际读取的字节数
	newContent = newContent[:n]
	if len(newContent) > 0 {
		// 将新内容转换为字符串
		contentStr := string(newContent)

		// 广播新内容到WebSocket客户端
		lw.BroadcastMessage(contentStr)

		// 如果启用了数据库存储，则解析并存储日志
		lw.parserMutex.RLock()
		enableStore := lw.enableDBStore
		parser := lw.logParser
		lw.parserMutex.RUnlock()

		log.Printf("[LogWatcher] 检测到日志文件变化，新内容长度: %d 字节, 存档=%s, 世界=%s",
			len(contentStr), lw.archiveName, lw.worldName)

		if enableStore && parser != nil {

			// 尝试处理日志内容
			if err := parser.ProcessAndSaveLog(contentStr); err != nil {
				log.Printf("[LogWatcher] 处理并存储日志失败: %v", err)

				// 尝试重新创建解析器
				newParser, err := logparser.NewLogParser(lw.archiveName, lw.worldName)
				if err != nil {
					log.Printf("[LogWatcher] 创建新的解析器失败: %v", err)
				} else {
					lw.parserMutex.Lock()
					lw.logParser = newParser
					lw.parserMutex.Unlock()

					// 使用新的解析器重新尝试
					if err := newParser.ProcessAndSaveLog(contentStr); err != nil {
						log.Printf("[LogWatcher] 使用新解析器处理并存储日志失败: %v", err)
					}
				}
			}
		} else {
			log.Printf("[LogWatcher] 未启用数据库存储或解析器为空, enableStore=%v, parser=%v",
				enableStore, parser != nil)

			// 如果解析器为空，尝试创建新的解析器
			if parser == nil {
				newParser, err := logparser.NewLogParser(lw.archiveName, lw.worldName)
				if err != nil {
					log.Printf("[LogWatcher] 创建新的解析器失败: %v", err)
				} else {
					lw.parserMutex.Lock()
					lw.logParser = newParser
					lw.enableDBStore = true
					lw.parserMutex.Unlock()

					// 使用新的解析器处理内容
					if err := newParser.ProcessAndSaveLog(contentStr); err != nil {
						log.Printf("[LogWatcher] 使用新解析器处理并存储日志失败: %v", err)
					}
				}
			}
		}

		// 更新最后读取位置
		lw.lastPosition = currentSize

		// 更新最后修改时间
		lw.lastModTime = currentModTime

		// 更新位置管理器中的位置记录
		positionManager.UpdatePosition(lw.logFile, currentSize, currentModTime)

		// 更新最后活动时间
		lw.lastActivity = time.Now()

		// 增加处理行数
		lw.processedLines += int64(strings.Count(contentStr, "\n") + 1)
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

	// 注册日志解析器API路由
	RegisterParserAPIRoutes(router)
}

// copyLogFile 复制日志文件
func copyLogFile(src, dst string) error {
	// 打开源文件
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开源文件失败: %v", err)
	}
	defer srcFile.Close()

	// 创建目标文件
	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("创建目标文件失败: %v", err)
	}
	defer dstFile.Close()

	// 复制内容
	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return fmt.Errorf("复制文件内容失败: %v", err)
	}

	return nil
}
