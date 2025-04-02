package tmux

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
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

	// 列出所有会话
	log.Printf("[TMUX] 正在获取tmux会话列表")
	sessions, err := s.tmux.ListSessions()

	if err != nil {
		log.Printf("[TMUX][错误] 获取tmux会话列表失败: %v", err)
		return false, fmt.Errorf("获取tmux会话列表失败: %v", err)
	}

	// 检查是否存在指定名称的会话
	for _, session := range sessions {
		if session.Name == s.SessionName {
			log.Printf("[TMUX] 服务器正在运行 会话名: %s", s.SessionName)
			return true, nil
		}
	}

	log.Printf("[TMUX] 服务器未运行 会话名: %s", s.SessionName)
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

// ListDSTServers 列出所有饥荒服务器会话
func ListDSTServers() ([]string, error) {
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

	// 筛选出饥荒服务器会话
	var dstSessions []string
	for _, session := range sessions {
		if strings.HasPrefix(session.Name, "dstserver_") {
			dstSessions = append(dstSessions, session.Name)
		}
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[TMUX] 已列出所有饥荒服务器会话，共 %d 个, 耗时: %v", len(dstSessions), elapsedTime)
	return dstSessions, nil
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
