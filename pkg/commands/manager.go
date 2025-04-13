package commands

import (
	"dont/models"
	"dont/pkg/commands/builtin"
	"dont/pkg/commands/types"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// CommandManager 基于数据库的命令管理器
type CommandManager struct {
	// 所有命令的映射，键为命令ID
	commands map[string]*types.Command
	// 按类别缓存的命令，键为类别名称
	categoryCache map[string][]*types.Command
	// 保护并发访问的互斥锁
	mu sync.RWMutex
	// 最后一次刷新时间
	lastRefreshTime time.Time
}

// NewCommandManager 创建一个新的命令管理器
func NewCommandManager() *CommandManager {
	return &CommandManager{
		commands:      make(map[string]*types.Command),
		categoryCache: make(map[string][]*types.Command),
	}
}

// Initialize 初始化命令管理器，加载所有命令
func (m *CommandManager) Initialize() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 从数据库加载所有命令
	dbCommands, err := models.GetAllCommands()
	if err != nil {
		log.Printf("[CommandManager][ERROR] 从数据库加载命令失败: %v", err)
		return fmt.Errorf("从数据库加载命令失败: %w", err)
	}

	// 清空现有缓存
	m.commands = make(map[string]*types.Command)
	m.categoryCache = make(map[string][]*types.Command)

	// 将命令添加到映射中并构建类别缓存
	for _, dbCmd := range dbCommands {
		cmd := dbCmd.ToTypeCommand()
		m.commands[cmd.ID] = cmd

		// 添加到类别缓存
		m.categoryCache[cmd.Category] = append(m.categoryCache[cmd.Category], cmd)
	}
	log.Printf("[CommandManager] 已从数据库加载 %d 个命令", len(dbCommands))

	// 检查是否需要初始化内置命令
	if len(dbCommands) == 0 || !m.hasBuiltinCommands() {
		log.Printf("[CommandManager] 数据库中没有内置命令，开始初始化...")
		if err := m.registerBuiltinCommands(); err != nil {
			log.Printf("[CommandManager][ERROR] 初始化内置命令失败: %v", err)
			return fmt.Errorf("初始化内置命令失败: %w", err)
		}
	}

	// 记录刷新时间
	m.lastRefreshTime = time.Now()

	return nil
}

// hasBuiltinCommands 检查是否已有内置命令
func (m *CommandManager) hasBuiltinCommands() bool {
	log.Printf("[CommandManager] 检查是否已有内置命令, 当前命令数量: %d", len(m.commands))

	var builtinCount int
	for id, cmd := range m.commands {
		if cmd.IsBuiltin {
			builtinCount++
			log.Printf("[CommandManager] 发现内置命令: %s (%s)", cmd.Name, id)
		}
	}

	log.Printf("[CommandManager] 内置命令检查结果: 共有 %d 个内置命令", builtinCount)
	return builtinCount > 0
}

