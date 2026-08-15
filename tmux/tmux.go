package tmux

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	dstinstall "dont/internal/dstserver"
	"dont/pkg/configpath"

	"github.com/GianlucaP106/gotmux/gotmux"
	"github.com/go-ini/ini"
)

// DSTServer 表示一个饥荒服务器实例
type DSTServer struct {
	ArchiveName    string // 存档名称
	WorldName      string // 世界名称
	UGCDirectory   string // 模组目录
	StorageRoot    string // 存档根目录
	ConfDir        string // 配置目录
	SessionName    string // tmux会话名称
	StartDirectory string // 启动目录
	ServerMode     string // 服务器启动模式，32或64
	tmux           *gotmux.Tmux
}

// NewDSTServer 创建一个新的饥荒服务器实例
func NewDSTServer(archiveName, worldName, ugcDirectory, storageRoot, confDir string, startDirectory string, serverMode ...string) (*DSTServer, error) {
	log.Printf("[TMUX] 创建饥荒服务器实例 存档: %s, 世界: %s", archiveName, worldName)

	// 初始化tmux客户端
	log.Printf("[TMUX] 初始化tmux客户端")
	tmux, err := gotmux.DefaultTmux()
	if err != nil {
		log.Printf("[TMUX][错误] 初始化tmux失败: %v", err)
		return nil, fmt.Errorf("初始化tmux失败: %v", err)
	}

	// 创建会话名称
	sessionName := fmt.Sprintf("dstserver_%s_%s", archiveName, worldName)
	log.Printf("[TMUX] 创建会话名称: %s", sessionName)

	// 如果提供了启动目录，则使用提供的启动目录
	// 否则使用默认目录
	startDir := ""
	if startDirectory != "" {
		startDir = startDirectory
		log.Printf("[TMUX] 使用指定的启动目录: %s", startDir)
	} else {
		log.Printf("[TMUX] 未指定启动目录，使用默认目录")
	}

	// 如果提供了启动模式，则使用提供的启动模式
	// 否则使用默认模式
	mode := "64" // 默认使用 64 位模式
	if len(serverMode) > 0 && serverMode[0] != "" {
		// 验证启动模式是否有效
		if serverMode[0] == "32" || serverMode[0] == "64" {
			mode = serverMode[0]
			log.Printf("[TMUX] 使用指定的启动模式: %s", mode)
		} else {
			log.Printf("[TMUX] 指定的启动模式无效: %s，将使用默认值64", serverMode[0])
		}
	} else {
		log.Printf("[TMUX] 未指定启动模式，使用默认模式: %s", mode)
	}

	server := &DSTServer{
		ArchiveName:    archiveName,
		WorldName:      worldName,
		UGCDirectory:   ugcDirectory,
		StorageRoot:    storageRoot,
		ConfDir:        confDir,
		SessionName:    sessionName,
		StartDirectory: startDir,
		ServerMode:     mode,
		tmux:           tmux,
	}

	log.Printf("[TMUX] 饥荒服务器实例创建成功 会话名: %s, 启动模式: %s", sessionName, mode)
	return server, nil
}

func NewDSTServerWithSessionName(archiveName, worldName, sessionName, ugcDirectory, storageRoot, confDir string, startDirectory string, serverMode ...string) (*DSTServer, error) {
	server, err := NewDSTServer(archiveName, worldName, ugcDirectory, storageRoot, confDir, startDirectory, serverMode...)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(sessionName) == "" || strings.ContainsAny(sessionName, ":.\x00\r\n") {
		return nil, fmt.Errorf("无效的 tmux 会话名称")
	}
	server.SessionName = sessionName
	return server, nil
}

// IsRunning 检查服务器是否正在运行
func (s *DSTServer) IsRunning() (bool, error) {
	status, err := s.RuntimeStatus()
	return status.State == RuntimeRunning, err
}

