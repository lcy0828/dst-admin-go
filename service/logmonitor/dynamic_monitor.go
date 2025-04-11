package logmonitor

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dont/routers/gamelog"
	"dont/service/logparser"
	"dont/tmux"
)

// DynamicLogMonitor 动态日志监控服务
// 根据服务器状态动态调整监控的日志文件
type DynamicLogMonitor struct {
	dstSavePath     string                         // DST存档路径
	checkInterval   time.Duration                  // 检查间隔
	stopChan        chan struct{}                  // 停止信号
	isRunning       bool                           // 是否运行中
	runningMutex    sync.Mutex                     // 运行状态互斥锁
	watcherMap      map[string]*gamelog.LogWatcher // 监控器映射
	watcherMapMutex sync.Mutex                     // 监控器映射互斥锁
	serverStatus    map[string]bool                // 服务器状态映射
	statusMutex     sync.Mutex                     // 状态互斥锁
	logRetention    int                            // 日志保留天数
	parserManager   *logparser.LogParserManager    // 日志解析器管理器
}

// NewDynamicLogMonitor 创建新的动态日志监控服务
func NewDynamicLogMonitor(dstSavePath string, checkInterval time.Duration, logRetention int) *DynamicLogMonitor {
	return &DynamicLogMonitor{
		dstSavePath:   dstSavePath,
		checkInterval: checkInterval,
		stopChan:      make(chan struct{}),
		watcherMap:    make(map[string]*gamelog.LogWatcher),
		serverStatus:  make(map[string]bool),
		logRetention:  logRetention,
		parserManager: logparser.GetLogParserManager(),
	}
}

// Start 启动监控服务
func (m *DynamicLogMonitor) Start() error {
	m.runningMutex.Lock()
	defer m.runningMutex.Unlock()

	if m.isRunning {
		return fmt.Errorf("动态日志监控服务已经在运行中")
	}

	m.isRunning = true
	go m.monitorLoop()

	log.Printf("[DynamicLogMonitor] 动态日志监控服务已启动，检查间隔: %v, 日志保留天数: %d",
		m.checkInterval, m.logRetention)
	return nil
}

// Stop 停止监控服务
func (m *DynamicLogMonitor) Stop() {
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

	log.Printf("[DynamicLogMonitor] 动态日志监控服务已停止")
}