// registerBuiltinCommands 注册所有内置命令
func (m *CommandManager) registerBuiltinCommands() error {
	// 获取内置命令
	builtinCommands := builtin.GetBuiltinCommands()

	// 将内置命令保存到数据库
	if err := models.InitBuiltinCommands(builtinCommands); err != nil {
		return fmt.Errorf("保存内置命令到数据库失败: %w", err)
	}

	// 重新加载命令
	dbCommands, err := models.GetAllCommands()
	if err != nil {
		return fmt.Errorf("重新加载命令失败: %w", err)
	}

	// 清空现有缓存
	m.commands = make(map[string]*types.Command)
	m.categoryCache = make(map[string][]*types.Command)

	// 将命令添加到映射中并构建类别缓存
	for _, dbCmd := range dbCommands {
		cmd := dbCmd.ToTypeCommand()
		m.commands[cmd.ID] = cmd

		// 添加到类别缓存
		m.categoryCache[cmd.Category] = append(m.categoryCache[cmd.Category], cmd)
	}

	log.Printf("[CommandManager] 已注册 %d 个内置命令", len(builtinCommands))
	return nil
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

	// 使用类别缓存
	if commands, ok := m.categoryCache[category]; ok {
		// 创建副本以避免外部修改
		result := make([]*types.Command, len(commands))
		copy(result, commands)
		return result
	}

	// 如果缓存中没有，返回空列表
	return []*types.Command{}
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

	// 添加到数据库
	dbCmd := models.FromTypeCommand(cmd)
	if err := models.CreateCommand(dbCmd); err != nil {
		log.Printf("[CommandManager][ERROR] 添加命令到数据库失败: %v", err)
		return fmt.Errorf("添加命令到数据库失败: %w", err)
	}

	// 添加到映射
	m.commands[cmd.ID] = cmd

	// 更新类别缓存
	m.categoryCache[cmd.Category] = append(m.categoryCache[cmd.Category], cmd)

	// 更新刷新时间
	m.lastRefreshTime = time.Now()

	log.Printf("[CommandManager] 成功添加命令: %s (%s)", cmd.Name, cmd.ID)
	return nil
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

	// 保存原始类别，用于更新类别缓存
	oldCategory := existingCmd.Category

	// 更新数据库
	dbCmd := models.FromTypeCommand(cmd)
	if err := models.UpdateCommand(dbCmd); err != nil {
		log.Printf("[CommandManager][ERROR] 更新命令到数据库失败: %v", err)
		return fmt.Errorf("更新命令到数据库失败: %w", err)
	}

	// 更新映射
	cmd.IsBuiltin = false // 确保不会将自定义命令标记为内置命令
	m.commands[cmd.ID] = cmd

	// 更新类别缓存
	if oldCategory != cmd.Category {
		// 如果类别发生变化，需要从旧类别中移除，并添加到新类别
		// 从旧类别中移除
		if commands, ok := m.categoryCache[oldCategory]; ok {
			newCommands := make([]*types.Command, 0, len(commands))
			for _, c := range commands {
				if c.ID != cmd.ID {
					newCommands = append(newCommands, c)
				}
			}
			m.categoryCache[oldCategory] = newCommands
		}

		// 添加到新类别
		m.categoryCache[cmd.Category] = append(m.categoryCache[cmd.Category], cmd)
	} else {
		// 如果类别没有变化，只需要更新现有类别中的命令
		if commands, ok := m.categoryCache[cmd.Category]; ok {
			for i, c := range commands {
				if c.ID == cmd.ID {
					commands[i] = cmd
					break
				}
			}
		}
	}

	// 更新刷新时间
	m.lastRefreshTime = time.Now()

	log.Printf("[CommandManager] 成功更新命令: %s (%s)", cmd.Name, cmd.ID)
	return nil
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

	// 从数据库删除
	if err := models.DeleteCommand(id); err != nil {
		log.Printf("[CommandManager][ERROR] 从数据库删除命令失败: %v", err)
		return fmt.Errorf("从数据库删除命令失败: %w", err)
	}

	// 从类别缓存中删除
	if commands, ok := m.categoryCache[cmd.Category]; ok {
		newCommands := make([]*types.Command, 0, len(commands))
		for _, c := range commands {
			if c.ID != id {
				newCommands = append(newCommands, c)
			}
		}
		m.categoryCache[cmd.Category] = newCommands
	}

	// 从映射中删除
	delete(m.commands, id)

	// 更新刷新时间
	m.lastRefreshTime = time.Now()

	log.Printf("[CommandManager] 成功删除命令: %s (%s)", cmd.Name, cmd.ID)
	return nil
}

// RefreshCommands 从数据库刷新命令
func (m *CommandManager) RefreshCommands() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 从数据库加载所有命令
	dbCommands, err := models.GetAllCommands()
	if err != nil {
		log.Printf("[CommandManager][ERROR] 从数据库刷新命令失败: %v", err)
		return fmt.Errorf("从数据库刷新命令失败: %w", err)
	}

	// 清空现有缓存
	m.commands = make(map[string]*types.Command)
	m.categoryCache = make(map[string][]*types.Command)

	// 将命令添加到映射中并构建类别缓存
	for _, dbCmd := range dbCommands {
		cmd := dbCmd.ToTypeCommand()
		m.commands[cmd.ID] = cmd

		// 添加到类别缓存
		m.categoryCache[cmd.Category] = append(m.categoryCache[cmd.Category], cmd)
	}

	// 更新刷新时间
	m.lastRefreshTime = time.Now()

	log.Printf("[CommandManager] 已刷新 %d 个命令", len(dbCommands))
	return nil
}
