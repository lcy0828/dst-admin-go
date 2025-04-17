package logmonitor

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dont/routers/gamelog"
)

// ServerInfo 表示服务器信息
type ServerInfo struct {
	SessionName    string `json:"session_name"`    // 会话名称
	ArchiveName    string `json:"archive_name"`    // 存档名称
	WorldName      string `json:"world_name"`      // 世界名称
	ServerMode     string `json:"server_mode"`     // 服务器启动模式（32位或64位）
	StartDirectory string `json:"start_directory"` // 启动目录
	Status         string `json:"status"`          // 服务器状态（运行中/已停止）
	StartTime      string `json:"start_time"`      // 启动时间
}

// ServerListResponse 表示服务器列表响应
type ServerListResponse struct {
	Status int          `json:"status"`
	Msg    string       `json:"msg"`
	Data   []ServerInfo `json:"data"`
	Meta   struct {
		Count       int    `json:"count"`
		ElapsedTime string `json:"elapsed_time"`
	} `json:"meta"`
}

// LogMonitor 日志监控服务
type LogMonitor struct {
	apiBaseURL      string
	dstSavePath     string
	checkInterval   time.Duration
	stopChan        chan struct{}
	isRunning       bool
	runningMutex    sync.Mutex
	watcherMap      map[string]*gamelog.LogWatcher
	watcherMapMutex sync.Mutex
	serverStatus    map[string]bool // 记录服务器状态，key为sessionName，value为是否运行中
	statusMutex     sync.Mutex
}

// NewLogMonitor 创建新的日志监控服务
func NewLogMonitor(apiBaseURL, dstSavePath string, checkInterval time.Duration) *LogMonitor {
	return &LogMonitor{
		apiBaseURL:    apiBaseURL,
		dstSavePath:   dstSavePath,
		checkInterval: checkInterval,
		stopChan:      make(chan struct{}),
		watcherMap:    make(map[string]*gamelog.LogWatcher),
		serverStatus:  make(map[string]bool),
	}
}

// Start 启动监控服务
func (m *LogMonitor) Start() error {
	m.runningMutex.Lock()
	defer m.runningMutex.Unlock()

	if m.isRunning {
		return fmt.Errorf("日志监控服务已经在运行中")
	}

	m.isRunning = true
	go m.monitorLoop()

	log.Printf("[LogMonitor] 日志监控服务已启动，检查间隔: %v", m.checkInterval)
	return nil
}

// Stop 停止监控服务
func (m *LogMonitor) Stop() {
	m.runningMutex.Lock()
	defer m.runningMutex.Unlock()

	if !m.isRunning {
		return
	}

	close(m.stopChan)
	m.isRunning = false

	// 停止所有日志监控器
	m.watcherMapMutex.Lock()
	defer m.watcherMapMutex.Unlock()

	for key, watcher := range m.watcherMap {
		watcher.Stop()
		delete(m.watcherMap, key)
	}

	log.Printf("[LogMonitor] 日志监控服务已停止")
}

// monitorLoop 监控循环
func (m *LogMonitor) monitorLoop() {
	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	// 立即执行一次检查
	m.checkServers()

	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.checkServers()
		}
	}
}

// checkServers 检查服务器状态并更新监控器
func (m *LogMonitor) checkServers() {
	// 获取服务器列表
	servers, err := m.getServerList()
	if err != nil {
		log.Printf("[LogMonitor] 获取服务器列表失败: %v", err)
		return
	}

	// 更新服务器状态
	m.updateServerStatus(servers)
}

// getServerList 获取服务器列表
func (m *LogMonitor) getServerList() ([]ServerInfo, error) {
	// 构建API URL
	url := fmt.Sprintf("%s/api/tmux/list", m.apiBaseURL)

	// 发送HTTP请求
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("请求服务器列表失败: %v", err)
	}
	defer resp.Body.Close()

	// 读取响应内容
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应内容失败: %v", err)
	}

	// 解析JSON响应
	var response ServerListResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("解析JSON响应失败: %v", err)
	}

	// 检查响应状态
	if response.Status != 200 {
		return nil, fmt.Errorf("API返回错误: %s", response.Msg)
	}

	return response.Data, nil
}