// monitorLoop 监控循环
func (m *DynamicLogMonitor) monitorLoop() {
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
func (m *DynamicLogMonitor) checkServers() {
	// 获取服务器列表
	servers, err := m.getServerList()
	if err != nil {
		// 不打印错误日志，因为这个方法会被频繁调用
		return
	}

	// 更新服务器状态
	m.updateServerStatus(servers)
}

// getServerList 获取服务器列表
func (m *DynamicLogMonitor) getServerList() ([]ServerInfo, error) {
	// 直接调用tmux包的函数获取运行中的服务器信息
	tmuxServers := tmux.GetRunningServers()

	// 打印调试信息
	//log.Printf("[DynamicLogMonitor] 获取到 %d 个运行中的服务器", len(tmuxServers))
	if len(tmuxServers) == 0 {
		return nil, fmt.Errorf("没有运行中的服务器")
	}
	// 将tmux.ServerInfo转换为本包的ServerInfo
	servers := make([]ServerInfo, len(tmuxServers))
	for i, server := range tmuxServers {
		servers[i] = ServerInfo{
			SessionName: server.SessionName,
			ArchiveName: server.ArchiveName,
			WorldName:   server.WorldName,
			ServerMode:  server.ServerMode,
			Status:      server.Status,
			StartTime:   server.StartTime,
		}

		// 打印服务器信息
		//log.Printf("[DynamicLogMonitor] 服务器 #%d: 会话=%s, 存档=%s, 世界=%s, 状态=%s",
		//	i, server.SessionName, server.ArchiveName, server.WorldName, server.Status)
	}

	return servers, nil
}

// updateServerStatus 更新服务器状态并管理监控器
func (m *DynamicLogMonitor) updateServerStatus(servers []ServerInfo) {
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
			} else {
				// 已经在监控中的服务器，触发一次日志读取
				m.watcherMapMutex.Lock()
				if watcher, exists := m.watcherMap[server.SessionName]; exists {
					log.Printf("[DynamicLogMonitor] 触发定期日志读取: %s", server.SessionName)
					go watcher.ReadNewContent()
				}
				m.watcherMapMutex.Unlock()
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
func (m *DynamicLogMonitor) startMonitoringServer(server ServerInfo) {
	log.Printf("[DynamicLogMonitor] 开始监控服务器日志: 会话=%s, 存档=%s, 世界=%s",
		server.SessionName, server.ArchiveName, server.WorldName)

	// 检查是否已经存在该服务器的监控器
	m.watcherMapMutex.Lock()
	if watcher, exists := m.watcherMap[server.SessionName]; exists {
		log.Printf("[DynamicLogMonitor] 服务器已有监控器: 会话=%s, 存档=%s, 世界=%s",
			server.SessionName, watcher.GetArchiveName(), watcher.GetWorldName())

		// 检查监控器是否正在运行
		if !watcher.IsRunning() {
			log.Printf("[DynamicLogMonitor] 监控器未运行，尝试重新启动: 会话=%s", server.SessionName)

			// 尝试重新启动监控器
			if err := watcher.Start(); err != nil {
				log.Printf("[DynamicLogMonitor] 重新启动监控器失败: %v", err)

				// 删除旧的监控器，稍后创建新的
				delete(m.watcherMap, server.SessionName)
			} else {
				log.Printf("[DynamicLogMonitor] 成功重新启动监控器: 会话=%s", server.SessionName)
				m.watcherMapMutex.Unlock()
				return
			}
		} else {
			m.watcherMapMutex.Unlock()
			return
		}
	}
	m.watcherMapMutex.Unlock()

	// 注意：我们不需要在这里显式地解析服务器类型
	// LogWatcher 将会自动检测世界类型（森林或洞穴）

	// 构建日志文件路径
	logFileName := "server_log.txt" // 无论森林还是洞穴，日志文件名都是server_log.txt
	logPath := filepath.Join(m.dstSavePath, server.ArchiveName, server.WorldName, logFileName)
	log.Printf("[DynamicLogMonitor] 监控日志文件: %s", logPath)

	// 检查日志文件是否存在
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		log.Printf("[DynamicLogMonitor] 日志文件不存在: %s", logPath)

		// 创建目录
		dir := filepath.Dir(logPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("[DynamicLogMonitor] 创建目录失败: %s, 错误: %v", dir, err)
			return
		}

		// 创建空文件
		file, err := os.Create(logPath)
		if err != nil {
			log.Printf("[DynamicLogMonitor] 创建日志文件失败: %s, 错误: %v", logPath, err)
			return
		}
		file.Close()
		log.Printf("[DynamicLogMonitor] 已创建空日志文件: %s", logPath)
	}

	// 创建日志监控器
	watcher, err := gamelog.NewLogWatcher(logPath, server.ArchiveName, server.WorldName)
	if err != nil {
		log.Printf("[DynamicLogMonitor] 创建日志监控器失败: %v", err)
		return
	}

	// 设置日志解析器
	parser, err := m.parserManager.GetParser(server.ArchiveName, server.WorldName)
	if err != nil {
		log.Printf("[DynamicLogMonitor] 获取日志解析器失败: %v", err)

		// 尝试创建新的解析器
		log.Printf("[DynamicLogMonitor] 尝试创建新的解析器")
		parser, err = logparser.NewLogParser(server.ArchiveName, server.WorldName)
		if err != nil {
			log.Printf("[DynamicLogMonitor] 创建新的解析器失败: %v", err)
			return
		}
		log.Printf("[DynamicLogMonitor] 成功创建新的解析器")
	} else {
		log.Printf("[DynamicLogMonitor] 成功获取日志解析器: 存档=%s, 世界=%s",
			server.ArchiveName, server.WorldName)
	}

	// 测试解析器
	testContent := "[00:00:00]: Starting Up\n[00:00:01]: Game version: 123456\n"
	log.Printf("[DynamicLogMonitor] 测试解析器处理内容: %s", testContent)
	if err := parser.ProcessAndSaveLog(testContent); err != nil {
		log.Printf("[DynamicLogMonitor] 测试解析器失败: %v", err)
	} else {
		log.Printf("[DynamicLogMonitor] 测试解析器成功")
	}

	// 读取现有日志文件内容
	log.Printf("[DynamicLogMonitor] 尝试读取现有日志文件内容: %s", logPath)
	existingContent, err := os.ReadFile(logPath)
	if err != nil {
		log.Printf("[DynamicLogMonitor] 读取现有日志文件失败: %v", err)
	} else if len(existingContent) > 0 {
		log.Printf("[DynamicLogMonitor] 成功读取现有日志文件，大小: %d 字节", len(existingContent))

		// 处理现有日志内容
		if err := parser.ProcessAndSaveLog(string(existingContent)); err != nil {
			log.Printf("[DynamicLogMonitor] 处理现有日志内容失败: %v", err)
		} else {
			log.Printf("[DynamicLogMonitor] 成功处理现有日志内容")
		}
	}

	watcher.SetLogParser(parser)
	watcher.EnableDBStore(true) // 启用数据库存储

	// 启动监控
	log.Printf("[DynamicLogMonitor] 尝试启动日志监控器: %s", logPath)
	if err := watcher.Start(); err != nil {
		log.Printf("[DynamicLogMonitor] 启动日志监控器失败: %v", err)
		watcher.Stop()
		return
	}
	log.Printf("[DynamicLogMonitor] 成功启动日志监控器: %s", logPath)

	// 手动触发一次日志读取
	watcher.ReadNewContent()

	log.Printf("[DynamicLogMonitor] 成功启动日志监控器: 会话=%s, 存档=%s, 世界=%s",
		server.SessionName, server.ArchiveName, server.WorldName)

	// 保存监控器引用
	m.watcherMapMutex.Lock()
	m.watcherMap[server.SessionName] = watcher
	log.Printf("[DynamicLogMonitor] 已保存监控器引用: %s, 当前有 %d 个监控器",
		server.SessionName, len(m.watcherMap))

	// 打印所有监控器的详细信息
	for k, v := range m.watcherMap {
		log.Printf("[DynamicLogMonitor] 当前监控器列表: %s -> 存档=%s, 世界=%s",
			k, v.GetArchiveName(), v.GetWorldName())
	}
	m.watcherMapMutex.Unlock()

	// 启动一个定时器，定期检查日志文件的变化
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// 检查服务器是否还在运行
				servers, err := m.getServerList()
				if err != nil {
					log.Printf("[DynamicLogMonitor] 获取服务器列表失败: %v", err)
					continue
				}

				// 检查服务器是否还在运行
				serverRunning := false
				for _, s := range servers {
					if s.SessionName == server.SessionName && s.Status == "running" {
						serverRunning = true
						break
					}
				}

				if !serverRunning {
					log.Printf("[DynamicLogMonitor] 服务器已停止运行，停止监控: %s", server.SessionName)
					return
				}

				// 手动触发日志读取
				m.watcherMapMutex.Lock()
				if w, exists := m.watcherMap[server.SessionName]; exists {
					log.Printf("[DynamicLogMonitor] 手动触发日志读取: %s", server.SessionName)
					w.ReadNewContent()
				}
				m.watcherMapMutex.Unlock()
			}
		}
	}()
}

