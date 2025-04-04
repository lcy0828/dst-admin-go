package commands

import (
	"dont/pkg/commands/builtin"
	"dont/pkg/commands/types"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// CommandManager 管理所有可用的命令
type CommandManager struct {
	// 所有命令的映射，键为命令ID
	commands map[string]*types.Command
	// 文件保存路径
	storagePath string
	// 保护并发访问的互斥锁
	mu sync.RWMutex
}

// NewCommandManager 创建一个新的命令管理器
func NewCommandManager(storagePath string) *CommandManager {
	return &CommandManager{
		commands:    make(map[string]*types.Command),
		storagePath: storagePath,
	}
}

// Initialize 初始化命令管理器，加载所有命令
func (m *CommandManager) Initialize() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 确保存储目录存在
	if err := os.MkdirAll(filepath.Dir(m.storagePath), 0755); err != nil {
		return fmt.Errorf("无法创建命令存储目录: %v", err)
	}

	// 尝试从文件加载自定义命令
	if _, err := os.Stat(m.storagePath); !os.IsNotExist(err) {
		data, err := os.ReadFile(m.storagePath)
		if err != nil {
			return fmt.Errorf("无法读取命令文件: %v", err)
		}

		var commands []*types.Command
		if err := json.Unmarshal(data, &commands); err != nil {
			return fmt.Errorf("无法解析命令文件: %v", err)
		}

		// 将命令添加到映射中
		for _, cmd := range commands {
			m.commands[cmd.ID] = cmd
		}
		log.Printf("[CommandManager] 已加载 %d 个自定义命令", len(commands))
	}

	// 注册内置命令
	m.registerBuiltinCommands()

	return nil
}

// registerBuiltinCommands 注册所有内置命令
func (m *CommandManager) registerBuiltinCommands() {
	// 这里将从内置命令包中注册所有命令
	builtinCommands := builtin.GetBuiltinCommands()

	for _, cmd := range builtinCommands {
		m.commands[cmd.ID] = cmd
	}
	log.Printf("[CommandManager] 已注册 %d 个内置命令", len(builtinCommands))
}

// GetCommand 根据ID获取命令
func (m *CommandManager) GetCommand(id string) (*types.Command, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cmd, exists := m.commands[id]
	if !exists {
		return nil, errors.New("命令不存在")
	}
	return cmd, nil
}

// GetAllCommands 获取所有命令
func (m *CommandManager) GetAllCommands() []*types.Command {
	m.mu.RLock()
	defer m.mu.RUnlock()

	commands := make([]*types.Command, 0, len(m.commands))
	for _, cmd := range m.commands {
		commands = append(commands, cmd)
	}
	return commands
}

// GetCommandsByCategory 获取指定类别的所有命令
func (m *CommandManager) GetCommandsByCategory(category string) []*types.Command {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var commands []*types.Command
	for _, cmd := range m.commands {
		if cmd.Category == category {
			commands = append(commands, cmd)
		}
	}
	return commands
}

// AddCommand 添加一个新的自定义命令
func (m *CommandManager) AddCommand(cmd *types.Command) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 确保ID不重复
	if _, exists := m.commands[cmd.ID]; exists {
		return errors.New("命令ID已存在")
	}

	// 标记为非内置命令
	cmd.IsBuiltin = false

	// 添加到映射
	m.commands[cmd.ID] = cmd

	// 保存到文件
	return m.saveToFile()
}

// UpdateCommand 更新一个命令
func (m *CommandManager) UpdateCommand(cmd *types.Command) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 检查命令是否存在
	existingCmd, exists := m.commands[cmd.ID]
	if !exists {
		return errors.New("命令不存在")
	}

	// 不允许修改内置命令
	if existingCmd.IsBuiltin {
		return errors.New("不能修改内置命令")
	}

	// 更新命令，但保持非内置标志
	cmd.IsBuiltin = false
	m.commands[cmd.ID] = cmd

	// 保存到文件
	return m.saveToFile()
}

// DeleteCommand 删除一个命令
func (m *CommandManager) DeleteCommand(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 检查命令是否存在
	cmd, exists := m.commands[id]
	if !exists {
		return errors.New("命令不存在")
	}

	// 不允许删除内置命令
	if cmd.IsBuiltin {
		return errors.New("不能删除内置命令")
	}

	// 从映射中删除
	delete(m.commands, id)

	// 保存到文件
	return m.saveToFile()
}

// saveToFile 将自定义命令保存到文件
func (m *CommandManager) saveToFile() error {
	// 筛选出所有非内置命令
	var customCommands []*types.Command
	for _, cmd := range m.commands {
		if !cmd.IsBuiltin {
			customCommands = append(customCommands, cmd)
		}
	}

	// 将命令序列化为JSON
	data, err := json.MarshalIndent(customCommands, "", "  ")
	if err != nil {
		return fmt.Errorf("无法序列化命令: %v", err)
	}

	// 写入文件
	if err := os.WriteFile(m.storagePath, data, fs.ModePerm); err != nil {
		return fmt.Errorf("无法写入命令文件: %v", err)
	}

	log.Printf("[CommandManager] 已保存 %d 个自定义命令到文件", len(customCommands))
	return nil
}