// updateServerStatus 更新服务器状态并管理监控器
func (m *LogMonitor) updateServerStatus(servers []ServerInfo) {
	m.statusMutex.Lock()
	defer m.statusMutex.Unlock()

	// 创建新的状态映射
	newStatus := make(map[string]bool)

	// 处理运行中的服务器
	for _, server := range servers {
		// 只处理状态为running的服务器
		if server.Status == "running" {
			newStatus[server.SessionName] = true

			// 检查是否已经在监控中
			if !m.serverStatus[server.SessionName] {
				// 新启动的服务器，创建监控器
				go m.startMonitoringServer(server)
			}
		}
	}

	// 处理已停止的服务器
	for sessionName, running := range m.serverStatus {
		if running && !newStatus[sessionName] {
			// 服务器已停止，等待一段时间后停止监控
			go m.stopMonitoringServerWithDelay(sessionName, 5*time.Second)
		}
	}

	// 更新状态映射
	m.serverStatus = newStatus
}

// startMonitoringServer 开始监控服务器日志
func (m *LogMonitor) startMonitoringServer(server ServerInfo) {
	log.Printf("[LogMonitor] 开始监控服务器日志: %s (存档: %s, 世界: %s)",
		server.SessionName, server.ArchiveName, server.WorldName)

	// 解析服务器类型（森林或洞穴）
	worldType := "forest"
	if strings.Contains(strings.ToLower(server.WorldName), "cave") {
		worldType = "cave"
	}

	// 构建日志文件路径
	var logFileName string
	if worldType == "forest" {
		logFileName = "server_log.txt"
	} else {
		logFileName = "server_log.txt" // 洞穴世界也是server_log.txt
	}

	logPath := filepath.Join(m.dstSavePath, server.ArchiveName, server.WorldName, logFileName)
	log.Printf("[LogMonitor] 监控日志文件: %s", logPath)

	// 创建日志监控器
	watcher, err := gamelog.NewLogWatcher(logPath, server.ArchiveName, server.WorldName)
	if err != nil {
		log.Printf("[LogMonitor] 创建日志监控器失败: %v", err)
		return
	}

	// 启动监控
	if err := watcher.Start(); err != nil {
		log.Printf("[LogMonitor] 启动日志监控器失败: %v", err)
		watcher.Stop()
		return
	}

	// 保存监控器引用
	m.watcherMapMutex.Lock()
	m.watcherMap[server.SessionName] = watcher
	m.watcherMapMutex.Unlock()
}

// stopMonitoringServerWithDelay 延迟停止监控服务器日志
func (m *LogMonitor) stopMonitoringServerWithDelay(sessionName string, delay time.Duration) {
	log.Printf("[LogMonitor] 服务器已停止，将在 %v 后停止监控: %s", delay, sessionName)

	// 等待指定时间
	time.Sleep(delay)

	// 停止监控
	m.watcherMapMutex.Lock()
	defer m.watcherMapMutex.Unlock()

	if watcher, ok := m.watcherMap[sessionName]; ok {
		watcher.Stop()
		delete(m.watcherMap, sessionName)
		log.Printf("[LogMonitor] 已停止监控服务器日志: %s", sessionName)
	}
}

// GetWatcherForServer 获取指定服务器的日志监控器
func (m *LogMonitor) GetWatcherForServer(sessionName string) (*gamelog.LogWatcher, bool) {
	m.watcherMapMutex.Lock()
	defer m.watcherMapMutex.Unlock()

	watcher, ok := m.watcherMap[sessionName]
	return watcher, ok
}

// GetAllWatchers 获取所有日志监控器
func (m *LogMonitor) GetAllWatchers() map[string]*gamelog.LogWatcher {
	m.watcherMapMutex.Lock()
	defer m.watcherMapMutex.Unlock()

	// 创建副本
	result := make(map[string]*gamelog.LogWatcher)
	for k, v := range m.watcherMap {
		result[k] = v
	}

	return result
}
