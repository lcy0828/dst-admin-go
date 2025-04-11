package tmux

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GianlucaP106/gotmux/gotmux"
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

// IsRunning 检查服务器是否正在运行
func (s *DSTServer) IsRunning() (bool, error) {
	log.Printf("[TMUX] 检查服务器运行状态 会话名: %s", s.SessionName)

	// 使用两种方法检查会话是否存在
	// 方法1: 使用gotmux的ListSessions方法
	log.Printf("[TMUX] 方法1: 使用gotmux的ListSessions方法检查")
	sessions, err := s.tmux.ListSessions()
	if err != nil {
		log.Printf("[TMUX][警告] 使用gotmux获取tmux会话列表失败: %v, 将尝试方法2", err)
	} else {
		// 检查是否存在指定名称的会话
		for _, session := range sessions {
			log.Printf("[TMUX] 检测到会话: %s", session.Name)
			if session.Name == s.SessionName {
				log.Printf("[TMUX] 方法1检测到服务器正在运行 会话名: %s", s.SessionName)
				return true, nil
			}
		}
		log.Printf("[TMUX] 方法1未检测到服务器运行, 将尝试方法2")
	}

	// 方法2: 直接使用tmux has-session命令检查
	log.Printf("[TMUX] 方法2: 使用tmux has-session命令检查")
	cmd := exec.Command("tmux", "has-session", "-t", s.SessionName)
	err = cmd.Run()
	if err == nil {
		// 如果命令执行成功，说明会话存在
		log.Printf("[TMUX] 方法2检测到服务器正在运行 会话名: %s", s.SessionName)
		return true, nil
	}

	// 如果两种方法都未检测到会话，则认为服务器未运行
	log.Printf("[TMUX] 两种方法都未检测到服务器运行 会话名: %s", s.SessionName)
	return false, nil
}

