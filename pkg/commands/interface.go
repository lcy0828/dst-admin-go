package commands

import (
	"dont/pkg/commands/types"
)

// CommandManagerInterface 命令管理器接口
type CommandManagerInterface interface {
	// Initialize 初始化命令管理器
	Initialize() error

	// GetCommand 根据ID获取命令
	GetCommand(id string) (*types.Command, error)

	// GetAllCommands 获取所有命令
	GetAllCommands() []*types.Command

	// GetCommandsByCategory 获取指定类别的所有命令
	GetCommandsByCategory(category string) []*types.Command

	// AddCommand 添加一个新的自定义命令
	AddCommand(cmd *types.Command) error

	// UpdateCommand 更新一个命令
	UpdateCommand(cmd *types.Command) error

	// DeleteCommand 删除一个命令
	DeleteCommand(id string) error

	// RefreshCommands 从数据库刷新命令
	RefreshCommands() error
}
