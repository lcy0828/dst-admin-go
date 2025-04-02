package tmux

import (
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/GianlucaP106/gotmux/gotmux"
)

// DSTServer 表示一个饥荒服务器实例
type DSTServer struct {
	ArchiveName  string // 存档名称
	WorldName    string // 世界名称
	UGCDirectory string // 模组目录
	StorageRoot  string // 存档根目录
	ConfDir      string // 配置目录
	SessionName  string // tmux会话名称
	tmux         *gotmux.Tmux
}

// NewDSTServer 创建一个新的饥荒服务器实例
func NewDSTServer(archiveName, worldName, ugcDirectory, storageRoot, confDir string) (*DSTServer, error) {
	// 初始化tmux客户端
	tmux, err := gotmux.DefaultTmux()
	if err != nil {
		return nil, fmt.Errorf("初始化tmux失败: %v", err)
	}

	// 创建会话名称
	sessionName := fmt.Sprintf("dstserver_%s_%s", archiveName, worldName)

	return &DSTServer{
		ArchiveName:  archiveName,
		WorldName:    worldName,
		UGCDirectory: ugcDirectory,
		StorageRoot:  storageRoot,
		ConfDir:      confDir,
		SessionName:  sessionName,
		tmux:         tmux,
	}, nil
}

// IsRunning 检查服务器是否正在运行
func (s *DSTServer) IsRunning() (bool, error) {
	// 列出所有会话
	sessions, err := s.tmux.ListSessions()

	if err != nil {
		return false, fmt.Errorf("获取tmux会话列表失败: %v", err)
	}

	// 检查是否存在指定名称的会话
	for _, session := range sessions {
		if session.Name == s.SessionName {
			return true, nil
		}
	}

	return false, nil

}

// Start 启动饥荒服务器
func (s *DSTServer) Start() error {
	// 检查服务器是否已经在运行
	running, err := s.IsRunning()
	if err != nil {
		return err
	}
	if running {
		return fmt.Errorf("服务器已经在运行中: %s", s.SessionName)
	}

	// 构建启动命令
	startCmd := fmt.Sprintf("./dontstarve_dedicated_server_nullrenderer -ugc_directory %s -persistent_storage_root %s -conf_dir %s -cluster %s -shard %s",
		s.UGCDirectory, s.StorageRoot, s.ConfDir, s.ArchiveName, s.WorldName)

	// 使用gotmux的Command方法创建会话
	_, err = s.tmux.Command("new-session", "-s", s.SessionName, "-d", startCmd)
	if err != nil {
		return fmt.Errorf("创建tmux会话失败: %v", err)
	}

	log.Printf("已启动饥荒服务器: %s", s.SessionName)
	return nil
}

// Stop 停止饥荒服务器
func (s *DSTServer) Stop() error {
	// 检查服务器是否在运行
	running, err := s.IsRunning()
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("服务器未运行: %s", s.SessionName)
	}

	// 向会话发送关闭命令
	err = s.SendCommand("c_shutdown(true)")
	if err != nil {
		return fmt.Errorf("发送关闭命令失败: %v", err)
	}

	log.Printf("已发送关闭命令到服务器: %s", s.SessionName)
	return nil
}

// SendCommand 向服务器发送命令
func (s *DSTServer) SendCommand(command string) error {
	// 检查服务器是否在运行
	running, err := s.IsRunning()
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("服务器未运行: %s", s.SessionName)
	}

	// 获取会话
	session, err := s.tmux.GetSessionByName(s.SessionName)
	if err != nil {
		return fmt.Errorf("获取会话失败: %v", err)
	}

	// 获取窗口
	windows, err := session.ListWindows()
	if err != nil {
		return fmt.Errorf("获取窗口列表失败: %v", err)
	}
	if len(windows) == 0 {
		return fmt.Errorf("会话没有窗口: %s", s.SessionName)
	}

	// 获取第一个窗口的第一个面板
	panes, err := windows[0].ListPanes()
	if err != nil {
		return fmt.Errorf("获取面板列表失败: %v", err)
	}
	if len(panes) == 0 {
		return fmt.Errorf("窗口没有面板: %s", s.SessionName)
	}

	// 向面板发送命令
	// 使用gotmux的Command方法发送命令
	_, err = s.tmux.Command("send-keys", "-t", s.SessionName, command, "C-m")
	if err != nil {
		return fmt.Errorf("发送命令失败: %v", err)
	}

	log.Printf("已发送命令到服务器: %s, 命令: %s", s.SessionName, command)
	return nil
}

// KillSession 强制终止会话
func (s *DSTServer) KillSession() error {
	// 检查服务器是否在运行
	running, err := s.IsRunning()
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("服务器未运行: %s", s.SessionName)
	}

	// 使用Command方法终止会话
	_, err = s.tmux.Command("kill-session", "-t", s.SessionName)
	if err != nil {
		return fmt.Errorf("终止会话失败: %v", err)
	}

	log.Printf("已强制终止会话: %s", s.SessionName)
	return nil
}

// ListDSTServers 列出所有饥荒服务器会话
func ListDSTServers() ([]string, error) {
	// 初始化tmux客户端
	tmux, err := gotmux.DefaultTmux()
	if err != nil {
		return nil, fmt.Errorf("初始化tmux失败: %v", err)
	}

	// 列出所有会话
	sessions, err := tmux.ListSessions()
	if err != nil {
		return nil, fmt.Errorf("获取tmux会话列表失败: %v", err)
	}

	// 筛选出饥荒服务器会话
	var dstSessions []string
	for _, session := range sessions {
		if strings.HasPrefix(session.Name, "dstserver_") {
			dstSessions = append(dstSessions, session.Name)
		}
	}

	return dstSessions, nil
}

// GetSessionInfo 获取会话信息
func GetSessionInfo(sessionName string) (map[string]string, error) {
	// 初始化tmux客户端
	tmux, err := gotmux.DefaultTmux()
	if err != nil {
		return nil, fmt.Errorf("初始化tmux失败: %v", err)
	}

	// 获取会话
	session, err := tmux.GetSessionByName(sessionName)
	if err != nil {
		return nil, fmt.Errorf("获取会话失败: %v", err)
	}

	// 解析会话名称获取存档和世界信息
	parts := strings.Split(sessionName, "_")
	if len(parts) < 3 {
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

	return info, nil
}

// ExecuteShellCommand 执行shell命令
func ExecuteShellCommand(command string) (string, error) {
	cmd := exec.Command("bash", "-c", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("执行命令失败: %v, 输出: %s", err, string(output))
	}
	return string(output), nil
}

// ExecuteTmuxCommand 执行tmux命令
func ExecuteTmuxCommand(args ...string) (string, error) {
	// 初始化tmux客户端
	tmux, err := gotmux.DefaultTmux()
	if err != nil {
		return "", fmt.Errorf("初始化tmux失败: %v", err)
	}

	// 执行命令
	return tmux.Command(args...)
}