// Start 启动饥荒服务器
func (s *DSTServer) Start() error {
	startTime := time.Now()
	log.Printf("[TMUX] 开始启动饥荒服务器 会话名: %s, 存档: %s, 世界: %s",
		s.SessionName, s.ArchiveName, s.WorldName)

	// 检查服务器是否已经在运行
	log.Printf("[TMUX] 检查服务器是否已经在运行")
	running, err := s.IsRunning()
	if err != nil {
		log.Printf("[TMUX][错误] 检查服务器状态失败: %v", err)
		return err
	}
	if running {
		log.Printf("[TMUX][错误] 服务器已经在运行中: %s", s.SessionName)
		return fmt.Errorf("服务器已经在运行中: %s", s.SessionName)
	}

	// 根据启动模式选择正确的可执行文件
	executableName := ""
	if s.ServerMode == "64" {
		executableName = "dontstarve_dedicated_server_nullrenderer_x64"
		log.Printf("[TMUX] 启动模式: 64位, 使用可执行文件: %s", executableName)
	} else {
		executableName = "dontstarve_dedicated_server_nullrenderer"
		log.Printf("[TMUX] 启动模式: 32位, 使用可执行文件: %s", executableName)
	}

	// 构建启动命令
	startCmd := fmt.Sprintf("./%s -ugc_directory %s -persistent_storage_root %s -conf_dir %s -cluster %s -shard %s",
		executableName, s.UGCDirectory, s.StorageRoot, s.ConfDir, s.ArchiveName, s.WorldName)
	log.Printf("[TMUX] 构建启动命令: %s", startCmd)

	// 使用gotmux的Command方法创建会话
	log.Printf("[TMUX] 正在创建tmux会话 会话名: %s", s.SessionName)

	// 如果指定了启动目录，则使用指定的启动目录
	var output string
	if s.StartDirectory != "" {
		// 使用 -c 参数指定启动目录
		log.Printf("[TMUX] 使用指定的启动目录: %s", s.StartDirectory)

		// 根据启动模式和目录存在情况选择正确的目录
		binDir := s.StartDirectory
		if !strings.HasSuffix(binDir, "/bin") && !strings.HasSuffix(binDir, "/bin64") {
			// 如果路径不以bin或bin64结尾，则根据启动模式选择目录
			bin64Path := fmt.Sprintf("%s/bin64", s.StartDirectory)
			binPath := fmt.Sprintf("%s/bin", s.StartDirectory)

			// 首先检查目录是否存在
			bin64Exists := false
			binExists := false

			if _, err := os.Stat(bin64Path); !os.IsNotExist(err) {
				bin64Exists = true
				log.Printf("[TMUX] 检测到bin64目录存在: %s", bin64Path)
			}

			if _, err := os.Stat(binPath); !os.IsNotExist(err) {
				binExists = true
				log.Printf("[TMUX] 检测到bin目录存在: %s", binPath)
			}

			// 根据启动模式和目录存在情况选择目录
			if s.ServerMode == "64" {
				// 64位模式优先使用bin64目录
				if bin64Exists {
					binDir = bin64Path
					log.Printf("[TMUX] 64位模式，使用bin64目录: %s", binDir)
				} else if binExists {
					binDir = binPath
					log.Printf("[TMUX] 64位模式，但bin64目录不存在，使用bin目录: %s", binDir)
				} else {
					log.Printf("[TMUX] 64位模式，但bin和bin64目录都不存在，使用原始目录: %s", binDir)
				}
			} else {
				// 32位模式优先使用bin目录
				if binExists {
					binDir = binPath
					log.Printf("[TMUX] 32位模式，使用bin目录: %s", binDir)
				} else if bin64Exists {
					binDir = bin64Path
					log.Printf("[TMUX] 32位模式，但bin目录不存在，使用bin64目录: %s", binDir)
				} else {
					log.Printf("[TMUX] 32位模式，但bin和bin64目录都不存在，使用原始目录: %s", binDir)
				}
			}
		}

		// 使用 -c 参数指定启动目录
		output, err = s.tmux.Command("new-session", "-s", s.SessionName, "-c", binDir, "-d", startCmd)
	} else {
		// 不指定启动目录，使用默认目录
		log.Printf("[TMUX] 使用默认启动目录")
		output, err = s.tmux.Command("new-session", "-s", s.SessionName, "-d", startCmd)
	}

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

// Stop 停止饥荒服务器
func (s *DSTServer) Stop() error {
	startTime := time.Now()
	log.Printf("[TMUX] 开始停止饥荒服务器 会话名: %s", s.SessionName)

	// 检查服务器是否在运行
	log.Printf("[TMUX] 检查服务器是否在运行")
	running, err := s.IsRunning()
	if err != nil {
		log.Printf("[TMUX][错误] 检查服务器状态失败: %v", err)
		return err
	}
	if !running {
		log.Printf("[TMUX][错误] 服务器未运行: %s", s.SessionName)
		return fmt.Errorf("服务器未运行: %s", s.SessionName)
	}

	// 向会话发送关闭命令
	log.Printf("[TMUX] 向会话发送关闭命令 会话名: %s", s.SessionName)
	err = s.SendCommand("c_shutdown(true)")
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
	startTime := time.Now()
	log.Printf("[TMUX] 开始向服务器发送命令 会话名: %s, 命令: %s", s.SessionName, command)

	// 检查服务器是否在运行
	log.Printf("[TMUX] 检查服务器是否在运行")
	running, err := s.IsRunning()
	if err != nil {
		log.Printf("[TMUX][错误] 检查服务器状态失败: %v", err)
		return err
	}
	if !running {
		log.Printf("[TMUX][错误] 服务器未运行: %s", s.SessionName)
		return fmt.Errorf("服务器未运行: %s", s.SessionName)
	}

	// 获取会话
	log.Printf("[TMUX] 获取会话 会话名: %s", s.SessionName)
	session, err := s.tmux.GetSessionByName(s.SessionName)
	if err != nil {
		log.Printf("[TMUX][错误] 获取会话失败: %v", err)
		return fmt.Errorf("获取会话失败: %v", err)
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

	// 检查服务器是否在运行
	log.Printf("[TMUX] 检查服务器是否在运行")
	running, err := s.IsRunning()
	if err != nil {
		log.Printf("[TMUX][错误] 检查服务器状态失败: %v", err)
		return err
	}
	if !running {
		log.Printf("[TMUX][错误] 服务器未运行: %s", s.SessionName)
		return fmt.Errorf("服务器未运行: %s", s.SessionName)
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

	info := &ServerInfo{
		SessionName:    server.SessionName,
		ArchiveName:    server.ArchiveName,
		WorldName:      server.WorldName,
		ServerMode:     server.ServerMode,
		StartDirectory: server.StartDirectory,
		Status:         "running",
		StartTime:      startTimeStr,
	}

	// 使用互斥锁保护并发访问
	serverInfoMapMutex.Lock()
	serverInfoMap[server.SessionName] = info
	serverInfoMapMutex.Unlock()

	log.Printf("[TMUX] 已保存服务器信息: %s, 模式: %s, 启动时间: %s (实际时间: %s)",
		server.SessionName, server.ServerMode, startTimeStr, now.Format("2006-01-02 15:04:05.000"))
}

// ListDSTServers 列出所有饥荒服务器会话
func ListDSTServers() ([]ServerInfo, error) {
	startTime := time.Now()
	log.Printf("[TMUX] 开始列出所有饥荒服务器会话")

	// 初始化tmux客户端
	log.Printf("[TMUX] 初始化tmux客户端")
	tmux, err := gotmux.DefaultTmux()
	if err != nil {
		log.Printf("[TMUX][错误] 初始化tmux失败: %v", err)
		return nil, fmt.Errorf("初始化tmux失败: %v", err)
	}

	// 列出所有会话
	log.Printf("[TMUX] 获取tmux会话列表")
	sessions, err := tmux.ListSessions()
	if err != nil {
		log.Printf("[TMUX][错误] 获取tmux会话列表失败: %v", err)
		return nil, fmt.Errorf("获取tmux会话列表失败: %v", err)
	}

	// 输出所有会话的详细信息到日志
	log.Printf("[TMUX] 找到 %d 个tmux会话", len(sessions))
	for i, session := range sessions {
		// 输出会话的所有字段
		sessionJSON, _ := json.Marshal(session)
		log.Printf("[TMUX] 会话 #%d 原始信息: %s", i+1, string(sessionJSON))

		// 尝试获取更多会话信息
		detailedSession, err := tmux.GetSessionByName(session.Name)
		if err == nil {
			detailedJSON, _ := json.Marshal(detailedSession)
			log.Printf("[TMUX] 会话 #%d 详细信息: %s", i+1, string(detailedJSON))
		}
	}

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
				log.Printf("[TMUX] 会话 %s 的原始创建时间: %s", session.Name, session.Created)
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
					log.Printf("[TMUX] 成功将创建时间 %s 解析为时间戳: %v", createdTime, parsedTime)
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
					log.Printf("[TMUX] 更新会话 %s 的启动时间为: %s (从原始创建时间: %s)",
						sessionName, info.StartTime, createdTime)
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
			parts := strings.Split(sessionName, "_")
			if len(parts) >= 3 && parts[0] == "dstserver" {
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
						log.Printf("[TMUX] 成功将新会话创建时间 %s 解析为时间戳: %v", createdTime, parsedTime)
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
						log.Printf("[TMUX] 新会话 %s 的启动时间设置为: %s (从原始创建时间: %s)",
							sessionName, startTime, createdTime)
					} else {
						log.Printf("[TMUX][警告] 无法解析新会话 %s 的创建时间: %s, 错误: %v",
							sessionName, createdTime, parseErr)
					}
				}

				info := ServerInfo{
					SessionName: sessionName,
					ArchiveName: parts[1],
					WorldName:   parts[2],
					ServerMode:  "unknown", // 未知模式
					Status:      "running",
					StartTime:   startTime,
				}

				// 添加到结果和映射中
				result = append(result, info)
				serverInfoMap[sessionName] = &info
			}
		}
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[TMUX] 已列出所有饥荒服务器会话，共 %d 个, 耗时: %v", len(result), elapsedTime)
	return result, nil
}