// Start 启动饥荒服务器
func (s *DSTServer) Start() error {
	startTime := time.Now()
	log.Printf("[TMUX] 开始启动饥荒服务器 会话名: %s, 存档: %s, 世界: %s",
		s.SessionName, s.ArchiveName, s.WorldName)

	status, err := s.RuntimeStatus()
	if err != nil {
		log.Printf("[TMUX][错误] 检查服务器状态失败: %v", err)
		return err
	}
	if status.State == RuntimeRunning || status.State == RuntimeStarting {
		log.Printf("[TMUX][错误] 服务器已经在运行中: %s", s.SessionName)
		return fmt.Errorf("服务器已经在运行中: %s", s.SessionName)
	}
	if status.SessionExists {
		if err := s.KillSession(); err != nil {
			return fmt.Errorf("清理启动失败的旧会话: %w", err)
		}
	}
	if err := s.validateClusterAuth(); err != nil {
		return err
	}

	layout, ok := dstinstall.Resolve(s.StartDirectory, s.ServerMode)
	if !ok {
		return fmt.Errorf("在配置路径 %s 中未找到可执行的 DST 专服程序", s.StartDirectory)
	}
	log.Printf("[TMUX] 已识别服务端布局: %s, 可执行文件: %s", layout.Kind, layout.Executable)

	startCmd := buildStartCommand(layout, layout.Executable, []string{
		"-ugc_directory", s.UGCDirectory,
		"-persistent_storage_root", s.StorageRoot,
		"-conf_dir", s.ConfDir,
		"-cluster", s.ArchiveName,
		"-shard", s.WorldName,
	})
	log.Printf("[TMUX] 构建启动命令: %s", startCmd)

	// 使用gotmux的Command方法创建会话
	log.Printf("[TMUX] 正在创建tmux会话 会话名: %s", s.SessionName)

	log.Printf("[TMUX] 使用服务端工作目录: %s", layout.WorkingDirectory)
	output, err := s.tmux.Command("new-session", "-s", s.SessionName, "-c", layout.WorkingDirectory, "-d", startCmd)

	if err != nil {
		log.Printf("[TMUX][错误] 创建tmux会话失败: %v, 输出: %s", err, output)
		return fmt.Errorf("创建tmux会话失败: %v", err)
	}

	elapsedTime := time.Since(startTime)
	// 记录最终启动信息，包含启动模式、目录和可执行文件
	log.Printf("[TMUX] 已启动饥荒服务器: %s, 启动模式: %s, 耗时: %v",
		s.SessionName, s.ServerMode, elapsedTime)

	// 保存服务器信息供查询
	SaveServerInfo(s)

	return nil
}

func shellArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// Stop 停止饥荒服务器
func (s *DSTServer) Stop() error {
	startTime := time.Now()
	log.Printf("[TMUX] 开始停止饥荒服务器 会话名: %s", s.SessionName)

	status, err := s.RuntimeStatus()
	if err != nil {
		log.Printf("[TMUX][错误] 检查服务器状态失败: %v", err)
		return err
	}
	if !status.SessionExists {
		return nil
	}

	// 向会话发送关闭命令
	log.Printf("[TMUX] 向会话发送关闭命令 会话名: %s", s.SessionName)
	err = s.sendCommand("c_shutdown(true)", false)
	if err != nil {
		log.Printf("[TMUX][错误] 发送关闭命令失败: %v", err)
		return fmt.Errorf("发送关闭命令失败: %v", err)
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[TMUX] 已发送关闭命令到服务器: %s, 耗时: %v", s.SessionName, elapsedTime)
	return nil
}

// SendCommand 向服务器发送命令
func (s *DSTServer) SendCommand(command string) error {
	return s.sendCommand(command, true)
}

func (s *DSTServer) sendCommand(command string, requireRunning bool) error {
	startTime := time.Now()
	log.Printf("[TMUX] 开始向服务器发送命令 会话名: %s, 命令: %s", s.SessionName, command)

	status, err := s.RuntimeStatus()
	if err != nil {
		log.Printf("[TMUX][错误] 检查服务器状态失败: %v", err)
		return err
	}
	if requireRunning && status.State != RuntimeRunning {
		return fmt.Errorf("服务器当前状态为 %s，无法执行控制台命令: %s", status.State, status.Message)
	}
	if !status.SessionExists {
		return fmt.Errorf("没有找到会话: %s", s.SessionName)
	}

	// 获取会话
	log.Printf("[TMUX] 获取会话 会话名: %s", s.SessionName)
	session, err := s.tmux.GetSessionByName(s.SessionName)
	if err != nil {
		// 检查错误是否是因为没有tmux会话
		if strings.Contains(err.Error(), "failed to list sessions") || strings.Contains(err.Error(), "no session") {
			log.Printf("[TMUX] 没有找到会话: %s", s.SessionName)
			return fmt.Errorf("没有找到会话: %s", s.SessionName)
		} else {
			log.Printf("[TMUX][错误] 获取会话失败: %v", err)
			return fmt.Errorf("获取会话失败: %v", err)
		}
	}

	// 获取窗口
	log.Printf("[TMUX] 获取窗口列表 会话名: %s", s.SessionName)
	windows, err := session.ListWindows()
	if err != nil {
		log.Printf("[TMUX][错误] 获取窗口列表失败: %v", err)
		return fmt.Errorf("获取窗口列表失败: %v", err)
	}
	if len(windows) == 0 {
		log.Printf("[TMUX][错误] 会话没有窗口: %s", s.SessionName)
		return fmt.Errorf("会话没有窗口: %s", s.SessionName)
	}

	// 获取第一个窗口的第一个面板
	log.Printf("[TMUX] 获取面板列表 会话名: %s, 窗口索引: %d", s.SessionName, windows[0].Index)
	panes, err := windows[0].ListPanes()
	if err != nil {
		log.Printf("[TMUX][错误] 获取面板列表失败: %v", err)
		return fmt.Errorf("获取面板列表失败: %v", err)
	}
	if len(panes) == 0 {
		log.Printf("[TMUX][错误] 窗口没有面板: %s", s.SessionName)
		return fmt.Errorf("窗口没有面板: %s", s.SessionName)
	}

	// 向面板发送命令
	// 使用gotmux的Command方法发送命令
	log.Printf("[TMUX] 发送命令 会话名: %s, 命令: %s", s.SessionName, command)
	output, err := s.tmux.Command("send-keys", "-t", s.SessionName, command, "C-m")
	if err != nil {
		log.Printf("[TMUX][错误] 发送命令失败: %v, 输出: %s", err, output)
		return fmt.Errorf("发送命令失败: %v", err)
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[TMUX] 已发送命令到服务器: %s, 命令: %s, 耗时: %v", s.SessionName, command, elapsedTime)
	return nil
}

// KillSession 强制终止会话
func (s *DSTServer) KillSession() error {
	startTime := time.Now()
	log.Printf("[TMUX] 开始强制终止会话 会话名: %s", s.SessionName)

	exists, err := s.SessionExists()
	if err != nil {
		log.Printf("[TMUX][错误] 检查服务器状态失败: %v", err)
		return err
	}
	if !exists {
		return nil
	}

	// 使用Command方法终止会话
	log.Printf("[TMUX] 执行终止会话命令 会话名: %s", s.SessionName)
	output, err := s.tmux.Command("kill-session", "-t", s.SessionName)
	if err != nil {
		log.Printf("[TMUX][错误] 终止会话失败: %v, 输出: %s", err, output)
		return fmt.Errorf("终止会话失败: %v", err)
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[TMUX] 已强制终止会话: %s, 耗时: %v", s.SessionName, elapsedTime)
	return nil
}

// Restart 重启饥荒服务器，先停止再启动
func (s *DSTServer) Restart() error {
	startTime := time.Now()
	log.Printf("[TMUX] 开始重启饥荒服务器 会话名: %s", s.SessionName)

	// 保存服务器的原始信息，确保重启后使用相同的参数
	originalServerInfo := &DSTServer{
		ArchiveName:    s.ArchiveName,
		WorldName:      s.WorldName,
		UGCDirectory:   s.UGCDirectory,
		StorageRoot:    s.StorageRoot,
		ConfDir:        s.ConfDir,
		SessionName:    s.SessionName,
		StartDirectory: s.StartDirectory,
		ServerMode:     s.ServerMode,
		tmux:           s.tmux,
	}

	// 从全局映射中获取服务器的完整信息
	serverInfoMapMutex.Lock()
	var originalInfo *ServerInfo
	if info, exists := serverInfoMap[s.SessionName]; exists {
		// 创建一个副本，避免引用原始对象
		originalInfo = &ServerInfo{
			SessionName:    info.SessionName,
			ArchiveName:    info.ArchiveName,
			WorldName:      info.WorldName,
			ServerMode:     info.ServerMode,
			StartDirectory: info.StartDirectory,
			Status:         info.Status,
			StartTime:      info.StartTime,
			IsMaster:       info.IsMaster,
		}
		log.Printf("[TMUX] 已保存服务器原始信息: 模式=%s, 启动时间=%s",
			originalInfo.ServerMode, originalInfo.StartTime)
	}
	serverInfoMapMutex.Unlock()

	// 检查服务器是否在运行
	log.Printf("[TMUX] 检查服务器是否在运行")
	running, err := s.IsRunning()
	if err != nil {
		log.Printf("[TMUX][错误] 检查服务器状态失败: %v", err)
		return err
	}

	// 如果服务器正在运行，先停止它
	if running {
		log.Printf("[TMUX] 服务器正在运行，先停止它")
		// 先尝试优雅地停止
		stopErr := s.Stop()
		if stopErr != nil {
			log.Printf("[TMUX][警告] 优雅停止服务器失败: %v, 将等待一段时间后再检查", stopErr)
		}

		// 设置最大等待时间为30秒
		maxWaitTime := 30 * time.Second
		checkInterval := 1 * time.Second // 每秒检查一次服务器状态
		timeoutTimer := time.NewTimer(maxWaitTime)
		checkTicker := time.NewTicker(checkInterval)
		defer timeoutTimer.Stop()
		defer checkTicker.Stop()

		log.Printf("[TMUX] 开始等待服务器停止，最长等待时间: %v", maxWaitTime)

		// 定期检查服务器是否已停止
		serverStopped := false
	waitLoop:
		for {
			select {
			case <-timeoutTimer.C:
				// 超时，强制终止
				log.Printf("[TMUX][警告] 等待服务器停止超时(%v)，尝试强制终止", maxWaitTime)
				killErr := s.KillSession()
				if killErr != nil {
					log.Printf("[TMUX][错误] 强制终止服务器失败: %v", killErr)
					return fmt.Errorf("等待服务器停止超时，强制终止也失败: %v", killErr)
				}
				break waitLoop

			case <-checkTicker.C:
				// 检查服务器是否已停止
				log.Printf("[TMUX] 检查服务器是否已停止")
				stillRunning, err := s.IsRunning()
				if err != nil {
					log.Printf("[TMUX][警告] 检查服务器状态失败: %v, 继续等待", err)
					continue
				}

				if !stillRunning {
					log.Printf("[TMUX] 服务器已成功停止")
					serverStopped = true
					break waitLoop
				} else {
					log.Printf("[TMUX] 服务器仍在运行，继续等待")
				}
			}
		}

		// 如果服务器已停止，等待一小段时间确保完全停止
		if serverStopped {
			log.Printf("[TMUX] 服务器已停止，等待额外的几秒确保完全停止")
			time.Sleep(2 * time.Second)
		} else {
			// 如果是通过强制终止停止的，等待更长时间
			log.Printf("[TMUX] 服务器已强制终止，等待额外的时间确保完全停止")
			time.Sleep(5 * time.Second)
		}
	}

	// 使用原始服务器对象启动服务器，确保使用相同的参数
	log.Printf("[TMUX] 开始启动服务器，使用原始参数")
	log.Printf("[TMUX] 原始参数: 模式=%s, 启动目录=%s",
		originalServerInfo.ServerMode, originalServerInfo.StartDirectory)

	// 使用原始服务器对象启动服务器
	err = originalServerInfo.Start()
	if err != nil {
		log.Printf("[TMUX][错误] 启动服务器失败: %v", err)
		return fmt.Errorf("启动服务器失败: %v", err)
	}

	// 如果有原始信息，恢复原始启动时间
	if originalInfo != nil {
		serverInfoMapMutex.Lock()
		if newInfo, exists := serverInfoMap[s.SessionName]; exists {
			// 恢复原始启动时间，但保留新的运行状态
			newInfo.StartTime = originalInfo.StartTime
			log.Printf("[TMUX] 已恢复服务器原始启动时间: %s", originalInfo.StartTime)
		}
		serverInfoMapMutex.Unlock()
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[TMUX] 已重启饥荒服务器: %s, 模式: %s, 启动目录: %s, 耗时: %v",
		s.SessionName, originalServerInfo.ServerMode, originalServerInfo.StartDirectory, elapsedTime)
	return nil
}

// ServerInfo 服务器信息结构体
type ServerInfo struct {
	SessionName    string `json:"session_name"`    // 会话名称
	ArchiveName    string `json:"archive_name"`    // 存档名称
	WorldName      string `json:"world_name"`      // 世界名称
	ServerMode     string `json:"server_mode"`     // 服务器启动模式（32位或64位）
	StartDirectory string `json:"start_directory"` // 启动目录
	Status         string `json:"status"`          // 服务器状态（运行中/已停止）
	StartTime      string `json:"start_time"`      // 启动时间
	IsMaster       bool   `json:"is_master"`       // 是否为主世界
}

// 全局变量，用于存储服务器信息
var serverInfoMap map[string]*ServerInfo

// 互斥锁，保护并发访问serverInfoMap
var serverInfoMapMutex sync.Mutex

// 初始化函数
func init() {
	serverInfoMap = make(map[string]*ServerInfo)
	log.Printf("[TMUX] 初始化serverInfoMap")
}

// SaveServerInfo 保存服务器信息
func SaveServerInfo(server *DSTServer) {
	// 获取当前时间，确保使用本地时区
	now := time.Now()
	startTimeStr := now.Format("2006-01-02 15:04:05")

	// 检查是否为主世界
	isMaster := false
	serverIniPath := filepath.Join(server.StorageRoot, server.ConfDir, server.ArchiveName, server.WorldName, "server.ini")
	if _, err := os.Stat(serverIniPath); !os.IsNotExist(err) {
		// 读取并解析server.ini文件
		if cfg, err := ini.Load(serverIniPath); err == nil {
			// 检查是否有SHARD部分和is_master配置项
			if cfg.Section("SHARD").HasKey("is_master") {
				// 获取is_master的值并转换为布尔值
				isMasterStr := cfg.Section("SHARD").Key("is_master").String()
				isMaster = strings.ToLower(isMasterStr) == "true"
				log.Printf("[TMUX] 服务器 %s 的is_master值为: %v", server.SessionName, isMaster)
			}
		}
	}

	info := &ServerInfo{
		SessionName:    server.SessionName,
		ArchiveName:    server.ArchiveName,
		WorldName:      server.WorldName,
		ServerMode:     server.ServerMode,
		StartDirectory: server.StartDirectory,
		Status:         "running",
		StartTime:      startTimeStr,
		IsMaster:       isMaster,
	}

	// 使用互斥锁保护并发访问
	serverInfoMapMutex.Lock()
	serverInfoMap[server.SessionName] = info
	serverInfoMapMutex.Unlock()

	log.Printf("[TMUX] 已保存服务器信息: %s, 模式: %s, 启动时间: %s, 主世界: %v (实际时间: %s)",
		server.SessionName, server.ServerMode, startTimeStr, isMaster, now.Format("2006-01-02 15:04:05.000"))
}

// ListDSTServers 列出所有饥荒服务器会话
// silent 参数控制是否输出日志信息，true表示不输出正常日志，只输出错误日志
func ListDSTServers(silent ...bool) ([]ServerInfo, error) {
	startTime := time.Now()

	// 判断是否需要输出日志
	isSilent := false
	if len(silent) > 0 && silent[0] {
		isSilent = true
	}

	if !isSilent {
		log.Printf("[TMUX] 开始列出所有饥荒服务器会话")
	}

	// 初始化tmux客户端
	if !isSilent {
		log.Printf("[TMUX] 初始化tmux客户端")
	}
	tmux, err := gotmux.DefaultTmux()
	if err != nil {
		log.Printf("[TMUX][错误] 初始化tmux失败: %v", err)
		return nil, fmt.Errorf("初始化tmux失败: %v", err)
	}

	// 列出所有会话
	//log.Printf("[TMUX] 获取tmux会话列表")
	sessions, err := tmux.ListSessions()
	if err != nil {
		// 检查错误是否是因为没有tmux会话
		if strings.Contains(err.Error(), "failed to list sessions") {
			// 没有tmux会话是正常情况，使用信息日志而不是错误日志
			if !isSilent {
				log.Printf("[TMUX] 没有运行中的tmux会话")
			}
			return nil, fmt.Errorf("获取tmux会话列表失败: %v", err)
		} else {
			// 其他错误仍然记录为错误
			log.Printf("[TMUX][错误] 获取tmux会话列表失败: %v", err)
			return nil, fmt.Errorf("获取tmux会话列表失败: %v", err)
		}
	}

	// 输出所有会话的详细信息到日志
	if !isSilent {
		log.Printf("[TMUX] 找到 %d 个tmux会话", len(sessions))
	}
	//for i, session := range sessions {
	//	// 输出会话的所有字段
	//	//sessionJSON, _ := json.Marshal(session)
	//	//log.Printf("[TMUX] 会话 #%d 原始信息: %s", i+1, string(sessionJSON))
	//
	//	// 尝试获取更多会话信息
	//	//detailedSession, err := tmux.GetSessionByName(session.Name)
	//	//if err == nil {
	//	//	detailedJSON, _ := json.Marshal(detailedSession)
	//	//	//log.Printf("[TMUX] 会话 #%d 详细信息: %s", i+1, string(detailedJSON))
	//	//}
	//}

	// 筛选出饥荒服务器会话并更新状态
	var result []ServerInfo
	runningSessionMap := make(map[string]bool)

	// 记录当前运行的会话及其创建时间
	runningSessionCreationTimes := make(map[string]string)
	for _, session := range sessions {
		if strings.HasPrefix(session.Name, "dstserver_") {
			runningSessionMap[session.Name] = true
			// 如果会话有创建时间，记录下来
			if session.Created != "" {
				//log.Printf("[TMUX] 会话 %s 的原始创建时间: %s", session.Name, session.Created)
				runningSessionCreationTimes[session.Name] = session.Created
			}
		}
	}

	// 获取互斥锁
	serverInfoMapMutex.Lock()
	defer serverInfoMapMutex.Unlock()

	// 更新所有已知服务器的状态
	for sessionName, info := range serverInfoMap {
		// 检查会话是否仍在运行
		if running, exists := runningSessionMap[sessionName]; exists && running {
			info.Status = "running"

			// 如果有tmux会话的创建时间，使用它替代当前的启动时间
			if createdTime, exists := runningSessionCreationTimes[sessionName]; exists && createdTime != "" {
				// 尝试将tmux的创建时间转换为我们的时间格式
				// tmux的时间格式可能是不同的，需要尝试多种格式
				var parsedTime time.Time
				var parseErr error

				// 先尝试将创建时间解析为Unix时间戳
				timestamp, err := strconv.ParseInt(createdTime, 10, 64)
				if err == nil {
					// 成功解析为时间戳
					parsedTime = time.Unix(timestamp, 0)
					parseErr = nil
					//log.Printf("[TMUX] 成功将创建时间 %s 解析为时间戳: %v", createdTime, parsedTime)
				} else {
					// 如果不是时间戳，尝试多种时间格式
					log.Printf("[TMUX] 创建时间 %s 不是时间戳格式，尝试其他格式", createdTime)
					timeFormats := []string{
						"2006-01-02 15:04:05",
						"Mon Jan 2 15:04:05 2006",
						"Mon Jan 2 15:04:05 MST 2006",
						"Mon Jan _2 15:04:05 2006",
						"Mon Jan _2 15:04:05 MST 2006",
					}

					for _, format := range timeFormats {
						parsedTime, parseErr = time.Parse(format, createdTime)
						if parseErr == nil {
							break
						}
					}
				}

				if parseErr == nil {
					// 成功解析时间，更新启动时间
					info.StartTime = parsedTime.Format("2006-01-02 15:04:05")
					//log.Printf("[TMUX] 更新会话 %s 的启动时间为: %s (从原始创建时间: %s)",
					//	sessionName, info.StartTime, createdTime)
				} else {
					log.Printf("[TMUX][警告] 无法解析会话 %s 的创建时间: %s, 错误: %v",
						sessionName, createdTime, parseErr)
				}
			}

			// 只保留启动时间信息
		} else {
			info.Status = "stopped"
		}

		// 添加到结果中
		result = append(result, *info)
	}

	// 检查是否有新的会话未记录
	for sessionName := range runningSessionMap {
		if _, exists := serverInfoMap[sessionName]; !exists {
			// 解析会话名称获取存档和世界信息
			parsed := ParseSessionName(sessionName)
			if parsed.Valid {
				// 创建新的服务器信息
				startTime := "unknown"

				// 如果有tmux会话的创建时间，使用它作为启动时间
				if createdTime, exists := runningSessionCreationTimes[sessionName]; exists && createdTime != "" {
					// 尝试将tmux的创建时间转换为我们的时间格式
					var parsedTime time.Time
					var parseErr error

					// 先尝试将创建时间解析为Unix时间戳
					timestamp, err := strconv.ParseInt(createdTime, 10, 64)
					if err == nil {
						// 成功解析为时间戳
						parsedTime = time.Unix(timestamp, 0)
						parseErr = nil
						//log.Printf("[TMUX] 成功将新会话创建时间 %s 解析为时间戳: %v", createdTime, parsedTime)
					} else {
						// 如果不是时间戳，尝试多种时间格式
						log.Printf("[TMUX] 新会话创建时间 %s 不是时间戳格式，尝试其他格式", createdTime)
						timeFormats := []string{
							"2006-01-02 15:04:05",
							"Mon Jan 2 15:04:05 2006",
							"Mon Jan 2 15:04:05 MST 2006",
							"Mon Jan _2 15:04:05 2006",
							"Mon Jan _2 15:04:05 MST 2006",
						}

						for _, format := range timeFormats {
							parsedTime, parseErr = time.Parse(format, createdTime)
							if parseErr == nil {
								break
							}
						}
					}

					if parseErr == nil {
						// 成功解析时间，更新启动时间
						startTime = parsedTime.Format("2006-01-02 15:04:05")
						//log.Printf("[TMUX] 新会话 %s 的启动时间设置为: %s (从原始创建时间: %s)",
						//	sessionName, startTime, createdTime)
					} else {
						log.Printf("[TMUX][警告] 无法解析新会话 %s 的创建时间: %s, 错误: %v",
							sessionName, createdTime, parseErr)
					}
				}

				// 检查是否为主世界
				isMaster := false

				// 尝试从存档目录中找到server.ini文件
				// 首先尝试从配置文件中获取存档路径
				configFile := configpath.Current()
				dstSavePath := "./Klei/DoNotStarveTogether"
				if _, err := os.Stat(configFile); !os.IsNotExist(err) {
					if cfg, err := ini.Load(configFile); err == nil {
						// 读取路径配置
						if cfg.Section("paths").HasKey("DST_SAVE_PATH") {
							dstSavePath = cfg.Section("paths").Key("DST_SAVE_PATH").String()
						}
					}
				}

				// 构建server.ini文件路径
				serverIniPath := filepath.Join(dstSavePath, parsed.ClusterName, parsed.ShardName, "server.ini")
				if _, err := os.Stat(serverIniPath); !os.IsNotExist(err) {
					// 读取并解析server.ini文件
					if cfg, err := ini.Load(serverIniPath); err == nil {
						// 检查是否有SHARD部分和is_master配置项
						if cfg.Section("SHARD").HasKey("is_master") {
							// 获取is_master的值并转换为布尔值
							isMasterStr := cfg.Section("SHARD").Key("is_master").String()
							isMaster = strings.ToLower(isMasterStr) == "true"
							if !isSilent {
								log.Printf("[TMUX] 新发现的服务器 %s 的is_master值为: %v", sessionName, isMaster)
							}
						}
					}
				}

				info := ServerInfo{
					SessionName: sessionName,
					ArchiveName: parsed.ClusterName,
					WorldName:   parsed.ShardName,
					ServerMode:  "unknown", // 未知模式
					Status:      "running",
					StartTime:   startTime,
					IsMaster:    isMaster,
				}

				// 添加到结果和映射中
				result = append(result, info)
				serverInfoMap[sessionName] = &info
			}
		}
	}

	elapsedTime := time.Since(startTime)
	if !isSilent {
		log.Printf("[TMUX] 已列出所有饥荒服务器会话，共 %d 个, 耗时: %v", len(result), elapsedTime)
	}
	return result, nil
}

// GetSessionInfo 获取会话信息
func GetSessionInfo(sessionName string) (map[string]string, error) {
	startTime := time.Now()
	log.Printf("[TMUX] 开始获取会话信息 会话名: %s", sessionName)

	// 初始化tmux客户端
	//log.Printf("[TMUX] 初始化tmux客户端")
	tmux, err := gotmux.DefaultTmux()
	if err != nil {
		log.Printf("[TMUX][错误] 初始化tmux失败: %v", err)
		return nil, fmt.Errorf("初始化tmux失败: %v", err)
	}

	// 获取会话
	log.Printf("[TMUX] 获取会话 会话名: %s", sessionName)
	session, err := tmux.GetSessionByName(sessionName)
	if err != nil {
		// 检查错误是否是因为没有tmux会话
		if strings.Contains(err.Error(), "failed to list sessions") || strings.Contains(err.Error(), "no session") {
			log.Printf("[TMUX] 没有找到会话: %s", sessionName)
			return nil, fmt.Errorf("没有找到会话: %s", sessionName)
		} else {
			log.Printf("[TMUX][错误] 获取会话失败: %v", err)
			return nil, fmt.Errorf("获取会话失败: %v", err)
		}
	}

	// 解析会话名称获取存档和世界信息
	parts := ParseSessionName(sessionName)
	if !parts.Valid {
		log.Printf("[TMUX][错误] 会话名称格式不正确: %s", sessionName)
		return nil, fmt.Errorf("会话名称格式不正确: %s", sessionName)
	}

	info := map[string]string{
		"SessionName": sessionName,
		"ArchiveName": parts.ClusterName,
		"WorldName":   parts.ShardName,
		"Created":     session.Created,
		"Attached":    fmt.Sprintf("%d", session.Attached),
		"Windows":     fmt.Sprintf("%d", session.Windows),
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[TMUX] 已获取会话信息 会话名: %s, 耗时: %v", sessionName, elapsedTime)
	return info, nil
}

// ExecuteShellCommand 执行shell命令
func ExecuteShellCommand(command string) (string, error) {
	log.Printf("[TMUX] 执行shell命令: %s", command)
	cmd := exec.Command("bash", "-c", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[TMUX][错误] 执行命令失败: %v, 输出: %s", err, string(output))
		return "", fmt.Errorf("执行命令失败: %v, 输出: %s", err, string(output))
	}
	log.Printf("[TMUX] 命令执行成功, 输出长度: %d字节", len(output))
	return string(output), nil
}

// ExecuteTmuxCommand 执行tmux命令
func ExecuteTmuxCommand(args ...string) (string, error) {
	log.Printf("[TMUX] 执行tmux命令: %v", args)

	// 初始化tmux客户端
	tmux, err := gotmux.DefaultTmux()
	if err != nil {
		log.Printf("[TMUX][错误] 初始化tmux失败: %v", err)
		return "", fmt.Errorf("初始化tmux失败: %v", err)
	}

	// 执行命令
	output, err := tmux.Command(args...)
	if err != nil {
		log.Printf("[TMUX][错误] 执行tmux命令失败: %v, 参数: %v", err, args)
		return "", err
	}

	log.Printf("[TMUX] tmux命令执行成功, 参数: %v, 输出长度: %d字节", args, len(output))
	return output, nil
}