// stopMonitoringServerWithDelay 延迟停止监控服务器日志
func (m *DynamicLogMonitor) stopMonitoringServerWithDelay(sessionName string, delay time.Duration) {
	// 等待指定时间
	time.Sleep(delay)

	// 停止监控
	m.watcherMapMutex.Lock()
	defer m.watcherMapMutex.Unlock()

	if watcher, ok := m.watcherMap[sessionName]; ok {
		watcher.Stop()
		delete(m.watcherMap, sessionName)
		// 只在调试模式下打印日志
		// log.Printf("[DynamicLogMonitor] 已停止监控服务器日志: %s", sessionName)
	}
}

// GetWatcherForServer 获取指定服务器的日志监控器
func (m *DynamicLogMonitor) GetWatcherForServer(sessionName string) (*gamelog.LogWatcher, bool) {
	m.watcherMapMutex.Lock()
	defer m.watcherMapMutex.Unlock()

	watcher, ok := m.watcherMap[sessionName]
	return watcher, ok
}

// GetAllWatchers 获取所有日志监控器
func (m *DynamicLogMonitor) GetAllWatchers() map[string]*gamelog.LogWatcher {
	// 直接返回空映射，避免死锁和超时
	result := make(map[string]*gamelog.LogWatcher)

	m.watcherMapMutex.Lock()
	// 打印调试信息
	log.Printf("[DynamicLogMonitor] GetAllWatchers: 当前有 %d 个监控器", len(m.watcherMap))

	// 复制监控器映射
	for k, v := range m.watcherMap {
		result[k] = v
		log.Printf("[DynamicLogMonitor] 监控器: %s, 存档=%s, 世界=%s",
			k, v.GetArchiveName(), v.GetWorldName())
	}
	m.watcherMapMutex.Unlock()

	return result
}

// 全局实例
var globalMonitor *DynamicLogMonitor
var globalMonitorMutex sync.Mutex

// SetDynamicLogMonitor 设置全局动态日志监控服务实例
func SetDynamicLogMonitor(monitor *DynamicLogMonitor) {
	globalMonitorMutex.Lock()
	defer globalMonitorMutex.Unlock()

	if monitor == nil {
		log.Printf("[DynamicLogMonitor] SetDynamicLogMonitor: 设置全局实例为空")
	} else {
		log.Printf("[DynamicLogMonitor] SetDynamicLogMonitor: 设置全局实例非空")
	}

	globalMonitor = monitor
}

// GetDynamicLogMonitor 获取全局动态日志监控服务实例
func GetDynamicLogMonitor() *DynamicLogMonitor {
	globalMonitorMutex.Lock()
	defer globalMonitorMutex.Unlock()

	if globalMonitor == nil {
		log.Printf("[DynamicLogMonitor] GetDynamicLogMonitor: 全局实例为空")
	} else {
		log.Printf("[DynamicLogMonitor] GetDynamicLogMonitor: 全局实例存在")
	}

	return globalMonitor
}