// GetSessionInfo 获取会话信息
func GetSessionInfo(sessionName string) (map[string]string, error) {
	startTime := time.Now()
	log.Printf("[TMUX] 开始获取会话信息 会话名: %s", sessionName)

	// 初始化tmux客户端
	log.Printf("[TMUX] 初始化tmux客户端")
	tmux, err := gotmux.DefaultTmux()
	if err != nil {
		log.Printf("[TMUX][错误] 初始化tmux失败: %v", err)
		return nil, fmt.Errorf("初始化tmux失败: %v", err)
	}

	// 获取会话
	log.Printf("[TMUX] 获取会话 会话名: %s", sessionName)
	session, err := tmux.GetSessionByName(sessionName)
	if err != nil {
		log.Printf("[TMUX][错误] 获取会话失败: %v", err)
		return nil, fmt.Errorf("获取会话失败: %v", err)
	}

	// 解析会话名称获取存档和世界信息
	parts := strings.Split(sessionName, "_")
	if len(parts) < 3 {
		log.Printf("[TMUX][错误] 会话名称格式不正确: %s", sessionName)
		return nil, fmt.Errorf("会话名称格式不正确: %s", sessionName)
	}

	info := map[string]string{
		"SessionName": sessionName,
		"ArchiveName": parts[1],
		"WorldName":   parts[2],
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
