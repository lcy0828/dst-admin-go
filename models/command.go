package models

import (
	"dont/pkg/commands/types"
	"errors"
	"fmt"
	"log"
	"time"
)

// Command 命令数据库模型
type Command struct {
	ID          int       `gorm:"primary_key" json:"id"`
	CommandID   string    `gorm:"unique_index" json:"command_id"` // 命令唯一标识符
	Name        string    `json:"name"`                           // 命令名称
	Description string    `json:"description"`                    // 命令描述
	Category    string    `json:"category"`                       // 命令类别
	IsBuiltin   bool      `json:"is_builtin"`                     // 是否为内置命令
	Script      string    `json:"script"`                         // 命令脚本
	NeedsParams bool      `json:"needs_params"`                   // 是否需要参数
	ParamDesc   string    `json:"param_desc"`                     // 参数描述
	Example     string    `json:"example"`                        // 使用示例
	CreatedAt   time.Time `json:"created_at"`                     // 创建时间
	UpdatedAt   time.Time `json:"updated_at"`                     // 更新时间
}

// InitCommandTable 初始化命令表
func InitCommandTable() {
	log.Println("[命令表] 开始初始化命令表...")

	// 检查表是否存在
	hasTable := db.HasTable(&Command{})
	log.Printf("[命令表] 表是否存在: %v", hasTable)

	// 自动迁移表结构
	if err := db.AutoMigrate(&Command{}).Error; err != nil {
		log.Printf("[命令表][ERROR] 初始化命令表失败: %v", err)
		return
	}

	// 检查表中是否有数据
	var count int
	db.Model(&Command{}).Count(&count)
	log.Printf("[命令表] 表中已有 %d 条命令记录", count)

	log.Println("[命令表] 命令表初始化完成")
}

// ToTypeCommand 将数据库模型转换为类型模型
func (c *Command) ToTypeCommand() *types.Command {
	return &types.Command{
		ID:          c.CommandID,
		Name:        c.Name,
		Description: c.Description,
		Category:    c.Category,
		IsBuiltin:   c.IsBuiltin,
		Script:      c.Script,
		NeedsParams: c.NeedsParams,
		ParamDesc:   c.ParamDesc,
		Example:     c.Example,
	}
}

