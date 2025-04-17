package taskbridge

import (
	"dont/pkg/commands"
	"dont/tmux"
	"fmt"
	"log"
	"path/filepath"
	"strings"
)

// TmuxCommandExecutor 提供执行tmux命令的功能
type TmuxCommandExecutor struct {
	commandManager commands.CommandManagerInterface
	dstSavePath    string
	dstUGCPath     string
	dstServerPath  string
	dstServerMode  string
}

// NewTmuxCommandExecutor 创建一个新的tmux命令执行器
func NewTmuxCommandExecutor(
	commandManager commands.CommandManagerInterface,
	dstSavePath string,
	dstUGCPath string,
	dstServerPath string,
	dstServerMode string,
) *TmuxCommandExecutor {
	return &TmuxCommandExecutor{
		commandManager: commandManager,
		dstSavePath:    dstSavePath,
		dstUGCPath:     dstUGCPath,
		dstServerPath:  dstServerPath,
		dstServerMode:  dstServerMode,
	}
}

// ExecuteCommand 执行模块化命令
func (e *TmuxCommandExecutor) ExecuteCommand(sessionName, commandID string, params []string) (string, error) {
	log.Printf("[TaskBridge] 执行模块化命令 会话名: %s, 命令ID: %s", sessionName, commandID)

	// 获取命令
	cmd, err := e.commandManager.GetCommand(commandID)
	if err != nil {
		log.Printf("[TaskBridge] 找不到命令: %s", commandID)
		return "", fmt.Errorf("找不到命令: %s", commandID)
	}

	// 验证参数
	if !cmd.ValidateParams(params...) {
		log.Printf("[TaskBridge] 命令参数无效: %v", params)
		return "", fmt.Errorf("命令参数无效")
	}

	// 生成脚本
	script := cmd.GenerateScript(params...)
	log.Printf("[TaskBridge] 生成脚本: %s", script)

	// 执行命令
	return e.executeOnServer(sessionName, script)
}

// ExecuteRawCommand 执行原始命令
func (e *TmuxCommandExecutor) ExecuteRawCommand(sessionName, command string) (string, error) {
	log.Printf("[TaskBridge] 执行原始命令 会话名: %s, 命令: %s", sessionName, command)
	return e.executeOnServer(sessionName, command)
}

// executeOnServer 在指定服务器上执行命令
func (e *TmuxCommandExecutor) executeOnServer(sessionName, command string) (string, error) {
	// 解析会话名称获取存档和世界信息
	parts := strings.Split(sessionName, "_")
	if len(parts) < 3 || parts[0] != "dstserver" {
		log.Printf("[TaskBridge] 会话名称格式不正确: %s", sessionName)
		return "", fmt.Errorf("会话名称格式不正确，应为 dstserver_存档名_世界名")
	}

	// 获取服务器实例引用
	server, err := tmux.NewDSTServer(
		parts[1],
		parts[2],
		e.dstUGCPath,
		filepath.Dir(e.dstSavePath),
		"DoNotStarveTogether",
		e.dstServerPath,
		e.dstServerMode,
	)
	if err != nil {
		log.Printf("[TaskBridge] 获取服务器实例引用失败: %v 会话名: %s", err, sessionName)
		return "", fmt.Errorf("获取服务器实例引用失败: %v", err)
	}

	// 发送命令
	if err := server.SendCommand(command); err != nil {
		log.Printf("[TaskBridge] 发送命令失败: %v 会话名: %s, 命令: %s", err, sessionName, command)
		return "", fmt.Errorf("发送命令失败: %v", err)
	}

	log.Printf("[TaskBridge] 命令发送成功 会话名: %s, 命令: %s", sessionName, command)
	return fmt.Sprintf("命令发送成功: %s", command), nil
}
