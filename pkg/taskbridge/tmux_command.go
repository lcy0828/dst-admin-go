package taskbridge

import (
	"context"
	"dont/pkg/commands"
	"encoding/base32"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

var ErrRuntimeConsoleUnavailable = errors.New("legacy tmux task is disabled until it is mapped to a managed Runtime")

type RuntimeConsole interface {
	Send(context.Context, string, string, string) error
}

// TmuxCommandExecutor 提供执行tmux命令的功能
type TmuxCommandExecutor struct {
	commandManager commands.CommandManagerInterface
	runtime        RuntimeConsole
}

// NewTmuxCommandExecutor 创建一个新的tmux命令执行器
func NewTmuxCommandExecutor(
	commandManager commands.CommandManagerInterface,
	dstSavePath string,
	dstUGCPath string,
	dstServerPath string,
	dstServerMode string,
	runtimes ...RuntimeConsole,
) *TmuxCommandExecutor {
	var runtime RuntimeConsole
	if len(runtimes) > 0 {
		runtime = runtimes[0]
	}
	return &TmuxCommandExecutor{commandManager: commandManager, runtime: runtime}
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
	cluster, shard, ok := ParseManagedSessionName(sessionName)
	if !ok {
		log.Printf("[TaskBridge] 会话名称格式不正确: %s", sessionName)
		return "", fmt.Errorf("%w: 无法从旧会话名可靠映射 Room/Shard", ErrRuntimeConsoleUnavailable)
	}
	if e.runtime == nil {
		return "", ErrRuntimeConsoleUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.runtime.Send(ctx, cluster, shard, command); err != nil {
		log.Printf("[TaskBridge] 发送命令失败: %v 会话名: %s, 命令: %s", err, sessionName, command)
		return "", fmt.Errorf("发送命令失败: %v", err)
	}

	log.Printf("[TaskBridge] 命令发送成功 会话名: %s, 命令: %s", sessionName, command)
	return fmt.Sprintf("命令发送成功: %s", command), nil
}

func ParseManagedSessionName(sessionName string) (string, string, bool) {
	parts := strings.Split(strings.TrimSpace(sessionName), "_")
	if len(parts) == 4 && parts[0] == "dstserver" && parts[1] == "v2" {
		encoding := base32.StdEncoding.WithPadding(base32.NoPadding)
		cluster, clusterErr := encoding.DecodeString(parts[2])
		shard, shardErr := encoding.DecodeString(parts[3])
		return string(cluster), string(shard), clusterErr == nil && shardErr == nil && len(cluster) > 0 && len(shard) > 0
	}
	// Ambiguous underscore-delimited names cannot be mapped safely.
	if len(parts) == 3 && parts[0] == "dstserver" && parts[1] != "" && parts[2] != "" {
		return parts[1], parts[2], true
	}
	return "", "", false
}