// FromTypeCommand 从类型模型创建数据库模型
func FromTypeCommand(tc *types.Command) *Command {
	now := time.Now()
	return &Command{
		CommandID:   tc.ID,
		Name:        tc.Name,
		Description: tc.Description,
		Category:    tc.Category,
		IsBuiltin:   tc.IsBuiltin,
		Script:      tc.Script,
		NeedsParams: tc.NeedsParams,
		ParamDesc:   tc.ParamDesc,
		Example:     tc.Example,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// GetCommandByID 根据命令ID获取命令
func GetCommandByID(commandID string) (*Command, error) {
	var command Command
	if err := db.Where("command_id = ?", commandID).First(&command).Error; err != nil {
		return nil, err
	}
	return &command, nil
}

// GetAllCommands 获取所有命令
func GetAllCommands() ([]*Command, error) {
	var commands []*Command
	if err := db.Find(&commands).Error; err != nil {
		return nil, err
	}
	return commands, nil
}

// GetCommandsByCategory 获取指定类别的命令
func GetCommandsByCategory(category string) ([]*Command, error) {
	var commands []*Command
	if err := db.Where("category = ?", category).Find(&commands).Error; err != nil {
		return nil, err
	}
	return commands, nil
}

// CreateCommand 创建新命令
func CreateCommand(command *Command) error {
	// 检查命令ID是否已存在
	var count int
	db.Model(&Command{}).Where("command_id = ?", command.CommandID).Count(&count)
	if count > 0 {
		return errors.New("命令ID已存在")
	}

	// 设置创建和更新时间
	now := time.Now()
	command.CreatedAt = now
	command.UpdatedAt = now

	return db.Create(command).Error
}

// UpdateCommand 更新命令
func UpdateCommand(command *Command) error {
	// 检查命令是否存在
	var existingCommand Command
	if err := db.Where("command_id = ?", command.CommandID).First(&existingCommand).Error; err != nil {
		return errors.New("命令不存在")
	}

	// 不允许修改内置命令
	if existingCommand.IsBuiltin {
		return errors.New("不能修改内置命令")
	}

	// 设置更新时间
	command.UpdatedAt = time.Now()
	command.ID = existingCommand.ID // 确保ID正确
	command.IsBuiltin = false       // 确保不会将自定义命令标记为内置命令

	return db.Model(&Command{}).Where("id = ?", existingCommand.ID).Updates(command).Error
}

// DeleteCommand 删除命令
func DeleteCommand(commandID string) error {
	// 检查命令是否存在
	var command Command
	if err := db.Where("command_id = ?", commandID).First(&command).Error; err != nil {
		return errors.New("命令不存在")
	}

	// 不允许删除内置命令
	if command.IsBuiltin {
		return errors.New("不能删除内置命令")
	}

	return db.Where("command_id = ?", commandID).Delete(&Command{}).Error
}

// InitBuiltinCommands 初始化内置命令
func InitBuiltinCommands(builtinCommands []*types.Command) error {
	log.Printf("[内置命令] 开始初始化 %d 个内置命令...", len(builtinCommands))

	// 开始事务
	tx := db.Begin()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[内置命令][ERROR] 初始化过程发生异常: %v", r)
			tx.Rollback()
		}
	}()

	if err := tx.Error; err != nil {
		log.Printf("[内置命令][ERROR] 创建事务失败: %v", err)
		return fmt.Errorf("创建事务失败: %w", err)
	}

	// 获取所有现有的内置命令
	var existingBuiltinCommands []*Command
	if err := tx.Where("is_builtin = ?", true).Find(&existingBuiltinCommands).Error; err != nil {
		log.Printf("[内置命令][ERROR] 查询现有内置命令失败: %v", err)
		tx.Rollback()
		return fmt.Errorf("查询现有内置命令失败: %w", err)
	}

	log.Printf("[内置命令] 数据库中已有 %d 个内置命令", len(existingBuiltinCommands))

	// 创建命令ID到命令的映射
	existingMap := make(map[string]*Command)
	for _, cmd := range existingBuiltinCommands {
		existingMap[cmd.CommandID] = cmd
		log.Printf("[内置命令] 现有内置命令: %s (%s)", cmd.Name, cmd.CommandID)
	}

	// 处理每个内置命令
	var updatedCount, createdCount int
	for _, builtinCmd := range builtinCommands {
		dbCmd := FromTypeCommand(builtinCmd)
		log.Printf("[内置命令] 处理内置命令: %s (%s)", dbCmd.Name, dbCmd.CommandID)

		// 检查命令是否已存在
		if existing, ok := existingMap[dbCmd.CommandID]; ok {
			// 更新现有命令
			existing.Name = dbCmd.Name
			existing.Description = dbCmd.Description
			existing.Category = dbCmd.Category
			existing.Script = dbCmd.Script
			existing.NeedsParams = dbCmd.NeedsParams
			existing.ParamDesc = dbCmd.ParamDesc
			existing.Example = dbCmd.Example
			existing.UpdatedAt = time.Now()

			if err := tx.Save(existing).Error; err != nil {
				log.Printf("[内置命令][ERROR] 更新内置命令失败: %v", err)
				tx.Rollback()
				return fmt.Errorf("更新内置命令失败: %w", err)
			}

			updatedCount++
			log.Printf("[内置命令] 更新内置命令成功: %s (%s)", dbCmd.Name, dbCmd.CommandID)

			// 从映射中删除，剩下的将被删除
			delete(existingMap, dbCmd.CommandID)
		} else {
			// 创建新命令
			if err := tx.Create(dbCmd).Error; err != nil {
				log.Printf("[内置命令][ERROR] 创建内置命令失败: %v", err)
				tx.Rollback()
				return fmt.Errorf("创建内置命令失败: %w", err)
			}

			createdCount++
			log.Printf("[内置命令] 创建内置命令成功: %s (%s)", dbCmd.Name, dbCmd.CommandID)
		}
	}

	// 删除不再存在的内置命令
	var deletedCount int
	for _, cmd := range existingMap {
		log.Printf("[内置命令] 删除过时的内置命令: %s (%s)", cmd.Name, cmd.CommandID)
		if err := tx.Delete(cmd).Error; err != nil {
			log.Printf("[内置命令][ERROR] 删除过时的内置命令失败: %v", err)
			tx.Rollback()
			return fmt.Errorf("删除过时的内置命令失败: %w", err)
		}
		deletedCount++
	}

	// 提交事务
	if err := tx.Commit().Error; err != nil {
		log.Printf("[内置命令][ERROR] 提交事务失败: %v", err)
		return fmt.Errorf("提交事务失败: %w", err)
	}

	log.Printf("[内置命令] 初始化完成: 创建 %d 个, 更新 %d 个, 删除 %d 个", createdCount, updatedCount, deletedCount)
	return nil
}
